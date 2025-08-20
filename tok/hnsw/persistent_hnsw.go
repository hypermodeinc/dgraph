/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package hnsw

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/golang/glog"
	c "github.com/hypermodeinc/dgraph/v25/tok/constraints"
	"github.com/hypermodeinc/dgraph/v25/tok/index"
	opt "github.com/hypermodeinc/dgraph/v25/tok/options"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/pkg/errors"
)

type persistentHNSW[T c.Float] struct {
	maxLevels      int
	efConstruction int
	efSearch       int
	pred           string
	vecEntryKey    string
	vecKey         string
	vecDead        string
	simType        SimilarityType[T]
	floatBits      int
	// nodeAllEdges[65443][1][3] indicates the 3rd neighbor in the first
	// layer for uuid 65443. The result will be a neighboring uuid.
	deadNodes map[uint64]struct{}
	cache     index.CacheType

	entryVec      []T
	isEntryVecSet atomic.Bool
}

func GetPersistantOptions[T c.Float](o opt.Options) string {
	sb := strings.Builder{}

	if val, ok, _ := opt.GetOpt(o, ExponentOpt, 3); ok {
		sb.WriteString(fmt.Sprintf(`"%s":"%d",`, ExponentOpt, val))
	}
	if val, ok, _ := opt.GetOpt(o, MaxLevelsOpt, 3); ok {
		sb.WriteString(fmt.Sprintf(`"%s":"%d",`, MaxLevelsOpt, val))
	}
	if val, ok, _ := opt.GetOpt(o, EfConstructionOpt, 3); ok {
		sb.WriteString(fmt.Sprintf(`"%s":"%d",`, EfConstructionOpt, val))
	}
	if val, ok, _ := opt.GetOpt(o, EfSearchOpt, 3); ok {
		sb.WriteString(fmt.Sprintf(`"%s":"%d",`, EfSearchOpt, val))
	}

	if simType, foundSimType := opt.GetInterfaceOpt(o, MetricOpt); foundSimType {
		sim, ok := simType.(SimilarityType[T])
		if !ok {
			glog.Errorf("cannot cast %T to SimilarityType", simType)
		}
		sb.WriteString(fmt.Sprintf(`"%s":"%s",`, MetricOpt, sim.indexType))
	}

	final := sb.String()
	if len(final) > 0 {
		// Remove last , and cover with brackets
		return "(" + final[:len(final)-1] + ")"
	}
	return ""
}

func (ph *persistentHNSW[T]) applyOptions(o opt.Options) error {
	if o.Specifies(ExponentOpt) {
		// Adjust defaults based on exponent.
		exponent, _, _ := opt.GetOpt(o, ExponentOpt, 3)

		if !o.Specifies(MaxLevelsOpt) {
			o.SetOpt(MaxLevelsOpt, exponent)
		}

		if !o.Specifies(EfConstructionOpt) {
			o.SetOpt(EfConstructionOpt, 50*exponent)
		}

		if !o.Specifies(EfSearchOpt) {
			o.SetOpt(EfSearchOpt, 30*exponent)
		}
	}

	var err error
	ph.maxLevels, _, err = opt.GetOpt(o, MaxLevelsOpt, 3)
	if err != nil {
		return err
	}
	ph.efConstruction, _, err = opt.GetOpt(o, EfConstructionOpt, 150)
	if err != nil {
		return err
	}
	ph.efSearch, _, err = opt.GetOpt(o, EfSearchOpt, 90)
	if err != nil {
		return err
	}
	simType, foundSimType := opt.GetInterfaceOpt(o, MetricOpt)
	if foundSimType {
		okSimType, ok := simType.(SimilarityType[T])
		if !ok {
			return fmt.Errorf("cannot cast %T to SimilarityType", simType)
		}
		ph.simType = okSimType
	} else {
		ph.simType = SimilarityType[T]{indexType: Euclidean, distanceScore: euclideanDistanceSq[T],
			insortHeap: insortPersistentHeapAscending[T], isBetterScore: isBetterScoreForDistance[T]}
	}
	return nil
}

