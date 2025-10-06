/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package hnsw

import (
	"container/heap"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	c "github.com/hypermodeinc/dgraph/v25/tok/constraints"
	"github.com/hypermodeinc/dgraph/v25/tok/index"
	"github.com/pkg/errors"
	"github.com/viterin/vek"
	"github.com/viterin/vek/vek32"
)

const (
	Euclidean            = "euclidean"
	Cosine               = "cosine"
	DotProd              = "dotproduct"
	EmptyHNSWTreeError   = "HNSW tree has no elements"
	VecKeyword           = "__vector_"
	visitedVectorsLevel  = "visited_vectors_level_"
	distanceComputations = "vector_distance_computations"
	searchTime           = "vector_search_time"
	VecEntry             = "__vector_entry"
	VecDead              = "__vector_dead"
	VectorIndexMaxLevels = 5
	EfConstruction       = 16
	EfSearch             = 12
	numEdgesConst        = 2
	// ByteData indicates the key stores data.
	ByteData = byte(0x00)
	// DefaultPrefix is the prefix used for data, index and reverse keys so that relative
	DefaultPrefix = byte(0x00)
	// NsSeparator is the separator between the namespace and attribute.
	NsSeparator = "-"
)

var (
	errNilVector           = errors.New("nil vector returned")
	errFetchingPostingList = errors.New("error fetching posting list")
)

type SearchResult struct {
	nnUids        []uint64
	traversalPath []uint64
	extraMetrics  map[string]uint64
}

func (s *SearchResult) GetNnUids() []uint64 {
	return s.nnUids
}

func (s *SearchResult) GetTraversalPath() []uint64 {
	return s.traversalPath
}

func (s *SearchResult) GetExtraMetrics() map[string]uint64 {
	return s.extraMetrics
}

func applyDistanceFunction[T c.Float](a, b []T, floatBits int, funcName string,
	applyFn32 func(a, b []float32) float32, applyFn64 func(a, b []float64) float64) (T, error) {
	if len(a) != len(b) {
		err := errors.New(fmt.Sprintf("can not compute %s on vectors of different lengths", funcName))
		return T(0), err
	}

	if floatBits == 32 {
		var a1, b1 []float32
		a1 = *(*[]float32)(unsafe.Pointer(&a))
		b1 = *(*[]float32)(unsafe.Pointer(&b))
		return T(applyFn32(a1, b1)), nil
	} else if floatBits == 64 {
		var a1, b1 []float64
		a1 = *(*[]float64)(unsafe.Pointer(&a))
		b1 = *(*[]float64)(unsafe.Pointer(&b))
		return T(applyFn64(a1, b1)), nil
	}

	panic("While applying function on two floats, found an invalid number of float bits")

}

// This needs to implement signature of SimilarityType[T].distanceScore
// function, hence it takes in a floatBits parameter,
// but doesn't actually use it.
func dotProduct[T c.Float](a, b []T, floatBits int) (T, error) {
	return applyDistanceFunction(a, b, floatBits, "dot product", vek32.Dot, vek.Dot)
}

// This needs to implement signature of SimilarityType[T].distanceScore
// function, hence it takes in a floatBits parameter.
func cosineSimilarity[T c.Float](a, b []T, floatBits int) (T, error) {
	return applyDistanceFunction(a, b, floatBits, "cosine distance", vek32.CosineSimilarity, vek.CosineSimilarity)
}

// This needs to implement signature of SimilarityType[T].distanceScore
// function, hence it takes in a floatBits parameter,
// but doesn't actually use it.
func euclideanDistanceSq[T c.Float](a, b []T, floatBits int) (T, error) {
	return applyDistanceFunction(a, b, floatBits, "euclidean distance", vek32.Distance, vek.Distance)
}

func EuclideanDistanceSq[T c.Float](a, b []T, floatBits int) (T, error) {
	return applyDistanceFunction(a, b, floatBits, "euclidean distance", vek32.Distance, vek.Distance)
}

// Used for distance, since shorter distance is better
func insortPersistentHeapAscending[T c.Float](
	slice []minPersistentHeapElement[T],
	val minPersistentHeapElement[T]) []minPersistentHeapElement[T] {
	i := sort.Search(len(slice), func(i int) bool { return slice[i].value > val.value })
	var empty T
	slice = append(slice, *initPersistentHeapElement(empty, notAUid, false))
	copy(slice[i+1:], slice[i:])
	slice[i] = val
	return slice
}

