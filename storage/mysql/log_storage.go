// Copyright 2016 Google LLC. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/trillian"
	"github.com/google/trillian/monitoring"
	"github.com/google/trillian/storage"
	"github.com/google/trillian/storage/cache"
	"github.com/google/trillian/storage/tree"
	"github.com/google/trillian/types"
	"github.com/transparency-dev/merkle/compact"
	"github.com/transparency-dev/merkle/rfc6962"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog/v2"
)

const (
	valuesPlaceholder5 = "(?,?,?,?,?)"

	insertLeafDataSQL      = "INSERT INTO LeafData(TreeId,LeafIdentityHash,LeafValue,ExtraData,QueueTimestampNanos) VALUES" + valuesPlaceholder5
	insertSequencedLeafSQL = "INSERT INTO SequencedLeafData(TreeId,LeafIdentityHash,MerkleLeafHash,SequenceNumber,IntegrateTimestampNanos) VALUES"

	selectNonDeletedTreeIDByTypeAndStateSQL = `
		SELECT TreeId FROM Trees
		  WHERE TreeType IN(?,?)
		  AND TreeState IN(?,?)
		  AND (Deleted IS NULL OR Deleted = 'false')`

	selectLatestSignedLogRootSQL = `SELECT TreeHeadTimestamp,TreeSize,RootHash,TreeRevision,RootSignature
			FROM TreeHead WHERE TreeId=?
			ORDER BY TreeHeadTimestamp DESC LIMIT 1`

	selectLeavesByRangeSQL = `SELECT s.MerkleLeafHash,l.LeafIdentityHash,l.LeafValue,s.SequenceNumber,l.ExtraData,l.QueueTimestampNanos,s.IntegrateTimestampNanos
			FROM LeafData l,SequencedLeafData s
			WHERE l.LeafIdentityHash = s.LeafIdentityHash
			AND s.SequenceNumber >= ? AND s.SequenceNumber < ? AND l.TreeId = ? AND s.TreeId = l.TreeId` + orderBySequenceNumberSQL

	// These statements need to be expanded to provide the correct number of parameter placeholders.
	selectLeavesByMerkleHashSQL = `SELECT s.MerkleLeafHash,l.LeafIdentityHash,l.LeafValue,s.SequenceNumber,l.ExtraData,l.QueueTimestampNanos,s.IntegrateTimestampNanos
			FROM LeafData l,SequencedLeafData s
			WHERE l.LeafIdentityHash = s.LeafIdentityHash
			AND s.MerkleLeafHash IN (` + placeholderSQL + `) AND l.TreeId = ? AND s.TreeId = l.TreeId`
	// TODO(#1548): rework the code so the dummy hash isn't needed (e.g. this assumes hash size is 32)
	dummyMerkleLeafHash = "00000000000000000000000000000000"
	// This statement returns a dummy Merkle leaf hash value (which must be
	// of the right size) so that its signature matches that of the other
	// leaf-selection statements.
	selectLeavesByLeafIdentityHashSQL = `SELECT '` + dummyMerkleLeafHash + `',l.LeafIdentityHash,l.LeafValue,-1,l.ExtraData,l.QueueTimestampNanos,s.IntegrateTimestampNanos
			FROM LeafData l LEFT JOIN SequencedLeafData s ON (l.LeafIdentityHash = s.LeafIdentityHash AND l.TreeID = s.TreeID)
			WHERE l.LeafIdentityHash IN (` + placeholderSQL + `) AND l.TreeId = ?`

	// Same as above except with leaves ordered by sequence so we only incur this cost when necessary
	orderBySequenceNumberSQL                     = " ORDER BY s.SequenceNumber"
	selectLeavesByMerkleHashOrderedBySequenceSQL = selectLeavesByMerkleHashSQL + orderBySequenceNumberSQL

	logIDLabel = "logid"
)

var (
	once             sync.Once
	queuedCounter    monitoring.Counter
	queuedDupCounter monitoring.Counter
	dequeuedCounter  monitoring.Counter

	queueLatency            monitoring.Histogram
	queueInsertLatency      monitoring.Histogram
	queueReadLatency        monitoring.Histogram
	queueInsertLeafLatency  monitoring.Histogram
	queueInsertEntryLatency monitoring.Histogram
	dequeueLatency          monitoring.Histogram
	dequeueSelectLatency    monitoring.Histogram
	dequeueRemoveLatency    monitoring.Histogram
)