func (ph *persistentHNSW[T]) NumBuildPasses() int {
	return 0
}

func (ph *persistentHNSW[T]) NumIndexPasses() int {
	return 1
}

func (ph *persistentHNSW[T]) NumSeedVectors() int {
	return 0
}

func (ph *persistentHNSW[T]) StartBuild() {
}

func (ph *persistentHNSW[T]) SetCaches(c []index.CacheType) {
	ph.cache = c[0]
}

func (ph *persistentHNSW[T]) EndBuild() []int {
	ph.cache = nil
	return []int{0}
}

func (ph *persistentHNSW[T]) NumThreads() int {
	return 1
}

func (ph *persistentHNSW[T]) BuildInsert(ctx context.Context, uid uint64, vec []T) error {
	floatVec := *(*[]float32)(unsafe.Pointer(&vec))
	vecBytes := types.FloatArrayAsBytes(floatVec)
	ph.cache.SetVector(uid, &vecBytes)
	newPh := &persistentHNSW[T]{
		maxLevels:      ph.maxLevels,
		efConstruction: ph.efConstruction,
		efSearch:       ph.efSearch,
		pred:           ph.pred,
		vecEntryKey:    ph.vecEntryKey,
		vecKey:         ph.vecKey,
		vecDead:        ph.vecDead,
		simType:        ph.simType,
		floatBits:      ph.floatBits,
		cache:          ph.cache,
	}
	return newPh.Insert(ctx, uid, vec)
}

func (ph *persistentHNSW[T]) AddSeedVector(vec []T) {
}

func (ph *persistentHNSW[T]) emptyFinalResultWithError(e error) (
	*index.SearchPathResult, error) {
	return index.NewSearchPathResult(), e
}

func (ph *persistentHNSW[T]) emptySearchResultWithError(e error) (*searchLayerResult[T], error) {
	return newLayerResult[T](0), e
}

// fillNeighborEdges(uuid, c, edges) will "fill" edges with the neighbors for
// all levels associated with given uuid and CacheType.
// It returns true when we were able to find the node (either in cache or
// in persistent store) and false otherwise.
// (Of course, it may also return an error if a problem was encountered).
func (ph *persistentHNSW[T]) fillNeighborEdges(uuid uint64, c index.CacheType, edges *[][]uint64) (bool, error) {
	edge := ph.cache.GetEdge(uuid)
	if edge == nil {
		return false, nil
	}
	if err := decodeUint64MatrixUnsafe(*edge, edges); err != nil {
		return false, err
	}
	return true, nil
}