// Used for cosine similarity, since higher similarity score is better
func insortPersistentHeapDescending[T c.Float](
	slice []minPersistentHeapElement[T],
	val minPersistentHeapElement[T]) []minPersistentHeapElement[T] {
	i := sort.Search(len(slice), func(i int) bool { return slice[i].value < val.value })
	var empty T
	slice = append(slice, *initPersistentHeapElement(empty, notAUid, false))
	copy(slice[i+1:], slice[i:])
	slice[i] = val
	return slice
}

func isBetterScoreForDistance[T c.Float](a, b T) bool {
	return a < b
}

func isBetterScoreForSimilarity[T c.Float](a, b T) bool {
	return a > b
}

func ParseEdges(s string) ([]uint64, error) {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return []uint64{}, nil
	}
	trimmedPre := strings.TrimPrefix(s, "[")
	if len(trimmedPre) == len(s) {
		return nil, cannotConvertToUintSlice(s)
	}
	trimmed := strings.TrimRight(trimmedPre, "]")
	if len(trimmed) == len(trimmedPre) {
		return nil, cannotConvertToUintSlice(s)
	}
	if len(trimmed) == 0 {
		return []uint64{}, nil
	}
	if strings.Contains(trimmed, ",") {
		// Splitting based on comma-separation.
		values := strings.Split(trimmed, ",")
		result := make([]uint64, len(values))
		for i := range values {
			trimmedVal := strings.TrimSpace(values[i])
			val, err := strconv.ParseUint(trimmedVal, 10, 64)
			if err != nil {
				return nil, cannotConvertToUintSlice(s)
			}
			result[i] = val
		}
		return result, nil
	}
	values := strings.Split(trimmed, " ")
	result := make([]uint64, 0, len(values))
	for i := range values {
		if len(values[i]) == 0 {
			// skip if we have an empty string. This can naturally
			// occur if input s was "[1.0     2.0]"
			// notice the extra whitespace in separation!
			continue
		}
		if len(values[i]) > 0 {
			val, err := strconv.ParseUint(values[i], 10, 64)
			if err != nil {
				return nil, cannotConvertToUintSlice(s)
			}
			result = append(result, val)
		}
	}
	return result, nil
}

func cannotConvertToUintSlice(s string) error {
	return errors.Errorf("Cannot convert %s to uint slice", s)
}

// TODO: Move SimilarityType to index package.
//
//	Remove "hnsw-isms".
type SimilarityType[T c.Float] struct {
	indexType     string
	distanceScore func(v, w []T, floatBits int) (T, error)
	insortHeap    func(slice []minPersistentHeapElement[T], val minPersistentHeapElement[T]) []minPersistentHeapElement[T]
	isBetterScore func(a, b T) bool
}

func GetSimType[T c.Float](indexType string, floatBits int) SimilarityType[T] {
	switch {
	case indexType == Euclidean:
		return SimilarityType[T]{indexType: Euclidean, distanceScore: euclideanDistanceSq[T],
			insortHeap: insortPersistentHeapAscending[T], isBetterScore: isBetterScoreForDistance[T]}
	case indexType == Cosine:
		return SimilarityType[T]{indexType: Cosine, distanceScore: cosineSimilarity[T],
			insortHeap: insortPersistentHeapDescending[T], isBetterScore: isBetterScoreForSimilarity[T]}
	case indexType == DotProd:
		return SimilarityType[T]{indexType: DotProd, distanceScore: dotProduct[T],
			insortHeap: insortPersistentHeapDescending[T], isBetterScore: isBetterScoreForSimilarity[T]}
	default:
		return SimilarityType[T]{indexType: Euclidean, distanceScore: euclideanDistanceSq[T],
			insortHeap: insortPersistentHeapAscending[T], isBetterScore: isBetterScoreForDistance[T]}
	}
}

// populateEdgeDataFromStore(keyString, uid, c, edgeData)
// will fill edgeData with the contents of the neighboring edges for
// a given DataKey by looking into the given cache (which may result
// in a call to the underlying persistent storage).
// If data is found for the key, this returns true, otherwise, it
// returns false. If the data was found (and there were no errors),
// it populates edgeData with the found contents.
func populateEdgeDataFromKeyWithCacheType(
	keyString string,
	uid uint64,
	c index.CacheType,
	edgeData *[][]uint64) (bool, error) {
	data := c.GetEdge(uid)
	if data == nil {
		return false, nil
	}
	err := decodeUint64MatrixUnsafe(*data, edgeData)
	return true, err
}

