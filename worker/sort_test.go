/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sync"
	"testing"

	"github.com/dgraph-io/badger/v4"
	bpb "github.com/dgraph-io/badger/v4/pb"
	"github.com/hypermodeinc/dgraph/v25/posting"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/tok"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/x"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

func readPostingListFromDisk(key []byte, pstore *badger.DB, readTs uint64) (*posting.List, error) {
	txn := pstore.NewTransactionAt(readTs, false)
	defer txn.Discard()

	// When we do rollups, an older version would go to the top of the LSM tree, which can cause
	// issues during txn.Get. Therefore, always iterate.
	iterOpts := badger.DefaultIteratorOptions
	iterOpts.AllVersions = true
	iterOpts.PrefetchValues = false
	itr := txn.NewKeyIterator(key, iterOpts)
	defer itr.Close()
	itr.Seek(key)
	return posting.ReadPostingList(key, itr)
}

func rollup(t *testing.T, key []byte, pstore *badger.DB, readTs uint64) {
	ol, err := readPostingListFromDisk(key, pstore, readTs)
	require.NoError(t, err)
	kvs, err := ol.Rollup(nil, readTs+1)
	require.NoError(t, err)
	require.NoError(t, writePostingListToDisk(kvs))
	posting.ResetCache()
}

func writePostingListToDisk(kvs []*bpb.KV) error {
	writer := posting.NewTxnWriter(pstore)
	for _, kv := range kvs {
		if err := writer.SetAt(kv.Key, kv.Value, kv.UserMeta[0], kv.Version); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func TestEmptyTypeSchema(t *testing.T) {
	dir, err := os.MkdirTemp("", "storetest_")
	x.Check(err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	x.Check(err)
	pstore = ps
	posting.Init(ps, 0, false)
	Init(ps)

	typeName := "1-temp"
	ts := uint64(10)
	txn := pstore.NewTransactionAt(ts, true)
	defer txn.Discard()
	e := &badger.Entry{
		Key:      x.TypeKey(typeName),
		Value:    make([]byte, 0),
		UserMeta: posting.BitSchemaPosting,
	}
	require.Nil(t, txn.SetEntry(e.WithDiscard()))
	require.Nil(t, txn.CommitAt(ts, nil))

	schema.Init(ps)
	require.Nil(t, schema.LoadFromDb(context.Background()))

	req := &pb.SchemaRequest{}
	types, err := GetTypes(context.Background(), req)
	require.Nil(t, err)

	require.Equal(t, 1, len(types))
	x.ParseNamespaceAttr(types[0].TypeName)
}

func TestDatetime(t *testing.T) {
	// Setup temporary directory for Badger DB
	dir, err := os.MkdirTemp("", "storetest_")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	require.NoError(t, err)
	posting.Init(ps, 0, false)
	Init(ps)

	// Set schema
	schemaTxt := `
		t: datetime @index(year) .
	`
	err = schema.ParseBytes([]byte(schemaTxt), 1)
	require.NoError(t, err)

	ctx := context.Background()
	newRunMutation := func(startTs, commitTs uint64, edges []*pb.DirectedEdge) {
		txn := posting.Oracle().RegisterStartTs(startTs)
		for _, edge := range edges {
			require.NoError(t, newRunMutation(ctx, edge, txn))
		}
		txn.Update()
		writer := posting.NewTxnWriter(ps)
		require.NoError(t, txn.CommitToDisk(writer, commitTs))
		require.NoError(t, writer.Flush())
		txn.UpdateCachedKeys(commitTs)
	}

	newRunMutation(1, 3, []*pb.DirectedEdge{
		{
			Entity:    1,
			Attr:      x.AttrInRootNamespace("t"),
			Value:     []byte("2020-01-01T00:00:00Z"),
			ValueType: pb.Posting_DEFAULT,
			Op:        pb.DirectedEdge_SET,
		},
	})

}

type indexMutationInfo struct {
	tokenizers   []tok.Tokenizer
	factorySpecs []*tok.FactoryCreateSpec
	edge         *pb.DirectedEdge // Represents the original uid -> value edge.
	val          types.Val
	op           pb.DirectedEdge_Op
}

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

func TestStringIndexWithLang(t *testing.T) {
	// Setup temporary directory for Badger DB
	dir, err := os.MkdirTemp("", "storetest_")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	require.NoError(t, err)
	posting.Init(ps, 0, false)
	Init(ps)

	// Set schema
	schemaTxt := `
		name: string @index(fulltext, trigram, term, exact) @lang .
	`

	err = schema.ParseBytes([]byte(schemaTxt), 1)
	require.NoError(t, err)

	ctx := context.Background()
	newRunMutation := func(startTs, commitTs uint64, edges []*pb.DirectedEdge) {
		txn := posting.Oracle().RegisterStartTs(startTs)
		require.NoError(t, newRunMutations(ctx, edges, txn))
		txn.Update()
		writer := posting.NewTxnWriter(ps)
		require.NoError(t, txn.CommitToDisk(writer, commitTs))
		require.NoError(t, writer.Flush())
		txn.UpdateCachedKeys(commitTs)
	}

	attr := x.AttrInRootNamespace("name")

	// Prepare 400 mutations across 4 threads, 100 per thread (kept modest for stability).
	const (
		threads     = 10
		perThread   = 20000
		total       = threads * perThread
		baseStartTs = uint64(10)
	)

	// uid -> value map
	values := make(map[uint64]string, total)
	for i := 0; i < total; i++ {
		uid := uint64(i + 1)
		// Simple deterministic values with shared tokens and unique numbers.
		values[uid] = fmt.Sprintf("title %d", i+1)
	}

	// Build expected token -> set of uids
	tokenizers := schema.State().Tokenizer(ctx, attr)
	expected := make(map[string]map[uint64]struct{}, total)
	for uid, val := range values {
		info := &indexMutationInfo{
			tokenizers: tokenizers,
			op:         pb.DirectedEdge_SET,
			val:        types.Val{Tid: types.StringID, Value: []byte(val)},
			edge: &pb.DirectedEdge{
				Attr:  attr,
				Value: []byte(val),
				Lang:  "en",
				Op:    pb.DirectedEdge_SET,
			},
		}
		toks, err := indexTokens(ctx, info)
		require.NoError(t, err)
		for _, tk := range toks {
			if expected[tk] == nil {
				expected[tk] = make(map[uint64]struct{})
			}
			expected[tk][uid] = struct{}{}
		}
	}

	// Run 4 threads; each thread writes 100 edges with distinct ts
	var wg sync.WaitGroup
	wg.Add(threads)
	for th := 0; th < threads; th++ {
		th := th
		go func() {
			defer wg.Done()
			start := th*perThread + 1
			end := start + perThread
			edges := make([]*pb.DirectedEdge, 0, perThread)
			for i := start; i < end; i++ {
				uid := uint64(i)
				edges = append(edges, &pb.DirectedEdge{
					Entity:    uid,
					Attr:      attr,
					Value:     []byte(values[uid]),
					ValueType: pb.Posting_DEFAULT,
					Lang:      "en",
					Op:        pb.DirectedEdge_SET,
				})
			}
			sTs := baseStartTs + uint64(th*10)
			cTs := sTs + 2
			newRunMutation(sTs, cTs, edges)
		}()
	}
	wg.Wait()

	// Verify all tokens have the expected UIDs.
	readTs := baseStartTs + uint64(threads*10) + 10
	for tk, uidset := range expected {
		key := x.IndexKey(attr, tk)
		txn := posting.Oracle().RegisterStartTs(readTs)
		pl, err := txn.Get(key)
		require.NoError(t, err)
		lst, err := pl.Uids(posting.ListOptions{ReadTs: readTs})
		require.NoError(t, err)
		got := make(map[uint64]struct{}, len(lst.Uids))
		for _, u := range lst.Uids {
			got[u] = struct{}{}
		}
		// Compare sets
		require.Equal(t, len(uidset), len(got), "mismatch uid count for token %q", tk)
		for u := range uidset {
			if _, ok := got[u]; !ok {
				t.Fatalf("missing uid %d for token %q", u, tk)
			}
		}
	}
}

func TestCount(t *testing.T) {
	t.Skip()
	// Setup temporary directory for Badger DB
	dir, err := os.MkdirTemp("", "storetest_")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	require.NoError(t, err)
	posting.Init(ps, 0, false)
	Init(ps)

	// Set schema
	schemaTxt := `
		friends: [uid] @count .
	`

	err = schema.ParseBytes([]byte(schemaTxt), 1)
	require.NoError(t, err)
	ctx := context.Background()
	newRunMutation := func(startTs, commitTs uint64, edges []*pb.DirectedEdge) {
		txn := posting.Oracle().RegisterStartTs(startTs)
		require.NoError(t, newRunMutations(ctx, edges, txn))
		txn.Update()
		writer := posting.NewTxnWriter(ps)
		require.NoError(t, txn.CommitToDisk(writer, commitTs))
		require.NoError(t, writer.Flush())
		txn.UpdateCachedKeys(commitTs)
	}

	pred := x.AttrInRootNamespace("friends")

	// Prepare mutations such that each subject gets multiple uid edges, and
	// each edge is added from a different thread. We also send multiple
	// batches per thread.
	const (
		subjects    = 10 // total number of subjects/entities
		edgesPer    = 5  // number of edges per subject
		threads     = 2  // one thread per edge ordinal, touching all subjects
		baseStartTs = uint64(10)
		total       = subjects * edgesPer
	)

	// 1) Pre-generate all mutations into one big slice
	edgesAll := make([]*pb.DirectedEdge, 0, total)
	for subj := 1; subj <= subjects; subj++ {
		uid := uint64(subj)
		for e := 0; e < edgesPer; e++ {
			// Unique object per (subject, edge-ordinal) pair to avoid duplicates.
			// Ensures exactly 'edgesPer' distinct UIDs per subject.
			obj := uint64(1_000_000 + subj*100 + e)
			edgesAll = append(edgesAll, &pb.DirectedEdge{
				Entity:    uid,
				Attr:      pred,
				ValueId:   obj,
				ValueType: pb.Posting_UID,
				Op:        pb.DirectedEdge_SET,
			})
		}
	}

	// Shuffle the edges to simulate randomness (determinism depends on rand.Seed in package scope)
	for i := range edgesAll {
		j := rand.Intn(i + 1)
		edgesAll[i], edgesAll[j] = edgesAll[j], edgesAll[i]
	}

	// 2) Dispatch pre-generated mutations into threads, in multiple batches per thread
	var wg sync.WaitGroup
	wg.Add(threads)
	for th := 0; th < threads; th++ {
		th := th
		go func() {
			defer wg.Done()
			// Split each thread's disjoint chunk into multiple batches/transactions
			const batches = 5
			chunk := total / threads
			chunkStart := th * chunk
			chunkEnd := chunkStart + chunk
			perBatch := chunk / batches
			for b := 0; b < batches; b++ {
				batchStart := chunkStart + b*perBatch
				batchEnd := batchStart + perBatch
				if b == batches-1 {
					batchEnd = chunkEnd
				}
				batch := edgesAll[batchStart:batchEnd]
				// Space out start/commit timestamps per thread and per batch to avoid collisions
				sTs := baseStartTs + uint64(th*100) + uint64(b*2)
				cTs := sTs + 1
				newRunMutation(sTs, cTs, batch)
			}
		}()
	}
	wg.Wait()

	// Verify the @count index for the exact number of edges per subject.
	countKey := x.CountKey(pred, edgesPer, false)
	txn := posting.Oracle().RegisterStartTs(math.MaxUint64)
	pl, err := txn.Get(countKey)
	require.NoError(t, err)
	uids, err := pl.Uids(posting.ListOptions{ReadTs: math.MaxUint64})
	require.NoError(t, err)
	fmt.Println(uids.Uids)
	require.Equal(t, subjects, len(uids.Uids))
}

func TestDeleteSetWithVarEdgeCorruptsData(t *testing.T) {
	// Setup temporary directory for Badger DB
	dir, err := os.MkdirTemp("", "storetest_")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	require.NoError(t, err)
	posting.Init(ps, 0, false)
	Init(ps)

	// Set schema
	schemaTxt := `
		room: string @index(hash) @upsert .
		person: string @index(hash) @upsert .
		office: uid @reverse @count .
	`
	err = schema.ParseBytes([]byte(schemaTxt), 1)
	require.NoError(t, err)

	ctx := context.Background()
	attrRoom := x.AttrInRootNamespace("room")
	attrPerson := x.AttrInRootNamespace("person")
	attrOffice := x.AttrInRootNamespace("office")

	uidRoom := uint64(1)
	uidJohn := uint64(2)

	newRunMutation := func(startTs, commitTs uint64, edges []*pb.DirectedEdge) {
		txn := posting.Oracle().RegisterStartTs(startTs)
		for _, edge := range edges {
			require.NoError(t, newRunMutation(ctx, edge, txn))
		}
		txn.Update()
		writer := posting.NewTxnWriter(ps)
		require.NoError(t, txn.CommitToDisk(writer, commitTs))
		require.NoError(t, writer.Flush())
		txn.UpdateCachedKeys(commitTs)
	}

	// Initial mutation: Set John → Leopard
	newRunMutation(1, 3, []*pb.DirectedEdge{
		{
			Entity:    uidJohn,
			Attr:      attrPerson,
			Value:     []byte("John Smith"),
			ValueType: pb.Posting_STRING,
			Op:        pb.DirectedEdge_SET,
		},
		{
			Entity:    uidRoom,
			Attr:      attrRoom,
			Value:     []byte("Leopard"),
			ValueType: pb.Posting_STRING,
			Op:        pb.DirectedEdge_SET,
		},
		{
			Entity:    uidJohn,
			Attr:      attrOffice,
			ValueId:   uidRoom,
			ValueType: pb.Posting_UID,
			Op:        pb.DirectedEdge_SET,
		},
	})

	key := x.DataKey(attrOffice, uidJohn)
	rollup(t, key, ps, 4)

	// Second mutation: Remove John from Leopard, assign Amanda
	uidAmanda := uint64(3)

	newRunMutation(6, 8, []*pb.DirectedEdge{
		{
			Entity:    uidJohn,
			Attr:      attrOffice,
			ValueId:   uidRoom,
			ValueType: pb.Posting_UID,
			Op:        pb.DirectedEdge_DEL,
		},
		{
			Entity:    uidAmanda,
			Attr:      attrPerson,
			Value:     []byte("Amanda Anderson"),
			ValueType: pb.Posting_STRING,
			Op:        pb.DirectedEdge_SET,
		},
		{
			Entity:    uidAmanda,
			Attr:      attrOffice,
			ValueId:   uidRoom,
			ValueType: pb.Posting_UID,
			Op:        pb.DirectedEdge_SET,
		},
	})

	// Read and validate: Amanda assigned, John unassigned
	txnRead := posting.Oracle().RegisterStartTs(10)

	list, err := txnRead.Get(key)
	require.NoError(t, err)

	uids, err := list.Uids(posting.ListOptions{ReadTs: 10})
	require.NoError(t, err)

	// This assertion FAILS in the broken case where both Amanda and John are assigned
	require.Equal(t, 0, len(uids.Uids), "John should no longer have an office assigned")

	keyRev := x.ReverseKey(attrOffice, uidRoom)
	listRev, err := txnRead.Get(keyRev)
	require.NoError(t, err)

	reverseUids, err := listRev.Uids(posting.ListOptions{ReadTs: 10})
	require.NoError(t, err)

	require.Equal(t, []uint64{uidAmanda}, reverseUids.Uids, "Only Amanda should be assigned on reverse edge")
}

func TestGetScalarList(t *testing.T) {
	dir, err := os.MkdirTemp("", "storetest_")
	x.Check(err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	x.Check(err)
	pstore = ps
	posting.Init(ps, 0, false)
	Init(ps)
	err = schema.ParseBytes([]byte("scalarPredicateCount4: uid ."), 1)
	require.NoError(t, err)

	runM := func(startTs, commitTs uint64, edges []*pb.DirectedEdge) {
		txn := posting.Oracle().RegisterStartTs(startTs)
		for _, edge := range edges {
			x.Check(newRunMutation(context.Background(), edge, txn))
		}
		txn.Update()
		writer := posting.NewTxnWriter(pstore)
		require.NoError(t, txn.CommitToDisk(writer, commitTs))
		require.NoError(t, writer.Flush())
		txn.UpdateCachedKeys(commitTs)
	}

	attr := x.AttrInRootNamespace("scalarPredicateCount4")

	runM(5, 7, []*pb.DirectedEdge{{
		ValueId:   3,
		ValueType: pb.Posting_UID,
		Attr:      attr,
		Entity:    1,
		Op:        pb.DirectedEdge_SET,
	}})

	key := x.DataKey(attr, 1)
	rollup(t, key, ps, 8)

	runM(9, 11, []*pb.DirectedEdge{{
		ValueId:   5,
		ValueType: pb.Posting_UID,
		Attr:      attr,
		Entity:    1,
		Op:        pb.DirectedEdge_SET,
	}})

	txn := posting.Oracle().RegisterStartTs(13)
	l, err := txn.Get(key)
	require.Nil(t, err)
	uids, err := l.Uids(posting.ListOptions{ReadTs: 13})
	require.Nil(t, err)
	require.Equal(t, 1, len(uids.Uids))
}

func TestMultipleTxnListCount(t *testing.T) {
	dir, err := os.MkdirTemp("", "storetest_")
	x.Check(err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	x.Check(err)
	pstore = ps
	posting.Init(ps, 0, false)
	Init(ps)
	err = schema.ParseBytes([]byte("scalarPredicateCount3: [uid] @count ."), 1)
	require.NoError(t, err)

	ctx := context.Background()
	attr := x.AttrInRootNamespace("scalarPredicateCount3")

	runM := func(startTs, commitTs uint64, edges []*pb.DirectedEdge) {
		txn := posting.Oracle().RegisterStartTs(startTs)
		for _, edge := range edges {
			x.Check(newRunMutation(ctx, edge, txn))
		}
		txn.Update()
		writer := posting.NewTxnWriter(pstore)
		require.NoError(t, txn.CommitToDisk(writer, commitTs))
		require.NoError(t, writer.Flush())
		txn.UpdateCachedKeys(commitTs)
	}

	runM(9, 11, []*pb.DirectedEdge{{
		ValueId:   3,
		ValueType: pb.Posting_UID,
		Attr:      attr,
		Entity:    1,
		Op:        pb.DirectedEdge_SET,
	}, {
		ValueId:   2,
		ValueType: pb.Posting_UID,
		Attr:      attr,
		Entity:    1,
		Op:        pb.DirectedEdge_SET,
	}})

	txn := posting.Oracle().RegisterStartTs(13)
	key := x.CountKey(attr, 1, false)
	l, err := txn.Get(key)
	require.Nil(t, err)
	uids, err := l.Uids(posting.ListOptions{ReadTs: 13})
	require.Nil(t, err)
	require.Equal(t, 0, len(uids.Uids))

	key = x.CountKey(attr, 2, false)
	l, err = txn.Get(key)
	require.Nil(t, err)
	uids, err = l.Uids(posting.ListOptions{ReadTs: 13})
	require.Nil(t, err)
	require.Equal(t, 1, len(uids.Uids))
}

func TestScalarPredicateRevCount(t *testing.T) {
	dir, err := os.MkdirTemp("", "storetest_")
	x.Check(err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	x.Check(err)
	pstore = ps
	posting.Init(ps, 0, false)
	Init(ps)
	err = schema.ParseBytes([]byte("scalarPredicateCount2: uid @reverse @count ."), 1)
	require.NoError(t, err)

	ctx := context.Background()
	attr := x.AttrInRootNamespace("scalarPredicateCount2")

	runM := func(startTs, commitTs uint64, edges []*pb.DirectedEdge) {
		txn := posting.Oracle().RegisterStartTs(startTs)
		for _, edge := range edges {
			x.Check(newRunMutation(ctx, edge, txn))
		}
		txn.Update()
		writer := posting.NewTxnWriter(pstore)
		require.NoError(t, txn.CommitToDisk(writer, commitTs))
		require.NoError(t, writer.Flush())
		txn.UpdateCachedKeys(commitTs)
	}

	runM(9, 11, []*pb.DirectedEdge{{
		ValueId:   3,
		ValueType: pb.Posting_UID,
		Attr:      attr,
		Entity:    1,
		Op:        pb.DirectedEdge_SET,
	}, {
		ValueId:   3,
		ValueType: pb.Posting_UID,
		Attr:      attr,
		Entity:    1,
		Op:        pb.DirectedEdge_DEL,
	}})

	txn := posting.Oracle().RegisterStartTs(13)
	key := x.DataKey(attr, 1)
	l, err := txn.Get(key)
	require.Nil(t, err)
	l.RLock()
	require.Equal(t, 0, l.GetLength(13))
	l.RUnlock()

	runM(15, 17, []*pb.DirectedEdge{{
		ValueId:   3,
		ValueType: pb.Posting_UID,
		Attr:      attr,
		Entity:    1,
		Op:        pb.DirectedEdge_SET,
	}})

	txn = posting.Oracle().RegisterStartTs(18)
	l, err = txn.Get(key)
	require.Nil(t, err)
	l.RLock()
	require.Equal(t, 1, l.GetLength(18))
	l.RUnlock()

	runM(18, 19, []*pb.DirectedEdge{{
		ValueId:   3,
		ValueType: pb.Posting_UID,
		Attr:      attr,
		Entity:    1,
		Op:        pb.DirectedEdge_DEL,
	}})

	txn = posting.Oracle().RegisterStartTs(20)
	l, err = txn.Get(key)
	require.Nil(t, err)
	l.RLock()
	require.Equal(t, 0, l.GetLength(20))
	l.RUnlock()
}

func TestScalarPredicateIntCount(t *testing.T) {
	dir, err := os.MkdirTemp("", "storetest_")
	x.Check(err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	x.Check(err)
	pstore = ps
	posting.Init(ps, 0, false)
	Init(ps)
	err = schema.ParseBytes([]byte("scalarPredicateCount1: string @count ."), 1)
	require.NoError(t, err)

	ctx := context.Background()
	attr := x.AttrInRootNamespace("scalarPredicateCount1")

	runM := func(startTs, commitTs uint64, edge *pb.DirectedEdge) {
		txn := posting.Oracle().RegisterStartTs(startTs)
		x.Check(newRunMutation(ctx, edge, txn))
		txn.Update()
		writer := posting.NewTxnWriter(pstore)
		require.NoError(t, txn.CommitToDisk(writer, commitTs))
		require.NoError(t, writer.Flush())
		txn.UpdateCachedKeys(commitTs)
	}

	runM(5, 7, &pb.DirectedEdge{
		Value:     []byte("a"),
		ValueType: pb.Posting_STRING,
		Attr:      attr,
		Entity:    1,
		Op:        pb.DirectedEdge_SET,
	})

	key := x.CountKey(attr, 1, false)
	rollup(t, key, ps, 8)

	runM(9, 11, &pb.DirectedEdge{
		Value:     []byte("a"),
		ValueType: pb.Posting_STRING,
		Attr:      attr,
		Entity:    1,
		Op:        pb.DirectedEdge_DEL,
	})

	txn := posting.Oracle().RegisterStartTs(20)
	l, err := txn.Get(key)
	require.Nil(t, err)
	l.RLock()
	require.Equal(t, 0, l.GetLength(20))
	l.RUnlock()
}

func TestScalarPredicateCount(t *testing.T) {
	dir, err := os.MkdirTemp("", "storetest_")
	x.Check(err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	x.Check(err)
	pstore = ps
	posting.Init(ps, 0, false)
	Init(ps)
	err = schema.ParseBytes([]byte("scalarPredicateCount: uid @count ."), 1)
	require.NoError(t, err)

	ctx := context.Background()
	attr := x.AttrInRootNamespace("scalarPredicateCount")

	runM := func(startTs, commitTs uint64, edge *pb.DirectedEdge) {
		txn := posting.Oracle().RegisterStartTs(startTs)
		x.Check(newRunMutation(ctx, edge, txn))
		txn.Update()
		writer := posting.NewTxnWriter(pstore)
		require.NoError(t, txn.CommitToDisk(writer, commitTs))
		require.NoError(t, writer.Flush())
		txn.UpdateCachedKeys(commitTs)
	}

	runM(5, 7, &pb.DirectedEdge{
		ValueId:   2,
		ValueType: pb.Posting_UID,
		Attr:      attr,
		Entity:    1,
		Op:        pb.DirectedEdge_SET,
	})

	key := x.CountKey(attr, 1, false)
	rollup(t, key, ps, 8)

	runM(9, 11, &pb.DirectedEdge{
		ValueId:   3,
		ValueType: pb.Posting_UID,
		Attr:      attr,
		Entity:    1,
		Op:        pb.DirectedEdge_SET,
	})

	txn := posting.Oracle().RegisterStartTs(15)
	l, err := txn.Get(key)
	require.Nil(t, err)
	l.RLock()
	require.Equal(t, 1, l.GetLength(15))
	l.RUnlock()
}

func TestSingleUidReplacement(t *testing.T) {
	dir, err := os.MkdirTemp("", "storetest_")
	x.Check(err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	x.Check(err)
	pstore = ps
	posting.Init(ps, 0, false)
	Init(ps)
	err = schema.ParseBytes([]byte("singleUidReplaceTest: uid ."), 1)
	require.NoError(t, err)

	ctx := context.Background()
	txn := posting.Oracle().RegisterStartTs(5)
	attr := x.AttrInRootNamespace("singleUidReplaceTest")

	// Txn 1. Set 1 -> 2
	x.Check(newRunMutation(ctx, &pb.DirectedEdge{
		ValueId: 2,
		Attr:    attr,
		Entity:  1,
		Op:      pb.DirectedEdge_SET,
	}, txn))

	txn.Update()
	writer := posting.NewTxnWriter(pstore)
	require.NoError(t, txn.CommitToDisk(writer, 7))
	require.NoError(t, writer.Flush())
	txn.UpdateCachedKeys(7)

	// Txn 2. Set 1 -> 3
	txn = posting.Oracle().RegisterStartTs(9)

	x.Check(newRunMutation(ctx, &pb.DirectedEdge{
		ValueId: 3,
		Attr:    attr,
		Entity:  1,
		Op:      pb.DirectedEdge_SET,
	}, txn))

	txn.Update()
	writer = posting.NewTxnWriter(pstore)
	require.NoError(t, txn.CommitToDisk(writer, 11))
	require.NoError(t, writer.Flush())
	txn.UpdateCachedKeys(11)

	key := x.DataKey(attr, 1)

	// Reading the david index, we should see 2 inserted, 1 deleted
	txn = posting.Oracle().RegisterStartTs(15)
	l, err := txn.Get(key)
	require.NoError(t, err)

	uids, err := l.Uids(posting.ListOptions{ReadTs: 15})
	require.NoError(t, err)
	require.Equal(t, uids.Uids, []uint64{3})
}

func TestSingleString(t *testing.T) {
	dir, err := os.MkdirTemp("", "storetest_")
	x.Check(err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	x.Check(err)
	pstore = ps
	posting.Init(ps, 0, false)
	Init(ps)
	err = schema.ParseBytes([]byte("singleUidTest: string @index(exact) @unique ."), 1)
	require.NoError(t, err)

	ctx := context.Background()
	txn := posting.Oracle().RegisterStartTs(5)
	attr := x.AttrInRootNamespace("singleUidTest")

	// Txn 1. Set 1 -> david 2 -> blush
	x.Check(newRunMutation(ctx, &pb.DirectedEdge{
		Value:  []byte("david"),
		Attr:   attr,
		Entity: 1,
		Op:     pb.DirectedEdge_SET,
	}, txn))

	x.Check(newRunMutation(ctx, &pb.DirectedEdge{
		Value:  []byte("blush"),
		Attr:   attr,
		Entity: 2,
		Op:     pb.DirectedEdge_SET,
	}, txn))

	txn.Update()
	writer := posting.NewTxnWriter(pstore)
	require.NoError(t, txn.CommitToDisk(writer, 7))
	require.NoError(t, writer.Flush())
	txn.UpdateCachedKeys(7)

	// Txn 2. Set 2 -> david 1 -> blush
	txn = posting.Oracle().RegisterStartTs(9)

	x.Check(newRunMutation(ctx, &pb.DirectedEdge{
		Value:  []byte("david"),
		Attr:   attr,
		Entity: 2,
		Op:     pb.DirectedEdge_SET,
	}, txn))

	x.Check(newRunMutation(ctx, &pb.DirectedEdge{
		Value:  []byte("blush"),
		Attr:   attr,
		Entity: 1,
		Op:     pb.DirectedEdge_SET,
	}, txn))

	txn.Update()
	writer = posting.NewTxnWriter(pstore)
	require.NoError(t, txn.CommitToDisk(writer, 11))
	require.NoError(t, writer.Flush())
	txn.UpdateCachedKeys(11)

	key := x.IndexKey(attr, string([]byte{2, 100, 97, 118, 105, 100}))

	// Reading the david index, we should see 2 inserted, 1 deleted
	txn = posting.Oracle().RegisterStartTs(15)
	l, err := txn.Get(key)
	require.NoError(t, err)

	found, mpost, err := l.FindPosting(15, 2)
	require.NoError(t, err)
	require.Equal(t, found, true)
	require.Equal(t, mpost.Op, uint32(0x1))

	found, _, err = l.FindPosting(15, 1)
	require.NoError(t, err)
	require.Equal(t, found, false)

	rollup(t, key, pstore, 16)

	txn = posting.Oracle().RegisterStartTs(18)
	l, err = txn.Get(key)
	require.NoError(t, err)

	found, mpost, err = l.FindPosting(18, 2)
	require.NoError(t, err)
	require.Equal(t, found, true)
	require.Equal(t, mpost.Op, uint32(0x0))

	found, _, err = l.FindPosting(18, 1)
	require.NoError(t, err)
	require.Equal(t, found, false)
}

func TestLangExact(t *testing.T) {
	dir, err := os.MkdirTemp("", "storetest_")
	x.Check(err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	x.Check(err)
	pstore = ps
	// Not using posting list cache
	posting.Init(ps, 0, false)
	Init(ps)
	err = schema.ParseBytes([]byte("testLang: string @index(term) @lang ."), 1)
	require.NoError(t, err)

	ctx := context.Background()
	txn := posting.Oracle().RegisterStartTs(5)
	attr := x.AttrInRootNamespace("testLang")

	edge := &pb.DirectedEdge{
		Value:  []byte("english"),
		Attr:   attr,
		Entity: 1,
		Op:     pb.DirectedEdge_SET,
		Lang:   "en",
	}

	x.Check(newRunMutation(ctx, edge, txn))

	edge = &pb.DirectedEdge{
		Value:  []byte("hindi"),
		Attr:   attr,
		Entity: 1,
		Op:     pb.DirectedEdge_SET,
		Lang:   "hi",
	}

	x.Check(newRunMutation(ctx, edge, txn))

	txn.Update()
	writer := posting.NewTxnWriter(pstore)
	require.NoError(t, txn.CommitToDisk(writer, 2))
	require.NoError(t, writer.Flush())
	txn.UpdateCachedKeys(2)

	key := x.DataKey(attr, 1)
	rollup(t, key, pstore, 4)

	pl, err := readPostingListFromDisk(key, pstore, 6)
	require.NoError(t, err)

	val, err := pl.ValueForTag(6, "hi")
	require.NoError(t, err)
	require.Equal(t, val.Value, []byte("hindi"))
}

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func randStringBytes(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}

func BenchmarkAddMutationWithIndex(b *testing.B) {
	dir, err := os.MkdirTemp("", "storetest_")
	x.Check(err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	x.Check(err)
	pstore = ps
	// Not using posting list cache
	posting.Init(ps, 0, false)
	Init(ps)
	err = schema.ParseBytes([]byte("benchmarkadd: string @index(term) ."), 1)
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	txn := posting.Oracle().RegisterStartTs(5)
	attr := x.AttrInRootNamespace("benchmarkadd")

	n := uint64(1000)
	values := make([]string, 0)
	for range n {
		values = append(values, randStringBytes(5))
	}

	for i := 0; i < b.N; i++ {
		edge := &pb.DirectedEdge{
			Value:  []byte(values[rand.Intn(int(n))]),
			Attr:   attr,
			Entity: rand.Uint64()%n + 1,
			Op:     pb.DirectedEdge_SET,
		}

		x.Check(newRunMutation(ctx, edge, txn))
	}
}

func TestRemoveDuplicates(t *testing.T) {
	toSet := func(uids []uint64) map[uint64]struct{} {
		m := make(map[uint64]struct{})
		for _, uid := range uids {
			m[uid] = struct{}{}
		}
		return m
	}

	for _, test := range []struct {
		setIn   []uint64
		setOut  []uint64
		uidsIn  []uint64
		uidsOut []uint64
	}{
		{setIn: nil, setOut: nil, uidsIn: nil, uidsOut: nil},
		{setIn: nil, setOut: []uint64{2}, uidsIn: []uint64{2}, uidsOut: []uint64{2}},
		{setIn: []uint64{2}, setOut: []uint64{2}, uidsIn: []uint64{2}, uidsOut: []uint64{}},
		{setIn: []uint64{2}, setOut: []uint64{2}, uidsIn: []uint64{2, 2}, uidsOut: []uint64{}},
		{
			setIn:   []uint64{2, 3},
			setOut:  []uint64{2, 3, 4, 5},
			uidsIn:  []uint64{3, 4, 5},
			uidsOut: []uint64{4, 5},
		},
	} {
		set := toSet(test.setIn)
		uidsOut := removeDuplicates(test.uidsIn, set)
		require.Equal(t, uidsOut, test.uidsOut)
		require.Equal(t, set, toSet(test.setOut))
	}
}
