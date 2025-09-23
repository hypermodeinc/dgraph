/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package posting

import (
	"bytes"
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/badger/v4"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func uids(l *List, readTs uint64) []uint64 {
	r, err := l.Uids(ListOptions{ReadTs: readTs})
	x.Check(err)
	return r.Uids
}

// indexTokensForTest is just a wrapper around indexTokens used for convenience.
func indexTokensForTest(attr, lang string, val types.Val) ([]string, error) {
	return indexTokens(context.Background(), &indexMutationInfo{
		tokenizers: schema.State().Tokenizer(context.Background(), x.AttrInRootNamespace(attr)),
		edge: &pb.DirectedEdge{
			Attr: x.AttrInRootNamespace(attr),
			Lang: lang,
		},
		val: val,
	})
}

func TestIndexingInt(t *testing.T) {
	require.NoError(t, schema.ParseBytes([]byte("age:int @index(int) ."), 1))
	a, err := indexTokensForTest("age", "", types.Val{Tid: types.StringID, Value: []byte("10")})
	require.NoError(t, err)
	require.EqualValues(t, []byte{0x6, 0x1, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0xa}, []byte(a[0]))
}

func TestIndexingIntNegative(t *testing.T) {
	require.NoError(t, schema.ParseBytes([]byte("age:int @index(int) ."), 1))
	a, err := indexTokensForTest("age", "", types.Val{Tid: types.StringID, Value: []byte("-10")})
	require.NoError(t, err)
	require.EqualValues(t, []byte{0x6, 0x0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xf6},
		[]byte(a[0]))
}

func TestIndexingFloat(t *testing.T) {
	require.NoError(t, schema.ParseBytes([]byte("age:float @index(float) ."), 1))
	a, err := indexTokensForTest("age", "", types.Val{Tid: types.StringID, Value: []byte("10.43")})
	require.NoError(t, err)
	require.EqualValues(t, []byte{0x7, 0x1, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0xa}, []byte(a[0]))
}

func TestIndexingTime(t *testing.T) {
	require.NoError(t, schema.ParseBytes([]byte("age:dateTime @index(year) ."), 1))
	a, err := indexTokensForTest("age", "", types.Val{Tid: types.StringID,
		Value: []byte("0010-01-01T01:01:01.000000001")})
	require.NoError(t, err)
	require.EqualValues(t, []byte{0x4, 0x0, 0xa}, []byte(a[0]))
}

func TestIndexing(t *testing.T) {
	require.NoError(t, schema.ParseBytes([]byte("name:string @index(term) ."), 1))
	a, err := indexTokensForTest("name", "", types.Val{Tid: types.StringID, Value: []byte("abc")})
	require.NoError(t, err)
	require.EqualValues(t, "\x01abc", a[0])
}

func TestIndexingMultiLang(t *testing.T) {
	require.NoError(t, schema.ParseBytes([]byte("name:string @index(fulltext) ."), 1))

	// ensure that default tokenizer is suitable for English
	a, err := indexTokensForTest("name", "", types.Val{Tid: types.StringID,
		Value: []byte("stemming")})
	require.NoError(t, err)
	require.EqualValues(t, "\x08stem", a[0])

	// ensure that Finnish tokenizer is used
	a, err = indexTokensForTest("name", "fi", types.Val{Tid: types.StringID,
		Value: []byte("edeltäneessä")})
	require.NoError(t, err)
	require.EqualValues(t, "\x08edeltän", a[0])

	// ensure that German tokenizer is used
	a, err = indexTokensForTest("name", "de", types.Val{Tid: types.StringID,
		Value: []byte("Auffassungsvermögen")})
	require.NoError(t, err)
	require.EqualValues(t, "\x08auffassungsvermog", a[0])

	// ensure that default tokenizer works differently than German
	a, err = indexTokensForTest("name", "", types.Val{Tid: types.StringID,
		Value: []byte("Auffassungsvermögen")})
	require.NoError(t, err)
	require.EqualValues(t, "\x08auffassungsvermögen", a[0])
}

func TestIndexingInvalidLang(t *testing.T) {
	require.NoError(t, schema.ParseBytes([]byte("name:string @index(fulltext) ."), 1))

	// tokenizer for "xx" language won't return an error.
	_, err := indexTokensForTest("name", "xx", types.Val{Tid: types.StringID,
		Value: []byte("error")})
	require.NoError(t, err)
}