// entryUuidInsert adds the entry uuid to the given key
func entryUuidInsert(
	ctx context.Context,
	c index.CacheType,
	predEntryKey string,
	entryUuid []byte) error {
	c.SetOther(predEntryKey, &entryUuid)
	return nil
}

func ConcatStrings(strs ...string) string {
	total := ""
	for _, s := range strs {
		total += s
	}
	return total
}

func getInsertLayer(maxLevels int) int {
	// multFactor is a multiplicative factor used to normalize the distribution
	var level int
	randFloat := rand.Float64()
	for i := range maxLevels {
		// calculate level based on section 3.1 here
		if randFloat < math.Pow(1.0/float64(5), float64(maxLevels-1-i)) {
			level = i
			break
		}
	}
	return level
}

var emptyVec = []byte{}

// adds the data corresponding to a uid to the given vec variable in the form of []T
// this does not allocate memory for vec, so it must be allocated before calling this function
func (ph *persistentHNSW[T]) getVecFromUid(uid uint64, c index.CacheType, vec *[]T) error {
	data := c.GetVector(uid)
	if data == nil {
		// no vector. Return empty array of floats
		index.BytesAsFloatArray(emptyVec, vec, ph.floatBits)
		return fmt.Errorf("%w; %w", errNilVector, errFetchingPostingList)
	}
	index.BytesAsFloatArray(*data, vec, ph.floatBits)
	return nil
}

// chooses whether to create the entry and start nodes based on if it already
// exists, and if it hasnt been created yet, it adds the startNode to all
// levels.
func (ph *persistentHNSW[T]) createEntryAndStartNodes(
	ctx context.Context,
	c index.CacheType,
	inUuid uint64,
	vec *[]T) (uint64, error) {

	data := c.GetOther(ph.vecEntryKey)

	create_edges := func(inUuid uint64) (uint64, error) {
		err := ph.addStartNodeToAllLevels(ctx, c, inUuid)
		if err != nil {
			return 0, err
		}
		// return entry node at all levels
		return 0, nil
	}

	if data == nil {
		// no entries in vector index yet b/c no entry exists, so put in all levels
		return create_edges(inUuid)
	}

	entry := BytesToUint64(*data) // convert entry Uuid returned from Get to uint64
	err := ph.getVecFromUid(entry, c, vec)
	if err != nil || len(*vec) == 0 {
		// The entry vector has been deleted. We have to create a new entry vector.
		entry, err := ph.calculateNewEntryVec(ctx, c, vec)
		if err != nil {
			// No other node exists, go with the new node that has come
			return create_edges(inUuid)
		}
		return create_edges(entry)
	}

	return entry, nil
}

// Converts the matrix into linear array that looks like
// [0: Number of rows  1: Length of row1 2-n: Data of row1 3: Length of row2 ..]
func encodeUint64MatrixUnsafe(matrix [][]uint64) []byte {
	if len(matrix) == 0 {
		return nil
	}

	// Calculate the total size
	var totalSize uint64
	for _, row := range matrix {
		totalSize += uint64(len(row))*uint64(unsafe.Sizeof(uint64(0))) + uint64(unsafe.Sizeof(uint64(0)))
	}
	totalSize += uint64(unsafe.Sizeof(uint64(0)))

	// Create a byte slice with the appropriate size
	data := make([]byte, totalSize)

	offset := 0
	// Write number of rows
	rows := uint64(len(matrix))
	copy(data[offset:offset+8], (*[8]byte)(unsafe.Pointer(&rows))[:])
	offset += 8

	// Write each row's length and data
	for _, row := range matrix {
		rowLen := uint64(len(row))
		copy(data[offset:offset+8], (*[8]byte)(unsafe.Pointer(&rowLen))[:])
		offset += 8
		for i := range row {
			copy(data[offset:offset+8], (*[8]byte)(unsafe.Pointer(&row[i]))[:])
			offset += 8
		}
	}

	return data
}

func decodeUint64MatrixUnsafe(data []byte, matrix *[][]uint64) error {
	if len(data) == 0 {
		return nil
	}

	offset := 0
	// Read number of rows
	rows := *(*uint64)(unsafe.Pointer(&data[offset]))
	offset += 8

	*matrix = make([][]uint64, rows)

	for i := 0; i < int(rows); i++ {
		// Read row length
		rowLen := *(*uint64)(unsafe.Pointer(&data[offset]))
		offset += 8

		(*matrix)[i] = make([]uint64, rowLen)
		for j := 0; j < int(rowLen); j++ {
			(*matrix)[i][j] = *(*uint64)(unsafe.Pointer(&data[offset]))
			offset += 8
		}
	}

	return nil
}

