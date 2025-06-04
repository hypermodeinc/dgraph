/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package algo

import (
	"container/heap"
	"sort"
	"sync"

	"github.com/hypermodeinc/dgraph/v25/codec"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
)

const jump = 32          // Jump size in InsersectWithJump.
const linVsBinRatio = 10 // When is linear search better than binary

// ApplyFilter applies a filter to our UIDList.
func ApplyFilter(u *pb.List, f func(uint64, int) bool) {
	out := u.Uids[:0]
	for i, uid := range u.Uids {
		if f(uid, i) {
			out = append(out, uid)
		}
	}
	u.Uids = out
}

// IntersectCompressedWith intersects a packed list of UIDs with another list
// and writes the output to o.
func IntersectCompressedWith(pack *pb.UidPack, afterUID uint64, v, o *pb.List) {
	if pack == nil {
		return
	}
	dec := codec.Decoder{Pack: pack}
	dec.Seek(afterUID, codec.SeekStart)
	n := dec.ApproxLen()
	m := len(v.Uids)

	if n > m {
		n, m = m, n
	}
	dst := o.Uids[:0]

	// If n equals 0, set it to 1 to avoid division by zero.
	if n == 0 {
		n = 1
	}

	// Select appropriate function based on heuristics.
	ratio := float64(m) / float64(n)
	if ratio < linVsBinRatio {
		IntersectCompressedWithLinJump(&dec, v.Uids, &dst)
	} else {
		IntersectCompressedWithBin(&dec, v.Uids, &dst)
	}
	o.Uids = dst
}

// IntersectCompressedWithLinJump performs the intersection linearly.
func IntersectCompressedWithLinJump(dec *codec.Decoder, v []uint64, o *[]uint64) {
	m := len(v)
	k := 0
	_, off := IntersectWithLin(dec.Uids(), v[k:], o)
	k += off

	for k < m {
		u := dec.LinearSeek(v[k])
		if len(u) == 0 {
			break
		}
		_, off := IntersectWithLin(u, v[k:], o)
		if off == 0 {
			off = 1 // If v[k] isn't in u, move forward.
		}

		k += off
	}
}

// IntersectCompressedWithBin is based on the paper
// "Fast Intersection Algorithms for Sorted Sequences"
// https://link.springer.com/chapter/10.1007/978-3-642-12476-1_3
// Call seek on dec before calling this function
func IntersectCompressedWithBin(dec *codec.Decoder, q []uint64, o *[]uint64) {
	ld := codec.ExactLen(dec.Pack)
	lq := len(q)

	if lq == 0 {
		// Not checking whether ld == 0 because that is an approximate size.
		// If the actual length of the list is zero, the for loop below will
		// do nothing as expected.
		return
	}

	// Pick the shorter list and do binary search
	if ld <= lq {
		for {
			blockUids := dec.Uids()
			if len(blockUids) == 0 {
				break
			}
			_, off := IntersectWithJump(blockUids, q, o)
			q = q[off:]
			if len(q) == 0 {
				return
			}
			dec.Next()
		}
		return
	}

	uids := dec.Uids()
	qidx := 0
	for {
		if qidx >= len(q) {
			return
		}
		u := q[qidx]
		if len(uids) == 0 || u > uids[len(uids)-1] {
			if lq*linVsBinRatio < ld {
				uids = dec.LinearSeek(u)
			} else {
				uids = dec.SeekToBlock(u, codec.SeekCurrent)
			}
			if len(uids) == 0 {
				return
			}
		}
		_, off := IntersectWithJump(uids, q[qidx:], o)
		if off == 0 {
			off = 1 // if v[k] isn't in u, move forward
		}
		qidx += off
	}
}

