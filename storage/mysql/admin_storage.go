// Copyright 2017 Google LLC. All Rights Reserved.
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
	"encoding/gob"
	"fmt"
	"sync"
	"time"

	"github.com/google/trillian"
	"github.com/google/trillian/storage"
	"github.com/google/trillian/storage/mysql/mysqlpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog/v2"
)

const (
	defaultSequenceIntervalSeconds = 60

	nonDeletedWhere = " WHERE (Deleted IS NULL OR Deleted = 'false')"

	selectTrees = `
		SELECT
			TreeId,
			TreeState,
			TreeType,
			HashStrategy,
			HashAlgorithm,
			SignatureAlgorithm,
			DisplayName,
			Description,
			CreateTimeMillis,
			UpdateTimeMillis,
			PrivateKey, -- Unused
			PublicKey, -- Used to store StorageSettings
			MaxRootDurationMillis,
			Deleted,
			DeleteTimeMillis
		FROM Trees`
	selectNonDeletedTrees = selectTrees + nonDeletedWhere
	selectTreeByID        = selectTrees + " WHERE TreeId = ?"

	updateTreeSQL = `UPDATE Trees
		SET TreeState = ?, TreeType = ?, DisplayName = ?, Description = ?, UpdateTimeMillis = ?, MaxRootDurationMillis = ?, PrivateKey = ?
		WHERE TreeId = ?`
)

// NewAdminStorage returns a MySQL storage.AdminStorage implementation backed by DB.
func NewAdminStorage(db *sql.DB) *mysqlAdminStorage {
	return &mysqlAdminStorage{db}
}

// mysqlAdminStorage implements storage.AdminStorage
type mysqlAdminStorage struct {
	db *sql.DB
}

func (s *mysqlAdminStorage) Snapshot(ctx context.Context) (storage.ReadOnlyAdminTX, error) {
	return s.beginInternal(ctx)
}

func (s *mysqlAdminStorage) beginInternal(ctx context.Context) (storage.AdminTX, error) {
	tx, err := s.db.BeginTx(ctx, nil /* opts */)
	if err != nil {
		return nil, err
	}
	return &adminTX{tx: tx}, nil
}

func (s *mysqlAdminStorage) ReadWriteTransaction(ctx context.Context, f storage.AdminTXFunc) error {
	tx, err := s.beginInternal(ctx)
	if err != nil {
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
	return tx.Commit()
}

func (s *mysqlAdminStorage) CheckDatabaseAccessible(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

type adminTX struct {
	tx *sql.Tx

	// mu guards reads/writes on closed, which happen on Commit/Close methods.
	//
	// We don't check closed on methods apart from the ones above, as we trust tx
	// to keep tabs on its state, and hence fail to do queries after closed.
	mu     sync.RWMutex
	closed bool
}

func (t *adminTX) Commit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return t.tx.Commit()
}

func (t *adminTX) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	if err := t.tx.Rollback(); err != nil && err != sql.ErrTxDone {
		return err
	}
	return nil
}

func (t *adminTX) GetTree(ctx context.Context, treeID int64) (*trillian.Tree, error) {
	stmt, err := t.tx.PrepareContext(ctx, selectTreeByID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := stmt.Close(); err != nil {
			klog.Errorf("stmt.Close(): %v", err)
		}
	}()

	// GetTree is an entry point for most RPCs, let's provide somewhat nicer error messages.
	tree, err := readTree(stmt.QueryRowContext(ctx, treeID))
	switch {
	case err == sql.ErrNoRows:
		// ErrNoRows doesn't provide useful information, so we don't forward it.
		return nil, status.Errorf(codes.NotFound, "tree %v not found", treeID)
	case err != nil:
		return nil, fmt.Errorf("error reading tree %v: %v", treeID, err)
	}
	return tree, nil
}

func (t *adminTX) ListTrees(ctx context.Context, includeDeleted bool) ([]*trillian.Tree, error) {
	var query string
	if includeDeleted {
		query = selectTrees
	} else {
		query = selectNonDeletedTrees
	}

	stmt, err := t.tx.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := stmt.Close(); err != nil {
			klog.Errorf("stmt.Close(): %v", err)
		}
	}()
	rows, err := stmt.QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			klog.Errorf("rows.Close(): %v", err)
		}
	}()
	trees := []*trillian.Tree{}
	for rows.Next() {
		tree, err := readTree(rows)
		if err != nil {
			return nil, err
		}
		trees = append(trees, tree)
	}
	return trees, nil
}