func createMetrics(mf monitoring.MetricFactory) {
	queuedCounter = mf.NewCounter("mysql_queued_leaves", "Number of leaves queued", logIDLabel)
	queuedDupCounter = mf.NewCounter("mysql_queued_dup_leaves", "Number of duplicate leaves queued", logIDLabel)
	dequeuedCounter = mf.NewCounter("mysql_dequeued_leaves", "Number of leaves dequeued", logIDLabel)

	queueLatency = mf.NewHistogram("mysql_queue_leaves_latency", "Latency of queue leaves operation in seconds", logIDLabel)
	queueInsertLatency = mf.NewHistogram("mysql_queue_leaves_latency_insert", "Latency of insertion part of queue leaves operation in seconds", logIDLabel)
	queueReadLatency = mf.NewHistogram("mysql_queue_leaves_latency_read_dups", "Latency of read-duplicates part of queue leaves operation in seconds", logIDLabel)
	queueInsertLeafLatency = mf.NewHistogram("mysql_queue_leaf_latency_leaf", "Latency of insert-leaf part of queue (single) leaf operation in seconds", logIDLabel)
	queueInsertEntryLatency = mf.NewHistogram("mysql_queue_leaf_latency_entry", "Latency of insert-entry part of queue (single) leaf operation in seconds", logIDLabel)

	dequeueLatency = mf.NewHistogram("mysql_dequeue_leaves_latency", "Latency of dequeue leaves operation in seconds", logIDLabel)
	dequeueSelectLatency = mf.NewHistogram("mysql_dequeue_leaves_latency_select", "Latency of selection part of dequeue leaves operation in seconds", logIDLabel)
	dequeueRemoveLatency = mf.NewHistogram("mysql_dequeue_leaves_latency_remove", "Latency of removal part of dequeue leaves operation in seconds", logIDLabel)
}

func labelForTX(t *logTreeTX) string {
	return strconv.FormatInt(t.treeID, 10)
}

func observe(hist monitoring.Histogram, duration time.Duration, label string) {
	hist.Observe(duration.Seconds(), label)
}

type mySQLLogStorage struct {
	*mySQLTreeStorage
	admin         storage.AdminStorage
	metricFactory monitoring.MetricFactory
}

// NewLogStorage creates a storage.LogStorage instance for the specified MySQL URL.
// It assumes storage.AdminStorage is backed by the same MySQL database as well.
func NewLogStorage(db *sql.DB, mf monitoring.MetricFactory) storage.LogStorage {
	if mf == nil {
		mf = monitoring.InertMetricFactory{}
	}
	return &mySQLLogStorage{
		admin:            NewAdminStorage(db),
		mySQLTreeStorage: newTreeStorage(db),
		metricFactory:    mf,
	}
}

func (m *mySQLLogStorage) CheckDatabaseAccessible(ctx context.Context) error {
	return m.db.PingContext(ctx)
}

func (m *mySQLLogStorage) getLeavesByMerkleHashStmt(ctx context.Context, num int, orderBySequence bool) (*sql.Stmt, error) {
	if orderBySequence {
		return m.getStmt(ctx, selectLeavesByMerkleHashOrderedBySequenceSQL, num, "?", "?")
	}

	return m.getStmt(ctx, selectLeavesByMerkleHashSQL, num, "?", "?")
}

func (m *mySQLLogStorage) getLeavesByLeafIdentityHashStmt(ctx context.Context, num int) (*sql.Stmt, error) {
	return m.getStmt(ctx, selectLeavesByLeafIdentityHashSQL, num, "?", "?")
}

func (m *mySQLLogStorage) GetActiveLogIDs(ctx context.Context) ([]int64, error) {
	// Include logs that are DRAINING in the active list as we're still
	// integrating leaves into them.
	rows, err := m.db.QueryContext(
		ctx, selectNonDeletedTreeIDByTypeAndStateSQL,
		trillian.TreeType_LOG.String(), trillian.TreeType_PREORDERED_LOG.String(),
		trillian.TreeState_ACTIVE.String(), trillian.TreeState_DRAINING.String())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			klog.Errorf("rows.Close(): %v", err)
		}
	}()
	ids := []int64{}
	for rows.Next() {
		var treeID int64
		if err := rows.Scan(&treeID); err != nil {
			return nil, err
		}
		ids = append(ids, treeID)
	}
	return ids, rows.Err()
}

