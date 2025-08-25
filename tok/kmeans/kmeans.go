package kmeans

import (
	"encoding/json"
	"fmt"
	"math"
	"sync"

	c "github.com/hypermodeinc/dgraph/v25/tok/constraints"
	"github.com/hypermodeinc/dgraph/v25/tok/hnsw"
	"github.com/hypermodeinc/dgraph/v25/tok/index"
	"github.com/hypermodeinc/dgraph/v25/x"
)

const (
	CentroidPrefix = "__centroid_"
)

type Kmeans[T c.Float] struct {
	floatBits int
	numPasses int
	centroids *vectorCentroids[T]
}

func CreateKMeans[T c.Float](floatBits int, pred string, distFunc func(a, b []T, floatBits int) (T, error)) index.VectorPartitionStrat[T] {
	return &Kmeans[T]{
		floatBits: floatBits,
		numPasses: 5,
		centroids: &vectorCentroids[T]{
			distFunc:  distFunc,
			floatBits: floatBits,
			pred:      pred,
		},
	}
}

func (km *Kmeans[T]) AddSeedVector(vec []T) {
	km.centroids.addSeedCentroid(vec)
}

func (km *Kmeans[T]) AddVector(vec []T) error {
	return km.centroids.addVector(vec)
}

func (km *Kmeans[T]) GetCentroids() [][]T {
	return km.centroids.centroids
}

func (km *Kmeans[T]) FindIndexForSearch(vec []T) ([]int, error) {
	if km.NumPasses() == 0 {
		return []int{0}, nil
	}
	res := make([]int, km.NumSeedVectors())
	for i := range res {
		res[i] = i
	}
	return res, nil
}

func (km *Kmeans[T]) FindIndexForInsert(vec []T) (int, error) {
	if km.NumPasses() == 0 {
		return 0, nil
	}
	return km.centroids.findCentroid(vec)
}

func (km *Kmeans[T]) NumPasses() int {
	return km.numPasses
}

func (km *Kmeans[T]) SetNumPasses(n int) {
	km.numPasses = n
}

func (km *Kmeans[T]) NumSeedVectors() int {
	return 1000
}

func (km *Kmeans[T]) StartBuildPass() {
	if km.centroids.weights == nil {
		km.centroids.randomInit()
	}
}

func (km *Kmeans[T]) EndBuildPass() {
	km.centroids.updateCentroids()
}

type vectorCentroids[T c.Float] struct {
	dimension  int
	numCenters int
	pred       string

	distFunc func(a, b []T, floatBits int) (T, error)

	centroids [][]T
	counts    []int64
	weights   [][]T
	mutexs    []*sync.Mutex
	floatBits int
}

func (vc *vectorCentroids[T]) findCentroid(input []T) (int, error) {
	minIdx := 0
	minDist := math.MaxFloat32
	for i, centroid := range vc.centroids {
		dist, err := vc.distFunc(centroid, input, vc.floatBits)
		if err != nil {
			return 0, err
		}
		if float64(dist) < minDist {
			minDist = float64(dist)
			minIdx = i
		}
	}
	return minIdx, nil
}

func (vc *vectorCentroids[T]) getCentroids(txn index.CacheType) ([][]T, error) {
	if len(vc.centroids) > 0 {
		return vc.centroids, nil
	}
	indexCountAttr := hnsw.ConcatStrings(vc.pred, CentroidPrefix)
	key := x.DataKey(indexCountAttr, 1)
	centroidsMarshalled, err := txn.Get(key)
	if err != nil {
		return nil, err
	}

	centroids := [][]T{}
	err = json.Unmarshal(centroidsMarshalled, &centroids)
	if err != nil {
		return nil, err
	}

	vc.centroids = centroids
	return vc.centroids, nil
}

func (vc *vectorCentroids[T]) findNClosestCentroids(input []T, n int, txn index.CacheType) ([]int, error) {
	cNS, err := vc.getCentroids(txn)
	if err != nil {
		return nil, err
	}
	if n <= 0 || len(vc.centroids) == 0 {
		return []int{}, nil
	}
	if n >= len(vc.centroids) {
		res := make([]int, len(vc.centroids))
		for i := range res {
			res[i] = i
		}
		return res, nil
	}
	res := []int{}
	resDist := []float64{}
	// get centroids

	for i, centroid := range cNS {
		dist, err := vc.distFunc(centroid, input, vc.floatBits)
		if err != nil {
			return nil, err
		}
		if len(res) < n {
			res = append(res, i)
			resDist = append(resDist, float64(dist))
		} else {
			// Find the farthest in current top-n
			maxIdx, maxDist := 0, resDist[0]
			for j, d := range resDist {
				if d > maxDist {
					maxIdx, maxDist = j, d
				}
			}
			if float64(dist) < maxDist {
				res[maxIdx] = i
				resDist[maxIdx] = float64(dist)
			}
		}
	}
	return res, nil
}

func (vc *vectorCentroids[T]) addVector(vec []T) error {
	idx, err := vc.findCentroid(vec)
	if err != nil {
		return err
	}
	vc.mutexs[idx].Lock()
	defer vc.mutexs[idx].Unlock()
	for i := 0; i < vc.dimension; i++ {
		vc.weights[idx][i] += vec[i]
	}
	vc.counts[idx]++
	return nil
}

func (vc *vectorCentroids[T]) updateCentroids() {
	x.AssertTrue(len(vc.centroids) == vc.numCenters)
	x.AssertTrue(len(vc.counts) == vc.numCenters)
	x.AssertTrue(len(vc.weights) == vc.numCenters)
	for i := 0; i < vc.numCenters; i++ {
		for j := 0; j < vc.dimension; j++ {
			x.AssertTrue(len(vc.centroids[i]) == vc.dimension)
			x.AssertTrue(len(vc.weights[i]) == vc.dimension)
			vc.centroids[i][j] = vc.weights[i][j] / T(vc.counts[i])
			vc.weights[i][j] = 0
		}
		fmt.Printf("%d, ", vc.counts[i])
		vc.counts[i] = 0
	}
	fmt.Println()
}

func (vc *vectorCentroids[T]) randomInit() {
	vc.dimension = len(vc.centroids[0])
	for i := range vc.centroids {
		x.AssertTrue(len(vc.centroids[i]) == vc.dimension)
	}
	vc.numCenters = len(vc.centroids)
	vc.counts = make([]int64, vc.numCenters)
	vc.weights = make([][]T, vc.numCenters)
	vc.mutexs = make([]*sync.Mutex, vc.numCenters)
	for i := 0; i < vc.numCenters; i++ {
		vc.weights[i] = make([]T, vc.dimension)
		vc.counts[i] = 0
		vc.mutexs[i] = &sync.Mutex{}
	}
}

func (vc *vectorCentroids[T]) addSeedCentroid(vec []T) {
	vc.centroids = append(vc.centroids, vec)
}
