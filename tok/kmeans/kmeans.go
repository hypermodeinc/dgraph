package kmeans

import (
	"fmt"
	"math"
	"sync"

	c "github.com/hypermodeinc/dgraph/v25/tok/constraints"
	"github.com/hypermodeinc/dgraph/v25/tok/index"
)

type Kmeans[T c.Float] struct {
	floatBits int
	centroids *vectorCentroids[T]
}

func CreateKMeans[T c.Float](floatBits int, distFunc func(a, b []T, floatBits int) (T, error)) index.VectorPartitionStrat[T] {
	return &Kmeans[T]{
		floatBits: floatBits,
		centroids: &vectorCentroids[T]{
			distFunc:  distFunc,
			floatBits: floatBits,
		},
	}
}

func (km *Kmeans[T]) AddSeedVector(vec []T) {
	km.centroids.addSeedCentroid(vec)
}

func (km *Kmeans[T]) AddVector(vec []T) error {
	return km.centroids.addVector(vec)
}

func (km *Kmeans[T]) FindIndexForSearch(vec []T) ([]int, error) {
	res := make([]int, km.NumSeedVectors())
	for i := range res {
		res[i] = i
	}
	return res, nil
}

func (km *Kmeans[T]) FindIndexForInsert(vec []T) (int, error) {
	return km.centroids.findCentroid(vec)
}

func (km *Kmeans[T]) NumPasses() int {
	return 5
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
	for i := 0; i < vc.numCenters; i++ {
		for j := 0; j < vc.dimension; j++ {
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
