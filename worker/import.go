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
	"sync"

	"github.com/dgraph-io/badger/v4"
	apiv2 "github.com/dgraph-io/dgo/v250/protos/api.v2"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/conn"
	"github.com/hypermodeinc/dgraph/v25/posting"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/x"
	"golang.org/x/sync/errgroup"

	"github.com/dustin/go-humanize"
	"github.com/golang/glog"
	"github.com/pkg/errors"
)

type pubSub struct {
	subscribers []chan *apiv2.StreamExtSnapshotRequest
	lock        sync.RWMutex
}

// Subscribe returns a new channel to receive published messages
func (ps *pubSub) subscribe() <-chan *apiv2.StreamExtSnapshotRequest {
	ch := make(chan *apiv2.StreamExtSnapshotRequest, 10) // buffered so slow consumers don't block
	ps.lock.Lock()
	defer ps.lock.Unlock()
	ps.subscribers = append(ps.subscribers, ch)
	return ch
}

func (ps *pubSub) publish(msg *apiv2.StreamExtSnapshotRequest) {
	ps.lock.RLock()
	defer ps.lock.RUnlock()
	for _, ch := range ps.subscribers {
		select {
		case ch <- msg:
		default:
			// drop message if subscriber is slow
			fmt.Println("Dropping message for slow subscriber")
		}
	}
}

func (ps *pubSub) close() {
	ps.lock.Lock()
	defer ps.lock.Unlock()
	for _, ch := range ps.subscribers {
		close(ch)
	}
	ps.subscribers = nil
}

