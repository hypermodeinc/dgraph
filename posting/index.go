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
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/dgryski/go-farm"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	ostats "go.opencensus.io/stats"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
	bpb "github.com/dgraph-io/badger/v4/pb"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/tok"
	"github.com/hypermodeinc/dgraph/v25/tok/hnsw"
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
		return nil, errors.Wrap(err, "Cannot convert value to scalar type")
	}

	var tokens []string
	for _, it := range info.tokenizers {
		toks, err := tok.BuildTokens(sv.Value, tok.GetTokenizerForLang(it, lang))
		if err != nil {
			return tokens, errors.Wrapf(err, "Cannot build tokens for attribute %s", attr)
		}
		tokens = append(tokens, toks...)
	}
	return tokens, nil
}

type MutationPipeline struct {
	txn *Txn
}

func NewMutationPipeline(txn *Txn) *MutationPipeline {
	return &MutationPipeline{txn: txn}
}

type PredicatePipeline struct {
	attr  string
	edges chan *pb.DirectedEdge
	wg    *sync.WaitGroup
	errCh chan error
}

func (pp *PredicatePipeline) close() {
	pp.wg.Done()
}

func (mp *MutationPipeline) ProcessVectorIndex(ctx context.Context, pipeline *PredicatePipeline, info predicateInfo) error {
	var wg errgroup.Group
	numThreads := 10

	for i := 0; i < numThreads; i++ {
		wg.Go(func() error {
			for edge := range pipeline.edges {
				uid := edge.Entity

				key := x.DataKey(pipeline.attr, uid)
				pl, err := mp.txn.Get(key)
				if err != nil {
					return err
				}
				if err := pl.AddMutationWithIndex(ctx, edge, mp.txn); err != nil {
					return err
				}
			}
			return nil
		})
	}

	if err := wg.Wait(); err != nil {
		return err
	}

	return nil
}