func (m *mySQLLogStorage) beginInternal(ctx context.Context, tree *trillian.Tree) (*logTreeTX, error) {
	once.Do(func() {
		createMetrics(m.metricFactory)
	})

	stCache := cache.NewLogSubtreeCache(rfc6962.DefaultHasher)
	ttx, err := m.beginTreeTx(ctx, tree, rfc6962.DefaultHasher.Size(), stCache)
	if err != nil && err != storage.ErrTreeNeedsInit {
		return nil, err
	}

	ltx := &logTreeTX{
		treeTX:   ttx,
		ls:       m,
		dequeued: make(map[string]dequeuedLeaf),
	}
	ltx.slr, ltx.readRev, err = ltx.fetchLatestRoot(ctx)
	if err == storage.ErrTreeNeedsInit {
		ltx.writeRevision = 0
		return ltx, err
	} else if err != nil {
		if err := ttx.Close(); err != nil {
			klog.Errorf("ttx.Close(): %v", err)
		}
		return nil, err
	}

	if err := ltx.root.UnmarshalBinary(ltx.slr.LogRoot); err != nil {
		if err := ttx.Close(); err != nil {
			klog.Errorf("ttx.Close(): %v", err)
		}
		return nil, err
	}

	ltx.writeRevision = ltx.readRev + 1
	return ltx, nil
}