func ProposeDrain(ctx context.Context, drainMode *apiv2.UpdateExtSnapshotStreamingStateRequest) ([]uint32, error) {
	memState := GetMembershipState()
	currentGroups := make([]uint32, 0)
	for gid := range memState.GetGroups() {
		currentGroups = append(currentGroups, gid)
	}
	updateExtSnapshotStreamingState := &apiv2.UpdateExtSnapshotStreamingStateRequest{
		Finish:   drainMode.Finish,
		Start:    drainMode.Start,
		DropData: drainMode.DropData,
	}

	for _, gid := range currentGroups {
		if groups().ServesGroup(gid) && groups().Node.AmLeader() {
			if _, err := (&grpcWorker{}).UpdateExtSnapshotStreamingState(ctx, updateExtSnapshotStreamingState); err != nil {
				return nil, err
			}
			continue
		}
		glog.Infof("[import:apply-drainmode] Connecting to the leader of the group [%v] from alpha addr [%v]", gid, groups().Node.MyAddr)

		pl := groups().Leader(gid)
		if pl == nil {
			glog.Errorf("[import:apply-drainmode] unable to connect to the leader of group [%v]", gid)
			return nil, fmt.Errorf("unable to connect to the leader of group [%v] : %v", gid, conn.ErrNoConnection)
		}
		con := pl.Get()
		c := pb.NewWorkerClient(con)
		glog.Infof("[import:apply-drainmode] Successfully connected to leader of group [%v]", gid)

		if _, err := c.UpdateExtSnapshotStreamingState(ctx, updateExtSnapshotStreamingState); err != nil {
			glog.Errorf("[import:apply-drainmode] unable to apply drainmode : %v", err)
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
func InStream(stream apiv2.Dgraph_StreamExtSnapshotServer) error {

	req, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("failed to receive initial stream message: %v", err)
	}

	groupId := req.GroupId
	if groupId == groups().Node.gid {
		glog.Infof("[import] Streaming P dir to current Group [%v]", groupId)
		return streamInGroup(stream, true)
	}

	glog.Infof("[import] Streaming P dir to other Group [%v]", groupId)
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

	glog.Infof("[import] sending forward true to leader of group [%v]", groupId)
	forwardReq := &apiv2.StreamExtSnapshotRequest{Forward: true}
	if err := alphaStream.Send(forwardReq); err != nil {
		return fmt.Errorf("failed to send forward request: %w", err)
	}

	return pipeTwoStream(stream, alphaStream, groupId)
}

func pipeTwoStream(in apiv2.Dgraph_StreamExtSnapshotServer, out pb.Worker_InternalStreamPDirClient, groupId uint32) error {
	buffer := make(chan *apiv2.StreamExtSnapshotRequest, 10)
	errCh := make(chan error, 1)
	ctx := in.Context()

	glog.Infof("[import:forward:diffrent-group] started streaming to group [%v]", groupId)
	go func() {
		defer close(buffer)
		for {
			select {
			case <-ctx.Done():
				glog.Info("[import:forward:diffrent-group] Context cancelled, stopping receive goroutine.")
				errCh <- fmt.Errorf("context deadline exceeded")
				return
			default:
				msg, err := in.Recv()
				if err != nil {
					if !errors.Is(err, io.EOF) {
						glog.Errorf("[import:forward:diffrent-group] Error receiving from in stream: %v", err)
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

			data := &apiv2.StreamExtSnapshotRequest{Pkt: &apiv2.StreamPacket{Data: msg.Pkt.Data}}

			if msg.Pkt.Done {
				d := apiv2.StreamPacket{Done: true}
				if err := out.Send(&apiv2.StreamExtSnapshotRequest{Pkt: &d}); err != nil {
					glog.Errorf(`[import:forward:diffrent-group] Error sending 'done' to out stream for group [%v]: %v`,
						groupId, err)
					return fmt.Errorf(`[import:forward:diffrent-group] Error sending 'done' to out 
					stream for group [%v]: %v`, groupId, err)
				}
				glog.Infof(`[import:forward:diffrent-group] All key-values have been transferred 
				to the [%v] group.`, groupId)
				break Loop
			}

			if err := out.Send(data); err != nil {
				glog.Errorf("[import:forward:diffrent-group] Error sending to outstream for group [%v]: %v",
					groupId, err)
				return fmt.Errorf("[import:forward:diffrent-group] error sending to outstream for group [%v]: %v",
					groupId, err)
			}

			size += len(msg.Pkt.Data)
			glog.Infof("[import:forward:diffrent-group] Sent batch of size: %s. Total so far: %s\n",
				humanize.IBytes(uint64(len(msg.Pkt.Data))), humanize.IBytes(uint64(size)))
		}
	}

	// Close the incoming stream properly
	if err := in.SendAndClose(&apiv2.StreamExtSnapshotResponse{}); err != nil {
		glog.Errorf("[import:forward:diffrent-group] failed to send close on in stream for group [%v]: %v",
			groupId, err)
		return fmt.Errorf("[import:forward:diffrent-group] failed to send close on in stream for group [%v]: %v",
			groupId, err)
	}

	// Wait for ACK from the out stream
	_, err := out.CloseAndRecv()
	if err != nil {
		glog.Errorf("[import:forward:diffrent-group] failed to receive ACK from group [%v]: %v", groupId, err)
		return fmt.Errorf("[import:forward:diffrent-group] failed to receive ACK from group [%v]: %w", groupId, err)
	}

	glog.Infof("[import:forward:diffrent-group] Received ACK from group [%v]", groupId)
	return nil
}

func (w *grpcWorker) UpdateExtSnapshotStreamingState(ctx context.Context,
	req *apiv2.UpdateExtSnapshotStreamingStateRequest) (*pb.Status, error) {
	if req == nil {
		return nil, errors.New("UpdateExtSnapshotStreamingStateRequest should not be empty")
	}

	if req.Start && req.Finish {
		return nil, errors.New("UpdateExtSnapshotStreamingStateRequest should not be both start and finish")
	}

	glog.Infof("[import] Applying import mode proposal: %v", req)
	extSnapshotStreamingState := &apiv2.UpdateExtSnapshotStreamingStateRequest{Start: req.Start,
		Finish: req.Finish, DropData: req.DropData}
	err := groups().Node.proposeAndWait(ctx, &pb.Proposal{UpdateExtSnapshotStreamingState: extSnapshotStreamingState})

	return &pb.Status{}, err
}

// InternalStreamSnapshot handles the stream of key-value pairs sent from proxy alpha.
// It writes the data to BadgerDB, sends an acknowledgment once all data is received,
// and proposes to accept the newly added data to other group nodes.
func (w *grpcWorker) InternalStreamPDir(stream pb.Worker_InternalStreamPDirServer) error {
	glog.Info("[import] updating import mode to false")
	defer x.ExtSnapshotStreamingState(false)
	// we send forward request to the leader of the group so if this node is leader then forward
	// will be true else false
	forwardReq, err := stream.Recv()
	if err != nil {
		return err
	}

	if err := streamInGroup(stream, forwardReq.Forward); err != nil {
		return err
	}

	return nil
}

// RunBadgerStream runs a BadgerDB stream to send key-value pairs to the specified group.
// It creates a new stream at the maximum sequence number and sends the data to the specified group.
// It also sends a final 'done' signal to mark completion.
func RunBadgerStream(ctx context.Context, ps *badger.DB, out apiv2.Dgraph_StreamExtSnapshotClient, groupId uint32) error {
	stream := ps.NewStreamAt(math.MaxUint64)
	stream.LogPrefix = "[import:ext-snapshot] Sending P dir to group [" + fmt.Sprintf("%d", groupId) + "]"
	stream.KeyToList = nil
	stream.Send = func(buf *z.Buffer) error {
		p := &apiv2.StreamPacket{Data: buf.Bytes()}
		if err := out.Send(&apiv2.StreamExtSnapshotRequest{Pkt: p}); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("failed to send data chunk: %w", err)
		}
		return nil
	}

	// Execute the stream process
	if err := stream.Orchestrate(ctx); err != nil {
		return fmt.Errorf("stream orchestration failed: %w", err)
	}

	// Send the final 'done' signal to mark completion
	glog.Infof("[import:ext-snapshot] Sending completion signal for group [%d]", groupId)
	done := &apiv2.StreamPacket{Done: true}

	if err := out.Send(&apiv2.StreamExtSnapshotRequest{Pkt: done}); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("failed to send 'done' signal for group [%d]: %w", groupId, err)
	}

	return nil
}

// postStreamProcessing handles the post-stream processing of data received from the buffer into the local BadgerDB.
// It loads the schema, updates the membership state, informs zero about tablets, resets caches, applies initial schema,
// applies initial types, and resets the GQL schema store.
func postStreamProcessing(ctx context.Context) error {
	glog.Info("[import:flush:current-node] post stream processing")
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
	glog.Info("[import:flush:current-node] post stream processing done")

	return nil
}

// streamInGroup handles the streaming of data within a group.
// This function is called on both leader and follower nodes with different behaviors:
// - Leader (forward=true): The leader node receives data and forwards it to all group members
// - Follower (forward=false): The follower node receives data and stores it in local BadgerDB.
//
// Parameters:
// - stream: The gRPC stream server for receiving streaming data
// - forward: Indicates if this node is forwarding data to other nodes
//   - true: This node is the group leader and will forward data to group members
//   - false: This node is a follower receiving forwarded data and storing locally
//
// The function:
// 1. Creates a context with cancellation support for graceful shutdown
// 2. Sets up a pub/sub system for message distribution
// 3. Uses an error group to manage concurrent operations
// 4. Tracks successful nodes for majority consensus (only relevant for leader)
// 5. Receives messages in a loop until EOF or error
// 6. Publishes received messages to all subscribers (for leader) or stores locally (for follower)
// 7. Handles cleanup and error cases appropriately
//
// Returns:
// - nil: If streaming completes successfully
// - error: If there's an issue receiving data or if majority consensus isn't achieved (for leader)
func streamInGroup(stream apiv2.Dgraph_StreamExtSnapshotServer, forward bool) error {
	node := groups().Node
	glog.Infof("[import] got stream,forwarding in group [%v]", forward)
	ctx := stream.Context()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ps := &pubSub{}
	eg, ctx := errgroup.WithContext(ctx)
	// We created this to check the majority
	successfulNodes := make(map[string]bool)

	// Receive messages and publish to all subscribers
	eg.Go(func() error {
		defer ps.close()
		for {
			select {
			case <-ctx.Done():
				glog.Info("[import] Context cancelled, stopping receive goroutine.")
				return ctx.Err()
			default:
				msg, err := stream.Recv()
				if err != nil {
					if !errors.Is(err, io.EOF) {
						glog.Errorf("[import] Error receiving from in stream: %v", err)
						return err
					}
					return nil
				}
				ps.publish(msg)
			}
		}
	})

	size := 0
	for _, member := range groups().state.Groups[node.gid].Members {
		if member.Addr == node.MyAddr {
			buffer := ps.subscribe()
			eg.Go(func() error {
				if forward {
					glog.Infof("[import:flush:current-node] this is a leader node of group [%v]"+
						"flushing P dir to current node [%v]", node.gid, member.Addr)
				}
				if err := flushInCurrentNode(ctx, buffer, size); err != nil {
					return err
				}
				if forward {
					successfulNodes[member.Addr] = true
					glog.Infof("[import] Successfully flushed data to node: %v", member.Addr)
				}
				return nil
			})
			continue
		}

		// we will not going to return any error from here because we carea about majroity of nodes if the mojority
		// of node able to get the data then the behind once can cathup qafter that
		if forward {
			member := member
			buffer := ps.subscribe()
			eg.Go(func() error {
				glog.Infof(`[import:forward:current-group] streaming P dir to [%v] from [%v]`, member.Addr, node.MyAddr)
				if member.AmDead {
					glog.Infof(`[import:forward:current-group] [%v] is dead, skipping`, member.Addr)
					return nil
				}
				pl, err := conn.GetPools().Get(member.Addr)
				if err != nil {
					successfulNodes[member.Addr] = false
					glog.Errorf("connection error to [%v]: %v", member.Addr, err)
					return nil
				}
				c := pb.NewWorkerClient(pl.Get())
				peerStream, err := c.InternalStreamPDir(ctx)
				if err != nil {
					successfulNodes[member.Addr] = false
					glog.Errorf("failed to establish stream with peer %v: %v", member.Addr, err)
					return nil
				}
				forwardReq := &apiv2.StreamExtSnapshotRequest{Forward: false}
				if err := peerStream.Send(forwardReq); err != nil {
					successfulNodes[member.Addr] = false
					glog.Errorf("failed to send forward request: %v", err)
					return nil
				}

				if err := connectToNodeStream(peerStream, buffer, size, member.Addr); err != nil {
					successfulNodes[member.Addr] = false
					glog.Errorf("failed to connect to node stream: %v", err)
					return nil
				}
				successfulNodes[member.Addr] = true
				glog.Infof("[import] Successfully connected and streamed data to node: %v", member.Addr)
				return nil
			})
		}
	}

	if err := eg.Wait(); err != nil {
		return err
	}

	if err := stream.SendAndClose(&apiv2.StreamExtSnapshotResponse{}); err != nil {
		return fmt.Errorf("[import] failed to send close on in: %w", err)
	}

	// if we are the leader and we are not able to get the majority of nodes then we will return error
	// because we want to make sure that all the nodes are able to get the data
	if forward && !checkMajority(successfulNodes) {
		glog.Error("[import] Majority of nodes failed to receive data.")
		return errors.New("Failed to achieve majority consensus")
	}

	return nil
}

// Calculate majority based on Raft quorum rules with special handling for small clusters
func checkMajority(successfulNodes map[string]bool) bool {
	totalNodes := len(successfulNodes)
	successfulCount := 0

	for _, success := range successfulNodes {
		if success {
			successfulCount++
		}
	}

	// Special cases for small clusters
	switch totalNodes {
	case 0:
		// No nodes - this should never happen
		glog.Error("[import] No nodes in cluster")
		return false
	case 1:
		// Single node - must succeed
		return successfulCount == 1
	case 2:
		// Two nodes - both must succeed
		return successfulCount == 2
	default:
		// Regular Raft quorum rule for 3+ nodes
		majority := totalNodes/2 + 1
		return successfulCount >= majority
	}
}

// connectToNodeStream handles the connection to a peer node for streaming data.
// It receives data from the buffer and sends it to the peer node using the provided stream.
//
// Parameters:
// - out: The gRPC stream client for sending data to the peer node
// - buffer: A channel of StreamPDirRequest messages containing the data to be sent
// - size: The size of the data being sent (updated in-place)
// - peerId: The identifier of the peer node
//
// The function:
// 1. Receives messages from the buffer in a loop until EOF or error
// 2. Sends each message to the peer node using the stream
// 3. Updates the size of the data being sent
// 4. Sends a 'done' signal to the peer node when all data has been sent
// 5. Handles cleanup and error cases appropriately
//
// Returns:
// - nil: If the connection is successful and all data is sent
// - error: If there's an issue sending data or if the context deadline is exceeded
func connectToNodeStream(out pb.Worker_InternalStreamPDirClient, buffer <-chan *apiv2.StreamExtSnapshotRequest,
	size int, peerId string) error {
	ctx := out.Context()
Loop:
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context deadline exceeded")

		default:
			msg, ok := <-buffer
			if !ok {
				break Loop
			}

			data := &apiv2.StreamExtSnapshotRequest{Pkt: &apiv2.StreamPacket{Data: msg.Pkt.Data}}

			if msg.Pkt.Done {
				glog.Infof("[import:forward:current-group] received done signal from [%v]", peerId)
				d := apiv2.StreamPacket{Done: true}
				if err := out.Send(&apiv2.StreamExtSnapshotRequest{Pkt: &d}); err != nil {
					return err
				}
				break Loop
			}

			if err := out.Send(data); err != nil {
				return err
			}

			size += len(msg.Pkt.Data)
		}
	}

	_, err := out.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("[import:forward:current-group] failed to receive ACK from [%v]: %w", peerId, err)
	}

	glog.Infof("[import:forward:current-group] successfully streamed to [%v]", peerId)
	return nil
}

