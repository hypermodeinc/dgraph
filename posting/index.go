/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package posting

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	ostats "go.opencensus.io/stats"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
	bpb "github.com/dgraph-io/badger/v4/pb"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/tok"
	"github.com/hypermodeinc/dgraph/v25/tok/hnsw"
	tokIndex "github.com/hypermodeinc/dgraph/v25/tok/index"
	"github.com/hypermodeinc/dgraph/v25/tok/kmeans"

	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var emptyCountParams countParams

type indexMutationInfo struct {
	tokenizers   []tok.Tokenizer
	factorySpecs []*tok.FactoryCreateSpec
	edge         *pb.DirectedEdge // Represents the original uid -> value edge.
	val          types.Val
	op           pb.DirectedEdge_Op
}

// indexTokens return tokens, without the predicate prefix and
// index rune, for specific tokenizers.
func indexTokens(ctx context.Context, info *indexMutationInfo) ([]string, error) {
	attr := info.edge.Attr
	lang := info.edge.GetLang()

	schemaType, err := schema.State().TypeOf(attr)
	if err != nil || !schemaType.IsScalar() {
		return nil, errors.Errorf("Cannot index attribute %s of type object.", attr)
	}

	if !schema.State().IsIndexed(ctx, attr) {
		return nil, errors.Errorf("Attribute %s is not indexed.", attr)
	}
	sv, err := types.Convert(info.val, schemaType)
	if err != nil {
		return nil, err
	}

	var tokens []string
	for _, it := range info.tokenizers {
		toks, err := tok.BuildTokens(sv.Value, tok.GetTokenizerForLang(it, lang))
		if err != nil {
			return tokens, err
		}
		tokens = append(tokens, toks...)
	}
	return tokens, nil
}

// addIndexMutations adds mutation(s) for a single term, to maintain the index,
// but only for the given tokenizers.
// TODO - See if we need to pass op as argument as t should already have Op.

func (txn *Txn) addIndexMutations(ctx context.Context, info *indexMutationInfo) ([]*pb.DirectedEdge, error) {
	if info.tokenizers == nil {
		info.tokenizers = schema.State().Tokenizer(ctx, info.edge.Attr)
	}

	if info.factorySpecs == nil {
		specs, err := schema.State().FactoryCreateSpec(ctx, info.edge.Attr)
		if err != nil {
			return nil, err
		}
		info.factorySpecs = specs
	}

	attr := info.edge.Attr
	uid := info.edge.Entity
	if uid == 0 {
		return []*pb.DirectedEdge{}, errors.New("invalid UID with value 0")
	}

	if len(info.factorySpecs) > 0 {
		inKey := x.DataKey(info.edge.Attr, uid)
		pl, err := txn.Get(inKey)
		if err != nil {
			return []*pb.DirectedEdge{}, err
		}
		data, err := pl.AllValues(txn.StartTs)
		if err != nil {
			return []*pb.DirectedEdge{}, err
		}

		if info.op == pb.DirectedEdge_DEL &&
			len(data) > 0 && data[0].Tid == types.VFloatID {
			// TODO look into better alternatives
			//      The issue here is that we will create dead nodes in the Vector Index
			//      assuming an HNSW index type. What we should do instead is invoke
			//      index.Remove(<key to dead index value>). However, we currently do
			//      not support this in VectorIndex code!!
			// if a delete & dealing with vfloats, add this to dead node in persistent store.
			// What we should do instead is invoke the factory.Remove(key) operation.
			deadAttr := hnsw.ConcatStrings(info.edge.Attr, hnsw.VecDead)
			deadKey := x.DataKey(deadAttr, 1)
			pl, err := txn.Get(deadKey)
			if err != nil {
				return []*pb.DirectedEdge{}, err
			}
			var deadNodes []uint64
			deadData, _ := pl.Value(txn.StartTs)
			if deadData.Value == nil {
				deadNodes = append(deadNodes, uid)
			} else {
				deadNodes, err = hnsw.ParseEdges(string(deadData.Value.([]byte)))
				if err != nil {
					return []*pb.DirectedEdge{}, err
				}
				deadNodes = append(deadNodes, uid)
			}
			deadNodesBytes, marshalErr := json.Marshal(deadNodes)
			if marshalErr != nil {
				return []*pb.DirectedEdge{}, marshalErr
			}
			edge := &pb.DirectedEdge{
				Entity:    1,
				Attr:      deadAttr,
				Value:     deadNodesBytes,
				ValueType: pb.Posting_ValType(0),
			}
			if err := pl.addMutation(ctx, txn, edge); err != nil {
				return nil, err
			}
		}

		// TODO: As stated earlier, we need to validate that it is okay to assume
		//       that we care about just data[0].
		//       Similarly, the current assumption is that we have at most one
		//       Vector Index, but this assumption may break later.
		if info.op != pb.DirectedEdge_DEL &&
			len(data) > 0 && data[0].Tid == types.VFloatID &&
			len(info.factorySpecs) > 0 {
			// retrieve vector from inUuid save as inVec
			inVec := types.BytesAsFloatArray(data[0].Value.([]byte))
			tc := hnsw.NewTxnCache(NewViTxn(txn), txn.StartTs)
			indexer, err := info.factorySpecs[0].CreateIndex(attr)
			if err != nil {
				return []*pb.DirectedEdge{}, err
			}
			edges, err := indexer.Insert(ctx, tc, uid, inVec)
			if err != nil {
				return []*pb.DirectedEdge{}, err
			}
			pbEdges := []*pb.DirectedEdge{}
			for _, e := range edges {
				pbe := indexEdgeToPbEdge(e)
				pbEdges = append(pbEdges, pbe)
			}
			return pbEdges, nil
		}
	}

	tokens, err := indexTokens(ctx, info)
	if err != nil {
		// This data is not indexable
		return []*pb.DirectedEdge{}, err
	}

	// Create a value token -> uid edge.
	edge := &pb.DirectedEdge{
		ValueId: uid,
		Attr:    attr,
		Op:      info.op,
	}

	for _, token := range tokens {
		if err := txn.addIndexMutation(ctx, edge, token); err != nil {
			return []*pb.DirectedEdge{}, err
		}
	}
	return []*pb.DirectedEdge{}, nil
}

func (txn *Txn) addIndexMutation(ctx context.Context, edge *pb.DirectedEdge, token string) error {
	key := x.IndexKey(edge.Attr, token)
	plist, err := txn.cache.GetFromDelta(key)
	if err != nil {
		return err
	}

	x.AssertTrue(plist != nil)
	if err = plist.addMutation(ctx, txn, edge); err != nil {
		return err
	}
	ostats.Record(ctx, x.NumEdges.M(1))
	return nil
}

// countParams is sent to updateCount function. It is used to update the count index.
// It deletes the uid from the key corresponding to <attr, countBefore> and adds it
// to <attr, countAfter>.
type countParams struct {
	attr        string
	countBefore int
	countAfter  int
	entity      uint64
	reverse     bool
}

// When we want to update count edges, we should set them with OVR instead of SET as SET will mess with count
func shouldAddCountEdge(found bool, edge *pb.DirectedEdge) bool {
	if found {
		if edge.Op != pb.DirectedEdge_DEL {
			edge.Op = pb.DirectedEdge_OVR
		}
		return true
	} else {
		return edge.Op != pb.DirectedEdge_DEL
	}
}

func (txn *Txn) addReverseMutationHelper(ctx context.Context, plist *List,
	hasCountIndex bool, edge *pb.DirectedEdge) (countParams, error) {
	countBefore, countAfter := 0, 0
	found := false

	plist.Lock()
	defer plist.Unlock()
	if hasCountIndex {
		countBefore, found, _ = plist.getPostingAndLengthNoSort(txn.StartTs, 0, edge.ValueId)
		if countBefore < 0 {
			return emptyCountParams, errors.Wrapf(ErrTsTooOld, "Adding reverse mutation helper count")
		}
	}

	if !(hasCountIndex && !shouldAddCountEdge(found, edge)) {
		if err := plist.addMutationInternal(ctx, txn, edge); err != nil {
			return emptyCountParams, err
		}
	}

	if hasCountIndex {
		pk, _ := x.Parse(plist.key)
		shouldCountOneUid := !schema.State().IsList(edge.Attr) && !pk.IsReverse()
		countAfter = countAfterMutation(countBefore, found, edge.Op, shouldCountOneUid)
		return countParams{
			attr:        edge.Attr,
			countBefore: countBefore,
			countAfter:  countAfter,
			entity:      edge.Entity,
			reverse:     true,
		}, nil
	}
	return emptyCountParams, nil
}

func (txn *Txn) addReverseMutation(ctx context.Context, t *pb.DirectedEdge) error {
	key := x.ReverseKey(t.Attr, t.ValueId)
	plist, err := txn.GetFromDelta(key)
	if err != nil {
		return err
	}
	x.AssertTrue(plist != nil)

	// We must create a copy here.
	edge := &pb.DirectedEdge{
		Entity:  t.ValueId,
		ValueId: t.Entity,
		Attr:    t.Attr,
		Op:      t.Op,
		Facets:  t.Facets,
	}
	if err := plist.addMutation(ctx, txn, edge); err != nil {
		return err
	}

	ostats.Record(ctx, x.NumEdges.M(1))
	return nil
}