func TestIndexingAliasedLang(t *testing.T) {
	require.NoError(t, schema.ParseBytes([]byte("name:string @index(fulltext) @lang ."), 1))
	_, err := indexTokensForTest("name", "es", types.Val{Tid: types.StringID,
		Value: []byte("base")})
	require.NoError(t, err)
	// es-es and es-419 are aliased to es
	_, err = indexTokensForTest("name", "es-es", types.Val{Tid: types.StringID,
		Value: []byte("alias")})
	require.NoError(t, err)
	_, err = indexTokensForTest("name", "es-419", types.Val{Tid: types.StringID,
		Value: []byte("alias")})
	require.NoError(t, err)
}

func addMutation(t *testing.T, l *List, edge *pb.DirectedEdge, op uint32,
	startTs uint64, commitTs uint64, index bool) {
	switch op {
	case Del:
		edge.Op = pb.DirectedEdge_DEL
	case Set:
		edge.Op = pb.DirectedEdge_SET
	default:
		x.Fatalf("Unhandled op: %v", op)
	}
	txn := Oracle().RegisterStartTs(startTs)
	txn.cache.SetIfAbsent(string(l.key), l)

	mp := NewMutationPipeline(txn)
	err := mp.Process(context.Background(), []*pb.DirectedEdge{edge})
	require.NoError(t, err)
	txn.Update()
	txn.UpdateCachedKeys(commitTs)
	writer := NewTxnWriter(pstore)
	require.NoError(t, txn.CommitToDisk(writer, commitTs))
	require.NoError(t, writer.Flush())
	newTxn := Oracle().RegisterStartTs(commitTs + 1)
	l1, err := newTxn.Get(l.key)
	require.NoError(t, err)
	*l = *l1 //nolint
}

const schemaPreVal = `
	name: string .
	name2: string .
	dob: dateTime .
	friend: [uid] .
`

const schemaVal = `
name: string @index(term) .
name2: string @index(term) .
dob: dateTime @index(year) .
friend: [uid] @reverse .
	`

const mutatedSchemaVal = `
name:string @index(term) .
name2:string .
dob:dateTime @index(year) .
friend:[uid] @reverse .
	`

// TODO(Txn): We can't read index key on disk if it was written in same txn.
func TestTokensTable(t *testing.T) {
	require.NoError(t, schema.ParseBytes([]byte(schemaVal), 1))

	attr := x.AttrInRootNamespace("name")
	key := x.DataKey(attr, 1)
	l, err := getNew(key, ps, math.MaxUint64, false)
	l.mutationMap.readTs = 1
	require.NoError(t, err)

	edge := &pb.DirectedEdge{
		Value:  []byte("david"),
		Attr:   attr,
		Entity: 157,
	}
	addMutation(t, l, edge, Set, 1, 2, true)

	key = x.IndexKey(attr, "\x01david")
	time.Sleep(10 * time.Millisecond)

	txn := ps.NewTransactionAt(3, false)
	_, err = txn.Get(key)
	require.NoError(t, err)

	require.EqualValues(t, []string{"\x01david"}, tokensForTest(attr))
}

// tokensForTest returns keys for a table. This is just for testing / debugging.
func tokensForTest(attr string) []string {
	pk := x.ParsedKey{Attr: attr}
	prefix := pk.IndexPrefix()
	txn := pstore.NewTransactionAt(math.MaxUint64, false)
	defer txn.Discard()
	it := txn.NewIterator(badger.DefaultIteratorOptions)
	defer it.Close()

	var out []string
	for it.Seek(prefix); it.Valid(); it.Next() {
		key := it.Item().Key()
		if !bytes.HasPrefix(key, prefix) {
			break
		}
		k, err := x.Parse(key)
		x.Check(err)
		x.AssertTrue(k.IsIndex())
		out = append(out, k.Term)
	}
	return out
}

// addEdgeToValue adds edge without indexing.
func addEdgeToValue(t *testing.T, attr string, src uint64,
	value string, startTs, commitTs uint64) {
	edge := &pb.DirectedEdge{
		Value:  []byte(value),
		Attr:   attr,
		Entity: src,
		Op:     pb.DirectedEdge_SET,
	}
	l, err := GetNoStore(x.DataKey(attr, src), startTs)
	require.NoError(t, err)
	// No index entries added here as we do not call AddMutationWithIndex.
	addMutation(t, l, edge, Set, startTs, commitTs, false)
}

