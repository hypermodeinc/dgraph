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
	"google.golang.org/grpc"

	"github.com/dustin/go-humanize"
	"github.com/golang/glog"
	"github.com/pkg/errors"
)

// streamProcessor defines the common interface for stream processing
type streamProcessor interface {
	SendAndClose(*apiv25.StreamSnapshotResponse) error
	Recv() (*apiv25.StreamSnapshotRequest, error)
	grpc.ServerStream
}

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
			continue
		}
		glog.Infof("Connecting to the leader of the group [%v] from alpha addr [%v]", gid, groups().Node.MyAddr)

		pl := groups().Leader(gid)
		if pl == nil {
			glog.Errorf("Unable to connect to the leader of group [%v]", gid)
			return nil, fmt.Errorf("unable to connect to the leader of group [%v] : %v", gid, conn.ErrNoConnection)
		}
		con := pl.Get()
		c := pb.NewWorkerClient(con)
		glog.Infof("Successfully connected to leader of group [%v]", gid)

		if _, err := c.ApplyDrainmode(ctx, drainMode); err != nil {
			glog.Errorf("Unable to apply drainmode : %v", err)
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
		return fmt.Errorf("Failed to receive initial stream message: %v", err)
	}

	groupId := req.GroupId
	if groupId == groups().Node.gid {
		return flushKvs(stream)
	}

	pl := groups().Leader(groupId)
	if pl == nil {
		glog.Errorf("Unable to connect to the leader of group [%v]", groupId)
		return fmt.Errorf("unable to connect to the leader of group [%v] : %v", groupId, conn.ErrNoConnection)
	}

	con := pl.Get()
	c := pb.NewWorkerClient(con)
	alphaStream, err := c.InternalStreamSnapshot(stream.Context())
	if err != nil {
		return fmt.Errorf("Failed to establish stream with leader: %v", err)
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
				glog.Info("Context cancelled, stopping receive goroutine.")
				errCh <- fmt.Errorf("Context deadline exceeded")
				return
			default:
				msg, err := in.Recv()
				if err != nil {
					if !errors.Is(err, io.EOF) {
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
			close(errCh)
			return err

		case msg, ok := <-buffer:
			if !ok {
				break Loop
			}

			data := &apiv25.StreamSnapshotRequest{Pairs: &apiv25.KVS{Data: msg.Pairs.Data}}

			if msg.Pairs.Done {
				if err := out.Send(&apiv25.StreamSnapshotRequest{Pairs: &apiv25.KVS{Done: true}}); err != nil {
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
		return fmt.Errorf("Failed to send close on in: %v", err)
	}

	// Wait for ACK from the out stream
	_, err := out.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("Failed to receive ACK from out stream: %w", err)
	}

	glog.Info("Received ACK")
	return nil
}

// flushKvs receives the stream of data from the client and writes it to BadgerDB.
// It also sends a streams the data to other nodes of the same group and reloads the schema from the DB.
func flushKvs(stream apiv25.Dgraph_StreamSnapshotServer) error {
	if err := processStreamData(stream); err != nil {
		return err
	}

	return postStreamProcessing(stream.Context())
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
	if err := processStreamData(stream); err != nil {
		return err
	}
	// Inform Zero about the new tablets.
	return postStreamProcessing(stream.Context())
}

func processStreamData(stream streamProcessor) error {
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
		req, err := stream.Recv()
		if err != nil {
			return err
		}

		kvs := req.GetPairs()
		// Check if all key-value pairs have been received.
		if kvs != nil && kvs.Done {
			glog.Info("All key-values have been received.")
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

	glog.Info("P dir writes DONE. Sending ACK")

	// Send an acknowledgment to the leader indicating completion.
	return stream.SendAndClose(&apiv25.StreamSnapshotResponse{Done: true})
}

func postStreamProcessing(ctx context.Context) error {
	if err := schema.LoadFromDb(ctx); err != nil {
		return errors.Wrapf(err, "Cannot load schema after streaming data")
	}
	if err := UpdateMembershipState(ctx); err != nil {
		return errors.Wrapf(err, "Cannot update membership state after streaming data")
	}

	gr.informZeroAboutTablets()

	posting.ResetCache()
	ResetAclCache()
	groups().applyInitialSchema()
	groups().applyInitialTypes()
	ResetGQLSchemaStore()

	return nil
}