// adds empty layers to all levels
func (ph *persistentHNSW[T]) addStartNodeToAllLevels(
	ctx context.Context,
	c index.CacheType,
	inUuid uint64) error {
	emptyEdgesBytes := encodeUint64MatrixUnsafe(make([][]uint64, ph.maxLevels))
	// creates empty at all levels only for entry node
	err := ph.newPersistentEdgeKeyValueEntry(ctx, c, inUuid, emptyEdgesBytes)
	if err != nil {
		return err
	}
	inUuidByte := Uint64ToBytes(inUuid)
	// add inUuid as entry for this structure from now on
	err = entryUuidInsert(ctx, c, ph.vecEntryKey, inUuidByte)
	if err != nil {
		return err
	}
	return nil
}

// creates a new edge with the given uuid and edges. Lock must be held before calling this function
func (ph *persistentHNSW[T]) newPersistentEdgeKeyValueEntry(ctx context.Context,
	c index.CacheType, uuid uint64, edges []byte) error {
	c.SetEdge(uuid, &edges)
	return nil
}

func (ph *persistentHNSW[T]) distance_betw(ctx context.Context, c index.CacheType, inUuid, outUuid uint64, inVec,
	outVec *[]T) T {
	err := ph.getVecFromUid(outUuid, c, outVec)
	if err != nil {
		log.Printf("[ERROR] While getting vector %s", err)
		return -1
	}

	d, err := ph.simType.distanceScore(*inVec, *outVec, ph.floatBits)
	if err != nil {
		log.Printf("[ERROR] While getting vector %s", err)
		return -1
	}
	return d
}

type HeapDataHolder struct {
	data    []uint64
	compare func(a, b uint64) bool
}

// Len is the number of elements in the collection.
func (h HeapDataHolder) Len() int {
	return len(h.data)
}

// Less reports whether the element with index i should sort before the element with index j.
func (h HeapDataHolder) Less(i, j int) bool {
	return h.compare(h.data[i], h.data[j])
}

// Swap swaps the elements with indexes i and j.
func (h HeapDataHolder) Swap(i, j int) {
	h.data[i], h.data[j] = h.data[j], h.data[i]
}

// Push adds an element to the heap.
func (h *HeapDataHolder) Push(x interface{}) {
	h.data = append(h.data, x.(uint64))
}

// Pop removes and returns the maximum element from the heap.
func (h *HeapDataHolder) Pop() interface{} {
	old := h.data
	n := len(old)
	x := old[n-1]
	h.data = old[0 : n-1]
	return x
}

// addNeighbors adds the neighbors of the given uuid to the given level.
// It returns the edge created and the error if any.
func (ph *persistentHNSW[T]) addNeighbors(ctx context.Context, c index.CacheType,
	uuid uint64, allLayerNeighbors [][]uint64) error {

	// Lock the vector key
	edges_data := c.GetEdge(uuid)
	var allLayerEdges [][]uint64
	if edges_data != nil {
		decodeUint64MatrixUnsafe(*edges_data, &allLayerEdges)
	} else {
		encodedData := encodeUint64MatrixUnsafe(allLayerNeighbors)
		c.SetEdge(uuid, &encodedData)
		return nil
	}

	var inVec, outVec []T
	for level := range ph.maxLevels {
		var err error
		allLayerEdges[level], err = ph.removeDeadNodes(allLayerEdges[level], c)
		if err != nil {
			return err
		}
		// This adds at most efConstruction number of edges for each layer for this node
		allLayerEdges[level] = append(allLayerEdges[level], allLayerNeighbors[level]...)
		if len(allLayerEdges[level]) > ph.efConstruction {
			err := ph.getVecFromUid(uuid, c, &inVec)
			if err != nil {
				log.Printf("[ERROR] While getting vector %s", err)
			} else {
				h := &HeapDataHolder{
					data: allLayerEdges[level],
					compare: func(i, j uint64) bool {
						return ph.distance_betw(ctx, c, uuid, i, &inVec, &outVec) >
							ph.distance_betw(ctx, c, uuid, j, &inVec, &outVec)
					}}

				for _, e := range allLayerNeighbors[level] {
					heap.Push(h, e)
				}
			}
			allLayerEdges[level] = allLayerEdges[level][:ph.efConstruction]
		}
	}

	inboundEdgesBytes := encodeUint64MatrixUnsafe(allLayerEdges)
	c.SetEdge(uuid, &inboundEdgesBytes)
	return nil
}