// searchPersistentLayer searches a layer of the hnsw graph for the nearest
// neighbors of the query vector and returns the traversal path and the nearest
// neighbors
func (ph *persistentHNSW[T]) searchPersistentLayer(
	c index.CacheType,
	level int,
	entry uint64,
	startVec, query []T,
	entryIsFilteredOut bool,
	expectedNeighbors int,
	filter index.SearchFilter[T]) (*searchLayerResult[T], error) {
	r := newLayerResult[T](level)

	bestDist, err := ph.simType.distanceScore(startVec, query, ph.floatBits)
	r.markFirstDistanceComputation()
	if err != nil {
		return ph.emptySearchResultWithError(err)
	}
	best := minPersistentHeapElement[T]{
		value:       bestDist,
		index:       entry,
		filteredOut: entryIsFilteredOut,
	}

	r.setFirstPathNode(best)
	candidateHeap := *buildPersistentHeapByInit([]minPersistentHeapElement[T]{best})

	var allLayerEdges [][]uint64

	//create set using map to append to on future visited nodes
	for candidateHeap.Len() != 0 {
		currCandidate := candidateHeap.Pop().(minPersistentHeapElement[T])
		if r.numNeighbors() < expectedNeighbors &&
			ph.simType.isBetterScore(r.lastNeighborScore(), currCandidate.value) {
			// If the "worst score" in our neighbors list is deemed to have
			// a better score than the current candidate -- and if we have at
			// least our expected number of nearest results -- we discontinue
			// the search.
			// Note that while this is faithful to the published
			// HNSW algorithms insofar as we stop when we reach a local
			// minimum, it leaves something to be desired in terms of
			// guarantees of getting best results.
			break
		}

		found, err := ph.fillNeighborEdges(currCandidate.index, c, &allLayerEdges)
		if err != nil {
			return ph.emptySearchResultWithError(err)
		}
		if !found {
			continue
		}
		var eVec []T
		improved := false
		for _, currUid := range allLayerEdges[level] {
			if r.indexVisited(currUid) {
				continue
			}
			// iterate over candidate's neighbors distances to get
			// best ones
			_ = ph.getVecFromUid(currUid, c, &eVec)
			// intentionally ignoring error -- we catch it
			// indirectly via eVec == nil check.
			if len(eVec) == 0 {
				continue
			}
			currDist, err := ph.simType.distanceScore(eVec, query, ph.floatBits)
			if err != nil {
				return ph.emptySearchResultWithError(err)
			}
			filteredOut := !filter(query, eVec, currUid)
			currElement := initPersistentHeapElement(
				currDist, currUid, filteredOut)
			r.addToVisited(*currElement)
			r.incrementDistanceComputations()

			// If we have not yet found k candidates, we can consider
			// any candidate. Otherwise, only consider those that
			// are better than our current k nearest neighbors.
			// Note that the "numNeighbors" function is a bit tricky:
			// If we previously added to the heap M elements that should
			// be filtered out, we ignore M elements in the numNeighbors
			// check! In this way, we can make sure to allow in up to
			// expectedNeighbors "unfiltered" elements.
			if r.numNeighbors() < expectedNeighbors || ph.simType.isBetterScore(currDist, r.lastNeighborScore()) {
				if candidateHeap.Len() > expectedNeighbors {
					candidateHeap.PopLast()
				}
				candidateHeap.Push(*currElement)
				r.addPathNode(*currElement, ph.simType, expectedNeighbors)
				improved = true
			}
		}

		if !improved && r.numNeighbors() >= expectedNeighbors {
			break
		}
	}
	return r, nil
}

// Search searches the hnsw graph for the nearest neighbors of the query vector
// and returns the traversal path and the nearest neighbors
func (ph *persistentHNSW[T]) Search(ctx context.Context, query []T,
	maxResults int, filter index.SearchFilter[T]) (nnUids []uint64, err error) {
	r, err := ph.SearchWithPath(ctx, ph.cache, query, maxResults, filter)
	return r.Neighbors, err
}

type resultRow[T c.Float] struct {
	uid  uint64
	dist T
}

func (ph *persistentHNSW[T]) MergeResults(ctx context.Context, c index.CacheType, list []uint64, query []T, maxResults int, filter index.SearchFilter[T]) ([]uint64, error) {
	var result []resultRow[T]

	for i := range list {
		var vec []T
		err := ph.getVecFromUid(list[i], c, &vec)
		if err != nil {
			return nil, err
		}

		dist, err := ph.simType.distanceScore(vec, query, ph.floatBits)
		if err != nil {
			return nil, err
		}
		result = append(result, resultRow[T]{
			uid:  list[i],
			dist: dist,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].dist < result[j].dist
	})

	uids := []uint64{}
	for i := range maxResults {
		if i > len(result) {
			break
		}
		uids = append(uids, result[i].uid)
	}

	return uids, nil
}