// TODO(pavelkalinnikov): This and many other methods of this storage
// implementation can leak a specific sql.ErrTxDone all the way to the client,
// if the transaction is rolled back as a result of a canceled context. It must
// return "generic" errors, and only log the specific ones for debugging.
func (m *mySQLLogStorage) ReadWriteTransaction(ctx context.Context, tree *trillian.Tree, f storage.LogTXFunc) error {
	tx, err := m.beginInternal(ctx, tree)
	if err != nil && err != storage.ErrTreeNeedsInit {
		return err
	}
	defer func() {
		if err := tx.Close(); err != nil {
			klog.Errorf("tx.Close(): %v", err)
		}
	}()
	if err := f(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (m *mySQLLogStorage) AddSequencedLeaves(ctx context.Context, tree *trillian.Tree, leaves []*trillian.LogLeaf, timestamp time.Time) ([]*trillian.QueuedLogLeaf, error) {
	tx, err := m.beginInternal(ctx, tree)
	if tx != nil {
		// Ensure we don't leak the transaction. For example if we get an
		// ErrTreeNeedsInit from beginInternal() or if AddSequencedLeaves fails
		// below.
		defer func() {
			if err := tx.Close(); err != nil {
				klog.Errorf("tx.Close(): %v", err)
			}
		}()
	}
	if err != nil {
		return nil, err
	}
	res, err := tx.AddSequencedLeaves(ctx, leaves, timestamp)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

func (m *mySQLLogStorage) SnapshotForTree(ctx context.Context, tree *trillian.Tree) (storage.ReadOnlyLogTreeTX, error) {
	tx, err := m.beginInternal(ctx, tree)
	if err != nil && err != storage.ErrTreeNeedsInit {
		return nil, err
	}
	return tx, err
}

func (m *mySQLLogStorage) QueueLeaves(ctx context.Context, tree *trillian.Tree, leaves []*trillian.LogLeaf, queueTimestamp time.Time) ([]*trillian.QueuedLogLeaf, error) {
	tx, err := m.beginInternal(ctx, tree)
	if tx != nil {
		// Ensure we don't leak the transaction. For example if we get an
		// ErrTreeNeedsInit from beginInternal() or if QueueLeaves fails
		// below.
		defer func() {
			if err := tx.Close(); err != nil {
				klog.Errorf("tx.Close(): %v", err)
			}
		}()
	}
	if err != nil {
		return nil, err
	}
	existing, err := tx.QueueLeaves(ctx, leaves, queueTimestamp)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	ret := make([]*trillian.QueuedLogLeaf, len(leaves))
	for i, e := range existing {
		if e != nil {
			ret[i] = &trillian.QueuedLogLeaf{
				Leaf:   e,
				Status: status.Newf(codes.AlreadyExists, "leaf already exists: %v", e.LeafIdentityHash).Proto(),
			}
			continue
		}
		ret[i] = &trillian.QueuedLogLeaf{Leaf: leaves[i]}
	}
	return ret, nil
}

type logTreeTX struct {
	treeTX
	ls       *mySQLLogStorage
	root     types.LogRootV1
	readRev  int64
	slr      *trillian.SignedLogRoot
	dequeued map[string]dequeuedLeaf
}

// GetMerkleNodes returns the requested nodes at the read revision.
func (t *logTreeTX) GetMerkleNodes(ctx context.Context, ids []compact.NodeID) ([]tree.Node, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.subtreeCache.GetNodes(ids, t.getSubtreesAtRev(ctx, t.readRev))
}

func (t *logTreeTX) DequeueLeaves(ctx context.Context, limit int, cutoffTime time.Time) ([]*trillian.LogLeaf, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.treeType == trillian.TreeType_PREORDERED_LOG {
		// TODO(pavelkalinnikov): Optimize this by fetching only the required
		// fields of LogLeaf. We can avoid joining with LeafData table here.
		return t.getLeavesByRangeInternal(ctx, int64(t.root.TreeSize), int64(limit))
	}

	start := time.Now()
	stx, err := t.tx.PrepareContext(ctx, selectQueuedLeavesSQL)
	if err != nil {
		klog.Warningf("Failed to prepare dequeue select: %s", err)
		return nil, err
	}
	defer func() {
		if err := stx.Close(); err != nil {
			klog.Errorf("stx.Close(): %v", err)
		}
	}()

	leaves := make([]*trillian.LogLeaf, 0, limit)
	rows, err := stx.QueryContext(ctx, t.treeID, cutoffTime.UnixNano(), limit)
	if err != nil {
		klog.Warningf("Failed to select rows for work: %s", err)
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			klog.Errorf("rows.Close(): %v", err)
		}
	}()

	for rows.Next() {
		leaf, dqInfo, err := t.dequeueLeaf(rows)
		if err != nil {
			klog.Warningf("Error dequeuing leaf: %v", err)
			return nil, err
		}

		if len(leaf.LeafIdentityHash) != t.hashSizeBytes {
			return nil, errors.New("dequeued a leaf with incorrect hash size")
		}

		k := string(leaf.LeafIdentityHash)
		if _, ok := t.dequeued[k]; ok {
			// dupe, user probably called DequeueLeaves more than once.
			continue
		}
		t.dequeued[k] = dqInfo
		leaves = append(leaves, leaf)
	}

	if rows.Err() != nil {
		return nil, rows.Err()
	}
	label := labelForTX(t)
	observe(dequeueSelectLatency, time.Since(start), label)
	observe(dequeueLatency, time.Since(start), label)
	dequeuedCounter.Add(float64(len(leaves)), label)

	return leaves, nil
}

// sortLeavesForInsert returns a slice containing the passed in leaves sorted
// by LeafIdentityHash, and paired with their original positions.
// QueueLeaves and AddSequencedLeaves use this to make the order that LeafData
// row locks are acquired deterministic and reduce the chance of deadlocks.
func sortLeavesForInsert(leaves []*trillian.LogLeaf) []leafAndPosition {
	ordLeaves := make([]leafAndPosition, len(leaves))
	for i, leaf := range leaves {
		ordLeaves[i] = leafAndPosition{leaf: leaf, idx: i}
	}
	sort.Sort(byLeafIdentityHashWithPosition(ordLeaves))
	return ordLeaves
}

func (t *logTreeTX) QueueLeaves(ctx context.Context, leaves []*trillian.LogLeaf, queueTimestamp time.Time) ([]*trillian.LogLeaf, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Don't accept batches if any of the leaves are invalid.
	for _, leaf := range leaves {
		if len(leaf.LeafIdentityHash) != t.hashSizeBytes {
			return nil, fmt.Errorf("queued leaf must have a leaf ID hash of length %d", t.hashSizeBytes)
		}
		leaf.QueueTimestamp = timestamppb.New(queueTimestamp)
		if err := leaf.QueueTimestamp.CheckValid(); err != nil {
			return nil, fmt.Errorf("got invalid queue timestamp: %w", err)
		}
	}
	start := time.Now()
	label := labelForTX(t)

	ordLeaves := sortLeavesForInsert(leaves)
	existingCount := 0
	existingLeaves := make([]*trillian.LogLeaf, len(leaves))

	for _, ol := range ordLeaves {
		i, leaf := ol.idx, ol.leaf

		leafStart := time.Now()
		if err := leaf.QueueTimestamp.CheckValid(); err != nil {
			return nil, fmt.Errorf("got invalid queue timestamp: %w", err)
		}
		qTimestamp := leaf.QueueTimestamp.AsTime()
		_, err := t.tx.ExecContext(ctx, insertLeafDataSQL, t.treeID, leaf.LeafIdentityHash, leaf.LeafValue, leaf.ExtraData, qTimestamp.UnixNano())
		insertDuration := time.Since(leafStart)
		observe(queueInsertLeafLatency, insertDuration, label)
		if isDuplicateErr(err) {
			// Remember the duplicate leaf, using the requested leaf for now.
			existingLeaves[i] = leaf
			existingCount++
			queuedDupCounter.Inc(label)
			continue
		}
		if err != nil {
			klog.Warningf("Error inserting %d into LeafData: %s", i, err)
			return nil, mysqlToGRPC(err)
		}

		// Create the work queue entry
		args := []interface{}{
			t.treeID,
			leaf.LeafIdentityHash,
			leaf.MerkleLeafHash,
		}
		args = append(args, queueArgs(t.treeID, leaf.LeafIdentityHash, qTimestamp)...)
		_, err = t.tx.ExecContext(
			ctx,
			insertUnsequencedEntrySQL,
			args...,
		)
		if err != nil {
			klog.Warningf("Error inserting into Unsequenced: %s", err)
			return nil, mysqlToGRPC(err)
		}
		leafDuration := time.Since(leafStart)
		observe(queueInsertEntryLatency, (leafDuration - insertDuration), label)
	}
	insertDuration := time.Since(start)
	observe(queueInsertLatency, insertDuration, label)
	queuedCounter.Add(float64(len(leaves)), label)

	if existingCount == 0 {
		return existingLeaves, nil
	}

	// For existing leaves, we need to retrieve the contents.  First collate the desired LeafIdentityHash values
	// We deduplicate the hashes to address https://github.com/google/trillian/issues/3603 but will be mapped
	// back to the existingLeaves slice below
	uniqueLeafMap := make(map[string]struct{}, len(existingLeaves))
	var toRetrieve [][]byte
	for _, existing := range existingLeaves {
		if existing != nil {
			key := string(existing.LeafIdentityHash)
			if _, ok := uniqueLeafMap[key]; !ok {
				uniqueLeafMap[key] = struct{}{}
				toRetrieve = append(toRetrieve, existing.LeafIdentityHash)
			}
		}
	}
	results, err := t.getLeafDataByIdentityHash(ctx, toRetrieve)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve existing leaves: %v", err)
	}
	if len(results) != len(toRetrieve) {
		return nil, fmt.Errorf("failed to retrieve all existing leaves: got %d, want %d", len(results), len(toRetrieve))
	}
	// Replace the requested leaves with the actual leaves.
	for i, requested := range existingLeaves {
		if requested == nil {
			continue
		}
		found := false
		for _, result := range results {
			if bytes.Equal(result.LeafIdentityHash, requested.LeafIdentityHash) {
				existingLeaves[i] = result
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("failed to find existing leaf for hash %x", requested.LeafIdentityHash)
		}
	}
	totalDuration := time.Since(start)
	readDuration := totalDuration - insertDuration
	observe(queueReadLatency, readDuration, label)
	observe(queueLatency, totalDuration, label)

	return existingLeaves, nil
}

func (t *logTreeTX) AddSequencedLeaves(ctx context.Context, leaves []*trillian.LogLeaf, timestamp time.Time) ([]*trillian.QueuedLogLeaf, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	res := make([]*trillian.QueuedLogLeaf, len(leaves))
	ok := status.New(codes.OK, "OK").Proto()

	// Leaves in this transaction are inserted in two tables. For each leaf, if
	// one of the two inserts fails, we remove the side effect by rolling back to
	// a savepoint installed before the first insert of the two.
	const savepoint = "SAVEPOINT AddSequencedLeaves"
	if _, err := t.tx.ExecContext(ctx, savepoint); err != nil {
		klog.Errorf("Error adding savepoint: %s", err)
		return nil, mysqlToGRPC(err)
	}
	// TODO(pavelkalinnikov): Consider performance implication of executing this
	// extra SAVEPOINT, especially for 1-entry batches. Optimize if necessary.

	// Note: LeafData inserts are presumably protected from deadlocks due to
	// sorting, but the order of the corresponding SequencedLeafData inserts
	// becomes indeterministic. However, in a typical case when leaves are
	// supplied in contiguous non-intersecting batches, the chance of having
	// circular dependencies between transactions is significantly lower.
	ordLeaves := sortLeavesForInsert(leaves)
	for _, ol := range ordLeaves {
		i, leaf := ol.idx, ol.leaf

		// This should fail on insert, but catch it early.
		if got, want := len(leaf.LeafIdentityHash), t.hashSizeBytes; got != want {
			return nil, status.Errorf(codes.FailedPrecondition, "leaves[%d] has incorrect hash size %d, want %d", i, got, want)
		}

		if _, err := t.tx.ExecContext(ctx, savepoint); err != nil {
			klog.Errorf("Error updating savepoint: %s", err)
			return nil, mysqlToGRPC(err)
		}

		res[i] = &trillian.QueuedLogLeaf{Status: ok}

		// TODO(pavelkalinnikov): Measure latencies.
		_, err := t.tx.ExecContext(ctx, insertLeafDataSQL,
			t.treeID, leaf.LeafIdentityHash, leaf.LeafValue, leaf.ExtraData, timestamp.UnixNano())
		// TODO(pavelkalinnikov): Detach PREORDERED_LOG integration latency metric.

		// TODO(pavelkalinnikov): Support opting out from duplicates detection.
		if isDuplicateErr(err) {
			res[i].Status = status.New(codes.FailedPrecondition, "conflicting LeafIdentityHash").Proto()
			// Note: No rolling back to savepoint because there is no side effect.
			continue
		} else if err != nil {
			klog.Errorf("Error inserting leaves[%d] into LeafData: %s", i, err)
			return nil, mysqlToGRPC(err)
		}

		_, err = t.tx.ExecContext(ctx, insertSequencedLeafSQL+valuesPlaceholder5,
			t.treeID, leaf.LeafIdentityHash, leaf.MerkleLeafHash, leaf.LeafIndex, 0)
		// TODO(pavelkalinnikov): Update IntegrateTimestamp on integrating the leaf.

		if isDuplicateErr(err) {
			res[i].Status = status.New(codes.FailedPrecondition, "conflicting LeafIndex").Proto()
			if _, err := t.tx.ExecContext(ctx, "ROLLBACK TO "+savepoint); err != nil {
				klog.Errorf("Error rolling back to savepoint: %s", err)
				return nil, mysqlToGRPC(err)
			}
		} else if err != nil {
			klog.Errorf("Error inserting leaves[%d] into SequencedLeafData: %s", i, err)
			return nil, mysqlToGRPC(err)
		}

		// TODO(pavelkalinnikov): Load LeafData for conflicting entries.
	}

	if _, err := t.tx.ExecContext(ctx, "RELEASE "+savepoint); err != nil {
		klog.Errorf("Error releasing savepoint: %s", err)
		return nil, mysqlToGRPC(err)
	}

	return res, nil
}

func (t *logTreeTX) GetLeavesByRange(ctx context.Context, start, count int64) ([]*trillian.LogLeaf, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.getLeavesByRangeInternal(ctx, start, count)
}

func (t *logTreeTX) getLeavesByRangeInternal(ctx context.Context, start, count int64) ([]*trillian.LogLeaf, error) {
	if count <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "invalid count %d, want > 0", count)
	}
	if start < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "invalid start %d, want >= 0", start)
	}

	if t.treeType == trillian.TreeType_LOG {
		treeSize := int64(t.root.TreeSize)
		if treeSize <= 0 {
			return nil, status.Errorf(codes.OutOfRange, "empty tree")
		} else if start >= treeSize {
			return nil, status.Errorf(codes.OutOfRange, "invalid start %d, want < TreeSize(%d)", start, treeSize)
		}
		// Ensure no entries queried/returned beyond the tree.
		if maxCount := treeSize - start; count > maxCount {
			count = maxCount
		}
	}
	// TODO(pavelkalinnikov): Further clip `count` to a safe upper bound like 64k.

	args := []interface{}{start, start + count, t.treeID}
	rows, err := t.tx.QueryContext(ctx, selectLeavesByRangeSQL, args...)
	if err != nil {
		klog.Warningf("Failed to get leaves by range: %s", err)
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			klog.Errorf("rows.Close(): %v", err)
		}
	}()

	ret := make([]*trillian.LogLeaf, 0, count)
	for wantIndex := start; rows.Next(); wantIndex++ {
		leaf := &trillian.LogLeaf{}
		var qTimestamp, iTimestamp int64
		if err := rows.Scan(
			&leaf.MerkleLeafHash,
			&leaf.LeafIdentityHash,
			&leaf.LeafValue,
			&leaf.LeafIndex,
			&leaf.ExtraData,
			&qTimestamp,
			&iTimestamp); err != nil {
			klog.Warningf("Failed to scan merkle leaves: %s", err)
			return nil, err
		}
		if leaf.LeafIndex != wantIndex {
			if wantIndex < int64(t.root.TreeSize) {
				return nil, fmt.Errorf("got unexpected index %d, want %d", leaf.LeafIndex, wantIndex)
			}
			break
		}
		leaf.QueueTimestamp = timestamppb.New(time.Unix(0, qTimestamp))
		if err := leaf.QueueTimestamp.CheckValid(); err != nil {
			return nil, fmt.Errorf("got invalid queue timestamp: %w", err)
		}
		leaf.IntegrateTimestamp = timestamppb.New(time.Unix(0, iTimestamp))
		if err := leaf.IntegrateTimestamp.CheckValid(); err != nil {
			return nil, fmt.Errorf("got invalid integrate timestamp: %w", err)
		}
		ret = append(ret, leaf)
	}
	if err := rows.Err(); err != nil {
		klog.Warningf("Failed to read returned leaves: %s", err)
		return nil, err
	}

	return ret, nil
}

