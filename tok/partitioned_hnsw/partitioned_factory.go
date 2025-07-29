/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package partitioned_hnsw

import (
	"errors"
	"fmt"
	"sync"

	c "github.com/hypermodeinc/dgraph/v25/tok/constraints"
	"github.com/hypermodeinc/dgraph/v25/tok/hnsw"
	"github.com/hypermodeinc/dgraph/v25/tok/index"
	opt "github.com/hypermodeinc/dgraph/v25/tok/options"
)

const (
	NumClustersOpt    string = "numClusters"
	vectorDimension   string = "vectorDimension"
	PartitionStratOpt string = "partitionStratOpt"
	PartitionedHNSW   string = "partionedhnsw"
)

type partitionedHNSWIndexFactory[T c.Float] struct {
	indexMap  map[string]index.VectorIndex[T]
	floatBits int
	mu        sync.RWMutex
}

// CreateFactory creates an instance of the private struct persistentIndexFactory.
// NOTE: if T and floatBits do not match in # of bits, there will be consequences.
func CreateFactory[T c.Float](floatBits int) index.IndexFactory[T] {
	return &partitionedHNSWIndexFactory[T]{
		indexMap:  map[string]index.VectorIndex[T]{},
		floatBits: floatBits,
	}
}

// Implements NamedFactory interface for use as a plugin.
func (hf *partitionedHNSWIndexFactory[T]) Name() string { return PartitionedHNSW }

func (hf *partitionedHNSWIndexFactory[T]) GetOptions(o opt.Options) string {
	return hnsw.GetPersistantOptions[T](o)
}

func (hf *partitionedHNSWIndexFactory[T]) isNameAvailableWithLock(name string) bool {
	_, nameUsed := hf.indexMap[name]
	return !nameUsed
}

// hf.AllowedOptions() allows persistentIndexFactory to implement the
// IndexFactory interface (see vector-indexer/index/index.go for details).
// We define here options for exponent, maxLevels, efSearch, efConstruction,
// and metric.
func (hf *partitionedHNSWIndexFactory[T]) AllowedOptions() opt.AllowedOptions {
	retVal := opt.NewAllowedOptions()
	retVal.AddIntOption(hnsw.ExponentOpt).
		AddIntOption(hnsw.MaxLevelsOpt).
		AddIntOption(hnsw.EfConstructionOpt).
		AddIntOption(hnsw.EfSearchOpt).
		AddIntOption(NumClustersOpt).
		AddStringOption(PartitionStratOpt).AddIntOption(vectorDimension)
	getSimFunc := func(optValue string) (any, error) {
		if optValue != hnsw.Euclidean && optValue != hnsw.Cosine && optValue != hnsw.DotProd {
			return nil, fmt.Errorf("Can't create a vector index for %s", optValue)
		}
		return hnsw.GetSimType[T](optValue, hf.floatBits), nil
	}

	retVal.AddCustomOption(hnsw.MetricOpt, getSimFunc)
	return retVal
}

// Create is an implementation of the IndexFactory interface function, invoked by an HNSWIndexFactory
// instance. It takes in a string name and a VectorSource implementation, and returns a VectorIndex and error
// flag. It creates an HNSW instance using the index name and populates other parts of the HNSW struct such as
// multFactor, maxLevels, efConstruction, maxNeighbors, and efSearch using struct parameters.
// It then populates the HNSW graphs using the InsertChunk function until there are no more items to populate.
// Finally, the function adds the name and hnsw object to the in memory map and returns the object.
func (hf *partitionedHNSWIndexFactory[T]) Create(
	name string,
	o opt.Options,
	floatBits int) (index.VectorIndex[T], error) {
	hf.mu.Lock()
	defer hf.mu.Unlock()
	return hf.createWithLock(name, o, floatBits)
}

func (hf *partitionedHNSWIndexFactory[T]) createWithLock(
	name string,
	o opt.Options,
	floatBits int) (index.VectorIndex[T], error) {
	if !hf.isNameAvailableWithLock(name) {
		err := errors.New("index with name " + name + " already exists")
		return nil, err
	}
	retVal := &partitionedHNSW[T]{
		pred:          name,
		floatBits:     floatBits,
		clusterMap:    map[int]index.VectorIndex[T]{},
		buildSyncMaps: map[int]*sync.Mutex{},
	}
	err := retVal.applyOptions(o)
	if err != nil {
		return nil, err
	}
	hf.indexMap[name] = retVal
	return retVal, nil
}

// Find is an implementation of the IndexFactory interface function, invoked by an persistentIndexFactory
// instance. It returns the VectorIndex corresponding with a string name using the in memory map.
func (hf *partitionedHNSWIndexFactory[T]) Find(name string) (index.VectorIndex[T], error) {
	hf.mu.RLock()
	defer hf.mu.RUnlock()
	return hf.findWithLock(name)
}

func (hf *partitionedHNSWIndexFactory[T]) findWithLock(name string) (index.VectorIndex[T], error) {
	vecInd := hf.indexMap[name]
	return vecInd, nil
}

// Remove is an implementation of the IndexFactory interface function, invoked by an persistentIndexFactory
// instance. It removes the VectorIndex corresponding with a string name using the in memory map.
func (hf *partitionedHNSWIndexFactory[T]) Remove(name string) error {
	hf.mu.Lock()
	defer hf.mu.Unlock()
	return hf.removeWithLock(name)
}

func (hf *partitionedHNSWIndexFactory[T]) removeWithLock(name string) error {
	delete(hf.indexMap, name)
	return nil
}

// CreateOrReplace is an implementation of the IndexFactory interface funciton,
// invoked by an persistentIndexFactory. It checks if a VectorIndex
// correpsonding with name exists. If it does, it removes it, and replaces it
// via the Create function using the passed VectorSource. If the VectorIndex
// does not exist, it creates that VectorIndex corresponding with the name using
// the VectorSource.
func (hf *partitionedHNSWIndexFactory[T]) CreateOrReplace(
	name string,
	o opt.Options,
	floatBits int) (index.VectorIndex[T], error) {
	hf.mu.Lock()
	defer hf.mu.Unlock()
	vi, err := hf.findWithLock(name)
	if err != nil {
		return nil, err
	}
	if vi != nil {
		err = hf.removeWithLock(name)
		if err != nil {
			return nil, err
		}
	}
	return hf.createWithLock(name, o, floatBits)
}