// SearchWithUid searches the hnsw graph for the nearest neighbors of the query uid
// and returns the traversal path and the nearest neighbors
func (ph *persistentHNSW[T]) SearchWithUid(_ context.Context, c index.CacheType, queryUid uint64,
	maxResults int, filter index.SearchFilter[T]) (nnUids []uint64, err error) {
	var queryVec []T
	err = ph.getVecFromUid(queryUid, c, &queryVec)
	if err != nil {
		if errors.Is(err, errFetchingPostingList) {
			// No vector. return empty result
			return []uint64{}, nil
		}
		return []uint64{}, err
	}

	if len(queryVec) == 0 {
		// No vector. return empty result
		return []uint64{}, nil
	}

	shouldFilterOutQueryVec := !filter(queryVec, queryVec, queryUid)

	// how normal search works is by cotinuously searching higher layers
	// for the best entry node to the last layer since we already know the
	// best entry node (since it already exists in the lowest level), we
	// can just search the last layer and return the results.
	r, err := ph.searchPersistentLayer(
		c, ph.maxLevels-1, queryUid, queryVec, queryVec,
		shouldFilterOutQueryVec, maxResults, filter)
	for _, n := range r.neighbors {
		nnUids = append(nnUids, n.index)
	}
	return nnUids, err
}

// There will be times when the entry node has been deleted. In that case, we want to make a new node
// the first vector.
func (ph *persistentHNSW[T]) calculateNewEntryVec(
	_ context.Context,
	c index.CacheType,
	startVec *[]T) (uint64, error) {

	itr, err := c.Find([]byte(ph.pred), func(value []byte) bool {
		index.BytesAsFloatArray(value, startVec, ph.floatBits)
		return len(*startVec) != 0
	})

	if err != nil {
		return 0, errors.Wrapf(err, EmptyHNSWTreeError)
	}
	if itr == 0 {
		return itr, errors.New(EmptyHNSWTreeError)
	}

	return itr, nil
}

func (ph *persistentHNSW[T]) PickStartNode(
	ctx context.Context,
	c index.CacheType,
	startVec *[]T) (uint64, error) {

	data := c.GetOther(ph.vecEntryKey)
	if data == nil {
		return ph.calculateNewEntryVec(ctx, c, startVec)
	}

	entry := BytesToUint64(*data)
	if err := ph.getVecFromUid(entry, c, startVec); err != nil && !errors.Is(err, errNilVector) {
		return 0, err
	}

	if len(*startVec) == 0 {
		return ph.calculateNewEntryVec(ctx, c, startVec)
	}
	return entry, nil
}

// SearchWithPath allows persistentHNSW to implement index.OptionalIndexSupport.
// See index.OptionalIndexSupport.SearchWithPath for more info.
func (ph *persistentHNSW[T]) SearchWithPath(
	ctx context.Context,
	c index.CacheType,
	query []T,
	maxResults int,
	filter index.SearchFilter[T]) (r *index.SearchPathResult, err error) {
	start := time.Now().UnixMilli()
	r = index.NewSearchPathResult()

	// 0-profile_vector_entry
	var startVec []T
	entry, err := ph.PickStartNode(ctx, c, &startVec)
	if err != nil {
		return ph.emptyFinalResultWithError(err)
	}

	// Calculates best entry for last level (maxLevels-1) by searching each
	// layer and using new best entry.
	for level := range ph.maxLevels - 1 {
		if isEqual(startVec, query) {
			break
		}
		filterOut := !filter(query, startVec, entry)
		layerResult, err := ph.searchPersistentLayer(
			c, level, entry, startVec, query, filterOut, ph.efSearch, filter)
		if err != nil {
			return ph.emptyFinalResultWithError(err)
		}
		layerResult.updateFinalMetrics(r)
		entry = layerResult.bestNeighbor().index

		layerResult.updateFinalPath(r)
		err = ph.getVecFromUid(entry, c, &startVec)
		if err != nil {
			return ph.emptyFinalResultWithError(err)
		}
	}
	filterOut := !filter(query, startVec, entry)
	layerResult, err := ph.searchPersistentLayer(
		c, ph.maxLevels-1, entry, startVec, query, filterOut, maxResults, filter)
	if err != nil {
		return ph.emptyFinalResultWithError(err)
	}
	layerResult.updateFinalMetrics(r)
	layerResult.updateFinalPath(r)
	layerResult.addFinalNeighbors(r)
	t := time.Now().UnixMilli()
	elapsed := t - start
	r.Metrics[searchTime] = uint64(elapsed)
	return r, nil
}