// IntersectWith intersects u with v. The update is made to o.
// u, v should be sorted.
func IntersectWith(u, v, o *pb.List) {
	n := len(u.Uids)
	m := len(v.Uids)

	if n > m {
		n, m = m, n
	}
	if o.Uids == nil {
		o.Uids = make([]uint64, 0, n)
	}
	dst := o.Uids[:0]
	if n == 0 {
		n = 1
	}
	// Select appropriate function based on heuristics.
	ratio := float64(m) / float64(n)
	switch {
	case ratio < 100:
		IntersectWithLin(u.Uids, v.Uids, &dst)
	case ratio < 500:
		IntersectWithJump(u.Uids, v.Uids, &dst)
	default:
		IntersectWithBin(u.Uids, v.Uids, &dst)
	}
	o.Uids = dst
}

// IntersectWithLin performs the intersection linearly.
func IntersectWithLin(u, v []uint64, o *[]uint64) (int, int) {
	n := len(u)
	m := len(v)
	i, k := 0, 0
	for i < n && k < m {
		uid := u[i]
		vid := v[k]
		switch {
		case uid > vid:
			for k = k + 1; k < m && v[k] < uid; k++ {
			}
		case uid == vid:
			*o = append(*o, uid)
			k++
			i++
		default:
			for i = i + 1; i < n && u[i] < vid; i++ {
			}
		}
	}
	return i, k
}

// IntersectWithJump performs the intersection linearly but jumping jump steps
// between iterations.
func IntersectWithJump(u, v []uint64, o *[]uint64) (int, int) {
	n := len(u)
	m := len(v)
	i, k := 0, 0
	for i < n && k < m {
		uid := u[i]
		vid := v[k]
		switch {
		case uid == vid:
			*o = append(*o, uid)
			k++
			i++
		case k+jump < m && uid > v[k+jump]:
			k += jump
		case i+jump < n && vid > u[i+jump]:
			i += jump
		case uid > vid:
			for k = k + 1; k < m && v[k] < uid; k++ {
			}
		default:
			for i = i + 1; i < n && u[i] < vid; i++ {
			}
		}
	}
	return i, k
}

// IntersectWithBin is based on the paper
// "Fast Intersection Algorithms for Sorted Sequences"
// https://link.springer.com/chapter/10.1007/978-3-642-12476-1_3
// Returns where to move the second array(q) to. O means not found
func IntersectWithBin(d, q []uint64, o *[]uint64) int {
	ld := len(d)
	lq := len(q)

	if ld < lq {
		ld, lq = lq, ld
		d, q = q, d
	}
	if ld == 0 || lq == 0 || d[ld-1] < q[0] || q[lq-1] < d[0] {
		return 0
	}

	val := d[0]
	minq := sort.Search(len(q), func(i int) bool {
		return q[i] >= val
	})

	val = d[len(d)-1]
	maxq := sort.Search(len(q), func(i int) bool {
		return q[i] > val
	})

	binIntersect(d, q[minq:maxq], o)
	return maxq
}

// binIntersect is the recursive function used.
// NOTE: len(d) >= len(q) (Must hold)
func binIntersect(d, q []uint64, final *[]uint64) {
	if len(d) == 0 || len(q) == 0 {
		return
	}
	midq := len(q) / 2
	qval := q[midq]
	midd := sort.Search(len(d), func(i int) bool {
		return d[i] >= qval
	})

	dd := d[0:midd]
	qq := q[0:midq]
	if len(dd) > len(qq) { // D > Q
		binIntersect(dd, qq, final)
	} else {
		binIntersect(qq, dd, final)
	}

	if midd >= len(d) {
		return
	}
	if d[midd] == qval {
		*final = append(*final, qval)
	} else {
		midd--
	}

	dd = d[midd+1:]
	qq = q[midq+1:]
	if len(dd) > len(qq) { // D > Q
		binIntersect(dd, qq, final)
	} else {
		binIntersect(qq, dd, final)
	}
}

type listInfo struct {
	l      *pb.List
	length int
}