// removeDeadNodes(nnEdges, tc) removes dead nodes from nnEdges and returns the new nnEdges
func (ph *persistentHNSW[T]) removeDeadNodes(nnEdges []uint64, c index.CacheType) ([]uint64, error) {
	// TODO add a path to delete deadNodes
	if ph.deadNodes == nil {
		data := c.GetOther(ph.vecDead)
		if data == nil {
			return nnEdges, nil
		}

		var deadNodes []uint64
		if data != nil { // if dead nodes exist, convert to []uint64
			var err error
			deadNodes, err = ParseEdges(string(*data))
			if err != nil {
				return []uint64{}, err
			}
		}

		ph.deadNodes = make(map[uint64]struct{})
		for _, n := range deadNodes {
			ph.deadNodes[n] = struct{}{}
		}
	}
	if len(ph.deadNodes) == 0 {
		return nnEdges, nil
	}

	var diff []uint64
	for _, s := range nnEdges {
		if _, ok := ph.deadNodes[s]; !ok {
			diff = append(diff, s)
			continue
		}
	}
	return diff, nil
}

func Uint64ToBytes(key uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, key)
	return b
}

func BytesToUint64(bytes []byte) uint64 {
	return binary.BigEndian.Uint64(bytes)
}

func isEqual[T c.Float](a []T, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i, val := range a {
		if val != b[i] {
			return false
		}
	}
	return true
}

// DataKey generates a data key with the given attribute and UID.
// The structure of a data key is as follows:
//
// byte 0: key type prefix (set to DefaultPrefix or ByteSplit if part of a multi-part list)
// byte 1-2: length of attr
// next len(attr) bytes: value of attr
// next byte: data type prefix (set to ByteData)
// next eight bytes: value of uid
// next eight bytes (optional): if the key corresponds to a split list, the startUid of
// the split stored in this key and the first byte will be sets to ByteSplit.
func DataKey(attr string, uid uint64) []byte {
	extra := 1 + 8 // ByteData + UID
	buf, prefixLen := generateKey(DefaultPrefix, attr, extra)

	rest := buf[prefixLen:]
	rest[0] = ByteData

	rest = rest[1:]
	binary.BigEndian.PutUint64(rest, uid)
	return buf
}

// genKey creates the key and writes the initial bytes (type byte, length of attribute,
// and the attribute itself). It leaves the rest of the key empty for further processing
// if necessary. It also returns next index from where further processing should be done.
func generateKey(typeByte byte, attr string, extra int) ([]byte, int) {
	// Separate namespace and attribute from attr and write namespace in the first 8 bytes of key.
	namespace, attr := ParseNamespaceBytes(attr)
	prefixLen := 1 + 8 + 2 + len(attr) // byteType + ns + len(pred) + pred
	buf := make([]byte, prefixLen+extra)
	buf[0] = typeByte
	AssertTrue(copy(buf[1:], namespace) == 8)
	rest := buf[9:]

	writeAttr(rest, attr)
	return buf, prefixLen
}

func ParseNamespaceBytes(attr string) ([]byte, string) {
	splits := strings.SplitN(attr, NsSeparator, 2)
	ns := make([]byte, 8)
	binary.BigEndian.PutUint64(ns, strToUint(splits[0]))
	return ns, splits[1]
}

// AssertTrue asserts that b is true. Otherwise, it would log fatal.
func AssertTrue(b bool) {
	if !b {
		log.Fatalf("%+v", errors.Errorf("Assert failed"))
	}
}

func writeAttr(buf []byte, attr string) []byte {
	AssertTrue(len(attr) < math.MaxUint16)
	binary.BigEndian.PutUint16(buf[:2], uint16(len(attr)))

	rest := buf[2:]
	AssertTrue(len(attr) == copy(rest, attr))

	return rest[len(attr):]
}

// For consistency, use base16 to encode/decode the namespace.
func strToUint(s string) uint64 {
	ns, err := strconv.ParseUint(s, 16, 64)
	Check(err)
	return ns
}

// Check logs fatal if err != nil.
func Check(err error) {
	if err != nil {
		err = errors.Wrap(err, "")
		log.Fatalf("%+v", err)
	}
}
