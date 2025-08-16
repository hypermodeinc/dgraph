//go:build integration

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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/posting"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var ts uint64

func commitTs(startTs uint64) uint64 {
	commit := timestamp()
	od := &pb.OracleDelta{
		MaxAssigned: atomic.LoadUint64(&ts),
	}
	od.Txns = append(od.Txns, &pb.TxnStatus{StartTs: startTs, CommitTs: commit})
	posting.Oracle().ProcessDelta(od)
	return commit
}

func commitTransaction(t *testing.T, edge *pb.DirectedEdge, l *posting.List) {
	startTs := timestamp()
	txn := posting.Oracle().RegisterStartTs(startTs)
	l = txn.Store(l)
	l.SetTs(startTs)
	require.NoError(t, l.AddMutationWithIndex(context.Background(), edge, txn))

	commit := commitTs(startTs)

	txn.Update()
	writer := posting.NewTxnWriter(pstore)
	require.NoError(t, txn.CommitToDisk(writer, commit))
	require.NoError(t, writer.Flush())
}

func timestamp() uint64 {
	return atomic.AddUint64(&ts, 1)
}

func addEdge(t *testing.T, edge *pb.DirectedEdge, l *posting.List) {
	edge.Op = pb.DirectedEdge_SET
	commitTransaction(t, edge, l)
}

func delEdge(t *testing.T, edge *pb.DirectedEdge, l *posting.List) {
	edge.Op = pb.DirectedEdge_DEL
	commitTransaction(t, edge, l)
}

func setClusterEdge(t *testing.T, dg *dgo.Dgraph, rdf string) {
	mu := &api.Mutation{SetNquads: []byte(rdf), CommitNow: true}
	err := testutil.RetryMutation(dg, mu)
	require.NoError(t, err)
}

func delClusterEdge(t *testing.T, dg *dgo.Dgraph, rdf string) {
	mu := &api.Mutation{DelNquads: []byte(rdf), CommitNow: true}
	require.NoError(t, testutil.RetryMutation(dg, mu))
}
func getOrCreate(key []byte) *posting.List {
	l, err := posting.GetNoStore(key, math.MaxUint64)
	x.Checkf(err, "While calling posting.Get")
	return l
}

func populateGraph(t *testing.T) {
	// Add uid edges : predicate neightbour.
	neighbour := x.AttrInRootNamespace("neighbour")
	edge := &pb.DirectedEdge{
		ValueId: 23,
		Attr:    neighbour,
	}
	edge.Entity = 10
	addEdge(t, edge, getOrCreate(x.DataKey(neighbour, 10)))

	edge.Entity = 11
	addEdge(t, edge, getOrCreate(x.DataKey(neighbour, 11)))

	edge.Entity = 12
	addEdge(t, edge, getOrCreate(x.DataKey(neighbour, 12)))

	edge.ValueId = 25
	addEdge(t, edge, getOrCreate(x.DataKey(neighbour, 12)))

	edge.ValueId = 26
	addEdge(t, edge, getOrCreate(x.DataKey(neighbour, 12)))

	edge.Entity = 10
	edge.ValueId = 31
	addEdge(t, edge, getOrCreate(x.DataKey(neighbour, 10)))

	edge.Entity = 12
	addEdge(t, edge, getOrCreate(x.DataKey(neighbour, 12)))

	// add value edges: friend : with name
	friend := x.AttrInRootNamespace("friend")
	edge.Attr = neighbour
	edge.Entity = 12
	edge.Value = []byte("photon")
	edge.ValueId = 0
	addEdge(t, edge, getOrCreate(x.DataKey(friend, 12)))

	edge.Entity = 10
	addEdge(t, edge, getOrCreate(x.DataKey(friend, 10)))
}

func populateClusterGraph(t *testing.T, dg *dgo.Dgraph) {
	data1 := [][]int{{10, 23}, {11, 23}, {12, 23}, {12, 25}, {12, 26}, {10, 31}, {12, 31}}
	for _, pair := range data1 {
		rdf := fmt.Sprintf(`<%#x> <neighbour> <%#x> .`, pair[0], pair[1])
		setClusterEdge(t, dg, rdf)
	}

	data2 := map[int]string{12: "photon", 10: "photon"}
	for key, val := range data2 {
		rdf := fmt.Sprintf(`<%#x> <friend> %q .`, key, val)
		setClusterEdge(t, dg, rdf)
	}
}