// IntersectSorted calculates the intersection of multiple lists and performs
// the intersections from the smallest to the largest list.
func IntersectSorted(lists []*pb.List) *pb.List {
	if len(lists) == 0 {
		return &pb.List{}
	}
	ls := make([]listInfo, 0, len(lists))
	for _, list := range lists {
		ls = append(ls, listInfo{
			l:      list,
			length: len(list.Uids),
		})
	}
	// Sort the lists based on length.
	sort.Slice(ls, func(i, j int) bool {
		return ls[i].length < ls[j].length
	})
	out := &pb.List{Uids: make([]uint64, ls[0].length)}
	if len(ls) == 1 {
		copy(out.Uids, ls[0].l.Uids)
		return out
	}

	IntersectWith(ls[0].l, ls[1].l, out)
	// Intersect from smallest to largest.
	for i := 2; i < len(ls); i++ {
		IntersectWith(out, ls[i].l, out)
		// Break if we reach size 0 as we can no longer
		// add any element.
		if len(out.Uids) == 0 {
			break
		}
	}
	return out
}

// Difference returns the difference of two lists.
func Difference(u, v *pb.List) *pb.List {
	if u == nil || v == nil {
		return &pb.List{Uids: make([]uint64, 0)}
	}
	n := len(u.Uids)
	m := len(v.Uids)
	out := make([]uint64, 0, n/2)
	i, k := 0, 0
	for i < n && k < m {
		uid := u.Uids[i]
		vid := v.Uids[k]
		switch {
		case uid < vid:
			for i < n && u.Uids[i] < vid {
				out = append(out, u.Uids[i])
				i++
			}
		case uid == vid:
			i++
			k++
		default:
			for k = k + 1; k < m && v.Uids[k] < uid; k++ {
			}
		}
	}
	for i < n && k >= m {
		out = append(out, u.Uids[i])
		i++
	}
	return &pb.List{Uids: out}
}

func MergeSortedMoreMem(lists []*pb.List) *pb.List {
	numThreads := 10
	if len(lists) > numThreads*numThreads {
		k := numThreads
		res := []*pb.List{}
		var wg sync.WaitGroup
		var mutex sync.Mutex
		wg.Add(k)
		for i := 0; i < k; i++ {
			go func() {
				defer wg.Done()
				end := (i + 1) * len(lists) / k
				if end > len(lists) {
					end = len(lists)
				}
				result := MergeSortedMoreMem(lists[i*len(lists)/k : end])
				mutex.Lock()
				res = append(res, result)
				mutex.Unlock()
			}()
		}
		wg.Wait()
		return MergeSortedMoreMem(res)
	} else {
		return internalMergeSort(lists)
	}
}

// MergeSorted merges sorted lists.
func internalMergeSort(lists []*pb.List) *pb.List {
	if len(lists) == 0 {
		return new(pb.List)
	}

	h := &uint64Heap{}
	heap.Init(h)
	maxSz := 0

	for i, l := range lists {
		if l == nil {
			continue
		}
		lenList := len(l.Uids)
		if lenList > 0 {
			heap.Push(h, elem{
				val:     l.Uids[0],
				listIdx: i,
			})
			if lenList > maxSz {
				maxSz = lenList
			}
		}
	}

	// Our final output. Give it an approximate capacity as copies are expensive.
	output := make([]uint64, 0, maxSz)
	// idx[i] is the element we are looking at for lists[i].
	idx := make([]int, len(lists))
	var last uint64   // Last element added to sorted / final output.
	for h.Len() > 0 { // While heap is not empty.
		me := (*h)[0] // Peek at the top element in heap.
		if len(output) == 0 || me.val != last {
			output = append(output, me.val) // Add if unique.
			last = me.val
		}
		l := lists[me.listIdx]
		if idx[me.listIdx] >= len(l.Uids)-1 {
			heap.Pop(h)
		} else {
			idx[me.listIdx]++
			val := l.Uids[idx[me.listIdx]]
			(*h)[0].val = val
			heap.Fix(h, 0) // Faster than Pop() followed by Push().
		}
	}
	return &pb.List{Uids: output}
}

func MergeSorted(lists []*pb.List) *pb.List {
	// Calculate total capacity needed
	totalCap := 0
	for _, l := range lists {
		if l != nil {
			totalCap += len(l.Uids)
		}
	}

	// Pre-allocate one big buffer
	bigBuffer := make([]uint64, totalCap)

	result := mergeSortedWithBuffer(lists, bigBuffer)
	return result
}