func (txn *Txn) addReverseAndCountMutation(ctx context.Context, t *pb.DirectedEdge) error {
	key := x.ReverseKey(t.Attr, t.ValueId)
	hasCountIndex := schema.State().HasCount(ctx, t.Attr)

	var getFn func(key []byte) (*List, error)
	if hasCountIndex {
		// We need to retrieve the full posting list from disk, to allow us to get the length of the
		// posting list for the counts.
		getFn = txn.Get
	} else {
		// We are just adding a reverse edge. No need to read the list from disk.
		getFn = txn.GetFromDelta
	}
	plist, err := getFn(key)
	if err != nil {
		return err
	}
	if plist == nil {
		return errors.Errorf("nil posting list for reverse key %s", hex.Dump(key))
	}

	// For single uid predicates, updating the reverse index requires that the existing
	// entries for this key in the index are removed.
	pred, ok := schema.State().Get(ctx, t.Attr)
	isSingleUidUpdate := ok && !pred.GetList() && pred.GetValueType() == pb.Posting_UID &&
		t.Op != pb.DirectedEdge_DEL && t.ValueId != 0
	if isSingleUidUpdate {
		dataKey := x.DataKey(t.Attr, t.Entity)
		dataList, err := getFn(dataKey)
		if err != nil {
			return errors.Wrapf(err, "cannot find single uid list to update with key %s",
				hex.Dump(dataKey))
		}
		err = dataList.Iterate(txn.StartTs, 0, func(p *pb.Posting) error {
			delEdge := &pb.DirectedEdge{
				Entity:  t.Entity,
				ValueId: p.Uid,
				Attr:    t.Attr,
				Op:      pb.DirectedEdge_DEL,
			}
			return txn.addReverseAndCountMutation(ctx, delEdge)
		})
		if err != nil {
			return errors.Wrapf(err, "cannot remove existing reverse index entries for key %s",
				hex.Dump(dataKey))
		}
	}

	// We must create a copy here.
	edge := &pb.DirectedEdge{
		Entity:  t.ValueId,
		ValueId: t.Entity,
		Attr:    t.Attr,
		Op:      t.Op,
		Facets:  t.Facets,
	}

	cp, err := txn.addReverseMutationHelper(ctx, plist, hasCountIndex, edge)
	if err != nil {
		return err
	}
	ostats.Record(ctx, x.NumEdges.M(1))

	if hasCountIndex && cp.countAfter != cp.countBefore {
		if err := txn.updateCount(ctx, cp); err != nil {
			return err
		}
	}

	return nil
}

func (l *List) handleDeleteAll(ctx context.Context, edge *pb.DirectedEdge, txn *Txn) error {
	isReversed := schema.State().IsReversed(ctx, edge.Attr)
	isIndexed := schema.State().IsIndexed(ctx, edge.Attr)
	hasCount := schema.State().HasCount(ctx, edge.Attr)
	delEdge := &pb.DirectedEdge{
		Attr:   edge.Attr,
		Op:     edge.Op,
		Entity: edge.Entity,
	}
	// To calculate length of posting list. Used for deletion of count index.
	var plen int
	err := l.Iterate(txn.StartTs, 0, func(p *pb.Posting) error {
		plen++
		switch {
		case isReversed:
			// Delete reverse edge for each posting.
			delEdge.ValueId = p.Uid
			return txn.addReverseAndCountMutation(ctx, delEdge)
		case isIndexed:
			// Delete index edge of each posting.
			val := types.Val{
				Tid:   types.TypeID(p.ValType),
				Value: p.Value,
			}
			factorySpecs, err := schema.State().FactoryCreateSpec(ctx, edge.Attr)
			if err != nil {
				return err
			}
			_, err = txn.addIndexMutations(ctx, &indexMutationInfo{
				tokenizers:   schema.State().Tokenizer(ctx, edge.Attr),
				factorySpecs: factorySpecs,
				edge:         edge,
				val:          val,
				op:           pb.DirectedEdge_DEL,
			})
			return err
		default:
			return nil
		}
	})
	if err != nil {
		return err
	}
	if hasCount {
		// Delete uid from count index. Deletion of reverses is taken care by addReverseMutation
		// above.
		if err := txn.updateCount(ctx, countParams{
			attr:        edge.Attr,
			countBefore: plen,
			countAfter:  0,
			entity:      edge.Entity,
		}); err != nil {
			return err
		}
	}

	return l.addMutation(ctx, txn, edge)
}

func (txn *Txn) addCountMutation(ctx context.Context, t *pb.DirectedEdge, count uint32,
	reverse bool) error {
	key := x.CountKey(t.Attr, count, reverse)
	plist, err := txn.cache.GetFromDelta(key)
	if err != nil {
		return err
	}

	x.AssertTruef(plist != nil, "plist is nil [%s] %d",
		t.Attr, t.ValueId)
	if err = plist.addMutation(ctx, txn, t); err != nil {
		return err
	}
	ostats.Record(ctx, x.NumEdges.M(1))
	return nil
}

func (txn *Txn) updateCount(ctx context.Context, params countParams) error {
	edge := pb.DirectedEdge{
		ValueId: params.entity,
		Attr:    params.attr,
		Op:      pb.DirectedEdge_DEL,
	}
	if params.countBefore > 0 {
		if err := txn.addCountMutation(ctx, &edge, uint32(params.countBefore),
			params.reverse); err != nil {
			return err
		}
	}

	if params.countAfter > 0 {
		edge.Op = pb.DirectedEdge_SET
		if err := txn.addCountMutation(ctx, &edge, uint32(params.countAfter),
			params.reverse); err != nil {
			return err
		}
	}
	return nil
}

// Gives the count of the posting after the mutation has finished. Currently we use this to figure after the mutation
// what is the count. For non scalar predicate, we need to use found and the operation that the user did to figure out
// if the new node was inserted or not. However, for single uid predicates this information is not useful. For scalar
// predicate, delete only works if the value was found. Set would just result in 1 alaways.
func countAfterMutation(countBefore int, found bool, op pb.DirectedEdge_Op, shouldCountOneUid bool) int {
	if shouldCountOneUid {
		if op == pb.DirectedEdge_SET {
			return 1
		} else if op == pb.DirectedEdge_DEL && found {
			return 0
		} else {
			return countBefore
		}
	}

	if !found && op != pb.DirectedEdge_DEL {
		return countBefore + 1
	} else if found && op == pb.DirectedEdge_DEL {
		return countBefore - 1
	}

	// Only conditions remaining are below, for which countAfter will be same as countBefore.
	// (found && op == pb.DirectedEdge_SET) || (!found && op == pb.DirectedEdge_DEL)
	return countBefore
}

func (txn *Txn) addMutationHelper(ctx context.Context, l *List, doUpdateIndex bool,
	hasCountIndex bool, t *pb.DirectedEdge) (types.Val, bool, countParams, error) {

	t1 := time.Now()
	l.Lock()
	defer l.Unlock()

	if dur := time.Since(t1); dur > time.Millisecond {
		span := trace.SpanFromContext(ctx)
		span.AddEvent(fmt.Sprintf("Acquired lock %v %v %v", dur, t.Attr, t.Entity))
	}

	getUID := func(t *pb.DirectedEdge) uint64 {
		if t.ValueType == pb.Posting_UID {
			return t.ValueId
		}
		return fingerprintEdge(t)
	}

	// For countIndex we need to check if some posting already exists for uid and length of posting
	// list, hence will are calling l.getPostingAndLength(). If doUpdateIndex or delNonListPredicate
	// is true, we just need to get the posting for uid, hence calling l.findPosting().
	countBefore, countAfter := 0, 0
	var currPost *pb.Posting
	var val types.Val
	var found bool
	var err error

	isScalarPredicate := !schema.State().IsList(t.Attr)
	delNonListPredicate := isScalarPredicate &&
		t.Op == pb.DirectedEdge_DEL && string(t.Value) != x.Star

	switch {
	case hasCountIndex:
		countBefore, found, currPost = l.getPostingAndLengthNoSort(txn.StartTs, 0, getUID(t))
		if countBefore == -1 {
			return val, false, emptyCountParams, errors.Wrapf(ErrTsTooOld, "Add mutation count index")
		}
	case doUpdateIndex || delNonListPredicate:
		found, currPost, err = l.findPosting(txn.StartTs, fingerprintEdge(t))
		if err != nil {
			return val, found, emptyCountParams, err
		}
	}

	// If the predicate schema is not a list, ignore delete triples whose object is not a star or
	// a value that does not match the existing value.
	if delNonListPredicate {
		newPost := NewPosting(t)

		// This is a scalar value of non-list type and a delete edge mutation, so if the value
		// given by the user doesn't match the value we have, we return found to be false, to avoid
		// deleting the uid from index posting list.
		// This second check is required because we fingerprint the scalar values as math.MaxUint64,
		// so even though they might be different the check in the doUpdateIndex block above would
		// return found to be true.
		if found && !(bytes.Equal(currPost.Value, newPost.Value) &&
			types.TypeID(currPost.ValType) == types.TypeID(newPost.ValType)) {
			return val, false, emptyCountParams, nil
		}
	}

	if !(hasCountIndex && !shouldAddCountEdge(found && currPost.Op != Del, t)) {
		if err = l.addMutationInternal(ctx, txn, t); err != nil {
			return val, found, emptyCountParams, err
		}
	}

	if found && doUpdateIndex {
		val = valueToTypesVal(currPost)
	}

	if hasCountIndex {
		pk, _ := x.Parse(l.key)
		shouldCountOneUid := isScalarPredicate && !pk.IsReverse()
		countAfter = countAfterMutation(countBefore, found, t.Op, shouldCountOneUid)
		return val, found, countParams{
			attr:        t.Attr,
			countBefore: countBefore,
			countAfter:  countAfter,
			entity:      t.Entity,
		}, nil
	}
	return val, found, emptyCountParams, nil
}