func (t *adminTX) CreateTree(ctx context.Context, tree *trillian.Tree) (*trillian.Tree, error) {
	if err := storage.ValidateTreeForCreation(ctx, tree); err != nil {
		return nil, err
	}
	if err := validateStorageSettings(tree); err != nil {
		return nil, err
	}

	id, err := storage.NewTreeID()
	if err != nil {
		return nil, err
	}

	// Use the time truncated-to-millis throughout, as that's what's stored.
	nowMillis := toMillisSinceEpoch(time.Now())
	now := fromMillisSinceEpoch(nowMillis)

	newTree := proto.Clone(tree).(*trillian.Tree)
	newTree.TreeId = id
	newTree.CreateTime = timestamppb.New(now)
	if err := newTree.CreateTime.CheckValid(); err != nil {
		return nil, fmt.Errorf("failed to build create time: %w", err)
	}
	newTree.UpdateTime = timestamppb.New(now)
	if err := newTree.UpdateTime.CheckValid(); err != nil {
		return nil, fmt.Errorf("failed to build update time: %w", err)
	}
	if err := newTree.MaxRootDuration.CheckValid(); err != nil {
		return nil, fmt.Errorf("could not parse MaxRootDuration: %w", err)
	}
	rootDuration := newTree.MaxRootDuration.AsDuration()

	// When creating a new tree we automatically add StorageSettings to allow us to
	// determine that this tree can support newer storage features. When reading
	// trees that do not have this StorageSettings populated, it must be assumed that
	// the tree was created with the oldest settings.
	// The gist of this code is super simple: create a new StorageSettings with the most
	// modern defaults if the created tree does not have one, and then create a struct that
	// represents this to store in the DB. Unfortunately because this involves anypb, struct
	// copies, marshalling, and proper error handling this turns into a scary amount of code.
	if tree.StorageSettings != nil {
		newTree.StorageSettings = proto.Clone(tree.StorageSettings).(*anypb.Any)
	} else {
		o := &mysqlpb.StorageOptions{
			SubtreeRevisions: false, // Default behaviour for new trees is to skip writing subtree revisions.
		}
		a, err := anypb.New(o)
		if err != nil {
			return nil, fmt.Errorf("failed to create new StorageOptions: %v", err)
		}
		newTree.StorageSettings = a
	}
	o := &mysqlpb.StorageOptions{}
	if err := anypb.UnmarshalTo(newTree.StorageSettings, o, proto.UnmarshalOptions{}); err != nil {
		return nil, fmt.Errorf("failed to unmarshal StorageOptions: %v", err)
	}
	ss := storageSettings{
		Revisioned: o.SubtreeRevisions,
	}
	buff := &bytes.Buffer{}
	enc := gob.NewEncoder(buff)
	if err := enc.Encode(ss); err != nil {
		return nil, fmt.Errorf("failed to encode storageSettings: %v", err)
	}

	insertTreeStmt, err := t.tx.PrepareContext(
		ctx,
		`INSERT INTO Trees(
			TreeId,
			TreeState,
			TreeType,
			HashStrategy,
			HashAlgorithm,
			SignatureAlgorithm,
			DisplayName,
			Description,
			CreateTimeMillis,
			UpdateTimeMillis,
			PrivateKey, -- Unused
			PublicKey, -- Used to store StorageSettings
			MaxRootDurationMillis)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := insertTreeStmt.Close(); err != nil {
			klog.Errorf("insertTreeStmt.Close(): %v", err)
		}
	}()

	_, err = insertTreeStmt.ExecContext(
		ctx,
		newTree.TreeId,
		newTree.TreeState.String(),
		newTree.TreeType.String(),
		"RFC6962_SHA256", // Unused, filling in for backward compatibility.
		"SHA256",         // Unused, filling in for backward compatibility.
		"ECDSA",          // Unused, filling in for backward compatibility.
		newTree.DisplayName,
		newTree.Description,
		nowMillis,
		nowMillis,
		[]byte{},     // PrivateKey: Unused, filling in for backward compatibility.
		buff.Bytes(), // Using the otherwise unused PublicKey for storing StorageSettings.
		rootDuration/time.Millisecond,
	)
	if err != nil {
		return nil, err
	}

	// MySQL silently truncates data when running in non-strict mode.
	// We shouldn't be using non-strict modes, but let's guard against it
	// anyway.
	if _, err := t.GetTree(ctx, newTree.TreeId); err != nil {
		// GetTree will fail for truncated enums (they get recorded as
		// empty strings, which will not match any known value).
		return nil, fmt.Errorf("enum truncated: %v", err)
	}

	insertControlStmt, err := t.tx.PrepareContext(
		ctx,
		`INSERT INTO TreeControl(
			TreeId,
			SigningEnabled,
			SequencingEnabled,
			SequenceIntervalSeconds)
		VALUES(?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := insertControlStmt.Close(); err != nil {
			klog.Errorf("insertControlStmt.Close(): %v", err)
		}
	}()
	_, err = insertControlStmt.ExecContext(
		ctx,
		newTree.TreeId,
		true, /* SigningEnabled */
		true, /* SequencingEnabled */
		defaultSequenceIntervalSeconds,
	)
	if err != nil {
		return nil, err
	}

	return newTree, nil
}

func (t *adminTX) UpdateTree(ctx context.Context, treeID int64, updateFunc func(*trillian.Tree)) (*trillian.Tree, error) {
	tree, err := t.GetTree(ctx, treeID)
	if err != nil {
		return nil, err
	}

	beforeUpdate := proto.Clone(tree).(*trillian.Tree)
	updateFunc(tree)
	if err := storage.ValidateTreeForUpdate(ctx, beforeUpdate, tree); err != nil {
		return nil, err
	}
	if err := validateStorageSettings(tree); err != nil {
		return nil, err
	}

	// TODO(pavelkalinnikov): When switching TreeType from PREORDERED_LOG to LOG,
	// ensure all entries in SequencedLeafData are integrated.

	// Use the time truncated-to-millis throughout, as that's what's stored.
	nowMillis := toMillisSinceEpoch(time.Now())
	now := fromMillisSinceEpoch(nowMillis)
	tree.UpdateTime = timestamppb.New(now)
	if err != nil {
		return nil, fmt.Errorf("failed to build update time: %v", err)
	}
	if err := tree.MaxRootDuration.CheckValid(); err != nil {
		return nil, fmt.Errorf("could not parse MaxRootDuration: %w", err)
	}
	rootDuration := tree.MaxRootDuration.AsDuration()

	stmt, err := t.tx.PrepareContext(ctx, updateTreeSQL)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := stmt.Close(); err != nil {
			klog.Errorf("stmt.Close(): %v", err)
		}
	}()

	if _, err = stmt.ExecContext(
		ctx,
		tree.TreeState.String(),
		tree.TreeType.String(),
		tree.DisplayName,
		tree.Description,
		nowMillis,
		rootDuration/time.Millisecond,
		[]byte{}, // PrivateKey: Unused, filling in for backward compatibility.
		// PublicKey should not be updated with any storageSettings here without
		// a lot of thought put into it. At the moment storageSettings are inferred
		// when reading the tree, even if no value is stored in the database.
		tree.TreeId); err != nil {
		return nil, err
	}

	return tree, nil
}

