# Experimental Beam Map Generation

Generates [Verifiable Maps](../../docs/papers/VerifiableDataStructures.pdf)
using [Beam Go](https://beam.apache.org/get-started/quickstart-go/).
Generating a map in batch scales better than incremental for large numbers of
key/values.

## Motivation

This experimental map replaces a more interactive DB-backed map which was deleted
and tracked by [#2284](https://github.com/google/trillian/issues/2284).
Unlike Logs, Maps don't have efficient consistency proofs.
Without efficient consistency proofs, clients of the data structure will struggle
to keep in sync with the freshest version of it while verify that everything that
they had previously relied on in the data structure is still present.

One solution proposed to this (e.g. in [Think Global, Act Local](https://arxiv.org/abs/2011.04551))
is to put the roots of the map in a log. Clients will verify their own keyspace
within the map *for each revision* and providing their area is correct, and that
everyone else has also done this for *their* own keyspaces, then evolution of the
map has been verified.

If these map revisions are being created quickly, then the number of revisions to
be verified can require too much computing power for all but very dedicated clients,
and this could easily lead to a situation where verification is not being performed.

This observation that verification cost scales linearly with the number of map
revisions came along at a similar time to performance problems that were identified
with large maps built on top of the interactive DB-backed map. Using a parallelized
batch solution solves both of these issues; the temptation to create map revisions
every few seconds (or less!) is removed, and massive scale is possible through
parallel processing of the subtrees within the map.

## Status

> :warning: **This code is experimental!** This code is free to change outside
> of semantic versioning in the trillian repository.

This batch map has been run at scale to create a map with 2^30 leaves in under
25 minutes.

The resulting map is output as tiles, in which the tree is divided from the
root in a configurable number of prefix strata.
Each strata contains a single byte of the 256-bit path.
Tiles are only output if they are non-empty.

The design of this `batchmap` is as a library rather than a service. The tiles
that are returned must be stored by the client and serving them to users is outside
the remit of this library. This may change in the future as common deployments
are identified.

## Running the demo

### Setup

The map uses Apache Beam to assemble and run a pipeline of tasks to construct the map.
Fortunately there is now a Go local portable runner, so you don't need to set up Python any more!

### Building the map

These instructions are for Linux/MacOS but can likely be adapted for Windows.

In another terminal:
1. Check out this repository and `cd` to the folder containing this README
2. `go run ./cmd/build/mapdemo.go --output=/tmp/mapv1 --runner=PrismRunner`

The pipeline should run and generate files under the ouput directory, each of which contains a tile from the map.
Note that a new file will be constructed for each tile output, which can get very large
if the `key_count` or `prefix_strata` parameters are changed from their default values!
If these parameters are set too high, one could run out of inodes on your file system.
You have been warned.

The demo intends only to show the usage of the API and provide a simple way to test locally running the pipeline.
It is not intended to demonstrate where data would be sourced from, or how the output Tiles should be used.
See the comments in the demo script for more details.

### Verifying the map

This requires a map to have been constructed using the previous instructions.
Verifying that a particular key/value is set correctly within the tiles can be done with the command:
* `go run cmd/verify/verify.go --logtostderr --map_dir=/tmp/mapv1 --key=5`

The `map_dir` must match the directory provided as `output` in the previous stage.
The parameters for `value_salt` and `tree_id` must also match those used in the map
construction as they are used during the value construction/hashing.

If the expected value is committed to correctly by the tiles, then you will see an output line similar to:

```
key 5 found at path 11cd1b2203ad4a3a11ff479d1ee75a59c9f33a73c5f5cf45bda87b656237e9ed, with value '[v1]5' (1e27e661ca57f2231fb41b7ef861ab702ce7412921e4df9eb106db0d8b442227) committed to by map root 4365e3c65742fdfeb60079b677ccf4a264405c0d18fc7db1706690a1b06db73c
```

Setting the `key` parameter to a key outside the range generated in the map will show non-inclusion, as will
changing the `tree_id` or `value_salt` parameters.
