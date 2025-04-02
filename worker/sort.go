/*
 * SPDX-FileCopyrightText:  Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"context"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/dgraph-io/badger/v4"
	"github.com/hypermodeinc/dgraph/v24/algo"
	"github.com/hypermodeinc/dgraph/v24/posting"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/schema"
	"github.com/hypermodeinc/dgraph/v24/tok"
	"github.com/hypermodeinc/dgraph/v24/types"
	"github.com/hypermodeinc/dgraph/v24/x"
	"go.opentelemetry.io/otel/attribute"
)

var emptySortResult pb.SortResult

type sortresult struct {
	reply *pb.SortResult
	// For multi sort we apply the offset in two stages. In the first stage a part of the offset
	// is applied but equal values in the bucket that the offset falls into are skipped. This
	// slice stores the remaining offset for individual uid lists that must be applied after all
	// multi sort is done.
	// TODO (pawan) - Offset has type int32 whereas paginate function returns an int. We should
	//                use a common type so that we can avoid casts between the two.
	multiSortOffsets []int32
	vals             [][]types.Val
	err              error
}

// SortOverNetwork sends sort query over the network.
func SortOverNetwork(ctx context.Context, q *pb.SortMessage) (*pb.SortResult, error) {
	gid, err := groups().BelongsToReadOnly(q.Order[0].Attr, q.ReadTs)
	if err != nil {
		return &emptySortResult, err
	} else if gid == 0 {
		return &emptySortResult,
			errors.Errorf("Cannot sort by unknown attribute %s", x.ParseAttr(q.Order[0].Attr))
	}

	if span := trace.SpanFromContext(ctx); span != nil {
		span.SetAttributes(
			attribute.String("attribute", q.Order[0].Attr),
			attribute.Int("groupId", int(gid)),
		)
	}

	if groups().ServesGroup(gid) {
		// No need for a network call, as this should be run from within this instance.
		return processSort(ctx, q)
	}

	result, err := processWithBackupRequest(
		ctx, gid, func(ctx context.Context, c pb.WorkerClient) (interface{}, error) {
			return c.Sort(ctx, q)
		})
	if err != nil {
		return &emptySortResult, err
	}
	return result.(*pb.SortResult), nil
}

// Sort is used to sort given UID matrix.
func (w *grpcWorker) Sort(ctx context.Context, s *pb.SortMessage) (*pb.SortResult, error) {
	if ctx.Err() != nil {
		return &emptySortResult, ctx.Err()
	}
	ctx, span := otel.Tracer("").Start(ctx, "worker.Sort")
	defer span.End()

	gid, err := groups().BelongsToReadOnly(s.Order[0].Attr, s.ReadTs)
	if err != nil {
		return &emptySortResult, err
	}

	span.AddEvent("Sorting", trace.WithAttributes(
		attribute.String("attribute", s.Order[0].Attr),
		attribute.Int("groupId", int(gid))))

	if gid != groups().groupId() {
		return nil, errors.Errorf("attr: %q groupId: %v Request sent to wrong server.",
			s.Order[0].Attr, gid)
	}

	var reply *pb.SortResult
	c := make(chan error, 1)
	go func() {
		var err error
		reply, err = processSort(ctx, s)
		c <- err
	}()

	select {
	case <-ctx.Done():
		return &emptySortResult, ctx.Err()
	case err := <-c:
		return reply, err
	}
}

var (
	errContinue = errors.Errorf("Continue processing buckets")
	errDone     = errors.Errorf("Done processing buckets")
)

func resultWithError(err error) *sortresult {
	return &sortresult{&emptySortResult, nil, nil, err}
}

func sortWithoutIndex(ctx context.Context, ts *pb.SortMessage) *sortresult {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("sortWithoutIndex", "true"))

	n := len(ts.UidMatrix)
	r := new(pb.SortResult)
	multiSortVals := make([][]types.Val, n)
	var multiSortOffsets []int32
	// Sort and paginate directly as it'd be expensive to iterate over the index which
	// might have millions of keys just for retrieving some values.
	sType, err := schema.State().TypeOf(ts.Order[0].Attr)
	if err != nil || !sType.IsScalar() {
		return resultWithError(errors.Errorf("Cannot sort attribute %s of type object.",
			ts.Order[0].Attr))
	}

	for i := range n {
		select {
		case <-ctx.Done():
			return resultWithError(ctx.Err())
		default:
			// Copy, otherwise it'd affect the destUids and hence the srcUids of Next level.
			tempList := &pb.List{Uids: ts.UidMatrix[i].Uids}
			var vals []types.Val
			if vals, err = sortByValue(ctx, ts, tempList, sType); err != nil {
				return resultWithError(err)
			}
			start, end, err := paginate(ts, tempList, vals)
			if err != nil {
				return resultWithError(err)
			}
			if len(ts.Order) > 1 {
				var offset int32
				// Usually start would equal ts.Offset unless the values around the offset index
				// (at offset-1, offset-2 index and so on) are equal. In that case we keep those
				// values and apply the remaining offset later.
				if int32(start) < ts.Offset {
					offset = ts.Offset - int32(start)
				}
				multiSortOffsets = append(multiSortOffsets, offset)
			}
			tempList.Uids = tempList.Uids[start:end]
			vals = vals[start:end]
			r.UidMatrix = append(r.UidMatrix, tempList)
			multiSortVals[i] = vals
		}
	}
	return &sortresult{r, multiSortOffsets, multiSortVals, nil}
}

func sortWithIndex(ctx context.Context, ts *pb.SortMessage) *sortresult {
	if ctx.Err() != nil {
		return resultWithError(ctx.Err())
	}

	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("sortWithIndex", "true"))

	n := len(ts.UidMatrix)
	out := make([]intersectedList, n)
	values := make([][]types.Val, 0, n) // Values corresponding to uids in the uid matrix.
	for i := range n {
		// offsets[i] is the offset for i-th posting list. It gets decremented as we
		// iterate over buckets.
		out[i].offset = int(ts.Offset)
		out[i].ulist = &pb.List{}
		out[i].skippedUids = &pb.List{}
		out[i].uset = map[uint64]struct{}{}
	}

	order := ts.Order[0]
	typ, err := schema.State().TypeOf(order.Attr)
	if err != nil {
		return resultWithError(errors.Errorf("Attribute %s not defined in schema", order.Attr))
	}

	// Get the tokenizers and choose the corresponding one.
	if !schema.State().IsIndexed(ctx, order.Attr) {
		return resultWithError(errors.Errorf("Attribute %s is not indexed.", order.Attr))
	}

	tokenizers := schema.State().Tokenizer(ctx, order.Attr)
	var tokenizer tok.Tokenizer
	for _, t := range tokenizers {
		// Get the first sortable index.
		if t.IsSortable() {
			tokenizer = t
			break
		}
	}

	if tokenizer == nil {
		// String type can have multiple tokenizers, only one of which is
		// sortable.
		if typ == types.StringID {
			return resultWithError(errors.Errorf(
				"Attribute %s does not have exact index for sorting.", order.Attr))
		}
		// Other types just have one tokenizer, so if we didn't find a
		// sortable tokenizer, then attribute isn't sortable.
		return resultWithError(errors.Errorf("Attribute %s is not sortable.", order.Attr))
	}

	var prefix []byte
	if len(order.Langs) > 0 {
		// Only one language is allowed.
		lang := order.Langs[0]
		tokenizer = tok.GetTokenizerForLang(tokenizer, lang)
		langTokenizer, ok := tokenizer.(tok.ExactTokenizer)
		if !ok {
			return resultWithError(errors.Errorf(
				"Failed to get tokenizer for Attribute %s for language %s.", order.Attr, lang))
		}
		prefix = langTokenizer.Prefix()
	} else {
		prefix = []byte{tokenizer.Identifier()}
	}

	// Iterate over every bucket / token.
	iterOpt := badger.DefaultIteratorOptions
	iterOpt.PrefetchValues = false
	iterOpt.Reverse = order.Desc
	iterOpt.Prefix = x.IndexKey(order.Attr, string(prefix))
	txn := pstore.NewTransactionAt(ts.ReadTs, false)
	defer txn.Discard()
	var seekKey []byte
	if !order.Desc {
		// We need to seek to the first key of this index type.
		seekKey = nil // Would automatically seek to iterOpt.Prefix.
	} else {
		// We need to reach the last key of this index type.
		prefix[len(prefix)-1]++
		seekKey = x.IndexKey(order.Attr, string(prefix))
	}
	itr := txn.NewIterator(iterOpt)
	defer itr.Close()

	r := new(pb.SortResult)
BUCKETS:
	// Outermost loop is over index buckets.
	for itr.Seek(seekKey); itr.Valid(); itr.Next() {
		item := itr.Item()
		key := item.Key() // No need to copy.
		select {
		case <-ctx.Done():
			return resultWithError(ctx.Err())
		default:
			k, err := x.Parse(key)
			if err != nil {
				glog.Errorf("Error while parsing key %s: %v", hex.Dump(key), err)
				continue
			}

			x.AssertTrue(k.IsIndex())
			token := k.Term
			// Intersect every UID list with the index bucket, and update their
			// results (in out).
			err = intersectBucket(ctx, ts, token, out)
			switch err {
			case errDone:
				break BUCKETS
			case errContinue:
				// Continue iterating over tokens / index buckets.
			default:
				return resultWithError(err)
			}
		}
	}

	var multiSortOffsets []int32
	for _, il := range out {
		r.UidMatrix = append(r.UidMatrix, il.ulist)
		if len(ts.Order) > 1 {
			// TODO - For lossy tokenizer, no need to pick all values.
			values = append(values, il.values)
			multiSortOffsets = append(multiSortOffsets, il.multiSortOffset)
		}
	}

	for i, ul := range ts.UidMatrix {
		// nullNodes is list of UIDs for which the value of the sort predicate is null.
		var nullNodes []uint64
		// present is a map[uid]->bool to keep track of the UIDs containing the sort predicate.
		present := make(map[uint64]bool)

		// Add the UIDs to the map, which are in the resultant intersected list and the UIDs which
		// have been skipped because of offset while intersection.
		for _, uid := range out[i].ulist.Uids {
			present[uid] = true
		}
		for _, uid := range out[i].skippedUids.Uids {
			present[uid] = true
		}

		// nullPreds is a list of UIDs which doesn't contain the sort predicate.
		for _, uid := range ul.Uids {
			if _, ok := present[uid]; !ok {
				nullNodes = append(nullNodes, uid)
			}
		}

		// Apply the offset on null nodes, if the nodes with value were not enough.
		if out[i].offset < len(nullNodes) {
			if out[i].offset >= 0 {
				nullNodes = nullNodes[out[i].offset:]
			}
		} else {
			nullNodes = nullNodes[:0]
		}
		remainingCount := int(ts.Count) - len(r.UidMatrix[i].Uids)
		canAppend := x.Min(uint64(remainingCount), uint64(len(nullNodes)))
		r.UidMatrix[i].Uids = append(r.UidMatrix[i].Uids, nullNodes[:canAppend]...)

		// The value list also need to contain null values for the appended uids.
		if len(ts.Order) > 1 {
			nullVals := make([]types.Val, canAppend)
			values[i] = append(values[i], nullVals...)
		}
	}

	select {
	case <-ctx.Done():
		return resultWithError(ctx.Err())
	default:
		return &sortresult{r, multiSortOffsets, values, nil}
	}
}

type orderResult struct {
	idx int
	r   *pb.Result
	err error
}

func multiSort(ctx context.Context, r *sortresult, ts *pb.SortMessage) error {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("multiSort", "true"))

	// SrcUids for other queries are all the uids present in the response of the first sort.
	dest := destUids(r.reply.UidMatrix)

	// For each uid in dest uids, we have multiple values which belong to different attributes.
	// 1  -> [ "Alice", 23, "1932-01-01"]
	// 10 -> [ "Bob", 35, "1912-02-01" ]
	sortVals := make([][]types.Val, len(dest.Uids))
	for idx := range sortVals {
		sortVals[idx] = make([]types.Val, len(ts.Order))
	}

	seen := make(map[uint64]struct{})
	// Walk through the uidMatrix and put values for this attribute in sortVals.
	for i, ul := range r.reply.UidMatrix {
		x.AssertTrue(len(ul.Uids) == len(r.vals[i]))
		for j, uid := range ul.Uids {
			uidx := algo.IndexOf(dest, uid)
			x.AssertTrue(uidx >= 0)

			if _, ok := seen[uid]; ok {
				// We have already seen this uid.
				continue
			}
			seen[uid] = struct{}{}
			sortVals[uidx][0] = r.vals[i][j]
		}
	}

	// Execute rest of the sorts concurrently.
	och := make(chan orderResult, len(ts.Order)-1)
	for i := 1; i < len(ts.Order); i++ {
		in := &pb.Query{
			Attr:    ts.Order[i].Attr,
			UidList: dest,
			Langs:   ts.Order[i].Langs,
			ReadTs:  ts.ReadTs,
		}
		go fetchValues(ctx, in, i, och)
	}

	var oerr error
	// TODO - Verify behavior with multiple langs.
	for i := 1; i < len(ts.Order); i++ {
		or := <-och
		if or.err != nil {
			if oerr == nil {
				oerr = or.err
			}
			continue
		}

		result := or.r
		x.AssertTrue(len(result.ValueMatrix) == len(dest.Uids))
		for i := range dest.Uids {
			var sv types.Val
			if len(result.ValueMatrix[i].Values) == 0 {
				// Assign nil value which is sorted as greater than all other values.
				sv.Value = nil
			} else {
				v := result.ValueMatrix[i].Values[0]
				val := types.ValueForType(types.TypeID(v.ValType))
				val.Value = v.Val
				var err error
				sv, err = types.Convert(val, val.Tid)
				if err != nil {
					return err
				}
			}
			sortVals[i][or.idx] = sv
		}
	}

	if oerr != nil {
		return oerr
	}

	desc := make([]bool, 0, len(ts.Order))
	for _, o := range ts.Order {
		desc = append(desc, o.Desc)
	}

	// Values have been accumulated, now we do the multisort for each list.
	for i, ul := range r.reply.UidMatrix {
		vals := make([][]types.Val, len(ul.Uids))
		for j, uid := range ul.Uids {
			idx := algo.IndexOf(dest, uid)
			x.AssertTrue(idx >= 0)
			vals[j] = sortVals[idx]
		}

		start, end := x.PageRange(int(ts.Count), int(r.multiSortOffsets[i]), len(ul.Uids))
		if end < len(ul.Uids)/2 {
			//nolint:gosec
			if err := types.SortTopN(vals, &ul.Uids, desc, "", end); err != nil {
				return err
			}
		} else {
			//nolint:gosec
			if err := types.Sort(vals, &ul.Uids, desc, ""); err != nil {
				return err
			}
		}

		ul.Uids = ul.Uids[start:end]
		r.reply.UidMatrix[i] = ul
	}

	return nil
}

// processSort does sorting with pagination. It works by iterating over index
// buckets. As it iterates, it intersects with each UID list of the UID
// matrix. To optimize for pagination, we maintain the "offsets and sizes" or
// pagination window for each UID list. For each UID list, we ignore the
// bucket if we haven't hit the offset. We stop getting results when we got
// enough for our pagination params. When all the UID lists are done, we stop
// iterating over the index.
func processSort(ctx context.Context, ts *pb.SortMessage) (*pb.SortResult, error) {
	span := trace.SpanFromContext(ctx)
	stop := x.SpanTimer(span, "processSort")
	defer stop()

	span.SetAttributes(
		attribute.Int("startTs", int(ts.ReadTs)),
	)

	if err := posting.Oracle().WaitForTs(ctx, ts.ReadTs); err != nil {
		return nil, err
	}
	span.SetAttributes(
		attribute.String("checksumMatch", "true"),
	)

	if ts.Count < 0 {
		return nil, errors.Errorf(
			"We do not yet support negative or infinite count with sorting: %s %d. "+
				"Try flipping order and return first few elements instead.",
			x.ParseAttr(ts.Order[0].Attr), ts.Count)
	}
	// TODO (pawan) - Why check only the first attribute, what if other attributes are of list type?
	if schema.State().IsList(ts.Order[0].Attr) {
		return nil, errors.Errorf("Sorting not supported on attr: %s of type: [scalar]",
			x.ParseAttr(ts.Order[0].Attr))
	}

	// We're not using any txn local cache here. So, no need to deal with that yet.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	resCh := make(chan *sortresult, 2)
	go func() {
		select {
		case <-time.After(3 * time.Millisecond):
			// Wait between ctx chan and time chan.
		case <-ctx.Done():
			resCh <- &sortresult{err: ctx.Err()}
			return
		}
		r := sortWithoutIndex(cctx, ts)
		resCh <- r
	}()

	go func() {
		sr := sortWithIndex(cctx, ts)
		resCh <- sr
	}()

	r := <-resCh
	if r.err == nil {
		cancel()
		// wait for other goroutine to get cancelled
		<-resCh
	} else {
		span.SetAttributes(
			attribute.String("processSortError", r.err.Error()),
		)
		r = <-resCh
	}

	if r.err != nil {
		return nil, r.err
	}
	// If request didn't have multiple attributes we return.
	if len(ts.Order) <= 1 {
		return r.reply, nil
	}

	err := multiSort(ctx, r, ts)
	return r.reply, err
}

func destUids(uidMatrix []*pb.List) *pb.List {
	included := make(map[uint64]struct{})
	for _, ul := range uidMatrix {
		for _, uid := range ul.Uids {
			included[uid] = struct{}{}
		}
	}

	res := &pb.List{Uids: make([]uint64, 0, len(included))}
	for uid := range included {
		res.Uids = append(res.Uids, uid)
	}
	sort.Slice(res.Uids, func(i, j int) bool { return res.Uids[i] < res.Uids[j] })
	return res
}

func fetchValues(ctx context.Context, in *pb.Query, idx int, or chan orderResult) {
	var err error
	in.Reverse = strings.HasPrefix(in.Attr, "~")
	if in.Reverse {
		in.Attr = strings.TrimPrefix(in.Attr, "~")
	}
	r, err := ProcessTaskOverNetwork(ctx, in)
	or <- orderResult{
		idx: idx,
		err: err,
		r:   r,
	}
}

type intersectedList struct {
	offset          int
	ulist           *pb.List
	skippedUids     *pb.List
	values          []types.Val
	uset            map[uint64]struct{}
	multiSortOffset int32
}

// intersectBucket intersects every UID list in the UID matrix with the
// indexed bucket.
func intersectBucket(ctx context.Context, ts *pb.SortMessage, token string,
	out []intersectedList) error {
	count := int(ts.Count)
	order := ts.Order[0]
	sType, err := schema.State().TypeOf(order.Attr)
	if err != nil || !sType.IsScalar() {
		return errors.Errorf("Cannot sort attribute %s of type object.", order.Attr)
	}
	scalar := sType

	key := x.IndexKey(order.Attr, token)
	// Don't put the Index keys in memory.
	pl, err := posting.GetNoStore(key, ts.GetReadTs())
	if err != nil {
		return err
	}
	var vals []types.Val

	// For each UID list, we need to intersect with the index bucket.
	for i, ul := range ts.UidMatrix {
		il := &out[i]
		// We need to reduce multiSortOffset while checking the count as we might have included
		// some extra uids from the bucket that the offset falls into. We are going to discard
		// the first multiSortOffset number of uids later after all sorts are applied.
		if count > 0 && len(il.ulist.Uids)-int(il.multiSortOffset) >= count {
			continue
		}

		// Intersect index with i-th input UID list.
		listOpt := posting.ListOptions{
			Intersect: ul,
			ReadTs:    ts.ReadTs,
			First:     0, // TODO: Should we set the first N here?
		}
		result, err := pl.Uids(listOpt) // The actual intersection work is done here.
		if err != nil {
			return err
		}

		// Duplicates will exist between buckets if there are multiple language
		// variants of a predicate.
		result.Uids = removeDuplicates(result.Uids, il.uset)

		// Check offsets[i].
		n := len(result.Uids)
		if il.offset >= n {
			// We are going to skip the whole intersection. No need to do actual
			// sorting. Just update offsets[i]. We now offset less. Also, keep track of the UIDs
			// that have been skipped for the offset.
			il.offset -= n
			il.skippedUids.Uids = append(il.skippedUids.Uids, result.Uids...)
			continue
		}

		// We are within the page. We need to apply sorting.
		// Sort results by value before applying offset.
		// TODO (pawan) - Why do we do this? Looks like it it is only useful for language.
		if vals, err = sortByValue(ctx, ts, result, scalar); err != nil {
			return err
		}

		// Result set might have reduced after sorting. As some uids might not have a
		// value in the lang specified.
		n = len(result.Uids)

		if il.offset > 0 {
			// Apply the offset.
			if len(ts.Order) == 1 {
				// Keep track of UIDs which had sort predicate but have been skipped because of
				// the offset.
				il.skippedUids.Uids = append(il.skippedUids.Uids, result.Uids[:il.offset]...)
				result.Uids = result.Uids[il.offset:n]
			} else {
				// In case of multi sort we can't apply the offset yet, as the order might change
				// after other sort orders are applied. So we need to pick all the uids in the
				// current bucket.
				// Since we are picking all values in this bucket, we have to apply this remaining
				// offset later and hence are storing it here.
				il.multiSortOffset = int32(il.offset)
			}
			il.offset = 0
			n = len(result.Uids)
		}

		// n is number of elements to copy from result to out.
		// In case of multiple sort, we don't want to apply the count and copy all uids for the
		// current bucket.
		if count > 0 && (len(ts.Order) == 1) {
			slack := count - len(il.ulist.Uids)
			if slack < n {
				n = slack
			}
		}

		il.ulist.Uids = append(il.ulist.Uids, result.Uids[:n]...)
		if len(ts.Order) > 1 {
			il.values = append(il.values, vals[:n]...)
		}
	} // end for loop over UID lists in UID matrix.

	// Check out[i] sizes for all i.
	for i := range ts.UidMatrix { // Iterate over UID lists.
		// We need to reduce multiSortOffset while checking the count as we might have included
		// some extra uids earlier for the multi-sort case.
		if len(out[i].ulist.Uids)-int(out[i].multiSortOffset) < count {
			return errContinue
		}

		if len(ts.Order) == 1 {
			x.AssertTruef(len(out[i].ulist.Uids) == count, "%d %d", len(out[i].ulist.Uids), count)
		}
	}
	// All UID lists have enough items (according to pagination). Let's notify
	// the outermost loop.
	return errDone
}

// removeDuplicates removes elements from uids if they are in set. It also adds
// all uids to set.
func removeDuplicates(uids []uint64, set map[uint64]struct{}) []uint64 {
	for i := 0; i < len(uids); i++ {
		uid := uids[i]
		if _, ok := set[uid]; ok {
			copy(uids[i:], uids[i+1:])
			uids = uids[:len(uids)-1]
			i-- // we just removed an entry, so go back one step
		} else {
			set[uid] = struct{}{}
		}
	}
	return uids
}

func paginate(ts *pb.SortMessage, dest *pb.List, vals []types.Val) (int, int, error) {
	count := int(ts.Count)
	offset := int(ts.Offset)
	start, end := x.PageRange(count, offset, len(dest.Uids))

	// For multiple sort, we need to take all equal values at the start and end.
	// This is because the final sort order depends on other sort attributes and we can't ignore
	// equal values at start or the end.
	if len(ts.Order) > 1 {
		for start < len(vals) && start > 0 {
			eq, err := types.Equal(vals[start], vals[start-1])
			if err != nil {
				return 0, 0, err
			}
			if !eq {
				break
			}
			start--
		}
		for end < len(dest.Uids) {
			eq, err := types.Equal(vals[end-1], vals[end])
			if err != nil {
				return 0, 0, err
			}
			if !eq {
				break
			}
			end++
		}
	}

	return start, end, nil
}

// sortByValue fetches values and sort UIDList.
func sortByValue(ctx context.Context, ts *pb.SortMessage, ul *pb.List,
	typ types.TypeID) ([]types.Val, error) {
	lenList := len(ul.Uids)
	uids := make([]uint64, 0, lenList)
	values := make([][]types.Val, 0, lenList)
	multiSortVals := make([]types.Val, 0, lenList)
	order := ts.Order[0]

	var lang string
	if langCount := len(order.Langs); langCount == 1 {
		lang = order.Langs[0]
	} else if langCount > 1 {
		return nil, errors.Errorf("Sorting on multiple language is not supported.")
	}

	// nullsList is the list of UIDs for which value doesn't exist.
	var nullsList []uint64
	var nullVals [][]types.Val
	for i := range lenList {
		select {
		case <-ctx.Done():
			return multiSortVals, ctx.Err()
		default:
			uid := ul.Uids[i]
			val, err := fetchValue(uid, order.Attr, order.Langs, typ, ts.ReadTs)
			if err != nil {
				// Value couldn't be found or couldn't be converted to the sort type.
				// It will be appended to the end of the result based on the pagination.
				val.Value = nil
				nullsList = append(nullsList, uid)
				nullVals = append(nullVals, []types.Val{val})
				continue
			}
			uids = append(uids, uid)
			values = append(values, []types.Val{val})
		}
	}
	err := types.Sort(values, &uids, []bool{order.Desc}, lang)
	ul.Uids = append(uids, nullsList...)
	values = append(values, nullVals...)
	if len(ts.Order) > 1 {
		for _, v := range values {
			multiSortVals = append(multiSortVals, v[0])
		}
	}
	return multiSortVals, err
}

// fetchValue gets the value for a given UID.
func fetchValue(uid uint64, attr string, langs []string, scalar types.TypeID,
	readTs uint64) (types.Val, error) {
	// Don't put the values in memory
	pl, err := posting.GetNoStore(x.DataKey(attr, uid), readTs)
	if err != nil {
		return types.Val{}, err
	}

	src, err := pl.ValueFor(readTs, langs)

	if err != nil {
		return types.Val{}, err
	}
	dst, err := types.Convert(src, scalar)
	if err != nil {
		return types.Val{}, err
	}

	return dst, nil
}