func (mp *MutationPipeline) InsertTokenizerIndexes(ctx context.Context, pipeline *PredicatePipeline, postings *map[uint64]*pb.PostingList, info predicateInfo) error {
	startTime := time.Now()
	defer func() {
		fmt.Println("Inserting tokenizer indexes for predicate", pipeline.attr, "took", time.Since(startTime))
	}()

	tokenizers := schema.State().Tokenizer(ctx, pipeline.attr)
	if len(tokenizers) == 0 {
		return nil
	}

	indexesGenInMutation := types.NewLockedShardedMap[string, *MutableLayer]()
	wg := &sync.WaitGroup{}

	syncMap := sync.Map{}

	chanFn := func(uids chan uint64, estimatedSize int) {
		defer wg.Done()
		indexGenInThread := make(map[string]*pb.PostingList, estimatedSize)
		tokenizers := schema.State().Tokenizer(ctx, pipeline.attr)

		factorySpecs, err := schema.State().FactoryCreateSpec(ctx, pipeline.attr)
		if err != nil {
			pipeline.errCh <- err
			return
		}

		indexEdge := &pb.DirectedEdge{
			Attr: pipeline.attr,
		}

		for uid := range uids {
			postingList := (*postings)[uid]
			newList := &pb.PostingList{}
			if info.isSingleEdge && len(postingList.Postings) == 2 {
				newList.Postings = append(newList.Postings, postingList.Postings[1])
				newList.Postings = append(newList.Postings, postingList.Postings[0])
			} else {
				newList = postingList
			}
			for _, posting := range newList.Postings {
				info := &indexMutationInfo{
					tokenizers:   tokenizers,
					factorySpecs: factorySpecs,
					op:           pb.DirectedEdge_SET,
					val: types.Val{
						Tid:   types.TypeID(posting.ValType),
						Value: posting.Value,
					},
				}

				info.edge = &pb.DirectedEdge{
					Attr:  pipeline.attr,
					Op:    pb.DirectedEdge_SET,
					Lang:  string(posting.LangTag),
					Value: posting.Value,
				}

				key := fmt.Sprintf("%s,%s", posting.LangTag, posting.Value)
				tokens, loaded := syncMap.Load(key)

				if !loaded {
					tokens, err = indexTokens(ctx, info)
					if err != nil {
						fmt.Println("ERRORRRING", err)
						x.Panic(err)
					}
					syncMap.Store(key, tokens)
				}

				indexEdge.Op = GetPostingOp(posting.Op)
				indexEdge.ValueId = uid
				mpost := makePostingFromEdge(mp.txn.StartTs, indexEdge)

				for _, token := range tokens.([]string) {
					key := x.IndexKey(pipeline.attr, token)
					pk, _ := x.Parse([]byte(key))
					fmt.Println("TOKENS", key, pk)
					val, ok := indexGenInThread[string(key)]
					if !ok {
						val = &pb.PostingList{}
					}
					val.Postings = append(val.Postings, mpost)
					indexGenInThread[string(key)] = val
				}
			}
		}

		for key, value := range indexGenInThread {
			pk, _ := x.Parse([]byte(key))
			fmt.Println("LOCAL MAP", pk, value)
			indexesGenInMutation.Update(key, func(val *MutableLayer, ok bool) *MutableLayer {
				if !ok {
					val = newMutableLayer()
					val.currentEntries = &pb.PostingList{}
				}
				for _, posting := range value.Postings {
					val.insertPosting(posting, false)
				}
				return val
			})
		}
	}

	numGo := 10
	wg.Add(numGo)
	chMap := make(map[int]chan uint64)

	for i := 0; i < numGo; i++ {
		uidCh := make(chan uint64, numGo)
		chMap[i] = uidCh
		go chanFn(uidCh, len(*postings)/numGo)
	}

	for uid := range *postings {
		chMap[int(uid)%numGo] <- uid
	}

	for i := 0; i < numGo; i++ {
		close(chMap[i])
	}

	wg.Wait()

	mp.txn.cache.Lock()
	defer mp.txn.cache.Unlock()

	indexGenInTxn := mp.txn.cache.deltas.GetIndexMapForPredicate(pipeline.attr)
	if indexGenInTxn == nil {
		indexGenInTxn = types.NewLockedShardedMap[string, *pb.PostingList]()
		mp.txn.cache.deltas.indexMap[pipeline.attr] = indexGenInTxn
	}

	fmt.Println("INSERTING INDEX", pipeline.attr, *postings)
	updateFn := func(key string, value *MutableLayer) {
		pk, _ := x.Parse([]byte(key))
		fmt.Println("UPDATE INDEX", pk, value)
		indexGenInTxn.Update(key, func(val *pb.PostingList, ok bool) *pb.PostingList {
			if !ok {
				val = &pb.PostingList{}
			}
			val.Postings = append(val.Postings, value.currentEntries.Postings...)
			return val
		})
	}

	if info.hasUpsert {
		err := indexesGenInMutation.Iterate(func(key string, value *MutableLayer) error {
			updateFn(key, value)
			mp.txn.addConflictKey(farm.Fingerprint64([]byte(key)))
			return nil
		})
		if err != nil {
			return err
		}
	} else {
		err := indexesGenInMutation.Iterate(func(key string, value *MutableLayer) error {
			updateFn(key, value)
			mp.txn.addConflictKeyWithUid([]byte(key), value.currentEntries, info.hasUpsert, info.noConflict)
			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// func (mp *MutationPipeline) InsertTokenizerIndexes(ctx context.Context, pipeline *PredicatePipeline, postings *map[uint64]*pb.PostingList, info predicateInfo) error {
// 	startTime := time.Now()
// 	defer func() {
// 		fmt.Println("Inserting tokenizer indexes for predicate", pipeline.attr, "took", time.Since(startTime))
// 	}()

// 	tokenizers := schema.State().Tokenizer(ctx, pipeline.attr)
// 	if len(tokenizers) == 0 {
// 		return nil
// 	}

// 	values := make(map[string]*pb.PostingList, len(tokenizers)*len(*postings))
// 	valPost := make(map[string]*pb.Posting)

// 	indexEdge1 := &pb.DirectedEdge{
// 		Attr: pipeline.attr,
// 	}

// 	for uid, postingList := range *postings {
// 		fmt.Println("POSTING", uid, postingList)
// 		for _, posting := range postingList.Postings {
// 			key := fmt.Sprintf("%s,%s", posting.LangTag, posting.Value)
// 			valPl, ok := values[key]
// 			if !ok {
// 				valPl = &pb.PostingList{}
// 			}

// 			indexEdge1.Op = GetPostingOp(posting.Op)
// 			indexEdge1.ValueId = uid

// 			mpost := makePostingFromEdge(mp.txn.StartTs, indexEdge1)
// 			valPl.Postings = append(valPl.Postings, mpost)
// 			values[key] = valPl

// 			newPosting := new(pb.Posting)
// 			newPosting.ValType = posting.ValType
// 			newPosting.Value = posting.Value
// 			newPosting.LangTag = posting.LangTag
// 			valPost[key] = newPosting
// 		}
// 	}

// 	keysCreated := make([]string, 0, len(values))
// 	for i := range values {
// 		keysCreated = append(keysCreated, i)
// 	}

// 	//fmt.Println("START")

// 	f := func(numGo int) *types.LockedShardedMap[string, *MutableLayer] {
// 		wg := &sync.WaitGroup{}

// 		globalMap := types.NewLockedShardedMap[string, *MutableLayer]()
// 		process := func(start int) {
// 			factorySpecs, err := schema.State().FactoryCreateSpec(ctx, pipeline.attr)
// 			if err != nil {
// 				pipeline.errCh <- err
// 					return
// 			}

// 			defer wg.Done()
// 			localMap := make(map[string]*pb.PostingList, len(values)/numGo)
// 			for i := start; i < len(values); i += numGo {
// 				key := keysCreated[i]
// 				valPl := values[key]
// 				if len(valPl.Postings) == 0 {
// 					continue
// 				}

// 				posting := valPost[key]
// 				// Build info per iteration without indexEdge.
// 				info := &indexMutationInfo{
// 					tokenizers:   tokenizers,
// 					factorySpecs: factorySpecs,
// 					op:           pb.DirectedEdge_SET,
// 					val: types.Val{
// 						Tid:   types.TypeID(posting.ValType),
// 						Value: posting.Value,
// 					},
// 				}

// 				info.edge = &pb.DirectedEdge{
// 					Attr:  pipeline.attr,
// 					Op:    pb.DirectedEdge_SET,
// 					Lang:  string(posting.LangTag),
// 					Value: posting.Value,
// 				}

// 				tokens, erri := indexTokens(ctx, info)
// 				if erri != nil {
// 					fmt.Println("ERRORRRING", erri)
// 					x.Panic(erri)
// 				}

// 				for _, token := range tokens {
// 					key := x.IndexKey(pipeline.attr, token)
// 					pk, _ := x.Parse([]byte(key))
// 					fmt.Println("TOKENS", key, i, numGo, pk)
// 					val, ok := localMap[string(key)]
// 					if !ok {
// 						val = &pb.PostingList{}
// 					}
// 					val.Postings = append(val.Postings, valPl.Postings...)
// 					localMap[string(key)] = val
// 				}
// 			}

// 			for key, value := range localMap {
// 				pk, _ := x.Parse([]byte(key))
// 				fmt.Println("LOCAL MAP", pk, numGo, value)
// 				globalMap.Update(key, func(val *MutableLayer, ok bool) *MutableLayer {
// 					if !ok {
// 						val = newMutableLayer()
// 						val.currentEntries = &pb.PostingList{}
// 					}
// 					for _, posting := range value.Postings {
// 						val.insertPosting(posting, false)
// 					}
// 					return val
// 				})
// 			}
// 		}

// 		for i := range numGo {
// 			wg.Add(1)
// 			go process(i)
// 		}

// 		wg.Wait()

// 		return globalMap
// 	}

// 	globalMapI := f(1)

// 	mp.txn.cache.Lock()
// 	defer mp.txn.cache.Unlock()

// 	globalMap := mp.txn.cache.deltas.GetIndexMapForPredicate(pipeline.attr)
// 	if globalMap == nil {
// 		globalMap = types.NewLockedShardedMap[string, *pb.PostingList]()
// 		mp.txn.cache.deltas.indexMap[pipeline.attr] = globalMap
// 	}

// 	updateFn := func(key string, value *MutableLayer) {
// 		globalMap.Update(key, func(val *pb.PostingList, ok bool) *pb.PostingList {
// 			if !ok {
// 				val = &pb.PostingList{}
// 			}
// 			val.Postings = append(val.Postings, value.currentEntries.Postings...)
// 			return val
// 		})
// 	}

// 	if info.hasUpsert {
// 		err := globalMapI.Iterate(func(key string, value *MutableLayer) error {
// 			updateFn(key, value)
// 			mp.txn.addConflictKey(farm.Fingerprint64([]byte(key)))
// 			return nil
// 		})
// 		if err != nil {
// 			return err
// 		}
// 	} else {
// 		err := globalMapI.Iterate(func(key string, value *MutableLayer) error {
// 			updateFn(key, value)
// 			mp.txn.addConflictKeyWithUid([]byte(key), value.currentEntries, info.hasUpsert, info.noConflict)
// 			return nil
// 		})
// 		if err != nil {
// 			return err
// 		}
// 	}

// 	return nil
// }

type predicateInfo struct {
	isList     bool
	index      bool
	reverse    bool
	count      bool
	noConflict bool
	hasUpsert  bool
	isUid      bool

	isSingleEdge bool
}

func (mp *MutationPipeline) ProcessList(ctx context.Context, pipeline *PredicatePipeline, info predicateInfo) error {
	su, schemaExists := schema.State().Get(ctx, pipeline.attr)

	mutations := make(map[uint64]*MutableLayer, 1000)

	for edge := range pipeline.edges {
		if edge.Op != pb.DirectedEdge_DEL && !schemaExists {
			return errors.Errorf("runMutation: Unable to find schema for %s", edge.Attr)
		}

		if err := ValidateAndConvert(edge, &su); err != nil {
			return err
		}

		uid := edge.Entity
		pl, exists := mutations[uid]
		if !exists {
			pl = newMutableLayer()
			pl.currentEntries = &pb.PostingList{}
		}

		mpost := NewPosting(edge)
		mpost.StartTs = mp.txn.StartTs
		if mpost.PostingType != pb.Posting_REF {
			edge.ValueId = FingerprintEdge(edge)
			mpost.Uid = edge.ValueId
		}

		pl.insertPosting(mpost, false)
		mutations[uid] = pl
	}

	postings := make(map[uint64]*pb.PostingList, 1000)
	for uid, pl := range mutations {
		postings[uid] = pl.currentEntries
	}

	if info.reverse {
		if err := mp.ProcessReverse(ctx, pipeline, &postings, info); err != nil {
			return err
		}
	}

	if info.index {
		if err := mp.InsertTokenizerIndexes(ctx, pipeline, &postings, info); err != nil {
			return err
		}
	}

	if info.count {
		return mp.ProcessCount(ctx, pipeline, &postings, info, true, false)
	}

	dataKey := x.DataKey(pipeline.attr, 0)
	baseKey := string(dataKey[:len(dataKey)-8]) // Avoid repeated conversion

	for uid, pl := range postings {
		if len(pl.Postings) == 0 {
			continue
		}

		binary.BigEndian.PutUint64(dataKey[len(dataKey)-8:], uid)
		if newPl, err := mp.txn.AddDelta(baseKey+string(dataKey[len(dataKey)-8:]), pl, info.isUid, true); err != nil {
			return err
		} else {
			if !info.noConflict {
				mp.txn.addConflictKeyWithUid(dataKey, newPl, info.hasUpsert, info.noConflict)
			}
		}
	}

	return nil
}

func findSingleValueInPostingList(pb *pb.PostingList) *pb.Posting {
	if pb == nil {
		return nil
	}
	for _, p := range pb.Postings {
		if p.Op == Set {
			return p
		}
	}
	return nil
}

func (mp *MutationPipeline) ProcessReverse(ctx context.Context, pipeline *PredicatePipeline, postings *map[uint64]*pb.PostingList, info predicateInfo) error {
	key := x.ReverseKey(pipeline.attr, 0)
	edge := &pb.DirectedEdge{
		Attr: pipeline.attr,
	}
	reverseredMap := make(map[uint64]*pb.PostingList, 1000)
	for uid, postingList := range *postings {
		for _, posting := range postingList.Postings {
			postingList, ok := reverseredMap[posting.Uid]
			if !ok {
				postingList = &pb.PostingList{}
			}
			edge.Entity = posting.Uid
			edge.ValueId = uid
			edge.ValueType = posting.ValType
			edge.Op = GetPostingOp(posting.Op)
			edge.Facets = posting.Facets

			postingList.Postings = append(postingList.Postings, makePostingFromEdge(mp.txn.StartTs, edge))
			reverseredMap[posting.Uid] = postingList
		}
	}

	if info.count {
		newInfo := predicateInfo{
			isList:     true,
			index:      info.index,
			reverse:    info.reverse,
			count:      info.count,
			noConflict: info.noConflict,
			hasUpsert:  info.hasUpsert,
		}
		return mp.ProcessCount(ctx, pipeline, &reverseredMap, newInfo, true, true)
	}

	for uid, pl := range reverseredMap {
		if len(pl.Postings) == 0 {
			continue
		}
		binary.BigEndian.PutUint64(key[len(key)-8:], uid)
		if newPl, err := mp.txn.AddDelta(string(key), pl, true, true); err != nil {
			return err
		} else {
			mp.txn.addConflictKeyWithUid(key, newPl, info.hasUpsert, info.noConflict)
		}
	}

	return nil
}

func makePostingFromEdge(startTs uint64, edge *pb.DirectedEdge) *pb.Posting {
	mpost := NewPosting(edge)
	mpost.StartTs = startTs
	if mpost.PostingType != pb.Posting_REF {
		edge.ValueId = FingerprintEdge(edge)
		mpost.Uid = edge.ValueId
	}
	return mpost
}

func (mp *MutationPipeline) handleOldDeleteForSingle(pipeline *PredicatePipeline, postings map[uint64]*pb.PostingList) error {
	edge := &pb.DirectedEdge{
		Attr: pipeline.attr,
	}

	dataKey := x.DataKey(pipeline.attr, 0)

	for uid, postingList := range postings {
		currValue := findSingleValueInPostingList(postingList)
		if currValue == nil {
			continue
		}

		binary.BigEndian.PutUint64(dataKey[len(dataKey)-8:], uid)
		list, err := mp.txn.GetScalarList(dataKey)
		if err != nil {
			return err
		}

		oldValList, err := list.StaticValue(mp.txn.StartTs)
		if err != nil {
			return err
		}

		oldVal := findSingleValueInPostingList(oldValList)

		if oldVal == nil {
			continue
		}

		if string(oldVal.Value) == string(currValue.Value) {
			postings[uid] = &pb.PostingList{}
			continue
		}

		edge.Op = pb.DirectedEdge_DEL
		edge.Value = oldVal.Value
		edge.ValueType = oldVal.ValType
		edge.ValueId = oldVal.Uid

		mpost := makePostingFromEdge(mp.txn.StartTs, edge)
		postingList.Postings = append(postingList.Postings, mpost)
		postings[uid] = postingList
	}

	return nil
}

func (txn *Txn) addConflictKeyWithUid(key []byte, pl *pb.PostingList, hasUpsert bool, hasNoConflict bool) {
	if hasNoConflict {
		return
	}
	txn.Lock()
	defer txn.Unlock()
	if txn.conflicts == nil {
		txn.conflicts = make(map[uint64]struct{})
	}
	keyHash := farm.Fingerprint64(key)
	if hasUpsert {
		txn.conflicts[keyHash] = struct{}{}
		return
	}
	for _, post := range pl.Postings {
		txn.conflicts[keyHash^post.Uid] = struct{}{}
	}
}

func (mp *MutationPipeline) ProcessCount(ctx context.Context, pipeline *PredicatePipeline, postings *map[uint64]*pb.PostingList, info predicateInfo, isListEdge bool, isReverseEdge bool) error {
	dataKey := x.DataKey(pipeline.attr, 0)
	if isReverseEdge {
		dataKey = x.ReverseKey(pipeline.attr, 0)
	}
	edge := pb.DirectedEdge{
		Attr: pipeline.attr,
	}

	countMap := make(map[int]*pb.PostingList, 2*len(*postings))

	insertEdgeCount := func(count int) {
		c, ok := countMap[count]
		if !ok {
			c = &pb.PostingList{}
			countMap[count] = c
		}
		c.Postings = append(c.Postings, makePostingFromEdge(mp.txn.StartTs, &edge))
		countMap[count] = c
	}

	for uid, postingList := range *postings {
		binary.BigEndian.PutUint64(dataKey[len(dataKey)-8:], uid)
		list, err := mp.txn.Get(dataKey)
		if err != nil {
			return err
		}

		list.Lock()
		prevCount := list.GetLength(mp.txn.StartTs)

		for _, post := range postingList.Postings {
			found, _, _ := list.findPosting(post.StartTs, post.Uid)
			if found {
				if post.Op == Set && isListEdge {
					post.Op = Ovr
				}
			} else {
				if post.Op == Del {
					continue
				}
			}

			if err := list.updateMutationLayer(post, !isListEdge, true); err != nil {
				return err
			}
		}

		newCount := list.GetLength(mp.txn.StartTs)
		updated := list.mutationMap.currentEntries != nil
		list.Unlock()

		if updated {
			if !isListEdge {
				if !info.noConflict {
					mp.txn.addConflictKey(farm.Fingerprint64(dataKey))
				}
			} else {
				mp.txn.addConflictKeyWithUid(dataKey, postingList, info.hasUpsert, info.noConflict)
			}
		}

		if newCount == prevCount {
			continue
		}

		//fmt.Println("COUNT STATS", uid, prevCount, newCount, postingList, list.Print())

		edge.ValueId = uid
		edge.Op = pb.DirectedEdge_DEL
		if prevCount > 0 {
			insertEdgeCount(prevCount)
		}
		edge.Op = pb.DirectedEdge_SET
		if newCount > 0 {
			insertEdgeCount(newCount)
		}
	}

	for c, pl := range countMap {
		//fmt.Println("COUNT", c, pl)
		ck := x.CountKey(pipeline.attr, uint32(c), isReverseEdge)
		if newPl, err := mp.txn.AddDelta(string(ck), pl, true, true); err != nil {
			return err
		} else {
			mp.txn.addConflictKeyWithUid(ck, newPl, info.hasUpsert, info.noConflict)
		}
	}

	return nil
}

func (mp *MutationPipeline) ProcessSingle(ctx context.Context, pipeline *PredicatePipeline, info predicateInfo) error {
	su, schemaExists := schema.State().Get(ctx, pipeline.attr)

	postings := make(map[uint64]*pb.PostingList, 1000)

	dataKey := x.DataKey(pipeline.attr, 0)
	insertDeleteAllEdge := !(info.index || info.reverse || info.count) // nolint

	var oldVal *pb.Posting
	for edge := range pipeline.edges {
		// fmt.Println("SINGLE EDGE", edge)
		if edge.Op != pb.DirectedEdge_DEL && !schemaExists {
			return errors.Errorf("runMutation: Unable to find schema for %s", edge.Attr)
		}

		if err := ValidateAndConvert(edge, &su); err != nil {
			return err
		}

		uid := edge.Entity
		pl, exists := postings[uid]

		setPosting := func() {
			mpost := makePostingFromEdge(mp.txn.StartTs, edge)
			if len(pl.Postings) == 0 {
				if insertDeleteAllEdge {
					pl = &pb.PostingList{
						Postings: []*pb.Posting{createDeleteAllPosting(), mpost},
					}
				} else {
					pl = &pb.PostingList{
						Postings: []*pb.Posting{mpost},
					}
				}
			} else {
				if pl.Postings[len(pl.Postings)-1].Op == Set {
					pl.Postings[len(pl.Postings)-1] = mpost
				} else {
					pl.Postings = append(pl.Postings, mpost)
				}
			}
			postings[uid] = pl
		}

		if exists {
			if edge.Op == pb.DirectedEdge_DEL {
				oldVal = findSingleValueInPostingList(pl)
				if string(edge.Value) == string(oldVal.Value) {
					setPosting()
				}
			} else {
				setPosting()
			}
			continue
		}

		pl = &pb.PostingList{}
		postings[uid] = pl

		if edge.Op == pb.DirectedEdge_DEL {
			binary.BigEndian.PutUint64(dataKey[len(dataKey)-8:], uid)
			list, err := mp.txn.GetScalarList(dataKey)
			if err != nil {
				return err
			}
			if list != nil {
				l, err := list.StaticValue(mp.txn.StartTs)
				if err != nil {
					return err
				}
				oldVal = findSingleValueInPostingList(l)
			}
			if oldVal != nil {
				if string(oldVal.Value) == string(edge.Value) {
					setPosting()
				}
			}
		} else {
			setPosting()
		}
	}

	if info.index || info.reverse || info.count {
		if err := mp.handleOldDeleteForSingle(pipeline, postings); err != nil {
			return err
		}
	}

	if info.index {
		if err := mp.InsertTokenizerIndexes(ctx, pipeline, &postings, info); err != nil {
			return err
		}
	}

	if info.reverse {
		if err := mp.ProcessReverse(ctx, pipeline, &postings, info); err != nil {
			return err
		}
	}

	if info.count {
		// Count should take care of updating the posting list
		return mp.ProcessCount(ctx, pipeline, &postings, info, false, false)
	}

	baseKey := string(dataKey[:len(dataKey)-8]) // Avoid repeated conversion

	for uid, pl := range postings {
		//fmt.Println("ADDING DELTA", uid, pipeline.attr, pl)
		binary.BigEndian.PutUint64(dataKey[len(dataKey)-8:], uid)
		key := baseKey + string(dataKey[len(dataKey)-8:])

		if !info.noConflict {
			mp.txn.addConflictKey(farm.Fingerprint64([]byte(key)))
		}

		if _, err := mp.txn.AddDelta(key, pl, false, false); err != nil {
			return err
		}
	}

	return nil
}

func runMutation(ctx context.Context, edge *pb.DirectedEdge, txn *Txn) error {
	ctx = schema.GetWriteContext(ctx)

	// We shouldn't check whether this Alpha serves this predicate or not. Membership information
	// isn't consistent across the entire cluster. We should just apply whatever is given to us.
	su, ok := schema.State().Get(ctx, edge.Attr)
	if edge.Op != pb.DirectedEdge_DEL {
		if !ok {
			return errors.Errorf("runMutation: Unable to find schema for %s", edge.Attr)
		}
	}

	key := x.DataKey(edge.Attr, edge.Entity)
	// The following is a performance optimization which allows us to not read a posting list from
	// disk. We calculate this based on how AddMutationWithIndex works. The general idea is that if
	// we're not using the read posting list, we don't need to retrieve it. We need the posting list
	// if we're doing count index or delete operation. For scalar predicates, we just get the last item merged.
	// In other cases, we can just create a posting list facade in memory and use it to store the delta in Badger.
	// Later, the rollup operation would consolidate all these deltas into a posting list.
	isList := su.GetList()
	var getFn func(key []byte) (*List, error)
	switch {
	case len(edge.Lang) == 0 && !isList:
		// Scalar Predicates, without lang
		getFn = txn.GetScalarList
	case len(edge.Lang) > 0 || su.GetCount():
		// Language or Count Index
		getFn = txn.Get
	case edge.Op == pb.DirectedEdge_DEL:
		// Covers various delete cases to keep things simple.
		getFn = txn.Get
	default:
		// Only count index needs to be read. For other indexes on list, we don't need to read any data.
		// For indexes on scalar prediactes, only the last element needs to be left.
		// Delete cases covered above.
		getFn = txn.GetFromDelta
	}

	plist, err := getFn(key)
	if err != nil {
		return err
	}
	return plist.AddMutationWithIndex(ctx, edge, txn)
}

func (mp *MutationPipeline) ProcessPredicate(ctx context.Context, pipeline *PredicatePipeline) error {
	defer pipeline.close()
	ctx = schema.GetWriteContext(ctx)

	// We shouldn't check whether this Alpha serves this predicate or not. Membership information
	// isn't consistent across the entire cluster. We should just apply whatever is given to us.
	su, ok := schema.State().Get(ctx, pipeline.attr)
	info := predicateInfo{}
	runForVectorIndex := false

	if ok {
		info.index = schema.State().IsIndexed(ctx, pipeline.attr)
		info.count = schema.State().HasCount(ctx, pipeline.attr)
		info.reverse = schema.State().IsReversed(ctx, pipeline.attr)
		info.noConflict = schema.State().HasNoConflict(pipeline.attr)
		info.hasUpsert = schema.State().HasUpsert(pipeline.attr)
		info.isList = schema.State().IsList(pipeline.attr)
		info.isUid = su.ValueType == pb.Posting_UID
		factorySpecs, err := schema.State().FactoryCreateSpec(ctx, pipeline.attr)
		if err != nil {
			return err
		}
		if len(factorySpecs) > 0 {
			runForVectorIndex = true
		}
	}

	if runForVectorIndex {
		return mp.ProcessVectorIndex(ctx, pipeline, info)
	}

	runListFn := false

	if ok {
		if info.isList || su.Lang {
			runListFn = true
		}
	}

	info.isSingleEdge = !runListFn

	if runListFn {
		if err := mp.ProcessList(ctx, pipeline, info); err != nil {
			return err
		}
	}

	if ok && !runListFn {
		if err := mp.ProcessSingle(ctx, pipeline, info); err != nil {
			return err
		}
	}

	for edge := range pipeline.edges {
		if err := runMutation(ctx, edge, mp.txn); err != nil {
			return err
		}
	}

	return nil
}

func isStarAll(v []byte) bool {
	return bytes.Equal(v, []byte(x.Star))
}

func ValidateAndConvert(edge *pb.DirectedEdge, su *pb.SchemaUpdate) error {
	if types.TypeID(edge.ValueType) == types.DefaultID && isStarAll(edge.Value) {
		return nil
	}

	storageType := TypeID(edge)
	schemaType := types.TypeID(su.ValueType)

	// type checks
	switch {
	case edge.Lang != "" && !su.GetLang():
		return errors.Errorf("Attr: [%v] should have @lang directive in schema to mutate edge: [%v]",
			x.ParseAttr(edge.Attr), edge)

	case !schemaType.IsScalar() && !storageType.IsScalar():
		return nil

	case !schemaType.IsScalar() && storageType.IsScalar():
		return errors.Errorf("Input for predicate %q of type uid is scalar. Edge: %v",
			x.ParseAttr(edge.Attr), edge)

	case schemaType.IsScalar() && !storageType.IsScalar():
		return errors.Errorf("Input for predicate %q of type scalar is uid. Edge: %v",
			x.ParseAttr(edge.Attr), edge)

	case schemaType == types.TypeID(pb.Posting_VFLOAT):
		if !(storageType == types.TypeID(pb.Posting_VFLOAT) || storageType == types.TypeID(pb.Posting_STRING) || //nolint
			storageType == types.TypeID(pb.Posting_DEFAULT)) {
			return errors.Errorf("Input for predicate %q of type vector is not vector."+
				" Did you forget to add quotes before []?. Edge: %v", x.ParseAttr(edge.Attr), edge)
		}

	// The suggested storage type matches the schema, OK! (Nothing to do ...)
	case storageType == schemaType && schemaType != types.DefaultID:
		return nil

	// We accept the storage type iff we don't have a schema type and a storage type is specified.
	case schemaType == types.DefaultID:
		schemaType = storageType
	}

	var (
		dst types.Val
		err error
	)

	src := types.Val{Tid: types.TypeID(edge.ValueType), Value: edge.Value}
	// check compatibility of schema type and storage type
	// The goal is to convert value on edge to value type defined by schema.
	if dst, err = types.Convert(src, schemaType); err != nil {
		return err
	}

	// convert to schema type
	b := types.ValueForType(types.BinaryID)
	if err = types.Marshal(dst, &b); err != nil {
		return err
	}

	if x.WorkerConfig.AclEnabled && x.ParseAttr(edge.GetAttr()) == "dgraph.rule.permission" {
		perm, ok := dst.Value.(int64)
		if !ok {
			return errors.Errorf("Value for predicate <dgraph.rule.permission> should be of type int")
		}
		if perm < 0 || perm > 7 {
			return errors.Errorf("Can't set <dgraph.rule.permission> to %d, Value for this"+
				" predicate should be between 0 and 7", perm)
		}
	}

	// TODO: Figure out why this is Enum. It really seems like an odd choice -- rather than
	//       specifying it as the same type as presented in su.
	edge.ValueType = schemaType.Enum()
	var ok bool
	edge.Value, ok = b.Value.([]byte)
	if !ok {
		return errors.Errorf("failure to convert edge type: '%+v' to schema type: '%+v'",
			storageType, schemaType)
	}

	return nil
}

func (mp *MutationPipeline) Process(ctx context.Context, edges []*pb.DirectedEdge) error {
	predicates := map[string]*PredicatePipeline{}
	var wg sync.WaitGroup
	numWg := 0
	eg, egCtx := errgroup.WithContext(ctx)
	for _, edge := range edges {
		//fmt.Println("PROCESSING EDGE", edge)
		if edge.Op == pb.DirectedEdge_DEL && string(edge.Value) == x.Star {
			l, err := mp.txn.Get(x.DataKey(edge.Attr, edge.Entity))
			if err != nil {
				return err
			}
			if err = l.handleDeleteAll(ctx, edge, mp.txn); err != nil {
				return err
			}
			continue
		}
		pred, ok := predicates[edge.Attr]
		if !ok {
			pred = &PredicatePipeline{
				attr:  edge.Attr,
				edges: make(chan *pb.DirectedEdge, 1000),
				wg:    &wg,
			}
			predicates[edge.Attr] = pred
			wg.Add(1)
			numWg += 1
			// Capture pred by passing it as a parameter to the closure
			eg.Go(func(p *PredicatePipeline) func() error {
				return func() error {
					return mp.ProcessPredicate(egCtx, p)
				}
			}(pred))
		}
		pred.edges <- edge
	}
	for _, pred := range predicates {
		close(pred.edges)
	}
	if numWg == 0 {
		return nil
	}
	// Wait for all predicate processors; returns first error (and cancels others via context).
	if err := eg.Wait(); err != nil {
		return err
	}
	return nil
}

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

		if err := streamTxn.cache.deltas.IterateBytes(func(key string, data []byte) error {
			version := atomic.AddUint64(&counter, 1)
			kv := bpb.KV{
				Key:      []byte(key),
				Value:    data,
				UserMeta: []byte{BitDeltaPosting},
				Version:  version,
			}
			kvs = append(kvs, &kv)
			return nil
		}); err != nil {
			return nil, err
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
		if err := streamTxn.cache.deltas.IterateBytes(func(key string, data []byte) error {
			version := atomic.AddUint64(&counter, 1)
			kv := bpb.KV{
				Key:      []byte(key),
				Value:    data,
				UserMeta: []byte{BitDeltaPosting},
				Version:  version,
			}
			kvs = append(kvs, &kv)
			return nil
		}); err != nil {
			return nil, err
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
