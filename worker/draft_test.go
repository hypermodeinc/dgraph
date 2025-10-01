/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/raft/v3/raftpb"
	"google.golang.org/protobuf/proto"

	"github.com/hypermodeinc/dgraph/v25/posting"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/raftwal"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func getEntryForMutation(index, startTs uint64) raftpb.Entry {
	proposal := &pb.Proposal{Mutations: &pb.Mutations{StartTs: startTs}}
	sz := proto.Size(proposal)
	data := make([]byte, 8+sz)
	x.Check2(x.MarshalToSizedBuffer(data[8:], proposal))
	data = data[:8+sz]
	return raftpb.Entry{Index: index, Term: 1, Type: raftpb.EntryNormal, Data: data}
}

func getEntryForCommit(index, startTs, commitTs uint64) raftpb.Entry {
	delta := &pb.OracleDelta{}
	delta.Txns = append(delta.Txns, &pb.TxnStatus{StartTs: startTs, CommitTs: commitTs})
	proposal := &pb.Proposal{Delta: delta}
	sz := proto.Size(proposal)
	data := make([]byte, 8+sz)
	x.Check2(x.MarshalToSizedBuffer(data[8:], proposal))
	data = data[:8+sz]
	return raftpb.Entry{Index: index, Term: 1, Type: raftpb.EntryNormal, Data: data}
}

// func BenchmarkProcessListIndex(b *testing.B) {
// 	dir, err := os.MkdirTemp("", "storetest_")
// 	x.Check(err)
// 	defer os.RemoveAll(dir)

// 	opt := badger.DefaultOptions(dir)
// 	ps, err := badger.OpenManaged(opt)
// 	x.Check(err)
// 	pstore = ps
// 	// Not using posting list cache
// 	posting.Init(ps, 0, false)
// 	Init(ps)
// 	err = schema.ParseBytes([]byte("testAttr: [string] @index(exact) ."), 1)
// 	require.NoError(b, err)

// 	ctx := context.Background()
// 	pipeline := &PredicatePipeline{
// 		attr:  "0-testAttr",
// 		edges: make(chan *pb.DirectedEdge, 1000),
// 		wg:    &sync.WaitGroup{},
// 		errCh: make(chan error, 1),
// 	}

// 	txn := posting.Oracle().RegisterStartTs(5)
// 	mp := &MutationPipeline{txn: txn}

// 	// Generate 1000 edges
// 	populatePipeline := func() {
// 		pipeline = &PredicatePipeline{
// 			attr:  "0-testAttr",
// 			edges: make(chan *pb.DirectedEdge, 1000),
// 			wg:    &sync.WaitGroup{},
// 			errCh: make(chan error, 1),
// 		}

// 		txn = posting.Oracle().RegisterStartTs(5)
// 		mp = &MutationPipeline{txn: txn}

// 		for i := 0; i < 1000; i++ {
// 			edge := &pb.DirectedEdge{
// 				Entity:    uint64(i + 1),
// 				Attr:      "0-testAttr",
// 				Value:     []byte(fmt.Sprintf("value%d", rand.Intn(1000))),
// 				ValueType: pb.Posting_STRING,
// 				Op:        pb.DirectedEdge_SET,
// 			}
// 			pipeline.edges <- edge
// 		}
// 	}

// 	b.ResetTimer()

// 	b.Run("Baseline", func(b *testing.B) {
// 		for i := 0; i < b.N; i++ {
// 			populatePipeline()
// 		}
// 	})

// 	b.Run("DefaultPipeline", func(b *testing.B) {
// 		for i := 0; i < b.N; i++ {
// 			populatePipeline()
// 			var wg sync.WaitGroup
// 			wg.Add(1)
// 			go func() {
// 				mp.DefaultPipeline(ctx, pipeline)
// 				wg.Done()
// 			}()
// 			close(pipeline.edges)
// 			wg.Wait()
// 		}
// 	})

