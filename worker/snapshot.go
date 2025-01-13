/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.etcd.io/etcd/raft/v3"

	"github.com/dgraph-io/badger/v4"
	api_v25 "github.com/dgraph-io/dgo/v240/protos/api.v25"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v24/conn"
	"github.com/hypermodeinc/dgraph/v24/posting"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/schema"
	"github.com/hypermodeinc/dgraph/v24/x"
)

const (
	// MB represents a megabyte.
	MB = 1 << 20
)

type badgerWriter interface {
	Write(buf *z.Buffer) error
	Flush() error
}

func FlushKvs(stream api_v25.Dgraph_StreamPSnapshotServer) error {

	// schema.Init(State.Pstore)
	// postingListCacheSize := (x.CachePercent[0] * (x.TotalCache << 20)) / 100
	// pstoreBlockCacheSize := (x.CachePercent[1] * (totalCacx.TotalCachehe << 20)) / 100
	// pstoreIndexCacheSize := (x.CachePercent[2] * (x.TotalCache << 20)) / 100
	// posting.Init(State.Pstore, postingListCacheSize, x.RemoveOnUpdate)
	// defer posting.Cleanup()
	// Init(State.Pstore)
	if err := pstore.DropAll(); err != nil {
		return err
	}
	var writer badgerWriter
	sw := pstore.NewStreamWriter()
	defer sw.Cancel()

	if err := sw.Prepare(); err != nil {
		return err
	}

	writer = sw

	// We can use count to check the number of posting lists returned in tests.
	size := 0
	for {
		kvs, err := stream.Recv()
		if kvs != nil {
			glog.Infoln("All key-values have been received.", string(kvs.Data))
		}

		if err != nil {
			return err
		}
		if kvs.Done {
			glog.Infoln("All key-values have been received.")
			break
		}

		size += len(kvs.Data)
		glog.Infof("Received batch of size: %s. Total so far: %s\n",
			humanize.IBytes(uint64(len(kvs.Data))), humanize.IBytes(uint64(size)))

		buf := z.NewBufferSlice(kvs.Data)
		if err := writer.Write(buf); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}

	glog.Infof("P dir writes DONE. Sending ACK")
	// Send an acknowledgement back to the leader.
	if err := stream.SendAndClose(&api_v25.ReceiveSnapshotKVRequest{Message: "done"}); err != nil {
		return err
	}

	// posting.ResetCache()
	// ResetAclCache()
	// State.Dispose()
	// InitServerState()

	return nil
}