func addDelEdgeToUID(t *testing.T, attr string, src uint64,
	dst uint64, startTs, commitTs uint64) {
	edge := &pb.DirectedEdge{
		ValueId: dst,
		Attr:    attr,
		Entity:  src,
		Op:      pb.DirectedEdge_DEL,
	}
	l, err := GetNoStore(x.DataKey(attr, src), startTs)
	require.NoError(t, err)
	// No index entries added here as we do not call AddMutationWithIndex.
	addMutation(t, l, edge, Del, startTs, commitTs, false)
}

// addEdgeToUID adds uid edge with reverse edge
func addEdgeToUID(t *testing.T, attr string, src uint64,
	dst uint64, startTs, commitTs uint64) {
	edge := &pb.DirectedEdge{
		ValueId: dst,
		Attr:    attr,
		Entity:  src,
		Op:      pb.DirectedEdge_SET,
	}
	l, err := GetNoStore(x.DataKey(attr, src), startTs)
	require.NoError(t, err)
	// No index entries added here as we do not call AddMutationWithIndex.
	addMutation(t, l, edge, Set, startTs, commitTs, false)
}

func TestCountReverseIndexWithData(t *testing.T) {
	require.NoError(t, pstore.DropAll())
	MemLayerInstance.clear()
	preIndex := "testcount: [uid] ."
	require.NoError(t, schema.ParseBytes([]byte(preIndex), 1))

	indexNameCountVal := "testcount: [uid] @count @reverse ."

	attr := x.AttrInRootNamespace("testcount")
	addEdgeToUID(t, attr, 1, 23, uint64(8), uint64(9))
	addEdgeToUID(t, attr, 1, 23, uint64(10), uint64(11))
	l, err := GetNoStore(x.DataKey(attr, 1), 12)
	require.NoError(t, err)
	l.RLock()
	require.Equal(t, l.GetLength(12), 2)
	l.RUnlock()

	require.NoError(t, schema.ParseBytes([]byte(indexNameCountVal), 1))
	currentSchema, _ := schema.State().Get(context.Background(), attr)
	rb := IndexRebuild{
		Attr:          attr,
		StartTs:       12,
		OldSchema:     nil,
		CurrentSchema: &currentSchema,
	}
	rebuildInfo := rb.needsTokIndexRebuild()
	prefixes, err := rebuildInfo.prefixesForTokIndexes()
	require.NoError(t, err)
	require.NoError(t, pstore.DropPrefix(prefixes...))
	require.NoError(t, rebuildCountIndex(context.Background(), &rb))

	l, err = GetNoStore(x.DataKey(attr, 1), 14)
	require.NoError(t, err)
	l.RLock()
	require.Equal(t, l.GetLength(14), 1)
	l.RUnlock()
}

func TestCountReverseIndexEmptyPosting(t *testing.T) {
	require.NoError(t, pstore.DropAll())
	MemLayerInstance.clear()
	preIndex := "testcount: [uid] ."
	require.NoError(t, schema.ParseBytes([]byte(preIndex), 1))

	indexNameCountVal := "testcount: [uid] @count @reverse ."

	attr := x.AttrInRootNamespace("testcount")
	addDelEdgeToUID(t, attr, 1, 23, uint64(10), uint64(11))
	l, err := GetNoStore(x.DataKey(attr, 1), 12)
	require.NoError(t, err)
	l.RLock()
	require.Equal(t, l.GetLength(12), -1)
	l.RUnlock()

	require.NoError(t, schema.ParseBytes([]byte(indexNameCountVal), 1))
	currentSchema, _ := schema.State().Get(context.Background(), attr)
	rb := IndexRebuild{
		Attr:          attr,
		StartTs:       12,
		OldSchema:     nil,
		CurrentSchema: &currentSchema,
	}
	rebuildInfo := rb.needsTokIndexRebuild()
	prefixes, err := rebuildInfo.prefixesForTokIndexes()
	require.NoError(t, err)
	require.NoError(t, pstore.DropPrefix(prefixes...))
	require.NoError(t, rebuildCountIndex(context.Background(), &rb))

	l, err = GetNoStore(x.DataKey(attr, 1), 14)
	require.NoError(t, err)
	l.RLock()
	require.Equal(t, l.GetLength(14), 0)
	l.RUnlock()
}

