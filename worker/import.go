/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"context"
	"fmt"
	"io"

	apiv25 "github.com/dgraph-io/dgo/v250/protos/api.v25"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/conn"
	"github.com/hypermodeinc/dgraph/v25/posting"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"

	"github.com/dustin/go-humanize"
	"github.com/golang/glog"
	"github.com/pkg/errors"
)

func ProposeDrain(ctx context.Context, drainMode *pb.DrainModeRequest) ([]uint32, error) {
	memState := GetMembershipState()
	currentGroups := make([]uint32, 0)
	for gid := range memState.GetGroups() {
		currentGroups = append(currentGroups, gid)
	}

	for _, gid := range currentGroups {
		if groups().ServesGroup(gid) && groups().Node.AmLeader() {
			if _, err := (&grpcWorker{}).ApplyDrainmode(ctx, drainMode); err != nil {
				return nil, err
			}
		}

		pl := groups().Leader(gid)
		if pl == nil {
			return nil, conn.ErrNoConnection
		}
		con := pl.Get()
		c := pb.NewWorkerClient(con)
		if _, err := c.ApplyDrainmode(ctx, drainMode); err != nil {
			return nil, err
		}
	}

	return currentGroups, nil
}

// InStream handles streaming of snapshots to a target group. It first checks the group
// associated with the incoming stream and, if it's the same as the current node's group, it
// flushes the data using FlushKvs. If the group is different, it establishes a connection
// with the leader of that group and streams data to it. The function returns an error if
// there are any issues in the process, such as a broken connection or failure to establish
// a stream with the leader.
func InStream(stream apiv25.Dgraph_StreamSnapshotServer) error {
	req, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("failed to receive initial stream message: %v", err)
	}

	groupId := req.GroupId
	if groupId == groups().Node.gid {
		return flushKvs(stream)
	}

	pl := groups().Leader(groupId)
	if pl == nil {
		return conn.ErrNoConnection
	}

	con := pl.Get()
	c := pb.NewWorkerClient(con)
	alphaStream, err := c.InternalStreamSnapshot(stream.Context())
	if err != nil {
		return fmt.Errorf("failed to establish stream with leader: %v", err)
	}

	return pipeTwoStream(stream, alphaStream)
}

func pipeTwoStream(in apiv25.Dgraph_StreamSnapshotServer, out pb.Worker_InternalStreamSnapshotClient) error {
	buffer := make(chan *apiv25.StreamSnapshotRequest, 10)
	errCh := make(chan error, 1)
	ctx := in.Context()

	go func() {
		defer close(buffer)
		for {
			select {
			case <-ctx.Done():
				glog.Infof("Context cancelled, stopping receive goroutine.")
				errCh <- fmt.Errorf("context deadline exceeded")
				return
			default:
				msg, err := in.Recv()
				if err != nil {
					if err != io.EOF {
						glog.Errorf("Error receiving from in stream: %v", err)
						errCh <- err
					}
					return
				}
				buffer <- msg
			}
		}
	}()

	size := 0

Loop:
	for {
		select {
		case err := <-errCh:
			return err

		case msg, ok := <-buffer:
			if !ok {
				// Channel closed, exit loop
				break Loop
			}

			data := &pb.KVS{Data: msg.Pairs.Data}

			if msg.Pairs.Done {
				if err := out.Send(&pb.KVS{Done: true}); err != nil {
					glog.Errorf("Error sending 'done' to out stream: %v", err)
					return err
				}
				glog.Infoln("All key-values have been transferred.")
				break Loop
			}

			if err := out.Send(data); err != nil {
				glog.Errorf("Error sending to outstream: %v", err)
				return err
			}

			size += len(msg.Pairs.Data)
			glog.Infof("Sent batch of size: %s. Total so far: %s\n",
				humanize.IBytes(uint64(len(msg.Pairs.Data))), humanize.IBytes(uint64(size)))
		}
	}

	// Close the incoming stream properly
	if err := in.SendAndClose(&apiv25.StreamSnapshotResponse{Done: true}); err != nil {
		return fmt.Errorf("failed to send close on in: %v", err)
	}

	// Wait for ACK from the out stream
	ack, err := out.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("failed to receive ACK from out stream: %w", err)
	}

	glog.Infof("Received ACK with message: %v\n", ack.Done)
	return nil
}

// flushKvs receives the stream of data from the client and writes it to BadgerDB.
// It also sends a streams the data to other nodes of the same group and reloads the schema from the DB.
func flushKvs(stream apiv25.Dgraph_StreamSnapshotServer) error {
	sw := pstore.NewStreamWriter()
	defer sw.Cancel()

	// we should delete all the existing data before starting the stream
	if err := sw.Prepare(); err != nil {
		return err
	}

	// We can use count to check the number of posting lists returned in tests.
	size := 0
	for {
		req, err := stream.Recv()
		if err != nil {
			return err
		}
		kvs := req.GetPairs()

		if kvs.Done {
			glog.Infoln("All key-values have been received.")
			break
		}

		size += len(kvs.Data)
		glog.Infof("Received batch of size: %s. Total so far: %s\n",
			humanize.IBytes(uint64(len(kvs.Data))), humanize.IBytes(uint64(size)))

		buf := z.NewBufferSlice(kvs.Data)
		if err := sw.Write(buf); err != nil {
			return err
		}
	}

	if err := sw.Flush(); err != nil {
		return err
	}

	glog.Infof("P dir writes DONE. Sending ACK")
	// Send an acknowledgement back to the leader.
	if err := stream.SendAndClose(&apiv25.StreamSnapshotResponse{Done: true}); err != nil {
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

func (w *grpcWorker) ApplyDrainmode(ctx context.Context, req *pb.DrainModeRequest) (*pb.Status, error) {
	drainMode := &pb.DrainModeRequest{State: req.State}
	node := groups().Node
	err := node.proposeAndWait(ctx, &pb.Proposal{Drainmode: drainMode}) // Subscribe on given prefixes.

	return &pb.Status{}, err
}

// InternalStreamSnapshot handles the stream of key-value pairs sent from proxy alpha.
// It writes the data to BadgerDB, sends an acknowledgment once all data is received,
// and proposes to accept the newly added data to other group nodes.
func (w *grpcWorker) InternalStreamSnapshot(stream pb.Worker_InternalStreamSnapshotServer) error {
	sw := pstore.NewStreamWriter()
	defer sw.Cancel()

	// Prepare the stream writer, which involves deleting existing data.
	if err := sw.Prepare(); err != nil {
		return err
	}

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
		if err := sw.Write(buf); err != nil {
			return err
		}
	}

	// Flush any remaining data to ensure it is written to BadgerDB.
	if err := sw.Flush(); err != nil {
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
		return errors.Wrapf(err, "cannot update membership state after streaming data")
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
