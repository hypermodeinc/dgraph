/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"context"
	"fmt"
	"io"
	"math"

	"github.com/dgraph-io/badger/v4"
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

// streamProcessor defines the common interface for stream processing
type streamProcessor interface {
	Recv() (*apiv25.StreamPDirRequest, error)
}

type streamProcessorSender interface {
	Send(*apiv25.StreamPDirRequest) error
}

func ProposeDrain(ctx context.Context, drainMode *pb.DrainModeRequest) ([]uint32, error) {
	isStreamPDirRunning := func() bool {
		tasks := GetOngoingTasks()
		for _, t := range tasks {
			if t == opStreamPDir.String() {
				return true
			}
		}
		return false
	}

	if isStreamPDirRunning() {
		return nil, errors.Errorf("another stream p dir operation is already running. " +
			"Please retry later.")
	}

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
		glog.Infof("[import] Connecting to the leader of the group [%v] from alpha addr [%v]", gid, groups().Node.MyAddr)

		pl := groups().Leader(gid)
		if pl == nil {
			glog.Errorf("[import] unable to connect to the leader of group [%v]", gid)
			return nil, fmt.Errorf("unable to connect to the leader of group [%v] : %v", gid, conn.ErrNoConnection)
		}
		con := pl.Get()
		c := pb.NewWorkerClient(con)
		glog.Infof("[import] Successfully connected to leader of group [%v]", gid)

		if _, err := c.ApplyDrainmode(ctx, drainMode); err != nil {
			glog.Errorf("[import] unable to apply drainmode : %v", err)
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
func InStream(stream apiv25.Dgraph_StreamPDirServer) error {
	closer, err := groups().Node.startTask(opStreamPDir)
	if err != nil {
		return errors.Wrapf(err, "cannot start stream p dir task")
	}
	defer closer.Done()

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
		glog.Errorf("[import]  Unable to connect to the leader of group [%v]", groupId)
		return fmt.Errorf("unable to connect to the leader of group [%v] : %v", groupId, conn.ErrNoConnection)
	}

	con := pl.Get()
	c := pb.NewWorkerClient(con)
	alphaStream, err := c.InternalStreamPDir(stream.Context())
	if err != nil {
		return fmt.Errorf("failed to establish stream with leader: %v", err)
	}

	return pipeTwoStream(stream, alphaStream)
}

func pipeTwoStream(in apiv25.Dgraph_StreamPDirServer, out pb.Worker_InternalStreamPDirClient) error {
	buffer := make(chan *apiv25.StreamPDirRequest, 10)
	errCh := make(chan error, 1)
	ctx := in.Context()

	go func() {
		defer close(buffer)
		for {
			select {
			case <-ctx.Done():
				glog.Info("[import]  Context cancelled, stopping receive goroutine.")
				errCh <- fmt.Errorf("context deadline exceeded")
				return
			default:
				msg, err := in.Recv()
				if err != nil {
					if !errors.Is(err, io.EOF) {
						glog.Errorf("[import] Error receiving from in stream: %v", err)
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

			data := &apiv25.StreamPDirRequest{StreamPacket: &apiv25.StreamPacket{Data: msg.StreamPacket.Data}}

			if msg.StreamPacket.Done {
				d := apiv25.StreamPacket{Done: true}
				if err := out.Send(&apiv25.StreamPDirRequest{StreamPacket: &d}); err != nil {
					glog.Errorf("Error sending 'done' to out stream: %v", err)
					return err
				}
				glog.Infoln("[import] All key-values have been transferred.")
				break Loop
			}

			if err := out.Send(data); err != nil {
				glog.Errorf("[import] Error sending to outstream: %v", err)
				return fmt.Errorf("error sending to outstream: %v", err)
			}

			size += len(msg.StreamPacket.Data)
			glog.Infof("[import] Sent batch of size: %s. Total so far: %s\n",
				humanize.IBytes(uint64(len(msg.StreamPacket.Data))), humanize.IBytes(uint64(size)))
		}
	}

	// Close the incoming stream properly
	if err := in.SendAndClose(&apiv25.StreamPDirResponse{Done: true}); err != nil {
		return fmt.Errorf("failed to send close on in: %v", err)
	}

	// Wait for ACK from the out stream
	_, err := out.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("failed to receive ACK from out stream: %w", err)
	}

	glog.Info("[import] Received ACK")
	return nil
}

// flushKvs receives the stream of data from the client and writes it to BadgerDB.
// It also sends a streams the data to other nodes of the same group and reloads the schema from the DB.
func flushKvs(stream apiv25.Dgraph_StreamPDirServer) error {
	if err := processStreamData(stream); err != nil {
		return err
	}

	if err := stream.SendAndClose(&apiv25.StreamPDirResponse{Done: true}); err != nil {
		return err
	}

	if err := postStreamProcessing(stream.Context()); err != nil {
		return err
	}

	glog.Info("[import] Streaming P dir to other nodes")
	currentNode := groups().Node
	streamProposal := &pb.ReqPDirStreamRequest{Addr: currentNode.MyAddr}
	glog.Info("[import] apply proposal to other nodes")

	err := currentNode.proposeAndWait(stream.Context(), &pb.Proposal{Reqpdirstream: streamProposal})
	if err != nil {
		return err
	}

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
func (w *grpcWorker) InternalStreamPDir(stream pb.Worker_InternalStreamPDirServer) error {
	closer, err := groups().Node.startTask(opStreamPDir)
	if err != nil {
		return errors.Wrapf(err, "cannot start stream p dir task")
	}
	defer closer.Done()

	if err := processStreamData(stream); err != nil {
		return err
	}

	if err := stream.SendAndClose(&apiv25.StreamPDirResponse{Done: true}); err != nil {
		return err
	}
	// Inform Zero about the new tablets.
	if err := postStreamProcessing(stream.Context()); err != nil {
		return err
	}

	glog.Info("[import] Streaming P dir to other nodes")
	currentNode := groups().Node
	streamProposal := &pb.ReqPDirStreamRequest{Addr: currentNode.MyAddr}
	glog.Info("[import] apply proposal to other nodes")

	err = currentNode.proposeAndWait(stream.Context(), &pb.Proposal{Reqpdirstream: streamProposal})
	if err != nil {
		return err
	}

	return nil
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

		kvs := req.GetStreamPacket()
		// Check if all key-value pairs have been received.
		if kvs != nil && kvs.Done {
			glog.Info("[import] All key-values have been received.")
			break
		}

		// Increment the total size and log the batch size received.
		size += len(kvs.Data)
		glog.Infof("[import] Received batch of size: %s. Total so far: %s\n",
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

	glog.Info("[import] P dir writes DONE. Sending ACK")

	// Send an acknowledgment to the leader indicating completion.
	return nil
}

func postStreamProcessing(ctx context.Context) error {
	if err := schema.LoadFromDb(ctx); err != nil {
		return errors.Wrapf(err, "cannot load schema after streaming data")
	}
	if err := UpdateMembershipState(ctx); err != nil {
		return errors.Wrapf(err, "cannot update membership state after streaming data")
	}

	gr.informZeroAboutTablets()

	posting.ResetCache()
	ResetAclCache()
	groups().applyInitialSchema()
	groups().applyInitialTypes()
	ResetGQLSchemaStore()

	return nil
}

func RunBadgerStream(ctx context.Context, ps *badger.DB, out streamProcessorSender, groupId uint32) error {
	stream := ps.NewStreamAt(math.MaxUint64)
	stream.LogPrefix = "Sending P dir"
	stream.KeyToList = nil
	stream.Send = func(buf *z.Buffer) error {
		p := &apiv25.StreamPacket{Data: buf.Bytes()}
		if err := out.Send(&apiv25.StreamPDirRequest{StreamPacket: p}); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("failed to send data chunk: %w", err)
		}
		return nil
	}

	// Execute the stream process
	if err := stream.Orchestrate(ctx); err != nil {
		return fmt.Errorf("stream orchestration failed: %w", err)
	}

	// Send the final 'done' signal to mark completion
	glog.Infof("Sending completion signal for group [%d]", groupId)
	done := &apiv25.StreamPacket{Done: true}

	if err := out.Send(&apiv25.StreamPDirRequest{StreamPacket: done}); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("failed to send 'done' signal for group [%d]: %w", groupId, err)
	}

	return nil
}

func (w *grpcWorker) ReqPDirStream(req *pb.ReqPDirStreamRequest, stream pb.Worker_ReqPDirStreamServer) error {
	if err := RunBadgerStream(stream.Context(), pstore, stream, gr.Node.gid); err != nil {
		return err
	}
	return nil
}

// It also sends a streams the data to other nodes of the same group and reloads the schema from the DB.
func FlushData(stream pb.Worker_ReqPDirStreamClient) error {
	glog.Info("[import] got stream from leader")
	if err := processStreamData(stream); err != nil {
		return err
	}

	return postStreamProcessing(stream.Context())
}