func (t *adminTX) SoftDeleteTree(ctx context.Context, treeID int64) (*trillian.Tree, error) {
	return t.updateDeleted(ctx, treeID, true /* deleted */, toMillisSinceEpoch(time.Now()) /* deleteTimeMillis */)
}

func (t *adminTX) UndeleteTree(ctx context.Context, treeID int64) (*trillian.Tree, error) {
	return t.updateDeleted(ctx, treeID, false /* deleted */, nil /* deleteTimeMillis */)
}

// updateDeleted updates the Deleted and DeleteTimeMillis fields of the specified tree.
// deleteTimeMillis must be either an int64 (in millis since epoch) or nil.
func (t *adminTX) updateDeleted(ctx context.Context, treeID int64, deleted bool, deleteTimeMillis interface{}) (*trillian.Tree, error) {
	if err := validateDeleted(ctx, t.tx, treeID, !deleted); err != nil {
		return nil, err
	}
	if _, err := t.tx.ExecContext(
		ctx,
		"UPDATE Trees SET Deleted = ?, DeleteTimeMillis = ? WHERE TreeId = ?",
		deleted, deleteTimeMillis, treeID); err != nil {
		return nil, err
	}
	return t.GetTree(ctx, treeID)
}

func (t *adminTX) HardDeleteTree(ctx context.Context, treeID int64) error {
	if err := validateDeleted(ctx, t.tx, treeID, true /* wantDeleted */); err != nil {
		return err
	}

	// TreeControl didn't have "ON DELETE CASCADE" on previous versions, so let's hit it explicitly
	if _, err := t.tx.ExecContext(ctx, "DELETE FROM TreeControl WHERE TreeId = ?", treeID); err != nil {
		return err
	}
	_, err := t.tx.ExecContext(ctx, "DELETE FROM Trees WHERE TreeId = ?", treeID)
	return err
}

func validateDeleted(ctx context.Context, tx *sql.Tx, treeID int64, wantDeleted bool) error {
	var nullDeleted sql.NullBool
	switch err := tx.QueryRowContext(ctx, "SELECT Deleted FROM Trees WHERE TreeId = ?", treeID).Scan(&nullDeleted); {
	case err == sql.ErrNoRows:
		return status.Errorf(codes.NotFound, "tree %v not found", treeID)
	case err != nil:
		return err
	}

	switch deleted := nullDeleted.Valid && nullDeleted.Bool; {
	case wantDeleted && !deleted:
		return status.Errorf(codes.FailedPrecondition, "tree %v is not soft deleted", treeID)
	case !wantDeleted && deleted:
		return status.Errorf(codes.FailedPrecondition, "tree %v already soft deleted", treeID)
	}
	return nil
}

func validateStorageSettings(tree *trillian.Tree) error {
	if tree.StorageSettings.MessageIs(&mysqlpb.StorageOptions{}) {
		return nil
	}
	if tree.StorageSettings == nil {
		// No storage settings is OK, we'll just use the defaults for new trees
		return nil
	}
	return fmt.Errorf("storage_settings must be nil or mysqlpb.StorageOptions, but got %v", tree.StorageSettings)
}

// storageSettings allows us to persist storage settings to the DB.
// It is a tempting trap to use protos for this, but the way they encode
// makes it impossible to tell the difference between no value ever written
// and a value that was written with the default values for each field.
// Using an explicit struct and gob encoding allows us to tell the difference.
type storageSettings struct {
	Revisioned bool
}