// AddMutationWithIndex is addMutation with support for indexing. It also
// supports reverse edges.
func (l *List) AddMutationWithIndex(ctx context.Context, edge *pb.DirectedEdge, txn *Txn) error {
	if edge.Attr == "" {
		return errors.Errorf("Predicate cannot be empty for edge with subject: [%v], object: [%v]"+
			" and value: [%v]", edge.Entity, edge.ValueId, edge.Value)
	}

	if edge.Op == pb.DirectedEdge_DEL && string(edge.Value) == x.Star {
		return l.handleDeleteAll(ctx, edge, txn)
	}

	doUpdateIndex := pstore != nil && schema.State().IsIndexed(ctx, edge.Attr)
	hasCountIndex := schema.State().HasCount(ctx, edge.Attr)

	// Add reverse mutation irrespective of hasMutated, server crash can happen after
	// mutation is synced and before reverse edge is synced
	if (pstore != nil) && (edge.ValueId != 0) && schema.State().IsReversed(ctx, edge.Attr) {
		if err := txn.addReverseAndCountMutation(ctx, edge); err != nil {
			return err
		}
	}

	val, found, cp, err := txn.addMutationHelper(ctx, l, doUpdateIndex, hasCountIndex, edge)
	if err != nil {
		return err
	}
	ostats.Record(ctx, x.NumEdges.M(1))
	if hasCountIndex && cp.countAfter != cp.countBefore {
		if err := txn.updateCount(ctx, cp); err != nil {
			return err
		}
	}
	if doUpdateIndex {
		// Exact matches.
		if found && val.Value != nil {
			if _, err := txn.addIndexMutations(ctx, &indexMutationInfo{
				tokenizers: schema.State().Tokenizer(ctx, edge.Attr),
				edge:       edge,
				val:        val,
				op:         pb.DirectedEdge_DEL,
			}); err != nil {
				return err
			}
		}
		if edge.Op != pb.DirectedEdge_DEL {
			val = types.Val{
				Tid:   types.TypeID(edge.ValueType),
				Value: edge.Value,
			}
			if _, err := txn.addIndexMutations(ctx, &indexMutationInfo{
				tokenizers: schema.State().Tokenizer(ctx, edge.Attr),
				edge:       edge,
				val:        val,
				op:         pb.DirectedEdge_SET,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// prefixesToDeleteTokensFor returns the prefixes to be deleted for index for the given attribute and token.
func prefixesToDeleteTokensFor(attr, tokenizerName string, hasLang bool) ([][]byte, error) {
	prefixes := [][]byte{}
	pk := x.ParsedKey{Attr: attr}
	prefix := pk.IndexPrefix()
	tokenizer, ok := tok.GetTokenizer(tokenizerName)
	if !ok {
		return nil, errors.Errorf("Could not find valid tokenizer for %s", tokenizerName)
	}
	if hasLang {
		// We just need the tokenizer identifier for ExactTokenizer having language.
		// It will be same for all the language.
		tokenizer = tok.GetTokenizerForLang(tokenizer, "en")
	}
	prefix = append(prefix, tokenizer.Identifier())
	prefixes = append(prefixes, prefix)
	// All the parts of any list that has been split into multiple parts.
	// Such keys have a different prefix (the last byte is set to 1).
	prefix = pk.IndexPrefix()
	prefix[0] = x.ByteSplit
	prefix = append(prefix, tokenizer.Identifier())
	prefixes = append(prefixes, prefix)

	return prefixes, nil
}

// rebuilder handles the process of rebuilding an index.
type rebuilder struct {
	attr    string
	prefix  []byte
	startTs uint64

	// The posting list passed here is the on disk version. It is not coming
	// from the LRU cache.
	fn func(uid uint64, pl *List, txn *Txn) ([]*pb.DirectedEdge, error)
}

func (r *rebuilder) RunWithoutTemp(ctx context.Context) error {
	ResetCache()
	stream := pstore.NewStreamAt(r.startTs)
	stream.LogPrefix = fmt.Sprintf("Rebuilding index for predicate %s (1/2):", r.attr)
	stream.Prefix = r.prefix
	stream.NumGo = 16
	txn := NewTxn(r.startTs)
	stream.KeyToList = func(key []byte, it *badger.Iterator) (*bpb.KVList, error) {
		// We should return quickly if the context is no longer valid.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		pk, err := x.Parse(key)
		if err != nil {
			return nil, errors.Wrapf(err, "could not parse key %s", hex.Dump(key))
		}

		l := new(List)
		l.key = key
		l.plist = new(pb.PostingList)

		found := false

		for it.Valid() {
			item := it.Item()
			if !bytes.Equal(item.Key(), l.key) {
				break
			}
			l.maxTs = x.Max(l.maxTs, item.Version())
			if item.IsDeletedOrExpired() {
				// Don't consider any more versions.
				break
			}

			found = true
			switch item.UserMeta() {
			case BitEmptyPosting:
				l.minTs = item.Version()
			case BitCompletePosting:
				if err := unmarshalOrCopy(l.plist, item); err != nil {
					return nil, err
				}
				l.minTs = item.Version()

				// No need to do Next here. The outer loop can take care of skipping
				// more versions of the same key.
			case BitDeltaPosting:
				err := item.Value(func(val []byte) error {
					pl := &pb.PostingList{}
					if err := proto.Unmarshal(val, pl); err != nil {
						return err
					}
					pl.CommitTs = item.Version()
					if l.mutationMap == nil {
						l.mutationMap = newMutableLayer()
					}
					l.mutationMap.insertCommittedPostings(pl)
					return nil
				})
				if err != nil {
					return nil, err
				}
			default:
				return nil, errors.Errorf(
					"Unexpected meta: %d for key: %s", item.UserMeta(), hex.Dump(key))
			}
			if found {
				break
			}
		}

		if _, err := r.fn(pk.Uid, l, txn); err != nil {
			return nil, err
		}
		return nil, nil
	}
	stream.Send = func(buf *z.Buffer) error {
		// TODO. Make an in memory txn with disk backing for more data than memory.
		return nil
	}

	start := time.Now()
	if err := stream.Orchestrate(ctx); err != nil {
		return err
	}

	if os.Getenv("DEBUG_SHOW_HNSW_TREE") != "" {
		printTreeStats(txn)
	}

	txn.Update()
	writer := NewTxnWriter(pstore)

	defer func() {
		glog.V(1).Infof("Rebuilding index for predicate %s: building index took: %v\n",
			r.attr, time.Since(start))
	}()

	ResetCache()

	return x.ExponentialRetry(int(x.Config.MaxRetries),
		20*time.Millisecond, func() error {
			err := txn.CommitToDisk(writer, r.startTs)
			if err == badger.ErrBannedKey {
				glog.Errorf("Error while writing to banned namespace.")
				return nil
			}
			return err
		})
}

func printTreeStats(txn *Txn) {
	txn.cache.Lock()

	numLevels := 20
	numNodes := make([]int, numLevels)
	numConnections := make([]int, numLevels)

	var temp [][]uint64
	for key, pl := range txn.cache.plists {
		pk, _ := x.Parse([]byte(key))
		if strings.HasSuffix(pk.Attr, "__vector_") {
			data := pl.getPosting(txn.cache.startTs)
			if data == nil || len(data.Postings) == 0 {
				continue
			}

			err := decodeUint64MatrixUnsafe(data.Postings[0].Value, &temp)
			if err != nil {
				fmt.Println("Error while decoding", err)
			}

			for i := range temp {
				if len(temp[i]) > 0 {
					numNodes[i] += 1
				}
				numConnections[i] += len(temp[i])
			}

		}
	}

	for i := range numLevels {
		fmt.Printf("%d, ", numNodes[i])
	}
	fmt.Println("")
	for i := range numLevels {
		fmt.Printf("%d, ", numConnections[i])
	}
	fmt.Println("")
	for i := range numLevels {
		if numNodes[i] == 0 {
			fmt.Printf("0, ")
			continue
		}
		fmt.Printf("%d, ", numConnections[i]/numNodes[i])
	}
	fmt.Println("")

	txn.cache.Unlock()
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

	for i := range rows {
		// Read row length
		rowLen := *(*uint64)(unsafe.Pointer(&data[offset]))
		offset += 8

		(*matrix)[i] = make([]uint64, rowLen)
		for j := range rowLen {
			(*matrix)[i][j] = *(*uint64)(unsafe.Pointer(&data[offset]))
			offset += 8
		}
	}

	return nil
}

func (r *rebuilder) Run(ctx context.Context) error {
	if r.startTs == 0 {
		glog.Infof("maxassigned is 0, no indexing work for predicate %s", r.attr)
		return nil
	}

	pred, ok := schema.State().Get(ctx, r.attr)
	if !ok {
		return errors.Errorf("Rebuilder.run: Unable to find schema for %s", r.attr)
	}

	// We write the index in a temporary badger first and then,
	// merge entries before writing them to p directory.
	tmpIndexDir, err := os.MkdirTemp(x.WorkerConfig.TmpDir, "dgraph_index_")
	if err != nil {
		return errors.Wrap(err, "error creating temp dir for reindexing")
	}
	defer os.RemoveAll(tmpIndexDir)
	glog.V(1).Infof("Rebuilding indexes using the temp folder %s\n", tmpIndexDir)

	dbOpts := badger.DefaultOptions(tmpIndexDir).
		WithSyncWrites(false).
		WithNumVersionsToKeep(math.MaxInt32).
		WithLogger(&x.ToGlog{}).
		WithCompression(options.None).
		WithLoggingLevel(badger.WARNING).
		WithMetricsEnabled(false)
	dbOpts.DetectConflicts = false
	dbOpts.NumLevelZeroTables = 200
	dbOpts.NumLevelZeroTablesStall = 400
	dbOpts.BaseLevelSize = 5 * 128 << 20
	dbOpts.BaseTableSize = 128 << 20
	dbOpts.MemTableSize = 128 << 20
	// Set cache if we have encryption.
	if len(x.WorkerConfig.EncryptionKey) > 0 {
		dbOpts.EncryptionKey = x.WorkerConfig.EncryptionKey
		dbOpts.BlockCacheSize = 100 << 20
		dbOpts.IndexCacheSize = 100 << 20
	}
	tmpDB, err := badger.OpenManaged(dbOpts)
	if err != nil {
		return errors.Wrap(err, "error opening temp badger for reindexing")
	}
	defer tmpDB.Close()

	glog.V(1).Infof(
		"Rebuilding index for predicate %s: Starting process. StartTs=%d. Prefix=\n%s\n",
		r.attr, r.startTs, hex.Dump(r.prefix))

	// Counter is used here to ensure that all keys are committed at different timestamp.
	// We set it to 1 in case there are no keys found and NewStreamAt is called with ts=0.
	var counter uint64 = 1

	tmpWriter := tmpDB.NewManagedWriteBatch()
	stream := pstore.NewStreamAt(r.startTs)
	stream.LogPrefix = fmt.Sprintf("Rebuilding index for predicate %s (1/2):", r.attr)
	stream.Prefix = r.prefix
	stream.MaxSize = (uint64(dbOpts.MemTableSize) * 9) / 10
	//TODO We need to create a single transaction irrespective of the type of the predicate
	if pred.ValueType == pb.Posting_VFLOAT {
		x.AssertTrue(false)
	}

	stream.UseKeyToListWithThreadId = true

	const maxThreadIds = 10000

	txns := make([]*Txn, maxThreadIds)
	for i := range txns {
		txns[i] = NewTxn(r.startTs)
	}

	stream.FinishThread = func(threadId int) (*bpb.KVList, error) {
		// Convert data into deltas.
		streamTxn := txns[threadId]
		streamTxn.Update()
		// txn.cache.Lock() is not required because we are the only one making changes to txn.
		kvs := make([]*bpb.KV, 0)

		for key, data := range streamTxn.cache.deltas {
			version := atomic.AddUint64(&counter, 1)
			kv := bpb.KV{
				Key:      []byte(key),
				Value:    data,
				UserMeta: []byte{BitDeltaPosting},
				Version:  version,
			}
			kvs = append(kvs, &kv)
		}
		txns[threadId] = NewTxn(r.startTs)
		return &bpb.KVList{Kv: kvs}, nil
	}

	stream.KeyToListWithThreadId = func(key []byte, itr *badger.Iterator, threadId int) (*bpb.KVList, error) {
		// We should return quickly if the context is no longer valid.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		pk, err := x.Parse(key)
		if err != nil {
			return nil, errors.Wrapf(err, "could not parse key %s", hex.Dump(key))
		}

		l, err := ReadPostingList(key, itr)
		if err != nil {
			return nil, errors.Wrapf(err, "error reading posting list from disk")
		}

		kvs := make([]*bpb.KV, 0)
		if pred.Count {
			kvs, err = l.Rollup(nil, r.startTs)
			if err != nil {
				return nil, err
			}
		}

		for _, kv := range kvs {
			version := atomic.AddUint64(&counter, 1)
			kv.Version = version
		}

		streamTxn := txns[threadId]
		_, err = r.fn(pk.Uid, l, streamTxn)
		if err != nil {
			return nil, err
		}

		if len(streamTxn.cache.plists) < 10000 {
			return &bpb.KVList{Kv: kvs}, nil
		}

		// Convert data into deltas.
		streamTxn.Update()
		// txn.cache.Lock() is not required because we are the only one making changes to txn.
		for key, data := range streamTxn.cache.deltas {
			version := atomic.AddUint64(&counter, 1)
			kv := bpb.KV{
				Key:      []byte(key),
				Value:    data,
				UserMeta: []byte{BitDeltaPosting},
				Version:  version,
			}
			kvs = append(kvs, &kv)
		}

		txns[threadId] = NewTxn(r.startTs)
		return &bpb.KVList{Kv: kvs}, nil
	}

	stream.Send = func(buf *z.Buffer) error {
		t1 := time.Now()
		defer func() {
			glog.V(1).Infof("Rebuilding index for predicate %s: writing index to badger %d bytes took %v",
				r.attr, len(buf.Bytes()), time.Since(t1))
		}()
		if err := tmpWriter.Write(buf); err != nil {
			return errors.Wrap(err, "error setting entries in temp badger")
		}

		return nil
	}

	start := time.Now()
	if err := stream.Orchestrate(ctx); err != nil {
		return err
	}
	if err := tmpWriter.Flush(); err != nil {
		return err
	}
	glog.V(1).Infof("Rebuilding index for predicate %s: building temp index took: %v\n",
		r.attr, time.Since(start))

	// Now we write all the created posting lists to disk.
	glog.V(1).Infof("Rebuilding index for predicate %s: writing index to badger", r.attr)
	start = time.Now()
	defer func() {
		glog.V(1).Infof("Rebuilding index for predicate %s: writing index took: %v\n",
			r.attr, time.Since(start))
	}()

	writer := pstore.NewManagedWriteBatch()
	tmpStream := tmpDB.NewStreamAt(counter)
	tmpStream.LogPrefix = fmt.Sprintf("Rebuilding index for predicate %s (2/2):", r.attr)
	tmpStream.KeyToList = func(key []byte, itr *badger.Iterator) (*bpb.KVList, error) {
		l, err := ReadPostingList(key, itr)
		if err != nil {
			return nil, errors.Wrap(err, "error in reading posting list from pstore")
		}
		// No need to write a loop after ReadPostingList to skip unread entries
		// for a given key because we only wrote BitDeltaPosting to temp badger.
		// We can write the data at their original timestamp in pstore badger.
		// We do the rollup at MaxUint64 so that we don't change the timestamp of resulting list.
		kvs, err := l.Rollup(nil, math.MaxUint64)
		if err != nil {
			return nil, err
		}

		MemLayerInstance.del(key)
		return &bpb.KVList{Kv: kvs}, nil
	}
	tmpStream.Send = func(buf *z.Buffer) error {
		return buf.SliceIterate(func(slice []byte) error {
			kv := &bpb.KV{}
			if err := proto.Unmarshal(slice, kv); err != nil {
				return err
			}
			// We choose to write the PL at r.startTs, so it won't be read by txns,
			// which occurred before this schema mutation.
			e := &badger.Entry{
				Key:      kv.Key,
				Value:    kv.Value,
				UserMeta: BitCompletePosting,
			}

			if len(kv.Value) == 0 {
				e = &badger.Entry{
					Key:      kv.Key,
					Value:    kv.Value,
					UserMeta: BitEmptyPosting,
				}
			}

			if err := writer.SetEntryAt(e.WithDiscard(), r.startTs); err != nil {
				return errors.Wrap(err, "error in writing index to pstore")
			}
			return nil
		})
	}

	if err := tmpStream.Orchestrate(ctx); err != nil {
		return err
	}
	glog.V(1).Infof("Rebuilding index for predicate %s: Flushing all writes.\n", r.attr)
	return writer.Flush()
}

// IndexRebuild holds the info needed to initiate a rebuilt of the indices.
type IndexRebuild struct {
	Attr          string
	StartTs       uint64
	OldSchema     *pb.SchemaUpdate
	CurrentSchema *pb.SchemaUpdate
}

type indexOp int

const (
	indexNoop    indexOp = iota // Index should be left alone.
	indexDelete          = iota // Index should be deleted.
	indexRebuild         = iota // Index should be deleted and rebuilt.
)

// GetQuerySchema returns the schema that can be served while indexes are getting built.
// Query schema is defined as current schema minus tokens to delete from current schema.
func (rb *IndexRebuild) GetQuerySchema() *pb.SchemaUpdate {
	// Copy the current schema.
	querySchema := *rb.CurrentSchema
	info := rb.needsTokIndexRebuild()

	// Compute old.Tokenizer minus info.tokenizersToDelete.
	interimTokenizers := make([]string, 0)
	for _, t1 := range rb.OldSchema.Tokenizer {
		found := false
		for _, t2 := range info.tokenizersToDelete {
			if t1 == t2 {
				found = true
				break
			}
		}
		if !found {
			interimTokenizers = append(interimTokenizers, t1)
		}
	}
	querySchema.Tokenizer = interimTokenizers

	if rb.needsCountIndexRebuild() == indexRebuild {
		querySchema.Count = false
	}
	if rb.needsReverseEdgesRebuild() == indexRebuild {
		querySchema.Directive = pb.SchemaUpdate_NONE
	}
	return &querySchema
}

// DropIndexes drops the indexes that need to be rebuilt.
func (rb *IndexRebuild) DropIndexes(ctx context.Context) error {
	rebuildInfo := rb.needsTokIndexRebuild()
	prefixes, err := rebuildInfo.prefixesForTokIndexes()
	if err != nil {
		return err
	}
	vectorIndexPrefixes, err := rebuildInfo.prefixesForVectorIndexes()
	if err != nil {
		return nil
	}
	prefixes = append(prefixes, vectorIndexPrefixes...)
	prefixes = append(prefixes, prefixesToDropReverseEdges(ctx, rb)...)
	prefixes = append(prefixes, prefixesToDropCountIndex(ctx, rb)...)
	prefixes = append(prefixes, prefixesToDropVectorIndexEdges(ctx, rb)...)
	if len(prefixes) > 0 {
		// This trace message now gets logged only if there are any prefixes to
		// to be deleted
		glog.Infof("Deleting indexes for %s", rb.Attr)
		return pstore.DropPrefix(prefixes...)
	}
	return nil
}

// BuildData updates data.
func (rb *IndexRebuild) BuildData(ctx context.Context) error {
	return rebuildListType(ctx, rb)
}

// NeedIndexRebuild returns true if any of the tokenizer, reverse
// or count indexes need to be rebuilt.
func (rb *IndexRebuild) NeedIndexRebuild() bool {
	return rb.needsTokIndexRebuild().op == indexRebuild ||
		rb.needsReverseEdgesRebuild() == indexRebuild ||
		rb.needsCountIndexRebuild() == indexRebuild
}

// BuildIndexes builds indexes.
func (rb *IndexRebuild) BuildIndexes(ctx context.Context) error {
	if err := rebuildTokIndex(ctx, rb); err != nil {
		return err
	}
	if err := rebuildReverseEdges(ctx, rb); err != nil {
		return err
	}
	return rebuildCountIndex(ctx, rb)
}

type indexRebuildInfo struct {
	op                     indexOp
	attr                   string
	tokenizersToDelete     []string
	tokenizersToRebuild    []string
	vectorIndexesToDelete  []*pb.VectorIndexSpec
	vectorIndexesToRebuild []*pb.VectorIndexSpec
}

func (rb *IndexRebuild) needsTokIndexRebuild() *indexRebuildInfo {
	x.AssertTruef(rb.CurrentSchema != nil, "Current schema cannot be nil.")

	// If the old schema is nil, we can treat it as an empty schema. Copy it
	// first to avoid overwriting it in rb.
	old := rb.OldSchema
	if old == nil {
		old = &pb.SchemaUpdate{}
	}

	currIndex := rb.CurrentSchema.Directive == pb.SchemaUpdate_INDEX
	prevIndex := old.Directive == pb.SchemaUpdate_INDEX

	// Index does not need to be rebuilt or deleted if the scheme directive
	// did not require an index before and now.
	if !currIndex && !prevIndex {
		return &indexRebuildInfo{
			op:   indexNoop,
			attr: rb.Attr,
		}
	}

	// Index only needs to be deleted if the schema directive changed and the
	// new directive does not require an index. Predicate is not checking
	// prevIndex since the previous if statement guarantees both values are
	// different.
	if !currIndex {
		return &indexRebuildInfo{
			op:                    indexDelete,
			attr:                  rb.Attr,
			tokenizersToDelete:    old.Tokenizer,
			vectorIndexesToDelete: old.IndexSpecs,
		}
	}

	// All tokenizers in the index need to be deleted and rebuilt if the value
	// types have changed.
	if currIndex && rb.CurrentSchema.ValueType != old.ValueType {
		return &indexRebuildInfo{
			op:                     indexRebuild,
			attr:                   rb.Attr,
			tokenizersToDelete:     old.Tokenizer,
			tokenizersToRebuild:    rb.CurrentSchema.Tokenizer,
			vectorIndexesToDelete:  old.IndexSpecs,
			vectorIndexesToRebuild: rb.CurrentSchema.IndexSpecs,
		}
	}

	// Index needs to be rebuilt if the tokenizers have changed
	prevTokens := make(map[string]struct{})
	for _, t := range old.Tokenizer {
		prevTokens[t] = struct{}{}
	}
	currTokens := make(map[string]struct{})
	for _, t := range rb.CurrentSchema.Tokenizer {
		currTokens[t] = struct{}{}
	}

	newTokenizers, deletedTokenizers := x.Diff(currTokens, prevTokens)

	prevFactoryNames := make(map[string]struct{})
	prevFactories := make(map[string]*pb.VectorIndexSpec)
	for _, t := range old.IndexSpecs {
		spec, err := tok.GetFactoryCreateSpecFromSpec(t)
		x.AssertTruef(err == nil, "Error while building index spec %s", err)
		name := spec.Name()

		prevFactoryNames[name] = struct{}{}
		prevFactories[name] = t
	}
	currFactoryNames := make(map[string]struct{})
	currFactories := make(map[string]*pb.VectorIndexSpec)
	for _, t := range rb.CurrentSchema.IndexSpecs {
		spec, err := tok.GetFactoryCreateSpecFromSpec(t)
		x.AssertTruef(err == nil, "Error while building index spec %s", err)
		name := spec.Name()

		currFactoryNames[name] = struct{}{}
		currFactories[name] = t
	}

	newFactoryNames, deletedFactoryNames := x.Diff(currFactoryNames, prevFactoryNames)

	// If the tokenizers and factories are the same, nothing needs to be done.
	if len(newTokenizers) == 0 && len(deletedTokenizers) == 0 &&
		len(newFactoryNames) == 0 && len(deletedFactoryNames) == 0 {
		return &indexRebuildInfo{
			op:   indexNoop,
			attr: rb.Attr,
		}
	}
	newFactories := []*pb.VectorIndexSpec{}
	for _, name := range newFactoryNames {
		newFactories = append(newFactories, currFactories[name])
	}
	deletedFactories := []*pb.VectorIndexSpec{}
	for _, name := range deletedFactoryNames {
		deletedFactories = append(deletedFactories, prevFactories[name])
	}

	return &indexRebuildInfo{
		op:                     indexRebuild,
		attr:                   rb.Attr,
		tokenizersToDelete:     deletedTokenizers,
		tokenizersToRebuild:    newTokenizers,
		vectorIndexesToDelete:  deletedFactories,
		vectorIndexesToRebuild: newFactories,
	}
}

func (rb *indexRebuildInfo) appendTokenizerPrefixesToDelete(
	tokenizer string,
	priorPrefixes [][]byte) ([][]byte, error) {
	retVal := priorPrefixes
	prefixesNonLang, err := prefixesToDeleteTokensFor(rb.attr, tokenizer, false)
	if err != nil {
		return nil, err
	}
	retVal = append(retVal, prefixesNonLang...)
	if tokenizer != "exact" {
		return retVal, nil
	}
	prefixesWithLang, err := prefixesToDeleteTokensFor(rb.attr, tokenizer, true)
	if err != nil {
		return nil, err
	}
	return append(retVal, prefixesWithLang...), nil
}

// TODO: Kill this function. Rather than calculating prefixes -- like we do
//
//	for tokenizers -- we should instead invoke the Remove(indexName)
//	operation of the VectorIndexFactory, and have it do all the deletion.
//	At the moment however, the Remove operation does not interact with
//	Dgraph transactions, so this is not yet possible.
func (rb *indexRebuildInfo) prefixesForVectorIndexes() ([][]byte, error) {
	prefixes := [][]byte{}
	var err error
	if rb.op == indexNoop {
		return prefixes, nil
	}

	for _, vectorSpec := range rb.vectorIndexesToDelete {
		glog.Infof("Computing prefix index for attr %s and index factory %s",
			rb.attr, vectorSpec.Name)
		// The mechanism currently is the same for tokenizers and
		// vector factories.
		prefixes, err = rb.appendTokenizerPrefixesToDelete(vectorSpec.Name, prefixes)
		if err != nil {
			return nil, err
		}
	}

	for _, vectorSpec := range rb.vectorIndexesToRebuild {
		glog.Infof("Computing prefix index for attr %s and index factory %s",
			rb.attr, vectorSpec.Name)
		// The mechanism currently is the same for tokenizers and
		// vector factories.
		prefixes, err = rb.appendTokenizerPrefixesToDelete(vectorSpec.Name, prefixes)
		if err != nil {
			return nil, err
		}
	}

	return prefixes, nil
}

func (rb *indexRebuildInfo) prefixesForTokIndexes() ([][]byte, error) {
	prefixes := [][]byte{}
	var err error
	if rb.op == indexNoop {
		return prefixes, nil
	}

	glog.Infof("Computing prefix index for attr %s and tokenizers %s", rb.attr,
		rb.tokenizersToDelete)
	for _, tokenizer := range rb.tokenizersToDelete {
		prefixes, err = rb.appendTokenizerPrefixesToDelete(tokenizer, prefixes)
		if err != nil {
			return nil, err
		}
	}

	glog.Infof("Deleting index for attr %s and tokenizers %s", rb.attr,
		rb.tokenizersToRebuild)
	// Before rebuilding, the existing index needs to be deleted.
	for _, tokenizer := range rb.tokenizersToRebuild {
		prefixes, err = rb.appendTokenizerPrefixesToDelete(tokenizer, prefixes)
		if err != nil {
			return nil, err
		}
	}

	return prefixes, nil
}

func rebuildVectorIndex(ctx context.Context, factorySpecs []*tok.FactoryCreateSpec, rb *IndexRebuild) error {
	pk := x.ParsedKey{Attr: rb.Attr}

	indexer, err := factorySpecs[0].CreateIndex(pk.Attr)
	if err != nil {
		return err
	}

	dimension := indexer.Dimension()
	// If dimension is -1, it means that the dimension is not set through options in case of partitioned hnsw.
	if dimension == -1 {
		numVectorsToCheck := 100
		lenFreq := make(map[int]int, numVectorsToCheck)
		maxFreq := 0
		MemLayerInstance.IterateDisk(ctx, IterateDiskArgs{
			Prefix:      pk.DataPrefix(),
			ReadTs:      rb.StartTs,
			AllVersions: false,
			Reverse:     false,
			CheckInclusion: func(uid uint64) error {
				return nil
			},
			Function: func(l *List, pk x.ParsedKey) error {
				val, err := l.Value(rb.StartTs)
				if err != nil {
					return err
				}
				inVec := types.BytesAsFloatArray(val.Value.([]byte))
				lenFreq[len(inVec)] += 1
				if lenFreq[len(inVec)] > maxFreq {
					maxFreq = lenFreq[len(inVec)]
					dimension = len(inVec)
				}
				numVectorsToCheck -= 1
				if numVectorsToCheck <= 0 {
					return ErrStopIteration
				}
				return nil
			},
			StartKey: x.DataKey(rb.Attr, 0),
		})

		indexer.SetDimension(rb.CurrentSchema, dimension)
	}

	fmt.Println("Selecting vector dimension to be:", dimension)

	norm := rebuilder{attr: rb.Attr, prefix: pk.DataPrefix(), startTs: rb.StartTs}
	norm.fn = func(uid uint64, pl *List, txn *Txn) ([]*pb.DirectedEdge, error) {
		val, err := pl.Value(rb.StartTs)
		if err != nil {
			return nil, err
		}
		if val.Tid == types.VFloatID {
			return nil, nil
		}

		// Convert to VFloatID and persist as binary bytes.
		sv, err := types.Convert(val, types.VFloatID)
		if err != nil {
			return nil, err
		}
		b := types.ValueForType(types.BinaryID)
		if err = types.Marshal(sv, &b); err != nil {
			return nil, err
		}

		edge := &pb.DirectedEdge{
			Attr:      rb.Attr,
			Entity:    uid,
			Value:     b.Value.([]byte),
			ValueType: types.VFloatID.Enum(),
		}
		inKey := x.DataKey(edge.Attr, uid)
		p, err := txn.Get(inKey)
		if err != nil {
			return []*pb.DirectedEdge{}, err
		}

		if err := p.addMutation(ctx, txn, edge); err != nil {
			return []*pb.DirectedEdge{}, err
		}
		return nil, nil
	}

	if err := norm.RunWithoutTemp(ctx); err != nil {
		return err
	}

	count := 0

	if indexer.NumSeedVectors() > 0 {
		err := MemLayerInstance.IterateDisk(ctx, IterateDiskArgs{
			Prefix:      pk.DataPrefix(),
			ReadTs:      rb.StartTs,
			AllVersions: false,
			Reverse:     false,
			CheckInclusion: func(uid uint64) error {
				return nil
			},
			Function: func(l *List, pk x.ParsedKey) error {
				val, err := l.Value(rb.StartTs)
				if err != nil {
					return err
				}

				if val.Tid != types.VFloatID {
					// Here, we convert the defaultID type vector into vfloat.
					sv, err := types.Convert(val, types.VFloatID)
					if err != nil {
						return err
					}
					b := types.ValueForType(types.BinaryID)
					if err = types.Marshal(sv, &b); err != nil {
						return err
					}

					val.Value = b.Value
					val.Tid = types.VFloatID
				}

				inVec := types.BytesAsFloatArray(val.Value.([]byte))
				if len(inVec) != dimension {
					return fmt.Errorf("vector dimension mismatch expected dimension %d but got %d", dimension, len(inVec))
				}
				count += 1
				indexer.AddSeedVector(inVec)
				if count == indexer.NumSeedVectors() {
					return ErrStopIteration
				}
				return nil
			},
			StartKey: x.DataKey(rb.Attr, 0),
		})
		if err != nil {
			return err
		}
	}

	txns := make([]*Txn, indexer.NumThreads())
	for i := range txns {
		txns[i] = NewTxn(rb.StartTs)
	}
	caches := make([]tokIndex.CacheType, indexer.NumThreads())
	for i := range caches {
		caches[i] = hnsw.NewTxnCache(NewViTxn(txns[i]), rb.StartTs)
	}

	if count < indexer.NumSeedVectors() {
		indexer.SetNumPasses(0)
	}

	for pass_idx := range indexer.NumBuildPasses() {
		fmt.Println("Building pass", pass_idx)

		indexer.StartBuild(caches)

		builder := rebuilder{attr: rb.Attr, prefix: pk.DataPrefix(), startTs: rb.StartTs}
		builder.fn = func(uid uint64, pl *List, txn *Txn) ([]*pb.DirectedEdge, error) {
			val, err := pl.Value(rb.StartTs)
			if err != nil {
				return []*pb.DirectedEdge{}, err
			}

			inVec := types.BytesAsFloatArray(val.Value.([]byte))
			if len(inVec) != dimension {
				return []*pb.DirectedEdge{}, nil
			}
			indexer.BuildInsert(ctx, uid, inVec)
			return []*pb.DirectedEdge{}, nil
		}

		err := builder.RunWithoutTemp(ctx)
		if err != nil {
			return err
		}

		indexer.EndBuild()
	}

	centroids := indexer.GetCentroids()

	if centroids != nil {
		txn := NewTxn(rb.StartTs)

		bCentroids, err := json.Marshal(centroids)
		if err != nil {
			return err
		}

		if err := addCentroidInDB(ctx, rb.Attr, bCentroids, txn); err != nil {
			return err
		}
		txn.Update()
		writer := NewTxnWriter(pstore)
		if err := txn.CommitToDisk(writer, rb.StartTs); err != nil {
			return err
		}
	}

	numIndexPasses := indexer.NumIndexPasses()

	if count < indexer.NumSeedVectors() {
		numIndexPasses = 1
	}

	for pass_idx := range numIndexPasses {
		fmt.Println("Indexing pass", pass_idx)

		indexer.StartBuild(caches)

		builder := rebuilder{attr: rb.Attr, prefix: pk.DataPrefix(), startTs: rb.StartTs}
		builder.fn = func(uid uint64, pl *List, txn *Txn) ([]*pb.DirectedEdge, error) {
			val, err := pl.Value(rb.StartTs)
			if err != nil {
				return []*pb.DirectedEdge{}, err
			}

			inVec := types.BytesAsFloatArray(val.Value.([]byte))
			if len(inVec) != dimension && centroids != nil {
				if pass_idx == 0 {
					glog.Warningf("Skipping vector with invalid dimension uid: %d, dimension: %d", uid, len(inVec))
				}
				return []*pb.DirectedEdge{}, nil
			}

			indexer.BuildInsert(ctx, uid, inVec)

			return []*pb.DirectedEdge{}, nil
		}

		err := builder.RunWithoutTemp(ctx)
		if err != nil {
			return err
		}

		for _, idx := range indexer.EndBuild() {
			txns[idx].Update()
			writer := NewTxnWriter(pstore)

			x.ExponentialRetry(int(x.Config.MaxRetries),
				20*time.Millisecond, func() error {
					err := txns[idx].CommitToDisk(writer, rb.StartTs)
					if err == badger.ErrBannedKey {
						glog.Errorf("Error while writing to banned namespace.")
						return nil
					}
					return err
				})

			txns[idx].cache.plists = nil
			txns[idx] = nil
		}
	}

	return nil
}

func addCentroidInDB(ctx context.Context, attr string, vec []byte, txn *Txn) error {
	indexCountAttr := hnsw.ConcatStrings(attr, kmeans.CentroidPrefix)
	countKey := x.DataKey(indexCountAttr, 1)
	pl, err := txn.Get(countKey)
	if err != nil {
		return err
	}

	edge := &pb.DirectedEdge{
		Entity:    1,
		Attr:      indexCountAttr,
		Value:     vec,
		ValueType: pb.Posting_ValType(12),
	}
	if err := pl.addMutation(ctx, txn, edge); err != nil {
		return err
	}
	return nil
}

// rebuildTokIndex rebuilds index for a given attribute.
// We commit mutations with startTs and ignore the errors.
func rebuildTokIndex(ctx context.Context, rb *IndexRebuild) error {
	rebuildInfo := rb.needsTokIndexRebuild()
	if rebuildInfo.op != indexRebuild {
		return nil
	}

	// Exit early if there are no tokenizers to rebuild.
	if len(rebuildInfo.tokenizersToRebuild) == 0 && len(rebuildInfo.vectorIndexesToRebuild) == 0 {
		return nil
	}

	glog.Infof("Rebuilding index for attr %s and tokenizers %s", rb.Attr,
		rebuildInfo.tokenizersToRebuild)
	tokenizers, err := tok.GetTokenizers(rebuildInfo.tokenizersToRebuild)
	if err != nil {
		return err
	}

	var factorySpecs []*tok.FactoryCreateSpec
	if len(rebuildInfo.vectorIndexesToRebuild) > 0 {
		factorySpec, err := tok.GetFactoryCreateSpecFromSpec(
			rebuildInfo.vectorIndexesToRebuild[0])
		if err != nil {
			return err
		}
		factorySpecs = []*tok.FactoryCreateSpec{factorySpec}
	}

	runForVectors := (len(factorySpecs) != 0)
	if runForVectors {
		return rebuildVectorIndex(ctx, factorySpecs, rb)
	}

	pk := x.ParsedKey{Attr: rb.Attr}
	builder := rebuilder{attr: rb.Attr, prefix: pk.DataPrefix(), startTs: rb.StartTs}
	builder.fn = func(uid uint64, pl *List, txn *Txn) ([]*pb.DirectedEdge, error) {
		edge := pb.DirectedEdge{Attr: rb.Attr, Entity: uid}
		edges := []*pb.DirectedEdge{}

		processAddIndexMutation := func(edge *pb.DirectedEdge, val types.Val) ([]*pb.DirectedEdge, error) {
			for {
				newEdges, err := txn.addIndexMutations(ctx, &indexMutationInfo{
					tokenizers:   tokenizers,
					factorySpecs: factorySpecs,
					edge:         edge,
					val:          val,
					op:           pb.DirectedEdge_SET,
				})
				switch err {
				case ErrRetry:
					time.Sleep(10 * time.Millisecond)
				default:
					return newEdges, err
				}
			}
		}

		// There are two cases to consider here:
		// 1. This can be a schema mutation where the user adds a index on existing vectors.
		// 2. This can be a vector mutation where the user adds vectors to the DB on a
		// predicate that is already indexed.
		if runForVectors {
			val, err := pl.Value(txn.StartTs)
			if err != nil {
				return []*pb.DirectedEdge{}, err
			}

			// In the first case, val.Tid is default, so we need to convert the
			// vector into the vfloat type and re-add it to the DB.
			if val.Tid != types.VFloatID {
				// Here, we convert the defaultID type vector into vfloat.
				sv, err := types.Convert(val, types.VFloatID)
				if err != nil {
					return []*pb.DirectedEdge{}, err
				}
				b := types.ValueForType(types.BinaryID)
				if err = types.Marshal(sv, &b); err != nil {
					return []*pb.DirectedEdge{}, err
				}
				edge.Value = b.Value.([]byte)
				edge.ValueType = types.VFloatID.Enum()

				inKey := x.DataKey(edge.Attr, uid)
				p, err := txn.Get(inKey)
				if err != nil {
					return []*pb.DirectedEdge{}, err
				}

				if err := p.addMutation(ctx, txn, &edge); err != nil {
					return []*pb.DirectedEdge{}, err
				}
			}
			// In the second case, we don't need to convert the vector as it is already
			// in the vfloat type. We just need to process it further.
			_, err = processAddIndexMutation(&edge, val)
			if err != nil {
				return []*pb.DirectedEdge{}, err
			}

			return edges, nil
		}

		err := pl.Iterate(txn.StartTs, 0, func(p *pb.Posting) error {
			// Add index entries based on p.
			val := types.Val{
				Value: p.Value,
				Tid:   types.TypeID(p.ValType),
			}
			edge.Lang = string(p.LangTag)

			newEdges, err := processAddIndexMutation(&edge, val)
			if err != nil {
				return err
			}
			edges = append(edges, newEdges...)
			return nil
		})
		if err != nil {
			return []*pb.DirectedEdge{}, err
		}
		return edges, err
	}

	if runForVectors {
		return builder.RunWithoutTemp(ctx)
	}
	return builder.Run(ctx)
}

func (rb *IndexRebuild) needsCountIndexRebuild() indexOp {
	x.AssertTruef(rb.CurrentSchema != nil, "Current schema cannot be nil.")

	// If the old schema is nil, treat it as an empty schema. Copy it to avoid
	// overwriting it in rb.
	old := rb.OldSchema
	if old == nil {
		old = &pb.SchemaUpdate{}
	}

	// Do nothing if the schema directive did not change.
	if rb.CurrentSchema.Count == old.Count {
		return indexNoop

	}

	// If the new schema does not require an index, delete the current index.
	if !rb.CurrentSchema.Count {
		return indexDelete
	}

	// Otherwise, the index needs to be rebuilt.
	return indexRebuild
}

func prefixesToDropCountIndex(ctx context.Context, rb *IndexRebuild) [][]byte {
	// Exit early if indices do not need to be rebuilt.
	op := rb.needsCountIndexRebuild()

	if op == indexNoop {
		return nil
	}

	pk := x.ParsedKey{Attr: rb.Attr}
	prefixes := append([][]byte{}, pk.CountPrefix(false))
	prefixes = append(prefixes, pk.CountPrefix(true))

	// All the parts of any list that has been split into multiple parts.
	// Such keys have a different prefix (the last byte is set to 1).
	countPrefix := pk.CountPrefix(false)
	countPrefix[0] = x.ByteSplit
	prefixes = append(prefixes, countPrefix)

	// Parts for count-reverse index.
	countReversePrefix := pk.CountPrefix(true)
	countReversePrefix[0] = x.ByteSplit
	prefixes = append(prefixes, countReversePrefix)

	return prefixes
}

// rebuildCountIndex rebuilds the count index for a given attribute.
func rebuildCountIndex(ctx context.Context, rb *IndexRebuild) error {
	op := rb.needsCountIndexRebuild()
	if op != indexRebuild {
		return nil
	}

	glog.Infof("Rebuilding count index for %s", rb.Attr)
	var reverse bool
	fn := func(uid uint64, pl *List, txn *Txn) ([]*pb.DirectedEdge, error) {
		t := &pb.DirectedEdge{
			ValueId: uid,
			Attr:    rb.Attr,
			Op:      pb.DirectedEdge_SET,
		}
		sz := pl.Length(rb.StartTs, 0)
		if sz == -1 {
			return []*pb.DirectedEdge{}, nil
		}
		for {
			err := txn.addCountMutation(ctx, t, uint32(sz), reverse)
			switch err {
			case ErrRetry:
				time.Sleep(10 * time.Millisecond)
			default:
				return []*pb.DirectedEdge{}, err
			}
		}
	}

	// Create the forward index.
	pk := x.ParsedKey{Attr: rb.Attr}
	builder := rebuilder{attr: rb.Attr, prefix: pk.DataPrefix(), startTs: rb.StartTs}
	builder.fn = fn
	if err := builder.Run(ctx); err != nil {
		return err
	}

	// Create the reverse index. The count reverse index is created if this
	// predicate has both a count and reverse directive in the schema. It's safe
	// to call builder.Run even if that's not the case as the reverse prefix
	// will be empty.
	reverse = true
	builder = rebuilder{attr: rb.Attr, prefix: pk.ReversePrefix(), startTs: rb.StartTs}
	builder.fn = fn
	return builder.Run(ctx)
}

func (rb *IndexRebuild) needsVectorIndexEdgesRebuild() indexOp {
	x.AssertTruef(rb.CurrentSchema != nil, "Current schema cannot be nil.")

	// If old schema is nil, treat it as an empty schema. Copy it to avoid
	// overwriting it in rb.
	old := rb.OldSchema
	if old == nil {
		old = &pb.SchemaUpdate{}
	}

	currIndex := rb.CurrentSchema.Directive == pb.SchemaUpdate_INDEX &&
		rb.CurrentSchema.ValueType == pb.Posting_VFLOAT
	prevIndex := old.Directive == pb.SchemaUpdate_INDEX &&
		old.ValueType == pb.Posting_VFLOAT

	prevFactoryNames := make(map[string]struct{})
	prevFactories := make(map[string]*pb.VectorIndexSpec)
	for _, t := range old.IndexSpecs {
		spec, err := tok.GetFactoryCreateSpecFromSpec(t)
		x.AssertTruef(err == nil, "Error while building index spec %s", err)
		name := spec.Name()

		prevFactoryNames[name] = struct{}{}
		prevFactories[name] = t
	}

	currFactoryNames := make(map[string]struct{})
	currFactories := make(map[string]*pb.VectorIndexSpec)
	for _, t := range rb.CurrentSchema.IndexSpecs {
		spec, err := tok.GetFactoryCreateSpecFromSpec(t)
		x.AssertTruef(err == nil, "Error while building index spec %s", err)
		name := spec.Name()

		currFactoryNames[name] = struct{}{}
		currFactories[name] = t
	}

	newFactoryNames, deletedFactoryNames := x.Diff(currFactoryNames, prevFactoryNames)

	// If the schema directive did not change, return indexNoop.
	if currIndex == prevIndex && len(newFactoryNames) == 0 && len(deletedFactoryNames) == 0 {
		return indexNoop
	}

	// If the current schema requires an index, index should be rebuilt.
	if currIndex || len(newFactoryNames) != 0 {
		return indexRebuild
	}
	// Otherwise, index should only be deleted.
	return indexDelete
}

// This needs to be moved to the implementation of vector-indexer API
func prefixesToDropVectorIndexEdges(ctx context.Context, rb *IndexRebuild) [][]byte {
	// Exit early if indices do not need to be rebuilt.
	op := rb.needsVectorIndexEdgesRebuild()
	if op == indexNoop {
		return nil
	}

	prefixes := append([][]byte{}, x.PredicatePrefix(hnsw.ConcatStrings(rb.Attr, hnsw.VecEntry)))
	prefixes = append(prefixes, x.PredicatePrefix(hnsw.ConcatStrings(rb.Attr, hnsw.VecDead)))
	prefixes = append(prefixes, x.PredicatePrefix(hnsw.ConcatStrings(rb.Attr, hnsw.VecKeyword)))

	for i := range hnsw.VectorIndexMaxLevels {
		prefixes = append(prefixes, x.PredicatePrefix(hnsw.ConcatStrings(rb.Attr, hnsw.VecKeyword, fmt.Sprint(i))))
	}

	return prefixes
}

func (rb *IndexRebuild) needsReverseEdgesRebuild() indexOp {
	x.AssertTruef(rb.CurrentSchema != nil, "Current schema cannot be nil.")

	// If old schema is nil, treat it as an empty schema. Copy it to avoid
	// overwriting it in rb.
	old := rb.OldSchema
	if old == nil {
		old = &pb.SchemaUpdate{}
	}

	currIndex := rb.CurrentSchema.Directive == pb.SchemaUpdate_REVERSE
	prevIndex := old.Directive == pb.SchemaUpdate_REVERSE

	// If the schema directive did not change, return indexNoop.
	if currIndex == prevIndex {
		return indexNoop
	}

	// If the current schema requires an index, index should be rebuilt.
	if currIndex {
		return indexRebuild
	}
	// Otherwise, index should only be deleted.
	return indexDelete
}

func prefixesToDropReverseEdges(ctx context.Context, rb *IndexRebuild) [][]byte {
	// Exit early if indices do not need to be rebuilt.
	op := rb.needsReverseEdgesRebuild()
	if op == indexNoop {
		return nil
	}

	pk := x.ParsedKey{Attr: rb.Attr}
	prefixes := append([][]byte{}, pk.ReversePrefix())

	// All the parts of any list that has been split into multiple parts.
	// Such keys have a different prefix (the last byte is set to 1).
	reversePrefix := pk.ReversePrefix()
	reversePrefix[0] = x.ByteSplit
	prefixes = append(prefixes, reversePrefix)

	return prefixes
}

// rebuildReverseEdges rebuilds the reverse edges for a given attribute.
func rebuildReverseEdges(ctx context.Context, rb *IndexRebuild) error {
	op := rb.needsReverseEdgesRebuild()
	if op != indexRebuild {
		return nil
	}

	glog.Infof("Rebuilding reverse index for %s", rb.Attr)
	pk := x.ParsedKey{Attr: rb.Attr}
	builder := rebuilder{attr: rb.Attr, prefix: pk.DataPrefix(), startTs: rb.StartTs}
	builder.fn = func(uid uint64, pl *List, txn *Txn) ([]*pb.DirectedEdge, error) {
		edge := pb.DirectedEdge{Attr: rb.Attr, Entity: uid}
		return []*pb.DirectedEdge{}, pl.Iterate(txn.StartTs, 0, func(pp *pb.Posting) error {
			puid := pp.Uid
			// Add reverse entries based on p.
			edge.ValueId = puid
			edge.Op = pb.DirectedEdge_SET
			edge.Facets = pp.Facets

			for {
				// we only need to build reverse index here.
				// We will update the reverse count index separately.
				err := txn.addReverseMutation(ctx, &edge)
				switch err {
				case ErrRetry:
					time.Sleep(10 * time.Millisecond)
				default:
					return err
				}
			}
		})
	}
	return builder.Run(ctx)
}

// needsListTypeRebuild returns true if the schema changed from a scalar to a
// list. It returns true if the index can be left as is.
func (rb *IndexRebuild) needsListTypeRebuild() (bool, error) {
	x.AssertTruef(rb.CurrentSchema != nil, "Current schema cannot be nil.")

	if rb.OldSchema == nil {
		return false, nil
	}
	if rb.CurrentSchema.List && !rb.OldSchema.List {
		return true, nil
	}
	if rb.OldSchema.List && !rb.CurrentSchema.List {
		return false, errors.Errorf("Type can't be changed from list to scalar for attr: [%s]"+
			" without dropping it first.", x.ParseAttr(rb.CurrentSchema.Predicate))
	}

	return false, nil
}

// rebuildListType rebuilds the index when the schema is changed from scalar to list type.
// We need to fingerprint the values to get the new ValueId.
func rebuildListType(ctx context.Context, rb *IndexRebuild) error {
	if needsRebuild, err := rb.needsListTypeRebuild(); !needsRebuild || err != nil {
		return err
	}

	pk := x.ParsedKey{Attr: rb.Attr}
	builder := rebuilder{attr: rb.Attr, prefix: pk.DataPrefix(), startTs: rb.StartTs}
	builder.fn = func(uid uint64, pl *List, txn *Txn) ([]*pb.DirectedEdge, error) {
		var mpost *pb.Posting
		err := pl.Iterate(txn.StartTs, 0, func(p *pb.Posting) error {
			// We only want to modify the untagged value. There could be other values with a
			// lang tag.
			if p.Uid == math.MaxUint64 {
				mpost = p
			}
			return nil
		})
		if err != nil {
			return []*pb.DirectedEdge{}, err
		}
		if mpost == nil {
			return []*pb.DirectedEdge{}, nil
		}
		// Delete the old edge corresponding to ValueId math.MaxUint64
		t := &pb.DirectedEdge{
			ValueId: mpost.Uid,
			Attr:    rb.Attr,
			Op:      pb.DirectedEdge_DEL,
		}

		// Ensure that list is in the cache run by txn. Otherwise, nothing would
		// get updated.
		pl = txn.cache.SetIfAbsent(string(pl.key), pl)
		if err := pl.addMutation(ctx, txn, t); err != nil {
			return []*pb.DirectedEdge{}, err
		}
		// Add the new edge with the fingerprinted value id.
		newEdge := &pb.DirectedEdge{
			Attr:      rb.Attr,
			Value:     mpost.Value,
			ValueType: mpost.ValType,
			Op:        pb.DirectedEdge_SET,
			Facets:    mpost.Facets,
		}
		return []*pb.DirectedEdge{}, pl.addMutation(ctx, txn, newEdge)
	}
	return builder.Run(ctx)
}

// DeleteAll deletes all entries in the posting list.
func DeleteAll() error {
	ResetCache()
	return pstore.DropAll()
}

func DeleteAllForNs(ns uint64) error {
	ResetCache()
	schema.State().DeletePredsForNs(ns)
	return DeleteData(ns)
}

// DeleteData deletes all data for the namespace but leaves types and schema intact.
func DeleteData(ns uint64) error {
	ResetCache()
	prefix := make([]byte, 9)
	prefix[0] = x.DefaultPrefix
	binary.BigEndian.PutUint64(prefix[1:], ns)
	return pstore.DropPrefix(prefix)
}

// DeletePredicate deletes all entries and indices for a given predicate.
func DeletePredicate(ctx context.Context, attr string, ts uint64) error {
	glog.Infof("Dropping predicate: [%s]", attr)
	ResetCache()
	preds := schema.State().PredicatesToDelete(attr)
	for _, pred := range preds {
		prefix := x.PredicatePrefix(pred)
		if err := schema.State().Delete(pred, ts); err != nil {
			return err
		}
		if err := pstore.DropPrefix(prefix); err != nil {
			return err
		}
	}

	return nil
}

// DeleteNamespace bans the namespace and deletes its predicates/types from the schema.
func DeleteNamespace(ns uint64) error {
	// TODO: We should only delete cache for certain keys, not all the keys.
	ResetCache()
	schema.State().DeletePredsForNs(ns)
	return pstore.BanNamespace(ns)
}