func (t *logTreeTX) GetLeavesByHash(ctx context.Context, leafHashes [][]byte, orderBySequence bool) ([]*trillian.LogLeaf, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tmpl, err := t.ls.getLeavesByMerkleHashStmt(ctx, len(leafHashes), orderBySequence)
	if err != nil {
		return nil, err
	}

	return t.getLeavesByHashInternal(ctx, leafHashes, tmpl, "merkle")
}

// getLeafDataByIdentityHash retrieves leaf data by LeafIdentityHash, returned
// as a slice of LogLeaf objects for convenience.  However, note that the
// returned LogLeaf objects will not have a valid MerkleLeafHash, LeafIndex, or IntegrateTimestamp.
func (t *logTreeTX) getLeafDataByIdentityHash(ctx context.Context, leafHashes [][]byte) ([]*trillian.LogLeaf, error) {
	tmpl, err := t.ls.getLeavesByLeafIdentityHashStmt(ctx, len(leafHashes))
	if err != nil {
		return nil, err
	}
	return t.getLeavesByHashInternal(ctx, leafHashes, tmpl, "leaf-identity")
}

func (t *logTreeTX) LatestSignedLogRoot(ctx context.Context) (*trillian.SignedLogRoot, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.slr == nil {
		return nil, storage.ErrTreeNeedsInit
	}

	return t.slr, nil
}

