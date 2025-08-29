/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package index

import (
	"context"

	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	c "github.com/hypermodeinc/dgraph/v25/tok/constraints"
	opts "github.com/hypermodeinc/dgraph/v25/tok/options"
)

// IndexFactory is responsible for being able to create, find, and remove
// VectorIndexes. There is no "update" as of now; just remove and create.
//
// It is expected that the IndexFactory has some notion of persistence, but
// it is perfectly happy to support a total in-memory solution. To achieve
// persistence, it is the responsibility of the implementations of IndexFactory
// to reference the persistent storage.
type IndexFactory[T c.Float] interface {
	// The Name returned represents the name of the factory rather than the
	// name of any particular index.
	Name() string

	// Fetch the string of all the options being used
	GetOptions(o opts.Options) string

	// Specifies the set of allowed options and a corresponding means to
	// parse a string version of those options.
	AllowedOptions() opts.AllowedOptions

	// Create is expected to create a VectorIndex, or generate an error
	// if the name already has a corresponding VectorIndex or other problems.
	// The name will be associated with with the generated VectorIndex
	// such that if we Create an index with the name "foo", then later
	// attempt to find the index with name "foo", it will refer to the
	// same object.
	// The set of vectors to use in the index process is defined by
	// source.
	Create(name string, o opts.Options, floatBits int) (VectorIndex[T], error)

	// Find is expected to retrieve the VectorIndex corresponding with the
	// name. If it attempts to find a name that does not exist, the VectorIndex
	// will return as a nil value. It should throw an error in persistent storage
	// issues when accessing information.
	Find(name string) (VectorIndex[T], error)

	// Remove is expected to delete the VectorIndex corresponding with the name.
	// If removing a name that doesn't exist, nothing will happen and no errors
	// are thrown. An error should only be thrown if there is issues accessing
	// persistent storage information.
	Remove(name string) error

	// CreateOrReplace will create a new index -- as defined by the Create
	// function -- if it does not yet exist, otherwise, it will replace any
	// index with the given name.
	CreateOrReplace(name string, o opts.Options, floatBits int) (VectorIndex[T], error)
}

// SearchFilter defines a predicate function that we will use to determine
// whether or not a given vector is "interesting". When used in the context
// of VectorIndex.Search, a true result means that we want to keep the result
// in the returned list, and a false result implies we should skip.
type SearchFilter[T c.Float] func(query, resultVal []T, resultUID uint64) bool

// AcceptAll implements SearchFilter by way of accepting all results.
func AcceptAll[T c.Float](_, _ []T, _ uint64) bool { return true }

// AcceptNone implements SearchFilter by way of rejecting all results.
func AcceptNone[T c.Float](_, _ []T, _ uint64) bool { return false }

// OptionalIndexSupport defines abilities that might not be universally
// supported by all VectorIndex types. A VectorIndex will technically
// define the functions required by OptionalIndexSupport, but may do so
// by way of simply returning an errors.ErrUnsupported result.
type OptionalIndexSupport[T c.Float] interface {
	// SearchWithPath(ctx, c, query, maxResults, filter) is similar to
	// Search(ctx, c, query, maxResults, filter), but returns an extended
	// set of content in the search results.
	// The full contents returned are indicated by the SearchPathResult.
	// See the description there for more info.
	SearchWithPath(
		ctx context.Context,
		c CacheType,
		query []T,
		maxResults int,
		filter SearchFilter[T]) (*SearchPathResult, error)
}

type VectorPartitionStrat[T c.Float] interface {
	FindIndexForSearch(vec []T) ([]int, error)
	FindIndexForInsert(vec []T) (int, error)
	NumPasses() int
	SetNumPasses(int)
	NumSeedVectors() int
	StartBuildPass()
	EndBuildPass()
	AddSeedVector(vec []T)
	AddVector(vec []T) error
	GetCentroids() [][]T
}

// A VectorIndex can be used to Search for vectors and add vectors to an index.
type VectorIndex[T c.Float] interface {
	OptionalIndexSupport[T]

	MergeResults(ctx context.Context, c CacheType, list []uint64, query []T, maxResults int,
		filter SearchFilter[T]) ([]uint64, error)

	// Search will find the uids for a given set of vectors based on the
	// input query, limiting to the specified maximum number of results.
	// The filter parameter indicates that we might discard certain parameters
	// based on some input criteria. The maxResults count is counted *after*
	// being filtered. In other words, we only count those results that had not
	// been filtered out.
	Search(ctx context.Context, c CacheType, query []T,
		maxResults int,
		filter SearchFilter[T]) ([]uint64, error)

	// SearchWithUid will find the uids for a given set of vectors based on the
	// input queryUid, limiting to the specified maximum number of results.
	// The filter parameter indicates that we might discard certain parameters
	// based on some input criteria. The maxResults count is counted *after*
	// being filtered. In other words, we only count those results that had not
	// been filtered out.
	SearchWithUid(ctx context.Context, c CacheType, queryUid uint64,
		maxResults int,
		filter SearchFilter[T]) ([]uint64, error)

	// Insert will add a vector and uuid into the existing VectorIndex. If
	// uuid already exists, it should throw an error to not insert duplicate uuids
	Insert(ctx context.Context, c CacheType, uuid uint64, vec []T) ([]*KeyValue, error)

	BuildInsert(ctx context.Context, uuid uint64, vec []T) error
	GetCentroids() [][]T
	AddSeedVector(vec []T)
	NumBuildPasses() int
	SetNumPasses(int)
	NumIndexPasses() int
	NumSeedVectors() int
	StartBuild(caches []CacheType)
	EndBuild() []int
	NumThreads() int
	Dimension() int
	SetDimension(schema *pb.SchemaUpdate, dimension int)
}

// A Txn is an interface representation of a persistent storage transaction,
// where multiple operations are performed on a database
type Txn interface {
	// StartTs gets the exact time that the transaction started, returned in uint64 format
	StartTs() uint64
	// Get uses a []byte key to return the Value corresponding to the key
	Get(key []byte) (rval []byte, rerr error)
	// GetWithLockHeld uses a []byte key to return the Value corresponding to the key with a mutex lock held
	GetWithLockHeld(key []byte) (rval []byte, rerr error)
	Find(prefix []byte, filter func(val []byte) bool) (uint64, error)
	// Adds a mutation operation on a index.Txn interface, where the mutation
	// is represented in the form of an index.DirectedEdge
	AddMutation(ctx context.Context, key []byte, t *KeyValue) error
	// Same as AddMutation but with a mutex lock held
	AddMutationWithLockHeld(ctx context.Context, key []byte, t *KeyValue) error
	// mutex lock
	LockKey(key []byte)
	// mutex unlock
	UnlockKey(key []byte)
}

// Local cache is an interface representation of the local cache of a persistent storage system
type LocalCache interface {
	// Get uses a []byte key to return the Value corresponding to the key
	Get(key []byte) (rval []byte, rerr error)
	// GetWithLockHeld uses a []byte key to return the Value corresponding to the key with a mutex lock held
	GetWithLockHeld(key []byte) (rval []byte, rerr error)
	Find(prefix []byte, filter func(val []byte) bool) (uint64, error)
}

// CacheType is an interface representation of the cache of a persistent storage system
type CacheType interface {
	Get(key []byte) (rval []byte, rerr error)
	Ts() uint64
	Find(prefix []byte, filter func(val []byte) bool) (uint64, error)
}