func mergeSortedWithBuffer(lists []*pb.List, buffer []uint64) *pb.List {
	numThreads := 10
	if len(lists) > numThreads*numThreads {
		// Calculate how much buffer each goroutine needs
		chunkSizes := make([]int, numThreads)
		totalNeeded := 0

		for i := 0; i < numThreads; i++ {
			start := i * len(lists) / numThreads
			end := (i + 1) * len(lists) / numThreads
			if end > len(lists) {
				end = len(lists)
			}

			chunkCap := 0
			for j := start; j < end; j++ {
				if lists[j] != nil {
					chunkCap += len(lists[j].Uids)
				}
			}
			chunkSizes[i] = chunkCap
			totalNeeded += chunkCap
		}

		// Calculate buffer offsets for each goroutine
		bufferOffsets := make([]int, numThreads)
		bufferOffsets[0] = 0
		for i := 1; i < numThreads; i++ {
			bufferOffsets[i] = bufferOffsets[i-1] + chunkSizes[i-1]
		}

		// Distribute buffer slices to each goroutine
		intermediateResults := make([]*pb.List, numThreads)
		var wg sync.WaitGroup

		wg.Add(numThreads)
		for i := 0; i < numThreads; i++ {
			go func(idx int) {
				defer wg.Done()
				start := idx * len(lists) / numThreads
				end := (idx + 1) * len(lists) / numThreads
				if end > len(lists) {
					end = len(lists)
				}

				// Give this goroutine its slice of the big buffer
				bufferStart := bufferOffsets[idx]
				bufferEnd := bufferStart + chunkSizes[idx]
				goroutineBuffer := buffer[bufferStart:bufferEnd:bufferEnd][:0]
				result := internalMergeSortWithBuffer(lists[start:end], goroutineBuffer)
				intermediateResults[idx] = result
			}(i)
		}
		wg.Wait()

		// Filter out nil results
		validResults := make([]*pb.List, 0, numThreads)
		for _, result := range intermediateResults {
			if result != nil && len(result.Uids) > 0 {
				validResults = append(validResults, result)
			}
		}

		// Use the remaining part of buffer for final merge
		finalBuffer := buffer[totalNeeded:][:0]
		return internalMergeSortWithBuffer(validResults, finalBuffer)
	} else {
		return internalMergeSortWithBuffer(lists, buffer)
	}
}

func internalMergeSortWithBuffer(lists []*pb.List, buffer []uint64) *pb.List {
	if len(lists) == 0 {
		return &pb.List{Uids: buffer[:0]}
	}

	h := &uint64Heap{}
	heap.Init(h)

	for i, l := range lists {
		if l == nil || len(l.Uids) == 0 {
			continue
		}
		heap.Push(h, elem{
			val:     l.Uids[0],
			listIdx: i,
		})
	}

	// Use the provided buffer
	output := buffer[:0]
	idx := make([]int, len(lists))
	var last uint64

	for h.Len() > 0 {
		me := (*h)[0]
		if len(output) == 0 || me.val != last {
			output = append(output, me.val)
			last = me.val
		}
		l := lists[me.listIdx]
		if idx[me.listIdx] >= len(l.Uids)-1 {
			heap.Pop(h)
		} else {
			idx[me.listIdx]++
			val := l.Uids[idx[me.listIdx]]
			(*h)[0].val = val
			heap.Fix(h, 0)
		}
	}

	return &pb.List{Uids: output}
}

// IndexOf performs a binary search on the uids slice and returns the index at
// which it finds the uid, else returns -1
func IndexOf(u *pb.List, uid uint64) int {
	i := sort.Search(len(u.Uids), func(i int) bool { return u.Uids[i] >= uid })
	if i < len(u.Uids) && u.Uids[i] == uid {
		return i
	}
	return -1
}

// ToUintsListForTest converts to list of uints for testing purpose only.
func ToUintsListForTest(ul []*pb.List) [][]uint64 {
	out := make([][]uint64, 0, len(ul))
	for _, u := range ul {
		out = append(out, u.Uids)
	}
	return out
}
