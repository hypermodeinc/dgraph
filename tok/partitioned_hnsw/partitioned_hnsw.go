// CreateFactory creates an instance of the private struct persistentIndexFactory.
// NOTE: if T and floatBits do not match in # of bits, there will be consequences.

package partitioned_hnsw

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	c "github.com/hypermodeinc/dgraph/v25/tok/constraints"
	hnsw "github.com/hypermodeinc/dgraph/v25/tok/hnsw"
	"github.com/hypermodeinc/dgraph/v25/tok/index"
	"github.com/hypermodeinc/dgraph/v25/tok/kmeans"
	opt "github.com/hypermodeinc/dgraph/v25/tok/options"
)

type partitionedHNSW[T c.Float] struct {
	floatBits int
	pred      string

	clusterMap      map[int]index.VectorIndex[T]
	numClusters     int
	vectorDimension int
	vecCount        int
	numPasses       int
	partition       index.VectorPartitionStrat[T]

	hnswOptions    opt.Options
	partitionStrat string

	caches        []index.CacheType
	buildPass     int
	buildSyncMaps map[int]*sync.Mutex
}

func (ph *partitionedHNSW[T]) applyOptions(o opt.Options) error {
	ph.numClusters, _, _ = opt.GetOpt(o, NumClustersOpt, 1000)
	ph.vectorDimension, _, _ = opt.GetOpt(o, vectorDimension, -1)
	ph.partitionStrat, _, _ = opt.GetOpt(o, PartitionStratOpt, "kmeans")

	if ph.partitionStrat != "kmeans" && ph.partitionStrat != "query" {
		return errors.New("partition strategy must be kmeans or query")
	}

	if ph.partitionStrat == "kmeans" {
		ph.partition = kmeans.CreateKMeans(ph.floatBits, ph.pred, hnsw.EuclideanDistanceSq[T])
	}

	ph.buildPass = 0
	ph.numPasses = 10
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
	if index%ph.numPasses != passIdx {
		return nil
	}
	ph.buildSyncMaps[index].Lock()
	defer ph.buildSyncMaps[index].Unlock()
	_, err = ph.clusterMap[index].Insert(ctx, ph.caches[index], uuid, vec)
	return err
}

func (ph *partitionedHNSW[T]) GetCentroids() [][]T {
	return ph.partition.GetCentroids()
}

func (ph *partitionedHNSW[T]) NumBuildPasses() int {
	return ph.partition.NumPasses()
}

func (ph *partitionedHNSW[T]) SetNumPasses(n int) {
	ph.partition.SetNumPasses(n)
}

func (ph *partitionedHNSW[T]) Dimension() int {
	return ph.vectorDimension
}

func (ph *partitionedHNSW[T]) SetDimension(schema *pb.SchemaUpdate, dimension int) {
	ph.vectorDimension = dimension
	for _, vs := range schema.IndexSpecs {
		if vs.Name == "partionedhnsw" {
			vs.Options = append(vs.Options, &pb.OptionPair{
				Key:   "vectorDimension",
				Value: strconv.Itoa(dimension),
			})
		}
	}
}

func (ph *partitionedHNSW[T]) NumIndexPasses() int {
	return ph.numPasses
}

func (ph *partitionedHNSW[T]) NumThreads() int {
	return ph.numClusters
}

func (ph *partitionedHNSW[T]) NumSeedVectors() int {
	return ph.partition.NumSeedVectors()
}

func (ph *partitionedHNSW[T]) StartBuild(caches []index.CacheType) {
	ph.caches = caches
	if ph.buildPass < ph.partition.NumPasses() {
		ph.partition.StartBuildPass()
		return
	}

	for i := range ph.clusterMap {
		ph.buildSyncMaps[i] = &sync.Mutex{}
		if i%ph.numPasses != (ph.buildPass - ph.partition.NumPasses()) {
			continue
		}
		ph.clusterMap[i].StartBuild([]index.CacheType{ph.caches[i]})
	}
}

func (ph *partitionedHNSW[T]) EndBuild() []int {
	res := []int{}

	if ph.buildPass >= ph.partition.NumPasses() {
		for i := range ph.clusterMap {
			if i%ph.numPasses != (ph.buildPass - ph.partition.NumPasses()) {
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

func (ph *partitionedHNSW[T]) Insert(ctx context.Context, txn index.CacheType, uid uint64, vec []T) ([]*index.KeyValue, error) {
	if len(vec) == 0 {
		ph.vectorDimension = len(vec)
	}

	if len(vec) != ph.vectorDimension {
		return nil, fmt.Errorf("connot insert vector length of %d vector lenth should be %d", len(vec), ph.vectorDimension)
	}

	index, err := ph.partition.FindIndexForInsert(vec)
	if err != nil {
		return nil, err
	}
	return ph.clusterMap[index].Insert(ctx, txn, uid, vec)
}

func (ph *partitionedHNSW[T]) Search(ctx context.Context, txn index.CacheType, query []T, maxResults int, filter index.SearchFilter[T]) ([]uint64, error) {
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
			ids, err := ph.clusterMap[i].Search(ctx, txn, query, maxResults, filter)
			if err != nil {
				return
			}
			mutex.Lock()
			res = append(res, ids...)
			mutex.Unlock()
		}(index)
	}
	wg.Wait()

	if len(res) == 0 {
		return res, nil
	}

	return ph.clusterMap[0].MergeResults(ctx, txn, res, query, maxResults, filter)
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