// fetchLatestRoot reads the latest root and the revision from the DB.
func (t *logTreeTX) fetchLatestRoot(ctx context.Context) (*trillian.SignedLogRoot, int64, error) {
	var timestamp, treeSize, treeRevision int64
	var rootHash, rootSignatureBytes []byte
	if err := t.tx.QueryRowContext(
		ctx, selectLatestSignedLogRootSQL, t.treeID).Scan(
		&timestamp, &treeSize, &rootHash, &treeRevision, &rootSignatureBytes,
	); err == sql.ErrNoRows {
		// It's possible there are no roots for this tree yet
		return nil, 0, storage.ErrTreeNeedsInit
	}

	// Put logRoot back together. Fortunately LogRoot has a deterministic serialization.
	logRoot, err := (&types.LogRootV1{
		RootHash:       rootHash,
		TimestampNanos: uint64(timestamp),
		TreeSize:       uint64(treeSize),
	}).MarshalBinary()
	if err != nil {
		return nil, 0, err
	}

	return &trillian.SignedLogRoot{LogRoot: logRoot}, treeRevision, nil
}

func (t *logTreeTX) StoreSignedLogRoot(ctx context.Context, root *trillian.SignedLogRoot) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	var logRoot types.LogRootV1
	if err := logRoot.UnmarshalBinary(root.LogRoot); err != nil {
		klog.Warningf("Failed to parse log root: %x %v", root.LogRoot, err)
		return err
	}
	if len(logRoot.Metadata) != 0 {
		return fmt.Errorf("unimplemented: mysql storage does not support log root metadata")
	}

	res, err := t.tx.ExecContext(
		ctx,
		insertTreeHeadSQL,
		t.treeID,
		logRoot.TimestampNanos,
		logRoot.TreeSize,
		logRoot.RootHash,
		t.writeRevision,
		[]byte{})
	if err != nil {
		klog.Warningf("Failed to store signed root: %s", err)
	}

	return checkResultOkAndRowCountIs(res, err, 1)
}