// InsertToPersistentStorage inserts a node into the hnsw graph and returns the
// traversal path and the edges created
func (ph *persistentHNSW[T]) Insert(ctx context.Context,
	inUuid uint64, inVec []T) error {
	_, err := ph.insertHelper(ctx, ph.cache, inUuid, inVec)
	return err
}

// InsertToPersistentStorage inserts a node into the hnsw graph and returns the
// traversal path and the edges created
func (ph *persistentHNSW[T]) insertHelper(ctx context.Context, c index.CacheType,
	inUuid uint64, inVec []T) ([]minPersistentHeapElement[T], error) {

	// return all the new edges created at all HNSW levels
	var startVec []T
	entry, err := ph.createEntryAndStartNodes(ctx, c, inUuid, &startVec)
	if err != nil {
		return []minPersistentHeapElement[T]{}, err
	}

	if entry == inUuid {
		// something interesting is you physically cannot add duplicate nodes,
		// it'll just overwrite w the same info
		// only situation where you can add duplicate nodes is if your
		// mutation adds the same node as entry
		return []minPersistentHeapElement[T]{}, nil
	}

	// startVecs: vectors used to calc where to start up until inLevel,
	// nns: nearest neighbors to return,
	// visited: all visited nodes
	// var nns []minPersistentHeapElement[T]
	visited := []minPersistentHeapElement[T]{}
	inLevel := getInsertLayer(ph.maxLevels) // calculate layer to insert node at (randomized every time)

	for level := range inLevel {
		// perform insertion for layers [level, max_level) only, when level < inLevel just find better start
		err := ph.getVecFromUid(entry, c, &startVec)
		if err != nil {
			return []minPersistentHeapElement[T]{}, err
		}
		layerResult, err := ph.searchPersistentLayer(c, level, entry, startVec,
			inVec, false, ph.efSearch, index.AcceptAll[T])
		if err != nil {
			return []minPersistentHeapElement[T]{}, err
		}
		entry = layerResult.bestNeighbor().index
	}

	var outboundEdgesAllLayers = make([][]uint64, ph.maxLevels)
	var inboundEdgesAllLayersMap = make(map[uint64][][]uint64)
	nnUidArray := []uint64{}
	for level := inLevel; level < ph.maxLevels; level++ {
		err := ph.getVecFromUid(entry, c, &startVec)
		if err != nil {
			return []minPersistentHeapElement[T]{}, err
		}
		layerResult, err := ph.searchPersistentLayer(c, level, entry, startVec,
			inVec, false, ph.efConstruction, index.AcceptAll[T])
		if err != nil {
			return []minPersistentHeapElement[T]{}, err
		}

		entry = layerResult.bestNeighbor().index

		nns := layerResult.neighbors
		for i := range nns {
			nnUidArray = append(nnUidArray, nns[i].index)
			if inboundEdgesAllLayersMap[nns[i].index] == nil {
				inboundEdgesAllLayersMap[nns[i].index] = make([][]uint64, ph.maxLevels)
			}
			inboundEdgesAllLayersMap[nns[i].index][level] =
				append(inboundEdgesAllLayersMap[nns[i].index][level], inUuid)
			// add nn to outboundEdges.
			// These should already be correctly ordered.
			outboundEdgesAllLayers[level] =
				append(outboundEdgesAllLayers[level], nns[i].index)
		}
	}
	err = ph.addNeighbors(ctx, c, inUuid, outboundEdgesAllLayers)
	if err != nil {
		return []minPersistentHeapElement[T]{}, err
	}
	for i := range nnUidArray {
		err = ph.addNeighbors(
			ctx, c, nnUidArray[i], inboundEdgesAllLayersMap[nnUidArray[i]])
		if err != nil {
			return []minPersistentHeapElement[T]{}, err
		}
	}

	return visited, nil
}
