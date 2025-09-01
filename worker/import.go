/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/conn"
	"github.com/hypermodeinc/dgraph/v25/posting"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/x"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

type pubSub struct {
	subscribers []chan *api.StreamExtSnapshotRequest
	sync.RWMutex
}

// Subscribe returns a new channel to receive published messages
func (ps *pubSub) subscribe() <-chan *api.StreamExtSnapshotRequest {
	ch := make(chan *api.StreamExtSnapshotRequest, 20)
	ps.Lock()
	defer ps.Unlock()
	ps.subscribers = append(ps.subscribers, ch)
	return ch
}

func (ps *pubSub) publish(msg *api.StreamExtSnapshotRequest) {
	// Send message to all subscribers without dropping any
	ps.RLock()
	defer ps.RUnlock()
	for _, ch := range ps.subscribers {
		ch <- msg
	}
}

func (ps *pubSub) close() {
	ps.Lock()
	defer ps.Unlock()
	for _, ch := range ps.subscribers {
		close(ch)
	}
	ps.subscribers = nil
}

func (ps *pubSub) handlePublisher(ctx context.Context, stream api.Dgraph_StreamExtSnapshotServer) error {
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

}

func (ps *pubSub) runForwardSubscriber(ctx context.Context, out api.Dgraph_StreamExtSnapshotClient, peerId string) error {
	buffer := ps.subscribe()
	size := 0
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

			data := &api.StreamExtSnapshotRequest{Pkt: &api.StreamPacket{Data: msg.Pkt.Data}}

			if msg.Pkt.Done {
				glog.Infof("[import] received done signal from [%v]", peerId)
				d := api.StreamPacket{Done: true}
				if err := out.Send(&api.StreamExtSnapshotRequest{Pkt: &d}); err != nil {
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

	return nil
}

func (ps *pubSub) runLocalSubscriber(ctx context.Context) error {
	buffer := ps.subscribe()
	size := 0
	glog.Infof("[import:flush] flushing external snapshot in badger db")
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
	glog.Infof("[import:flush] successfully flushed data in badger db")
	return postStreamProcessing(ctx)
}

func ProposeDrain(ctx context.Context, drainMode *api.UpdateExtSnapshotStreamingStateRequest) ([]uint32, error) {
	memState := GetMembershipState()
	currentGroups := make([]uint32, 0)
	for gid := range memState.GetGroups() {
		currentGroups = append(currentGroups, gid)
	}

	for _, gid := range currentGroups {
		if groups().ServesGroup(gid) && groups().Node.AmLeader() {
			if _, err := (&grpcWorker{}).UpdateExtSnapshotStreamingState(ctx, drainMode); err != nil {
				return nil, err
			}
			continue
		}
		glog.Infof("[import:apply-drainmode] Connecting to the leader of the group [%v] from alpha addr [%v]",
			gid, groups().Node.MyAddr)

		pl := groups().Leader(gid)
		if pl == nil {
			glog.Errorf("[import:apply-drainmode] unable to connect to the leader of group [%v]", gid)
			return nil, fmt.Errorf("unable to connect to the leader of group [%v] : %v", gid, conn.ErrNoConnection)
		}
		con := pl.Get()
		c := pb.NewWorkerClient(con)
		glog.Infof("[import:apply-drainmode] Successfully connected to leader of group [%v]", gid)

		if _, err := c.UpdateExtSnapshotStreamingState(ctx, drainMode); err != nil {
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
func InStream(stream api.Dgraph_StreamExtSnapshotServer) error {
	req, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("failed to receive initial stream message: %v", err)
	}

	groupId := req.GroupId
	if groupId == groups().Node.gid {
		glog.Infof("[import] Streaming external snapshot to current Group [%v]", groupId)
		return streamInGroup(stream, true)
	}

	glog.Infof("[import] Streaming external snapshot to other Group [%v]", groupId)
	pl := groups().Leader(groupId)
	if pl == nil {
		glog.Errorf("[import]  Unable to connect to the leader of group [%v]", groupId)
		return fmt.Errorf("unable to connect to the leader of group [%v] : %v", groupId, conn.ErrNoConnection)
	}

	con := pl.Get()
	c := pb.NewWorkerClient(con)
	alphaStream, err := c.StreamExtSnapshot(stream.Context())
	if err != nil {
		return fmt.Errorf("failed to establish stream with leader: %v", err)
	}

	glog.Infof("[import] sending forward true to leader of group [%v]", groupId)
	forwardReq := &api.StreamExtSnapshotRequest{Forward: true}
	if err := alphaStream.Send(forwardReq); err != nil {
		return fmt.Errorf("failed streamInGroupto send forward request: %w", err)
	}

	return pipeTwoStream(stream, alphaStream, groupId)
}

func pipeTwoStream(in api.Dgraph_StreamExtSnapshotServer, out pb.Worker_StreamExtSnapshotClient,
	groupId uint32) error {

	currentGroup := groups().Node.gid
	glog.Infof("[import] [forward from group-%v to group-%v] forwarding stream", currentGroup, groupId)

	defer func() {
		if err := in.SendAndClose(&api.StreamExtSnapshotResponse{}); err != nil {
			glog.Errorf("[import] [forward from group %v to group %v] failed to send close on in"+
				" stream for group [%v]: %v", currentGroup, groupId, groupId, err)
		}
	}()

	defer func() {
		// Wait for ACK from the out stream
		_, err := out.CloseAndRecv()
		if err != nil {
			glog.Errorf("[import] [forward from group %v to group %v] failed to receive ACK from group [%v]: %v",
				currentGroup, groupId, groupId, err)
		}
	}()

	ps := &pubSub{}
	eg, egCtx := errgroup.WithContext(in.Context())

	eg.Go(func() error {
		if err := ps.runForwardSubscriber(egCtx, out, fmt.Sprintf("%d", groupId)); err != nil {
			return err
		}
		return nil
	})

	eg.Go(func() error {
		if err := ps.handlePublisher(egCtx, in); err != nil {
			return err
		}
		return nil
	})

	if err := eg.Wait(); err != nil {
		return err
	}

	glog.Infof("[import] [forward from group %v to group %v] Received ACK from group [%v]", currentGroup, groupId, groupId)
	return nil
}

func (w *grpcWorker) UpdateExtSnapshotStreamingState(ctx context.Context,
	req *api.UpdateExtSnapshotStreamingStateRequest) (*pb.Status, error) {
	if req == nil {
		return nil, errors.New("UpdateExtSnapshotStreamingStateRequest must not be nil")
	}

	if req.Start && req.Finish {
		return nil, errors.New("UpdateExtSnapshotStreamingStateRequest cannot have both Start and Finish set to true")
	}

	glog.Infof("[import] Applying import mode proposal: %v", req)
	err := groups().Node.proposeAndWait(ctx, &pb.Proposal{ExtSnapshotState: req})

	return &pb.Status{}, err
}

// StreamExtSnapshot handles the stream of key-value pairs sent from proxy alpha.
// It receives a Forward flag from the stream to determine if the current node is the leader.
// If the node is the leader (Forward is true), it streams the data to its followers.
// Otherwise, it simply writes the data to BadgerDB and flushes it.
func (w *grpcWorker) StreamExtSnapshot(stream pb.Worker_StreamExtSnapshotServer) error {
	glog.Info("[import] updating import mode to false")
	defer x.ExtSnapshotStreamingState(false)
	// Receive the first message to check the Forward flag.
	// If Forward is true, this node is the leader and should forward the stream to its followers.
	// If Forward is false, the node just writes and flushes the data.
	forwardReq, err := stream.Recv()
	if err != nil {
		return err
	}

	if err := streamInGroup(stream, forwardReq.Forward); err != nil {
		return err
	}

	return nil
}

// postStreamProcessing handles the post-stream processing of data received from the buffer into the local BadgerDB.
// It loads the schema, updates the membership state, informs zero about tablets, resets caches, applies initial schema,
// applies initial types, and resets the GQL schema store.
func postStreamProcessing(ctx context.Context) error {
	glog.Info("[import:flush] post stream processing")
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
	glog.Info("[import:flush] post stream processing done")

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
func streamInGroup(stream api.Dgraph_StreamExtSnapshotServer, forward bool) error {
	node := groups().Node
	glog.Infof("[import] got stream,forwarding in group [%v]", forward)

	ps := &pubSub{}
	eg, errGCtx := errgroup.WithContext(stream.Context())
	// We created this to check the majority
	successfulNodes := make(map[string]bool)

	for _, member := range groups().state.Groups[node.gid].Members {
		if member.Addr == node.MyAddr {
			eg.Go(func() error {
				if err := ps.runLocalSubscriber(errGCtx); err != nil {
					glog.Errorf("[import:flush] failed to run local subscriber: %v", err)
					updateNodeStatus(&ps.RWMutex, successfulNodes, member.Addr, false)
					return err
				}
				updateNodeStatus(&ps.RWMutex, successfulNodes, member.Addr, true)
				return nil
			})
			continue
		}

		// We are not going to return any error from here because we care about the majority of nodes.
		// If the majority of nodes are able to receive the data, the remaining ones can catch up later.
		if forward {
			eg.Go(func() error {
				glog.Infof(`[import:forward] streaming external snapshot to [%v] from [%v]`, member.Addr, node.MyAddr)
				if member.AmDead {
					glog.Infof(`[import:forward] [%v] is dead, skipping`, member.Addr)
					return nil
				}
				pl, err := conn.GetPools().Get(member.Addr)
				if err != nil {
					updateNodeStatus(&ps.RWMutex, successfulNodes, member.Addr, false)
					glog.Errorf("connection error to [%v]: %v", member.Addr, err)
					return nil
				}
				c := pb.NewWorkerClient(pl.Get())
				peerStream, err := c.StreamExtSnapshot(errGCtx)
				if err != nil {
					updateNodeStatus(&ps.RWMutex, successfulNodes, member.Addr, false)
					glog.Errorf("failed to establish stream with peer %v: %v", member.Addr, err)
					return nil
				}
				defer func() {
					_, err = peerStream.CloseAndRecv()
					if err != nil {
						glog.Errorf("[import:forward] failed to receive ACK from [%v]: %v", member.Addr, err)
					}
				}()

				forwardReq := &api.StreamExtSnapshotRequest{Forward: false}
				if err := peerStream.Send(forwardReq); err != nil {
					updateNodeStatus(&ps.RWMutex, successfulNodes, member.Addr, false)
					glog.Errorf("failed to send forward request: %v", err)
					return nil
				}

				if err := ps.runForwardSubscriber(errGCtx, peerStream, member.Addr); err != nil {
					updateNodeStatus(&ps.RWMutex, successfulNodes, member.Addr, false)
					glog.Errorf("failed to run forward subscriber: %v", err)
					return nil
				}

				updateNodeStatus(&ps.RWMutex, successfulNodes, member.Addr, true)
				glog.Infof("[import] Successfully connected and streamed data to node: %v", member.Addr)
				return nil
			})
		}
	}

	eg.Go(func() error {
		defer ps.close()
		defer func() {
			if err := stream.SendAndClose(&api.StreamExtSnapshotResponse{}); err != nil {
				glog.Errorf("[import] failed to send close on in: %v", err)
			}
		}()
		if err := ps.handlePublisher(errGCtx, stream); err != nil {
			return err
		}

		return nil
	})

	if err := eg.Wait(); err != nil {
		return err
	}

	// If this node is the leader and fails to reach a majority of nodes, we return an error.
	// This ensures that the data is reliably received by enough nodes before proceeding.
	if forward && !checkMajority(successfulNodes) {
		glog.Error("[import] Majority of nodes failed to receive data.")
		return errors.New("failed to send data to majority of the nodes")
	}

	return nil
}

func updateNodeStatus(ps *sync.RWMutex, successfulNodes map[string]bool, addr string, status bool) {
	ps.Lock()
	successfulNodes[addr] = status
	ps.Unlock()
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