// flushInCurrentNode handles the flushing of data received from the buffer into the local BadgerDB.
//
// Parameters:
// - ctx: The context for the operation
// - buffer: A channel of StreamPDirRequest messages containing the data to be flushed
// - size: The size of the data being flushed (updated in-place)
//
// The function:
// 1. Receives messages from the buffer in a loop until EOF or error
// 2. Flushed each message to the local BadgerDB using a stream writer
// 3. Updates the size of the data being flushed
// 4. Sends a 'done' signal to the peer node when all data has been sent
// 5. Handles cleanup and error cases appropriately
//
// Returns:
// - nil: If the flushing is successful and all data is sent
// - error: If there's an issue flushing data or if the context deadline is exceeded
func flushInCurrentNode(ctx context.Context, buffer <-chan *apiv2.StreamExtSnapshotRequest, size int) error {
	glog.Infof("[import:flush:current-node] flushing P dir in badger db")
	sw := pstore.NewStreamWriter()
	defer sw.Cancel()
	if err := sw.Prepare(); err != nil {
		return err
	}
Loop:
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context deadline exceeded")

		default:
			msg, ok := <-buffer
			if !ok {
				break Loop
			}
			kvs := msg.GetPkt()
			if kvs != nil && kvs.Done {
				break
			}

			size += len(kvs.Data)
			buf := z.NewBufferSlice(kvs.Data)
			if err := sw.Write(buf); err != nil {
				return err
			}
		}
	}

	if err := sw.Flush(); err != nil {
		return err
	}
	glog.Infof("[import:flush:current-node] successfully flushed data in badger db")
	return postStreamProcessing(ctx)
}