func TestRebuildTokIndex(t *testing.T) {
	require.NoError(t, schema.ParseBytes([]byte(schemaPreVal), 1))

	addEdgeToValue(t, x.AttrInRootNamespace("name2"), 91, "Michonne", uint64(1), uint64(2))
	addEdgeToValue(t, x.AttrInRootNamespace("name2"), 92, "David", uint64(3), uint64(4))

	require.NoError(t, schema.ParseBytes([]byte(schemaVal), 1))
	currentSchema, _ := schema.State().Get(context.Background(), x.AttrInRootNamespace("name2"))
	rb := IndexRebuild{
		Attr:          x.AttrInRootNamespace("name2"),
		StartTs:       5,
		OldSchema:     nil,
		CurrentSchema: &currentSchema,
	}
	rebuildInfo := rb.needsTokIndexRebuild()
	prefixes, err := rebuildInfo.prefixesForTokIndexes()
	require.NoError(t, err)
	require.NoError(t, pstore.DropPrefix(prefixes...))
	require.NoError(t, rebuildTokIndex(context.Background(), &rb))

	// Check index entries in data store.
	txn := ps.NewTransactionAt(6, false)
	defer txn.Discard()
	it := txn.NewIterator(badger.DefaultIteratorOptions)
	defer it.Close()
	pk := x.ParsedKey{Attr: x.AttrInRootNamespace("name2")}
	prefix := pk.IndexPrefix()
	var idxKeys []string
	var idxVals []*List
	for it.Seek(prefix); it.Valid(); it.Next() {
		item := it.Item()
		key := item.Key()
		if !bytes.HasPrefix(key, prefix) {
			break
		}
		if item.UserMeta()&BitEmptyPosting == BitEmptyPosting {
			continue
		}
		idxKeys = append(idxKeys, string(key))
		l, err := GetNoStore(key, 6)
		require.NoError(t, err)
		idxVals = append(idxVals, l)
	}
	require.Len(t, idxKeys, 2)
	require.Len(t, idxVals, 2)
	require.EqualValues(t, idxKeys[0], x.IndexKey(x.AttrInRootNamespace("name2"), "\x01david"))
	require.EqualValues(t, idxKeys[1], x.IndexKey(x.AttrInRootNamespace("name2"), "\x01michonne"))

	uids1 := uids(idxVals[0], 6)
	uids2 := uids(idxVals[1], 6)
	require.Len(t, uids1, 1)
	require.Len(t, uids2, 1)
	require.EqualValues(t, 92, uids1[0])
	require.EqualValues(t, 91, uids2[0])
}

func TestRebuildTokIndexWithDeletion(t *testing.T) {
	addEdgeToValue(t, x.AttrInRootNamespace("name2"), 91, "Michonne", uint64(1), uint64(2))
	addEdgeToValue(t, x.AttrInRootNamespace("name2"), 92, "David", uint64(3), uint64(4))

	require.NoError(t, schema.ParseBytes([]byte(schemaVal), 1))
	currentSchema, _ := schema.State().Get(context.Background(), x.AttrInRootNamespace("name2"))
	rb := IndexRebuild{
		Attr:          x.AttrInRootNamespace("name2"),
		StartTs:       5,
		OldSchema:     nil,
		CurrentSchema: &currentSchema,
	}
	rebuildInfo := rb.needsTokIndexRebuild()
	prefixes, err := rebuildInfo.prefixesForTokIndexes()
	require.NoError(t, err)
	require.NoError(t, pstore.DropPrefix(prefixes...))
	require.NoError(t, rebuildTokIndex(context.Background(), &rb))

	// Mutate the schema (the index in name2 is deleted) and rebuild the index.
	require.NoError(t, schema.ParseBytes([]byte(mutatedSchemaVal), 1))
	newSchema, _ := schema.State().Get(context.Background(), x.AttrInRootNamespace("name2"))
	rb = IndexRebuild{
		Attr:          x.AttrInRootNamespace("name2"),
		StartTs:       6,
		OldSchema:     &currentSchema,
		CurrentSchema: &newSchema,
	}
	rebuildInfo = rb.needsTokIndexRebuild()
	prefixes, err = rebuildInfo.prefixesForTokIndexes()
	require.NoError(t, err)
	require.NoError(t, pstore.DropPrefix(prefixes...))
	require.NoError(t, rebuildTokIndex(context.Background(), &rb))

	// Check index entries in data store.
	txn := ps.NewTransactionAt(7, false)
	defer txn.Discard()
	it := txn.NewIterator(badger.DefaultIteratorOptions)
	defer it.Close()
	pk := x.ParsedKey{Attr: x.AttrInRootNamespace("name2")}
	prefix := pk.IndexPrefix()
	var idxKeys []string
	var idxVals []*List
	for it.Seek(prefix); it.Valid(); it.Next() {
		item := it.Item()
		key := item.Key()
		if !bytes.HasPrefix(key, prefix) {
			break
		}
		if item.UserMeta()&BitEmptyPosting == BitEmptyPosting {
			continue
		}
		idxKeys = append(idxKeys, string(key))
		l, err := GetNoStore(key, 7)
		require.NoError(t, err)
		idxVals = append(idxVals, l)
	}

	// The index keys should not be available anymore.
	require.Len(t, idxKeys, 0)
	require.Len(t, idxVals, 0)
}

