/*
 * SPDX-FileCopyrightText:  Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"context"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/badger/v4"
	bpb "github.com/dgraph-io/badger/v4/pb"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v24/posting"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/schema"
	"github.com/hypermodeinc/dgraph/v24/tok/hnsw"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var (
	errEmptyPredicate = errors.Errorf("Predicate not specified")
	errNotLeader      = errors.Errorf("Server is not leader of this group")
	emptyPayload      = api.Payload{}
)

// size of kvs won't be too big, we would take care before proposing.
func populateKeyValues(ctx context.Context, kvs []*bpb.KV) error {
	glog.Infof("Writing %d keys\n", len(kvs))
	if len(kvs) == 0 {
		return nil
	}
	writer := posting.NewTxnWriter(pstore)
	if err := writer.Write(&bpb.KVList{Kv: kvs}); err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	pk, err := x.Parse(kvs[0].Key)
	if err != nil {
		return errors.Errorf("while parsing KV: %+v, got error: %v", kvs[0], err)
	}
	return schema.Load(pk.Attr)
}

func batchAndProposeKeyValues(ctx context.Context, kvs chan *pb.KVS) error {
	glog.Infoln("Receiving predicate. Batching and proposing key values")
	n := groups().Node
	proposal := &pb.Proposal{}
	size := 0
	var pk x.ParsedKey

	for kvPayload := range kvs {
		buf := z.NewBufferSlice(kvPayload.GetData())
		err := buf.SliceIterate(func(s []byte) error {
			kv := &bpb.KV{}
			x.Check(proto.Unmarshal(s, kv))
			if len(pk.Attr) == 0 {
				// This only happens once.
				var err error
				pk, err = x.Parse(kv.Key)
				if err != nil {
					return errors.Errorf("while parsing kv: %+v, got error: %v", kv, err)
				}

				if !pk.IsSchema() {
					return errors.Errorf("Expecting first key to be schema key: %+v", kv)
				}

				// Delete on all nodes. Remove the schema at timestamp kv.Version-1 and set it at
				// kv.Version. kv.Version will be the TxnTs of the predicate move.
				p := &pb.Proposal{CleanPredicate: pk.Attr, StartTs: kv.Version - 1}
				glog.Infof("Predicate being received: %v", pk.Attr)
				if err := n.proposeAndWait(ctx, p); err != nil {
					glog.Errorf("Error while cleaning predicate %v %v\n", pk.Attr, err)
					return err
				}
			}

			proposal.Kv = append(proposal.Kv, kv)
			size += len(kv.Key) + len(kv.Value)
			if size >= 32<<20 { // 32 MB
				if err := n.proposeAndWait(ctx, proposal); err != nil {
					return err
				}
				proposal = &pb.Proposal{}
				size = 0
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if size > 0 {
		// Propose remaining keys.
		if err := n.proposeAndWait(ctx, proposal); err != nil {
			return err
		}
	}
	return nil
}

// Returns count which can be used to verify whether we have moved all keys
// for a predicate or not.
func (w *grpcWorker) ReceivePredicate(stream pb.Worker_ReceivePredicateServer) error {
	if !groups().Node.AmLeader() {
		return errors.Errorf("ReceivePredicate failed: Not the leader of group")
	}
	// No new deletion/background cleanup would start after we start streaming tablet,
	// so all the proposals for a particular tablet would atmost wait for deletion of
	// single tablet. Only leader needs to do this.
	mu := groups().blockDeletes
	mu.Lock()
	defer mu.Unlock()

	// Values can be pretty big so having less buffer is safer.
	kvs := make(chan *pb.KVS, 3)
	che := make(chan error, 1)
	// We can use count to check the number of posting lists returned in tests.
	count := 0
	ctx := stream.Context()
	payload := &api.Payload{}

	glog.Infof("Got ReceivePredicate. Group: %d. Am leader: %v",
		groups().groupId(), groups().Node.AmLeader())

	go func() {
		// Takes care of throttling and batching.
		che <- batchAndProposeKeyValues(ctx, kvs)
	}()
	for {
		kvBuf, err := stream.Recv()
		if err == io.EOF {
			payload.Data = []byte(fmt.Sprintf("%d", count))
			if err := stream.SendAndClose(payload); err != nil {
				glog.Errorf("Received %d keys. Error in loop: %v", count, err)
				return err
			}
			break
		}
		if err != nil {
			glog.Errorf("Received %d keys. Error in loop: %v", count, err)
			return err
		}
		glog.V(2).Infof("Received batch of size: %s", humanize.IBytes(uint64(len(kvBuf.Data))))

		buf := z.NewBufferSlice(kvBuf.Data)
		if err := buf.SliceIterate(func(_ []byte) error {
			count++
			return nil
		}); err != nil {
			glog.Errorf("error while counting in buf: %v\n", err)
			return err
		}

		select {
		case kvs <- kvBuf:
		case <-ctx.Done():
			close(kvs)
			<-che
			glog.Infof("Received %d keys. Context deadline\n", count)
			return ctx.Err()
		case err := <-che:
			glog.Infof("Received %d keys. Error via channel: %v\n", count, err)
			return err
		}
	}
	close(kvs)
	err := <-che
	glog.Infof("Proposed %d keys. Error: %v\n", count, err)
	return err
}

func (w *grpcWorker) MovePredicate(ctx context.Context,
	in *pb.MovePredicatePayload) (*api.Payload, error) {
	ctx, span := trace.SpanFromContext(ctx).TracerProvider().Tracer("grpcWorker").Start(ctx, "MovePredicate")
	defer span.End()

	n := groups().Node
	if !n.AmLeader() {
		return &emptyPayload, errNotLeader
	}
	// Don't do a predicate move if the cluster is in draining mode.
	if err := x.HealthCheck(); err != nil {
		return &emptyPayload, errors.Wrap(err, "Move predicate request rejected")
	}

	if groups().groupId() != in.SourceGid {
		return &emptyPayload,
			errors.Errorf("Group id doesn't match, received request for %d, my gid: %d",
				in.SourceGid, groups().groupId())
	}
	if len(in.Predicate) == 0 {
		return &emptyPayload, errEmptyPredicate
	}

	//TODO: need to find possibly a better way to not move __vector_ predicates
	if in.DestGid == 0 && !strings.Contains(in.Predicate, hnsw.VecKeyword) {
		glog.Infof("Was instructed to delete tablet: %v", in.Predicate)
		// Expected Checksum ensures that all the members of this group would block until they get
		// the latest membership status where this predicate now belongs to another group. So they
		// know that they are no longer serving this predicate, before they delete it from their
		// state. Without this checksum, the members could end up deleting the predicate and then
		// serve a request asking for that predicate, causing Jepsen failures.
		p := &pb.Proposal{
			CleanPredicate:   in.Predicate,
			ExpectedChecksum: in.ExpectedChecksum,
			StartTs:          in.TxnTs,
		}
		return &emptyPayload, groups().Node.proposeAndWait(ctx, p)
	}

	if strings.Contains(in.Predicate, hnsw.VecKeyword) {
		return &emptyPayload, nil
	}

	if err := posting.Oracle().WaitForTs(ctx, in.TxnTs); err != nil {
		return &emptyPayload, errors.Errorf("While waiting for txn ts: %d. Error: %v", in.TxnTs, err)
	}

	gid, err := groups().BelongsTo(in.Predicate)
	switch {
	case err != nil:
		return &emptyPayload, err
	case gid == 0:
		return &emptyPayload, errNonExistentTablet
	case gid != groups().groupId():
		return &emptyPayload, errUnservedTablet
	}

	msg := fmt.Sprintf("Move predicate request: %+v", in)
	glog.Info(msg)
	span.SetAttributes(attribute.String("predicate", in.Predicate))

	err = movePredicateHelper(ctx, in)
	if err != nil {
		span.SetStatus(1, err.Error())
	}
	return &emptyPayload, err
}

func movePredicateHelper(ctx context.Context, in *pb.MovePredicatePayload) error {
	// Note: Manish thinks it *should* be OK for a predicate receiver to not have to stop other
	// operations like snapshots and rollups. Note that this is the sender. This should stop other
	// operations.
	closer, err := groups().Node.startTask(opPredMove)
	if err != nil {
		return errors.Wrapf(err, "unable to start task opPredMove")
	}
	defer closer.Done()

	span := trace.SpanFromContext(ctx)

	pl := groups().Leader(in.DestGid)
	if pl == nil {
		return errors.Errorf("Unable to find a connection for group: %d\n", in.DestGid)
	}
	c := pb.NewWorkerClient(pl.Get())
	out, err := c.ReceivePredicate(ctx)
	if err != nil {
		return errors.Wrapf(err, "while calling ReceivePredicate")
	}

	txn := pstore.NewTransactionAt(in.TxnTs, false)
	defer txn.Discard()

	// Send schema first.
	schemaKey := x.SchemaKey(in.Predicate)
	item, err := txn.Get(schemaKey)
	switch {
	case err == badger.ErrKeyNotFound:
		// The predicate along with the schema could have been deleted. In that case badger would
		// return ErrKeyNotFound. We don't want to try and access item.Value() in that case.
	case err != nil:
		return err
	default:
		val, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}
		buf := z.NewBuffer(1024, "PredicateMove.MovePredicateHelper")
		defer func() {
			if err := buf.Release(); err != nil {
				glog.Warningf("error in releasing buffer: %v", err)
			}
		}()

		kv := &bpb.KV{}
		kv.Key = schemaKey
		kv.Value = val
		kv.Version = in.TxnTs
		kv.UserMeta = []byte{item.UserMeta()}
		badger.KVToBuffer(kv, buf)

		kvs := &pb.KVS{
			Data: buf.Bytes(),
		}
		if err := out.Send(kvs); err != nil {
			return errors.Errorf("while sending: %v", err)
		}
	}

	// sends all data except schema, schema key has different prefix
	// Read the predicate keys and stream to keysCh.
	stream := pstore.NewStreamAt(in.TxnTs)
	stream.LogPrefix = fmt.Sprintf("Sending predicate: [%s]", in.Predicate)
	stream.Prefix = x.PredicatePrefix(in.Predicate)
	stream.KeyToList = func(key []byte, itr *badger.Iterator) (*bpb.KVList, error) {
		// For now, just send out full posting lists, because we use delete markers to delete older
		// data in the prefix range. So, by sending only one version per key, and writing it at a
		// provided timestamp, we can ensure that these writes are above all the delete markers.
		l, err := posting.ReadPostingList(key, itr)
		if err != nil {
			return nil, err
		}
		// Setting all the data at in.TxnTs
		kvs, err := l.Rollup(itr.Alloc, math.MaxUint64)
		for _, kv := range kvs {
			// Let's set all of them at this move timestamp.
			kv.Version = in.TxnTs
		}
		return &bpb.KVList{Kv: kvs}, err
	}
	stream.Send = func(buf *z.Buffer) error {
		kvs := &pb.KVS{
			Data: buf.Bytes(),
		}
		return out.Send(kvs)
	}
	if span.IsRecording() {
		span.AddEvent("Starting stream list orchestrate", trace.WithAttributes(
			attribute.String("predicate", in.Predicate)))
	}
	if err := stream.Orchestrate(out.Context()); err != nil {
		return err
	}

	payload, err := out.CloseAndRecv()
	if err != nil {
		return err
	}
	recvCount, err := strconv.Atoi(string(payload.Data))
	if err != nil {
		return err
	}

	msg := fmt.Sprintf("Receiver %s says it got %d keys.\n", pl.Addr, recvCount)
	if span.IsRecording() {
		span.AddEvent("Moving predicate", trace.WithAttributes(
			attribute.String("predicate", in.Predicate)))
	}
	glog.Infof(msg)
	return nil
}