func initTest(t *testing.T, schemaStr string) {
	err := schema.ParseBytes([]byte(schemaStr), 1)
	require.NoError(t, err)
	populateGraph(t)
}

func initClusterTest(t *testing.T, schemaStr string) *dgo.Dgraph {
	dg, err := testutil.DgraphClient(testutil.GetSockAddr())
	if err != nil {
		t.Fatalf("Error while getting a dgraph client: %v", err)
	}
	testutil.DropAll(t, dg)

	require.NoError(t, dg.Alter(context.Background(), &api.Operation{Schema: schemaStr}))
	populateClusterGraph(t, dg)

	return dg
}

func TestVectorSchema(t *testing.T) {
	dg := initClusterTest(t, `
	neighbour: [uid] .
	vectortest: float32vector @index(hnsw(metric: "euclidean")) .`)

	resp, err := testutil.RetryQuery(dg, "schema {}")
	require.NoError(t, err)

	x.AssertTrue(strings.Contains(string(resp.Json), `{"predicate":"vectortest","type":"float32vector","tokenizer":["hnsw(\"metric\":\"euclidean\")"],"index_specs":[{"name":"hnsw","options":[{"key":"metric","value":"euclidean"}]}]}]`))
}

func TestProcessTask(t *testing.T) {
	dg := initClusterTest(t, `neighbour: [uid] .`)

	resp, err := runQuery(dg, "neighbour", []uint64{10, 11, 12}, nil)
	require.NoError(t, err)
	require.JSONEq(t, `{
		  "q": [
		    {
		      "neighbour": [
		        { "uid": "0x17" },
		        { "uid": "0x1f" }
		      ]
		    },
		    {
		      "neighbour": [
		        { "uid": "0x17" }
		      ]
		    },
		    {
		      "neighbour": [
		        { "uid": "0x17" },
		        { "uid": "0x19" },
		        { "uid": "0x1a" },
		        { "uid": "0x1f" }
		      ]
		    }
		  ]
		}`,
		string(resp.Json),
	)
}

func runQuery(dg *dgo.Dgraph, attr string, uids []uint64, srcFunc []string) (*api.Response, error) {
	x.AssertTrue(uids == nil || srcFunc == nil)

	var query string
	if uids != nil {
		var uidv []string
		for _, uid := range uids {
			uidv = append(uidv, fmt.Sprintf("%#x", uid))
		}
		query = fmt.Sprintf(`
			{
				q(func: uid(%s)) {
					%s { uid }
				}
			}`, strings.Join(uidv, ","), attr,
		)
	} else {
		var langs, args string
		if srcFunc[1] != "" {
			langs = "@" + srcFunc[1]
		}
		args = strings.Join(srcFunc[2:], " ")
		query = fmt.Sprintf(`
			{
				q(func: %s(%s%s, %q)) {
					uid
				}
			}`, srcFunc[0], attr, langs, args)
	}

	resp, err := testutil.RetryQuery(dg, query)

	return resp, err
}