func TestRebuildReverseEdges(t *testing.T) {
	friendAttr := x.AttrInRootNamespace("friend")
	addEdgeToUID(t, friendAttr, 1, 23, uint64(10), uint64(11))
	addEdgeToUID(t, friendAttr, 1, 24, uint64(12), uint64(13))
	addEdgeToUID(t, friendAttr, 2, 23, uint64(14), uint64(15))

	require.NoError(t, schema.ParseBytes([]byte(schemaVal), 1))
	currentSchema, _ := schema.State().Get(context.Background(), friendAttr)
	rb := IndexRebuild{
		Attr:          friendAttr,
		StartTs:       16,
		OldSchema:     nil,
		CurrentSchema: &currentSchema,
	}
	// TODO: Remove after fixing sync marks.
	require.NoError(t, rebuildReverseEdges(context.Background(), &rb))

	// Check index entries in data store.
	txn := ps.NewTransactionAt(17, false)
	defer txn.Discard()
	iterOpts := badger.DefaultIteratorOptions
	iterOpts.AllVersions = true
	it := txn.NewIterator(iterOpts)
	defer it.Close()
	pk := x.ParsedKey{Attr: friendAttr}
	prefix := pk.ReversePrefix()
	var revKeys []string
	var revVals []*List
	var prevKey []byte
	it.Seek(prefix)
	for it.ValidForPrefix(prefix) {
		item := it.Item()
		key := item.Key()
		if bytes.Equal(key, prevKey) {
			it.Next()
			continue
		}
		prevKey = append(prevKey[:0], key...)
		revKeys = append(revKeys, string(key))
		l, err := ReadPostingList(key, it)
		require.NoError(t, err)
		revVals = append(revVals, l)
	}
	require.Len(t, revKeys, 2)
	require.Len(t, revVals, 2)

	uids0 := uids(revVals[0], 17)
	uids1 := uids(revVals[1], 17)
	require.Len(t, uids0, 2)
	require.Len(t, uids1, 1)
	require.EqualValues(t, 1, uids0[0])
	require.EqualValues(t, 2, uids0[1])
	require.EqualValues(t, 1, uids1[0])
}

func TestNeedsTokIndexRebuild(t *testing.T) {
	rb := IndexRebuild{}
	rb.OldSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID}
	rb.CurrentSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID}
	rebuildInfo := rb.needsTokIndexRebuild()
	require.Equal(t, indexNoop, rebuildInfo.op)
	require.Equal(t, []string(nil), rebuildInfo.tokenizersToDelete)
	require.Equal(t, []string(nil), rebuildInfo.tokenizersToRebuild)

	rb.OldSchema = nil
	rebuildInfo = rb.needsTokIndexRebuild()
	require.Equal(t, indexNoop, rebuildInfo.op)
	require.Equal(t, []string(nil), rebuildInfo.tokenizersToDelete)
	require.Equal(t, []string(nil), rebuildInfo.tokenizersToRebuild)

	rb.OldSchema = &pb.SchemaUpdate{ValueType: pb.Posting_STRING, Directive: pb.SchemaUpdate_INDEX,
		Tokenizer: []string{"exact"}}
	rb.CurrentSchema = &pb.SchemaUpdate{ValueType: pb.Posting_STRING,
		Directive: pb.SchemaUpdate_INDEX,
		Tokenizer: []string{"exact"}}
	rebuildInfo = rb.needsTokIndexRebuild()
	require.Equal(t, indexNoop, rebuildInfo.op)
	require.Equal(t, []string(nil), rebuildInfo.tokenizersToDelete)
	require.Equal(t, []string(nil), rebuildInfo.tokenizersToRebuild)

	rb.OldSchema = &pb.SchemaUpdate{ValueType: pb.Posting_STRING, Directive: pb.SchemaUpdate_INDEX,
		Tokenizer: []string{"term"}}
	rb.CurrentSchema = &pb.SchemaUpdate{ValueType: pb.Posting_STRING,
		Directive: pb.SchemaUpdate_INDEX}
	rebuildInfo = rb.needsTokIndexRebuild()
	require.Equal(t, indexOp(indexRebuild), rebuildInfo.op)
	require.Equal(t, []string{"term"}, rebuildInfo.tokenizersToDelete)
	require.Equal(t, []string(nil), rebuildInfo.tokenizersToRebuild)

	rb.OldSchema = &pb.SchemaUpdate{ValueType: pb.Posting_STRING, Directive: pb.SchemaUpdate_INDEX,
		Tokenizer: []string{"exact"}}
	rb.CurrentSchema = &pb.SchemaUpdate{ValueType: pb.Posting_FLOAT,
		Directive: pb.SchemaUpdate_INDEX,
		Tokenizer: []string{"exact"}}
	rebuildInfo = rb.needsTokIndexRebuild()
	require.Equal(t, indexOp(indexRebuild), rebuildInfo.op)
	require.Equal(t, []string{"exact"}, rebuildInfo.tokenizersToDelete)
	require.Equal(t, []string{"exact"}, rebuildInfo.tokenizersToRebuild)

	rb.OldSchema = &pb.SchemaUpdate{ValueType: pb.Posting_STRING, Directive: pb.SchemaUpdate_INDEX,
		Tokenizer: []string{"exact"}}
	rb.CurrentSchema = &pb.SchemaUpdate{ValueType: pb.Posting_FLOAT,
		Directive: pb.SchemaUpdate_NONE}
	rebuildInfo = rb.needsTokIndexRebuild()
	require.Equal(t, indexOp(indexDelete), rebuildInfo.op)
	require.Equal(t, []string{"exact"}, rebuildInfo.tokenizersToDelete)
	require.Equal(t, []string(nil), rebuildInfo.tokenizersToRebuild)
}