func (t *logTreeTX) getLeavesByHashInternal(ctx context.Context, leafHashes [][]byte, tmpl *sql.Stmt, desc string) ([]*trillian.LogLeaf, error) {
	stx := t.tx.StmtContext(ctx, tmpl)
	defer func() {
		if err := stx.Close(); err != nil {
			klog.Errorf("stx.Close(): %v", err)
		}
	}()

	var args []interface{}
	for _, hash := range leafHashes {
		args = append(args, []byte(hash))
	}
	args = append(args, t.treeID)
	rows, err := stx.QueryContext(ctx, args...)
	if err != nil {
		klog.Warningf("Query() %s hash = %v", desc, err)
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			klog.Errorf("rows.Close(): %v", err)
		}
	}()

	// The tree could include duplicates so we don't know how many results will be returned
	var ret []*trillian.LogLeaf
	for rows.Next() {
		leaf := &trillian.LogLeaf{}
		// We might be using a LEFT JOIN in our statement, so leaves which are
		// queued but not yet integrated will have a NULL IntegrateTimestamp
		// when there's no corresponding entry in SequencedLeafData, even though
		// the table definition forbids that, so we use a nullable type here and
		// check its validity below.
		var integrateTS sql.NullInt64
		var queueTS int64

		if err := rows.Scan(&leaf.MerkleLeafHash, &leaf.LeafIdentityHash, &leaf.LeafValue, &leaf.LeafIndex, &leaf.ExtraData, &queueTS, &integrateTS); err != nil {
			klog.Warningf("LogID: %d Scan() %s = %s", t.treeID, desc, err)
			return nil, err
		}
		leaf.QueueTimestamp = timestamppb.New(time.Unix(0, queueTS))
		if err := leaf.QueueTimestamp.CheckValid(); err != nil {
			return nil, fmt.Errorf("got invalid queue timestamp: %w", err)
		}
		if integrateTS.Valid {
			leaf.IntegrateTimestamp = timestamppb.New(time.Unix(0, integrateTS.Int64))
			if err := leaf.IntegrateTimestamp.CheckValid(); err != nil {
				return nil, fmt.Errorf("got invalid integrate timestamp: %w", err)
			}
		}

		if got, want := len(leaf.MerkleLeafHash), t.hashSizeBytes; got != want {
			return nil, fmt.Errorf("LogID: %d Scanned leaf %s does not have hash length %d, got %d", t.treeID, desc, want, got)
		}

		ret = append(ret, leaf)
	}
	if err := rows.Err(); err != nil {
		klog.Warningf("Failed to read returned leaves: %s", err)
		return nil, err
	}

	return ret, nil
}

// leafAndPosition records original position before sort.
type leafAndPosition struct {
	leaf *trillian.LogLeaf
	idx  int
}

// byLeafIdentityHashWithPosition allows sorting (as above), but where we need
// to remember the original position
type byLeafIdentityHashWithPosition []leafAndPosition

func (l byLeafIdentityHashWithPosition) Len() int {
	return len(l)
}

func (l byLeafIdentityHashWithPosition) Swap(i, j int) {
	l[i], l[j] = l[j], l[i]
}

func (l byLeafIdentityHashWithPosition) Less(i, j int) bool {
	return bytes.Compare(l[i].leaf.LeafIdentityHash, l[j].leaf.LeafIdentityHash) == -1
}
