/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"context"
	"fmt"

	api_v25 "github.com/dgraph-io/dgo/v240/protos/api.v25"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/dustin/go-humanize"
	"github.com/golang/glog"
	"github.com/hypermodeinc/dgraph/v24/conn"
	"github.com/hypermodeinc/dgraph/v24/posting"

	"github.com/hypermodeinc/dgraph/v24/protos/pb"

	"github.com/hypermodeinc/dgraph/v24/schema"
	"github.com/pkg/errors"
)

func ProposeDrain(ctx context.Context, drainMode *pb.Drainmode) error {
	pl := groups().connToZeroLeader()
	if pl == nil {
		return conn.ErrNoConnection
	}
	con := pl.Get()
	c := pb.NewZeroClient(con)
	status, err := c.ApplyDrainmode(ctx, drainMode)
	fmt.Println("status ans erros", status, err)
	return err
}

// DoStreamPDir handles streaming of snapshots to a target group. It first checks the group
// associated with the incoming stream and, if it's the same as the current node's group, it
// flushes the data using FlushKvs1. If the group is different, it establishes a connection
// with the leader of that group and streams data to it. The function returns an error if
// there are any issues in the process, such as a broken connection or failure to establish
// a stream with the leader.
func DoStreamPDir(stream api_v25.Dgraph_StreamSnapshotServer) error {
	groupId, err := checkGroup(stream)
	if err != nil {
		return err
	}

	if groupId == groups().Node.gid {
		return FlushKvs(stream)
	}

	pl := groups().Leader(groupId)
	if pl == nil {
		return fmt.Errorf("ubable to connect with group[%v] leader", groupId)
	}

	con := pl.Get()
	c := pb.NewWorkerClient(con)
	out, err := c.StreamPt(stream.Context())
	if err != nil {
		return fmt.Errorf("failed to establish stream with leader: %v", err)
	}

	// time.Sleep(time.Minute * 3)
	return streamToAnotherGroup(stream, out)
}

// streamToAnotherGroup takes an incoming stream and sends it to another group's leader (out).
// It receives data from the incoming stream, forwards it to the leader, and handles the completion signal.
func streamToAnotherGroup(in api_v25.Dgraph_StreamSnapshotServer, out pb.Worker_StreamPtClient) error {
	size := 0
	for {
		// Receive a request from the incoming stream.
		req, err := in.Recv()
		if err != nil {
			return err
		}

		// Prepare the data to send to the leader.
		data := &pb.KVS{Data: req.Kvs.Data}
		if req.Kvs.Done {
			glog.Infoln("All key-values have been received.")
			// Send the completion signal to the leader.
			if err := out.Send(&pb.KVS{Done: true}); err != nil {
				return fmt.Errorf("failed to send 'done' signal: %w", err)
			}
			break
		}

		// Send the data to the leader.
		if err := out.Send(data); err != nil {
			glog.Errorf("Error sending to leader Alpha: %v", err)
			return err
		}

		// Update and log the size of the data sent so far.
		size += len(req.Kvs.Data)
		glog.Infof("Sent batch of size: %s. Total so far: %s\n",
			humanize.IBytes(uint64(len(req.Kvs.Data))), humanize.IBytes(uint64(size)))
	}

	// Send a close signal to the incoming stream.
	if err := in.SendAndClose(&api_v25.ReceiveSnapshotKVRequest{Done: true}); err != nil {
		return fmt.Errorf("failed to send: %v", err)
	}

	// Receive an acknowledgment from the leader.
	ack, err := out.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("failed to receive ACK: %w", err)
	}

	glog.Infof("Received ACK with message: %v\n", ack.Done)

	return nil
}

// checkGroup receives the initial message from the stream and extracts the group ID.
// It returns the group ID if successful, otherwise an error if there is an issue
// receiving the message.
func checkGroup(stream api_v25.Dgraph_StreamSnapshotServer) (uint32, error) {
	req, err := stream.Recv()
	if err != nil {
		return 0, fmt.Errorf("failed to receive initial stream message: %v", err)
	}

	return req.GroupId, nil
}