func BenchmarkEqFilter(b *testing.B) {
	dg, err := testutil.DgraphClient(testutil.GetSockAddr())
	if err != nil {
		panic(err)
	}
	err = dg.Alter(context.Background(), &api.Operation{DropAll: true})
	if err != nil {
		panic(err)
	}

	n := 10000
	for i := 0; i < n; i++ {
		rdf := fmt.Sprintf("<%#x> <dgraph.type> \"abc\" .", i+1)
		mu := &api.Mutation{SetNquads: []byte(rdf), CommitNow: true}
		err := testutil.RetryMutation(dg, mu)
		if err != nil {
			panic(err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		query := `
		{
			q(func: uid(1, 2)) @filter(type(abc)) {
				uid
			}
		}
		`
		_, err := testutil.RetryQuery(dg, query)
		if err != nil {
			panic(err)
		}
	}
}

// Index-related test. Similar to TestProcessTaskIndex but we call MergeLists only
// at the end. In other words, everything is happening only in mutation layers,
// and not committed to BadgerDB until near the end.
func TestProcessTaskIndexMLayer(t *testing.T) {
	dg := initClusterTest(t, `friend:string @index(term) .`)

	resp, err := runQuery(dg, "friend", nil, []string{"anyofterms", "", "hey photon"})
	require.NoError(t, err)
	require.JSONEq(t, `{
		  "q": [
		    { "uid": "0xa" },
		    { "uid": "0xc" }
		  ]
		}`,
		string(resp.Json),
	)

	// Now try changing 12's friend value from "photon" to "notphotonExtra" to
	// "notphoton".
	setClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> %q .", 12, "notphotonExtra"))
	setClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> %q .", 12, "notphoton"))

	// Issue a similar query.
	resp, err = runQuery(dg, "friend", nil,
		[]string{"anyofterms", "", "hey photon notphoton notphotonExtra"})
	require.NoError(t, err)
	require.JSONEq(t, `{
		  "q": [
		    { "uid": "0xa" },
		    { "uid": "0xc" }
		  ]
		}`,
		string(resp.Json),
	)

	// Try redundant deletes.
	delClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> %q .", 10, "photon"))
	delClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> %q .", 10, "photon"))

	// Delete followed by set.
	delClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> %q .", 12, "notphoton"))
	setClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> %q .", 12, "ignored"))

	// Issue a similar query.
	resp, err = runQuery(dg, "friend", nil,
		[]string{"anyofterms", "", "photon notphoton ignored"})
	require.NoError(t, err)
	require.JSONEq(t, `{
		  "q": [
		    { "uid": "0xc" }
		  ]
		}`,
		string(resp.Json),
	)

	resp, err = runQuery(dg, "friend", nil,
		[]string{"anyofterms", "", "photon notphoton ignored"})
	require.NoError(t, err)
	require.JSONEq(t, `{
		  "q": [
		    { "uid": "0xc" }
		  ]
		}`,
		string(resp.Json),
	)
}

func TestCountReverseIndex(t *testing.T) {
	schemaStr := "friend: [uid] @count ."
	dg, err := testutil.DgraphClient(testutil.GetSockAddr())
	if err != nil {
		t.Fatalf("Error while getting a dgraph client: %v", err)
	}
	testutil.DropAll(t, dg)

	require.NoError(t, dg.Alter(context.Background(), &api.Operation{Schema: schemaStr}))

	n := 1000
	rdf := ""
	for i := 2; i < n; i++ {
		rdf = rdf + fmt.Sprintf("<%#x> <friend> <%#x> . \n", 1, i)
	}
	setClusterEdge(t, dg, rdf)

	resp, err := runQuery(dg, "friend", []uint64{1}, nil)
	require.NoError(t, err)
	count := strings.Count(string(resp.Json), "uid")

	resp, err = runQuery(
		dg,
		"count(friend)",
		nil,
		[]string{"eq", "", fmt.Sprintf("%d", count)})
	require.Equal(t, string(resp.Json), `{"q":[{"uid":"0x1"}]}`)

	for i := 1; i < n; i++ {
		if i == count {
			continue
		}
		resp, err = runQuery(
			dg,
			"count(friend)",
			nil,
			[]string{"eq", "", fmt.Sprintf("%d", i)})
		require.NotEqual(
			t, string(resp.Json), `{"q":[{"uid":"0x1"}]}`, fmt.Sprintf("Failed for iteration %d", i))
	}
}

func TestCountIndexOverwrite(t *testing.T) {
	schemaStr := "friend: [uid] @reverse @count ."
	dg, err := testutil.DgraphClient(testutil.GetSockAddr())
	if err != nil {
		t.Fatalf("Error while getting a dgraph client: %v", err)
	}
	testutil.DropAll(t, dg)

	require.NoError(t, dg.Alter(context.Background(), &api.Operation{Schema: schemaStr}))

	for i := 0; i < 1000; i++ {
		setClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> <%#x> .", 1, 2))
	}
	resp, err := runQuery(
		dg,
		"count(friend)",
		nil,
		[]string{"eq", "", "1"})
	require.Equal(
		t, string(resp.Json), `{"q":[{"uid":"0x1"}]}`)
}

func TestCountReverseWithDeletes(t *testing.T) {
	schemaStr := "friend: [uid] @reverse @count ."
	dg, err := testutil.DgraphClient(testutil.GetSockAddr())
	if err != nil {
		t.Fatalf("Error while getting a dgraph client: %v", err)
	}
	testutil.DropAll(t, dg)

	require.NoError(t, dg.Alter(context.Background(), &api.Operation{Schema: schemaStr}))

	n := 100
	for i := 0; i < 1000; i++ {
		node := rand.Intn(n-1) + 2
		if i%2 == 0 {
			setClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> <%#x> .", 1, node))
		} else {
			delClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> <%#x> .", 1, node))
		}
	}

	for i := 2; i <= n; i++ {
		delClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> <%#x> .", 1, i))
	}
	setClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> <%#x> .", 1, 2))

	resp, err := runQuery(dg, "friend", []uint64{1}, nil)
	require.NoError(t, err)
	require.JSONEq(t, `{
		  "q": [
		    {
		      "friend": [
		        { "uid": "0x2" }
		      ]
		    }
		  ]
		}`,
		string(resp.Json),
	)
}

// Index-related test. Similar to TestProcessTaskIndeMLayer except we call
// MergeLists in between a lot of updates.
func TestProcessTaskIndex(t *testing.T) {
	dg := initClusterTest(t, `friend:string @index(term) .`)

	resp, err := runQuery(dg, "friend", nil, []string{"anyofterms", "", "hey photon"})
	require.NoError(t, err)
	require.JSONEq(t, `{
		  "q": [
		    { "uid": "0xa" },
		    { "uid": "0xc" }
		  ]
		}`,
		string(resp.Json),
	)

	// Now try changing 12's friend value from "photon" to "notphotonExtra" to
	// "notphoton".
	setClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> %q .", 12, "notphotonExtra"))
	setClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> %q .", 12, "notphoton"))

	// Issue a similar query.
	resp, err = runQuery(dg, "friend", nil,
		[]string{"anyofterms", "", "hey photon notphoton notphotonExtra"})
	require.NoError(t, err)
	require.JSONEq(t, `{
		  "q": [
		    { "uid": "0xa" },
		    { "uid": "0xc" }
		  ]
		}`,
		string(resp.Json),
	)

	// Try redundant deletes.
	delClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> %q .", 10, "photon"))
	delClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> %q .", 10, "photon"))

	// Delete followed by set.
	delClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> %q .", 12, "notphoton"))
	setClusterEdge(t, dg, fmt.Sprintf("<%#x> <friend> %q .", 12, "ignored"))

	// Issue a similar query.
	resp, err = runQuery(dg, "friend", nil,
		[]string{"anyofterms", "", "photon notphoton ignored"})
	require.NoError(t, err)
	require.JSONEq(t, `{
		  "q": [
		    { "uid": "0xc" }
		  ]
		}`,
		string(resp.Json),
	)
}

func TestMain(m *testing.M) {
	posting.Config.CommitFraction = 0.10
	gr = new(groupi)
	gr.gid = 1
	gr.tablets = make(map[string]*pb.Tablet)
	addTablets := func(attrs []string, gid uint32, namespace uint64) {
		for _, attr := range attrs {
			gr.tablets[x.NamespaceAttr(namespace, attr)] = &pb.Tablet{GroupId: gid}
		}
	}

	addTablets([]string{"name", "name2", "age", "http://www.w3.org/2000/01/rdf-schema#range", "",
		"friend", "dgraph.type", "dgraph.graphql.xid", "dgraph.graphql.schema"},
		1, x.RootNamespace)
	addTablets([]string{"friend_not_served"}, 2, x.RootNamespace)
	addTablets([]string{"name"}, 1, 0x2)

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

	m.Run()
}
