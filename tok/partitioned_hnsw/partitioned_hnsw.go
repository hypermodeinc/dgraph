// CreateFactory creates an instance of the private struct persistentIndexFactory.
// NOTE: if T and floatBits do not match in # of bits, there will be consequences.

package partitioned_hnsw

import (
	"context"
	"errors"
	"sync"

	c "github.com/hypermodeinc/dgraph/v25/tok/constraints"
	hnsw "github.com/hypermodeinc/dgraph/v25/tok/hnsw"
	"github.com/hypermodeinc/dgraph/v25/tok/index"
	"github.com/hypermodeinc/dgraph/v25/tok/kmeans"
	opt "github.com/hypermodeinc/dgraph/v25/tok/options"
)

type partitionedHNSW[T c.Float] struct {
	floatBits int
	pred      string

	clusterMap  map[int]index.VectorIndex[T]
	numClusters int
	partition   index.VectorPartitionStrat[T]

	hnswOptions    opt.Options
	partitionStrat string

	caches        []index.CacheType
	buildPass     int
	buildSyncMaps map[int]*sync.Mutex
}

func (ph *partitionedHNSW[T]) applyOptions(o opt.Options) error {
	ph.numClusters, _, _ = opt.GetOpt(o, NumClustersOpt, 1000)
	ph.partitionStrat, _, _ = opt.GetOpt(o, PartitionStratOpt, "kmeans")

	if ph.partitionStrat != "kmeans" && ph.partitionStrat != "query" {
		return errors.New("partition strategy must be kmeans or query")
	}

	if ph.partitionStrat == "kmeans" {
		ph.partition = kmeans.CreateKMeans(ph.floatBits, hnsw.EuclideanDistanceSq[T])
	}

	ph.buildPass = 0
	ph.hnswOptions = o
	for i := range ph.numClusters {
		factory := hnsw.CreateFactory[T](ph.floatBits)
		vi, err := factory.Create(ph.pred, ph.hnswOptions, ph.floatBits)
		if err != nil {
			return err
		}
		err = hnsw.UpdateIndexSplit(vi, i)
		if err != nil {
			return err
		}
		ph.clusterMap[i] = vi
	}
	return nil
}

func (ph *partitionedHNSW[T]) AddSeedVector(vec []T) {
	ph.partition.AddSeedVector(vec)
}

func (ph *partitionedHNSW[T]) BuildInsert(ctx context.Context, uuid uint64, vec []T) error {
	passIdx := ph.buildPass - ph.partition.NumPasses()
	if passIdx < 0 {
		return ph.partition.AddVector(vec)
	}
	index, err := ph.partition.FindIndexForInsert(vec)
	if err != nil {
		return err
	}
	if index%NUM_PASSES != passIdx {
		return nil
	}
	ph.buildSyncMaps[index].Lock()
	defer ph.buildSyncMaps[index].Unlock()
	return ph.clusterMap[index].BuildInsert(ctx, uuid, vec)
}

const NUM_PASSES = 5

func (ph *partitionedHNSW[T]) NumBuildPasses() int {
	return ph.partition.NumPasses()
}

func (ph *partitionedHNSW[T]) NumIndexPasses() int {
	return NUM_PASSES
}

func (ph *partitionedHNSW[T]) NumThreads() int {
	return ph.numClusters
}

func (ph *partitionedHNSW[T]) NumSeedVectors() int {
	return ph.partition.NumSeedVectors()
}

func (ph *partitionedHNSW[T]) SetCaches(caches []index.CacheType) {
	ph.caches = caches
	for i := range ph.clusterMap {
		ph.clusterMap[i].SetCaches([]index.CacheType{ph.caches[i]})
	}
}

func (ph *partitionedHNSW[T]) StartBuild() {
	if ph.buildPass < ph.partition.NumPasses() {
		ph.partition.StartBuildPass()
		return
	}

	for i := range ph.clusterMap {
		ph.buildSyncMaps[i] = &sync.Mutex{}
		if i%NUM_PASSES != (ph.buildPass - ph.partition.NumPasses()) {
			continue
		}
		ph.clusterMap[i].StartBuild()
	}
}

func (ph *partitionedHNSW[T]) EndBuild() []int {
	res := []int{}

	if ph.buildPass >= ph.partition.NumPasses() {
		for i := range ph.clusterMap {
			if i%NUM_PASSES != (ph.buildPass - ph.partition.NumPasses()) {
				continue
			}
			ph.clusterMap[i].EndBuild()
			res = append(res, i)
		}
	}

	ph.buildPass += 1

	if len(res) > 0 {
		return res
	}

	if ph.buildPass < ph.partition.NumPasses() {
		ph.partition.EndBuildPass()
	}
	return []int{}
}

func (ph *partitionedHNSW[T]) Insert(ctx context.Context, uid uint64, vec []T) error {
	index, err := ph.partition.FindIndexForInsert(vec)
	if err != nil {
		return err
	}
	return ph.clusterMap[index].Insert(ctx, uid, vec)
}

func (ph *partitionedHNSW[T]) Search(ctx context.Context, query []T, maxResults int, filter index.SearchFilter[T]) ([]uint64, error) {
	indexes, err := ph.partition.FindIndexForSearch(query)
	if err != nil {
		return nil, err
	}
	res := []uint64{}
	mutex := &sync.Mutex{}
	var wg sync.WaitGroup
	for _, index := range indexes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids, err := ph.clusterMap[i].Search(ctx, query, maxResults, filter)
			if err != nil {
				return
			}
			mutex.Lock()
			res = append(res, ids...)
			mutex.Unlock()
		}(index)
	}
	wg.Wait()
	return ph.clusterMap[0].MergeResults(ctx, ph.caches[0], res, query, maxResults, filter)
}

func (ph *partitionedHNSW[T]) SearchWithPath(ctx context.Context, txn index.CacheType, query []T, maxResults int, filter index.SearchFilter[T]) (*index.SearchPathResult, error) {
	indexes, err := ph.partition.FindIndexForSearch(query)
	if err != nil {
		return nil, err
	}
	return ph.clusterMap[indexes[0]].SearchWithPath(ctx, txn, query, maxResults, filter)
}

func (ph *partitionedHNSW[T]) SearchWithUid(ctx context.Context, txn index.CacheType, uid uint64, maxResults int, filter index.SearchFilter[T]) ([]uint64, error) {
	// #TODO
	return ph.clusterMap[0].SearchWithUid(ctx, txn, uid, maxResults, filter)
}

func (ph *partitionedHNSW[T]) MergeResults(ctx context.Context, txn index.CacheType, list []uint64, query []T, maxResults int, filter index.SearchFilter[T]) ([]uint64, error) {
	return ph.clusterMap[0].MergeResults(ctx, txn, list, query, maxResults, filter)
}