// FlushKvs receives the stream of data from the client and writes it to BadgerDB.
// It also sends a streams the data to other nodes of the same group and reloads the schema from the DB.
func FlushKvs(stream api_v25.Dgraph_StreamSnapshotServer) error {
	var writer badgerWriter
	sw := pstore.NewStreamWriter()
	defer sw.Cancel()

	// we should delete all the existing data before starting the stream
	if err := sw.Prepare(); err != nil {
		return err
	}

	writer = sw

	// We can use count to check the number of posting lists returned in tests.
	size := 0
	for {
		req, err := stream.Recv()
		if err != nil {
			return err
		}
		kvs := req.GetKvs()

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
	if err := stream.SendAndClose(&api_v25.ReceiveSnapshotKVRequest{Done: true}); err != nil {
		return err
	}

	// Reload the schema from the DB.
	if err := schema.LoadFromDb(stream.Context()); err != nil {
		return errors.Wrapf(err, "cannot load schema after streaming data")
	}
	if err := UpdateMembershipState(stream.Context()); err != nil {
		return errors.Wrapf(err, "cannot update membership state after restore")
	}

	// Inform Zero about the new tablets.
	gr.informZeroAboutTablets()

	posting.ResetCache()
	ResetAclCache()
	groups().applyInitialSchema()
	groups().applyInitialTypes()

	ResetGQLSchemaStore()

	return nil
}

func (w *grpcWorker) ApplyDrainmode(ctx context.Context, req *pb.Drainmode) (*pb.Status, error) {
	drainMode := &pb.Drainmode{State: req.State}
	node := groups().Node
	err := node.proposeAndWait(ctx, &pb.Proposal{Drainmode: drainMode}) // Subscribe on given prefixes.

	return &pb.Status{}, err
}

// StreamPt handles the stream of key-value pairs sent from proxy alpha.
// It writes the data to BadgerDB, sends an acknowledgment once all data is received,
// and proposes to accept the newly added data to other group nodes.
func (w *grpcWorker) StreamPt(stream pb.Worker_StreamPtServer) error {
	var writer badgerWriter
	sw := pstore.NewStreamWriter()
	defer sw.Cancel()

	// Prepare the stream writer, which involves deleting existing data.
	if err := sw.Prepare(); err != nil {
		return err
	}

	writer = sw

	// Track the total size of key-value data received.
	size := 0
	for {
		// Receive a batch of key-value pairs from the stream.
		kvs, err := stream.Recv()
		if err != nil {
			return err
		}

		// Check if all key-value pairs have been received.
		if kvs != nil && kvs.Done {
			glog.Infoln("All key-values have been received.")
			break
		}

		// Increment the total size and log the batch size received.
		size += len(kvs.Data)
		glog.Infof("Received batch of size: %s. Total so far: %s\n",
			humanize.IBytes(uint64(len(kvs.Data))), humanize.IBytes(uint64(size)))

		// Write the received data to BadgerDB.
		buf := z.NewBufferSlice(kvs.Data)
		if err := writer.Write(buf); err != nil {
			return err
		}
	}

	// Flush any remaining data to ensure it is written to BadgerDB.
	if err := writer.Flush(); err != nil {
		return err
	}

	glog.Infof("P dir writes DONE. Sending ACK")

	// Send an acknowledgment to the leader indicating completion.
	if err := stream.SendAndClose(&pb.ReceiveSnapshotKVRequest{Done: true}); err != nil {
		return err
	}
	// Reload the schema from the DB.
	if err := schema.LoadFromDb(stream.Context()); err != nil {
		return errors.Wrapf(err, "cannot load schema after streaming data")
	}
	if err := UpdateMembershipState(stream.Context()); err != nil {
		return errors.Wrapf(err, "cannot update membership state after restore")
	}

	// Inform Zero about the new tablets.
	gr.informZeroAboutTablets()

	posting.ResetCache()
	ResetAclCache()
	groups().applyInitialSchema()
	groups().applyInitialTypes()

	ResetGQLSchemaStore()
	return nil
}