// populateSnapshot gets data for a shard from the leader and writes it to BadgerDB on the follower.
func (n *node) populateSnapshot(snap *pb.Snapshot, pl *conn.Pool) error {
	c := pb.NewWorkerClient(pl.Get())

	// We should absolutely cancel the context when we return from this function, that way, the
	// leader who is sending the snapshot would stop sending.
	ctx, cancel := context.WithCancel(n.ctx)
	defer cancel()

	// Set my RaftContext on the snapshot, so it's easier to locate me.
	snap.Context = n.RaftContext
	stream, err := c.StreamSnapshot(ctx)
	if err != nil {
		return err
	}

	if err := stream.Send(snap); err != nil {
		return err
	}

	var writer badgerWriter
	if snap.SinceTs == 0 {
		sw := pstore.NewStreamWriter()
		defer sw.Cancel()

		if err := sw.Prepare(); err != nil {
			return err
		}

		writer = sw
	} else {
		writer = pstore.NewManagedWriteBatch()
	}

	// We can use count to check the number of posting lists returned in tests.
	size := 0
	var done *pb.KVS
	for {
		kvs, err := stream.Recv()
		if err != nil {
			return err
		}
		if kvs.Done {
			done = kvs
			glog.V(1).Infoln("All key-values have been received.")
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		size += len(kvs.Data)
		glog.V(1).Infof("Received batch of size: %s. Total so far: %s\n",
			humanize.IBytes(uint64(len(kvs.Data))), humanize.IBytes(uint64(size)))

		buf := z.NewBufferSlice(kvs.Data)
		if err := writer.Write(buf); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}

	if err := deleteStalePreds(ctx, done, snap.ReadTs); err != nil {
		return err
	}
	// Reset the cache after having received a snapshot.
	posting.ResetCache()

	glog.Infof("Snapshot writes DONE. Sending ACK")
	// Send an acknowledgement back to the leader.
	if err := stream.Send(&pb.Snapshot{Done: true}); err != nil {
		return err
	}

	x.VerifySnapshot(pstore, snap.ReadTs)
	glog.Infof("Populated snapshot with data size: %s\n", humanize.IBytes(uint64(size)))
	return nil
}

func deleteStalePreds(ctx context.Context, kvs *pb.KVS, ts uint64) error {
	if kvs == nil {
		return nil
	}

	// Look for predicates present in the receiver but not in the list sent by the leader.
	// These predicates were deleted in between snapshots and need to be deleted from the
	// receiver to keep the schema in sync.
	currPredicates := schema.State().Predicates()
	snapshotPreds := make(map[string]struct{})
	for _, pred := range kvs.Predicates {
		snapshotPreds[pred] = struct{}{}
	}
	for _, pred := range currPredicates {
		if _, ok := snapshotPreds[pred]; !ok {
		LOOP:
			for {
				// While retrieving the snapshot, we mark the node as unhealthy. So it is better to
				// a blocking delete of predicate as we know that no new writes will arrive at
				// this alpha.
				err := posting.DeletePredicate(ctx, pred, ts)
				switch err {
				case badger.ErrBlockedWrites:
					time.Sleep(1 * time.Second)
				case nil:
					break LOOP
				default:
					glog.Warningf(
						"Cannot delete removed predicate %s after streaming snapshot: %v",
						pred, err)
					return errors.Wrapf(err,
						"cannot delete removed predicate %s after streaming snapshot", pred)
				}
			}
		}
	}

	// Look for types present in the receiver but not in the list sent by the leader.
	// These types were deleted in between snapshots and need to be deleted from the
	// receiver to keep the schema in sync.
	currTypes := schema.State().Types()
	snapshotTypes := make(map[string]struct{})
	for _, typ := range kvs.Types {
		snapshotTypes[typ] = struct{}{}
	}
	for _, typ := range currTypes {
		if _, ok := snapshotTypes[typ]; !ok {
			if err := schema.State().DeleteType(typ, ts); err != nil {
				return errors.Wrapf(err, "cannot delete removed type %s after streaming snapshot",
					typ)
			}
		}
	}

	return nil
}

func doStreamSnapshot(snap *pb.Snapshot, out pb.Worker_StreamSnapshotServer) error {
	// We choose not to try and match the requested snapshot from the latest snapshot at the leader.
	// This is the job of the Raft library. At the leader end, we service whatever is asked of us.
	// If this snapshot is old, Raft should cause the follower to request another one, to overwrite
	// the data from this one.
	//
	// Snapshot request contains the txn read timestamp to be used to get a consistent snapshot of
	// the data. This is what we use in orchestrate.
	//
	// Note: This would also pick up schema updates done "after" the snapshot index. Guess that
	// might be OK. Otherwise, we'd want to version the schemas as well. Currently, they're stored
	// at timestamp=1.

	// We no longer check if this node is the leader, because the leader can switch between snapshot
	// requests. Therefore, we wait until this node has reached snap.ReadTs, before servicing the
	// request. Any other node in the group should have the same data as the leader, once it is past
	// the read timestamp.
	glog.Infof("Waiting to reach timestamp: %d", snap.ReadTs)
	if err := posting.Oracle().WaitForTs(out.Context(), snap.ReadTs); err != nil {
		return err
	}

	stream := pstore.NewStreamAt(snap.ReadTs)
	stream.LogPrefix = "Sending Snapshot"
	// Use the default implementation. We no longer try to generate a rolled up posting list here.
	// Instead, we just stream out all the versions as they are.
	stream.KeyToList = nil
	stream.Send = func(buf *z.Buffer) error {
		kvs := &pb.KVS{Data: buf.Bytes()}
		return out.Send(kvs)
	}
	stream.ChooseKey = func(item *badger.Item) bool {
		if item.Version() >= snap.SinceTs {
			return true
		}

		if item.Version() != 1 {
			return false
		}

		// Type and Schema keys always have a timestamp of 1. They all need to be sent
		// with the snapshot.
		pk, err := x.Parse(item.Key())
		if err != nil {
			return false
		}
		return pk.IsSchema() || pk.IsType()
	}

	// Get the list of all the predicate and types at the time of the snapshot so that the receiver
	// can delete predicates
	predicates := schema.State().Predicates()
	types := schema.State().Types()

	if err := stream.Orchestrate(out.Context()); err != nil {
		return err
	}

	// Indicate that sending is done.
	done := &pb.KVS{
		Done:       true,
		Predicates: predicates,
		Types:      types,
	}
	if err := out.Send(done); err != nil {
		return err
	}

	glog.Infof("Streaming done. Waiting for ACK...")
	ack, err := out.Recv()
	if err != nil {
		return err
	}
	glog.Infof("Received ACK with done: %v\n", ack.Done)
	return nil
}

func (w *grpcWorker) StreamSnapshot(stream pb.Worker_StreamSnapshotServer) error {
	// Pause rollups during snapshot streaming.
	closer, err := groups().Node.startTask(opSnapshot)
	if err != nil {
		return err
	}
	defer closer.Done()

	n := groups().Node
	if n == nil || n.Raft() == nil {
		return conn.ErrNoNode
	}

	// Indicate that we're streaming right now. Used to cancel
	// calculateSnapshot.  However, this logic isn't foolproof. A leader might
	// have already proposed a snapshot, which it can apply while this streaming
	// is going on. That can happen after the reqSnap check we're doing below.
	// However, I don't think we need to tackle this edge case for now.
	atomic.AddInt32(&n.streaming, 1)
	defer atomic.AddInt32(&n.streaming, -1)

	snap, err := stream.Recv()
	if err != nil {
		// If we don't even receive a request (here or if no StreamSnapshot is called), we can't
		// report the snapshot to be a failure, because we don't know which follower is asking for
		// one. Technically, I (the leader) can figure out from my Raft state, but I (Manish) think
		// this is such a rare scenario, that we don't need to build a mechanism to track that. If
		// we see it in the wild, we could add a timeout based mechanism to receive this request,
		// but timeouts are always hard to get right.
		return err
	}
	glog.Infof("Got StreamSnapshot request: %+v\n", snap)
	if err := doStreamSnapshot(snap, stream); err != nil {
		glog.Errorf("While streaming snapshot: %v. Reporting failure.", err)
		n.Raft().ReportSnapshot(snap.Context.GetId(), raft.SnapshotFailure)
		return err
	}
	glog.Infof("Stream snapshot: OK")
	return nil
}