func TestNeedsCountIndexRebuild(t *testing.T) {
	rb := IndexRebuild{}
	rb.OldSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID}
	rb.CurrentSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID, Count: true}
	require.Equal(t, indexOp(indexRebuild), rb.needsCountIndexRebuild())

	rb.OldSchema = nil
	require.Equal(t, indexOp(indexRebuild), rb.needsCountIndexRebuild())

	rb.OldSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID, Count: false}
	rb.CurrentSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID, Count: false}
	require.Equal(t, indexNoop, rb.needsCountIndexRebuild())

	rb.OldSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID, Count: true}
	rb.CurrentSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID, Count: false}
	require.Equal(t, indexOp(indexDelete), rb.needsCountIndexRebuild())
}

func TestNeedsReverseEdgesRebuild(t *testing.T) {
	rb := IndexRebuild{}
	rb.OldSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID, Directive: pb.SchemaUpdate_INDEX}
	rb.CurrentSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID,
		Directive: pb.SchemaUpdate_REVERSE}
	require.Equal(t, indexOp(indexRebuild), rb.needsReverseEdgesRebuild())

	rb.OldSchema = nil
	require.Equal(t, indexOp(indexRebuild), rb.needsReverseEdgesRebuild())

	rb.OldSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID, Directive: pb.SchemaUpdate_REVERSE}
	rb.CurrentSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID,
		Directive: pb.SchemaUpdate_REVERSE}
	require.Equal(t, indexNoop, rb.needsReverseEdgesRebuild())

	rb.CurrentSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID,
		Directive: pb.SchemaUpdate_REVERSE}
	rb.CurrentSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID,
		Directive: pb.SchemaUpdate_INDEX}
	require.Equal(t, indexOp(indexDelete), rb.needsReverseEdgesRebuild())
}

func TestNeedsListTypeRebuild(t *testing.T) {
	rb := IndexRebuild{}
	rb.OldSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID, List: false}
	rb.CurrentSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID, List: true}
	rebuild, err := rb.needsListTypeRebuild()
	require.True(t, rebuild)
	require.NoError(t, err)

	rb.OldSchema = nil
	rebuild, err = rb.needsListTypeRebuild()
	require.False(t, rebuild)
	require.NoError(t, err)

	rb.OldSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID, List: true}
	rb.CurrentSchema = &pb.SchemaUpdate{ValueType: pb.Posting_UID, List: false,
		Predicate: x.AttrInRootNamespace("")} // This is added to prevent a crash in rebuilder.
	// We don't expect rebuilder to have predicates without namespace.
	rebuild, err = rb.needsListTypeRebuild()
	require.False(t, rebuild)
	require.Error(t, err)
}