// 	b.Run("ProcessListWithoutIndex", func(b *testing.B) {
// 		for i := 0; i < b.N; i++ {
// 			populatePipeline()
// 			var wg sync.WaitGroup
// 			wg.Add(1)
// 			go func() {
// 				mp.ProcessListWithoutIndex(ctx, pipeline)
// 				wg.Done()
// 			}()
// 			close(pipeline.edges)
// 			wg.Wait()
// 		}
// 	})

// 	b.Run("ProcessListIndex", func(b *testing.B) {
// 		for i := 0; i < b.N; i++ {
// 			populatePipeline()
// 			var wg sync.WaitGroup
// 			wg.Add(1)
// 			go func() {
// 				mp.ProcessListIndex(ctx, pipeline)
// 				wg.Done()
// 			}()
// 			close(pipeline.edges)
// 			wg.Wait()
// 		}
// 	})
// }

func TestCalculateSnapshot(t *testing.T) {
	dir := t.TempDir()
	ds := raftwal.Init(dir)
	defer ds.Close()

	n := newNode(ds, 1, 1, "")
	var entries []raftpb.Entry
	// Txn: 1 -> 5 // 5 should be the ReadTs.
	// Txn: 2 // Should correspond to the index. Subtract 1 from the index.
	// Txn: 3 -> 4
	entries = append(entries, getEntryForMutation(1, 1), getEntryForMutation(2, 3),
		getEntryForMutation(3, 2), getEntryForCommit(4, 3, 4), getEntryForCommit(5, 1, 5))
	require.NoError(t, n.Store.Save(&raftpb.HardState{}, entries, &raftpb.Snapshot{}))
	n.Applied.SetDoneUntil(5)
	posting.Oracle().RegisterStartTs(2)
	snap, err := n.calculateSnapshot(0, n.Applied.DoneUntil(), posting.Oracle().MinPendingStartTs())
	require.NoError(t, err)
	require.Equal(t, uint64(5), snap.ReadTs)
	require.Equal(t, uint64(1), snap.Index)

	// Check state of Raft store.
	var cs raftpb.ConfState
	require.NoError(t, n.Store.CreateSnapshot(snap.Index, &cs, nil))

	first, err := n.Store.FirstIndex()
	require.NoError(t, err)
	require.Equal(t, uint64(2), first)

	last, err := n.Store.LastIndex()
	require.NoError(t, err)
	require.Equal(t, uint64(5), last)

	// This time commit all txns.
	// Txn: 7 -> 8
	// Txn: 2 -> 9
	entries = entries[:0]
	entries = append(entries, getEntryForMutation(6, 7), getEntryForCommit(7, 7, 8),
		getEntryForCommit(8, 2, 9))
	require.NoError(t, n.Store.Save(&raftpb.HardState{}, entries, &raftpb.Snapshot{}))
	n.Applied.SetDoneUntil(8)
	posting.Oracle().ResetTxns()
	snap, err = n.calculateSnapshot(0, n.Applied.DoneUntil(), posting.Oracle().MinPendingStartTs())
	require.NoError(t, err)
	require.Equal(t, uint64(9), snap.ReadTs)
	require.Equal(t, uint64(8), snap.Index)

	// Check state of Raft store.
	require.NoError(t, n.Store.CreateSnapshot(snap.Index, &cs, nil))
	first, err = n.Store.FirstIndex()
	require.NoError(t, err)
	require.Equal(t, uint64(9), first)

	entries = entries[:0]
	entries = append(entries, getEntryForMutation(9, 11))
	require.NoError(t, n.Store.Save(&raftpb.HardState{}, entries, &raftpb.Snapshot{}))
	n.Applied.SetDoneUntil(9)
	snap, err = n.calculateSnapshot(0, n.Applied.DoneUntil(), posting.Oracle().MinPendingStartTs())
	require.NoError(t, err)
	require.Nil(t, snap)
}
