/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package conn

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.etcd.io/etcd/raft/v3/raftpb"
	"go.opentelemetry.io/otel/trace"

	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/x"
)

type sendmsg struct {
	to   uint64
	data []byte
}

type lockedSource struct {
	lk  sync.Mutex
	src rand.Source
}

func (r *lockedSource) Int63() int64 {
	r.lk.Lock()
	defer r.lk.Unlock()
	return r.src.Int63()
}

func (r *lockedSource) Seed(seed int64) {
	r.lk.Lock()
	defer r.lk.Unlock()
	r.src.Seed(seed)
}

// ProposalCtx stores the context for a proposal with extra information.
type ProposalCtx struct {
	Found uint32
	ErrCh chan error
	Ctx   context.Context
}

type proposals struct {
	sync.RWMutex
	all map[uint64]*ProposalCtx
}

func (p *proposals) Store(key uint64, pctx *ProposalCtx) bool {
	if key == 0 {
		return false
	}
	p.Lock()
	defer p.Unlock()
	if p.all == nil {
		p.all = make(map[uint64]*ProposalCtx)
	}
	if _, has := p.all[key]; has {
		return false
	}
	p.all[key] = pctx
	return true
}

func (p *proposals) Ctx(key uint64) context.Context {
	if pctx := p.Get(key); pctx != nil {
		return pctx.Ctx
	}
	return context.Background()
}

func (p *proposals) Get(key uint64) *ProposalCtx {
	p.RLock()
	defer p.RUnlock()
	return p.all[key]
}

func (p *proposals) Delete(key uint64) {
	if key == 0 {
		return
	}
	p.Lock()
	defer p.Unlock()
	delete(p.all, key)
}

func (p *proposals) Done(key uint64, err error) {
	if key == 0 {
		return
	}
	p.Lock()
	defer p.Unlock()
	pd, has := p.all[key]
	if !has {
		// If we assert here, there would be a race condition between a context
		// timing out, and a proposal getting applied immediately after. That
		// would cause assert to fail. So, don't assert.
		return
	}
	delete(p.all, key)
	pd.ErrCh <- err
}

// RaftServer is a wrapper around node that implements the Raft service.
type RaftServer struct {
	m    sync.RWMutex
	node *Node
	// embedding the pb.UnimplementedRaftServer struct to ensure forward compatibility of the server.
	pb.UnimplementedRaftServer
}

// UpdateNode safely updates the node.
func (w *RaftServer) UpdateNode(n *Node) {
	w.m.Lock()
	defer w.m.Unlock()
	w.node = n
}

// GetNode safely retrieves the node.
func (w *RaftServer) GetNode() *Node {
	w.m.RLock()
	defer w.m.RUnlock()
	return w.node
}

// NewRaftServer returns a pointer to a new RaftServer instance.
func NewRaftServer(n *Node) *RaftServer {
	return &RaftServer{node: n}
}

// IsPeer checks whether this node is a peer of the node sending the request.
func (w *RaftServer) IsPeer(ctx context.Context, rc *pb.RaftContext) (
	*pb.PeerResponse, error) {
	node := w.GetNode()
	if node == nil || node.Raft() == nil {
		return &pb.PeerResponse{}, ErrNoNode
	}

	confState := node.ConfState()

	if confState == nil {
		return &pb.PeerResponse{}, nil
	}

	for _, raftIdx := range confState.Voters {
		if rc.Id == raftIdx {
			return &pb.PeerResponse{Status: true}, nil
		}
	}
	return &pb.PeerResponse{}, nil
}

// JoinCluster handles requests to join the cluster.
func (w *RaftServer) JoinCluster(ctx context.Context,
	rc *pb.RaftContext) (*api.Payload, error) {
	if ctx.Err() != nil {
		return &api.Payload{}, ctx.Err()
	}

	node := w.GetNode()
	if node == nil || node.Raft() == nil {
		return nil, ErrNoNode
	}

	return node.joinCluster(ctx, rc)
}

// RaftMessage handles RAFT messages.
func (w *RaftServer) RaftMessage(server pb.Raft_RaftMessageServer) error {
	ctx := server.Context()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	span := trace.SpanFromContext(ctx)

	node := w.GetNode()
	if node == nil || node.Raft() == nil {
		return ErrNoNode
	}
	span.AddEvent(fmt.Sprintf("Stream server is node %d", node.Id))

	var rc *pb.RaftContext
	raft := node.Raft()
	step := func(data []byte) error {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()

		for idx := 0; idx < len(data); {
			x.AssertTruef(len(data[idx:]) >= 4,
				"Slice left of size: %v. Expected at least 4.", len(data[idx:]))

			sz := int(binary.LittleEndian.Uint32(data[idx : idx+4]))
			idx += 4
			msg := raftpb.Message{}
			if idx+sz > len(data) {
				return errors.Errorf(
					"Invalid query. Specified size %v overflows slice [%v,%v)\n",
					sz, idx, len(data))
			}
			if err := msg.Unmarshal(data[idx : idx+sz]); err != nil {
				x.Check(err)
			}
			// This should be done in order, and not via a goroutine.
			// Step can block forever. See: https://github.com/etcd-io/etcd/issues/10585
			// So, add a context with timeout to allow it to get out of the blockage.
			if glog.V(2) {
				switch msg.Type {
				case raftpb.MsgHeartbeat, raftpb.MsgHeartbeatResp:
					atomic.AddInt64(&node.heartbeatsIn, 1)
				case raftpb.MsgReadIndex, raftpb.MsgReadIndexResp:
				case raftpb.MsgApp, raftpb.MsgAppResp:
				case raftpb.MsgProp:
				default:
					glog.Infof("RaftComm: [%#x] Received msg of type: %s from %#x",
						msg.To, msg.Type, msg.From)
				}
			}
			if err := raft.Step(ctx, msg); err != nil {
				glog.Warningf("Error while raft.Step from %#x: %v. Closing RaftMessage stream.",
					rc.GetId(), err)
				return errors.Wrapf(err, "error while raft.Step from %#x", rc.GetId())
			}
			idx += sz
		}
		return nil
	}

	for loop := 1; ; loop++ {
		batch, err := server.Recv()
		if err != nil {
			return err
		}
		if loop%1e6 == 0 {
			glog.V(2).Infof("%d messages received by %#x from %#x", loop, node.Id, rc.GetId())
		}
		if loop == 1 {
			rc = batch.GetContext()
			span.AddEvent(fmt.Sprintf("Stream from %#x", rc.GetId()))
			if rc != nil {
				node.Connect(rc.Id, rc.Addr)
			}
		}
		if batch.Payload == nil {
			continue
		}
		data := batch.Payload.Data
		if err := step(data); err != nil {
			return err
		}
	}
}

// Heartbeat rpc call is used to check connection with other workers after worker
// tcp server for this instance starts.
func (w *RaftServer) Heartbeat(_ *api.Payload, stream pb.Raft_HeartbeatServer) error {
	ticker := time.NewTicker(echoDuration)
	defer ticker.Stop()

	node := w.GetNode()
	if node == nil {
		return ErrNoNode
	}
	// TODO(Aman): Send list of ongoing tasks as part of heartbeats.
	// Currently, there is a cyclic dependency of imports worker -> conn -> worker.
	info := pb.HealthInfo{
		Instance: "alpha",
		Address:  node.MyAddr,
		Group:    strconv.Itoa(int(node.RaftContext.GetGroup())),
		Version:  x.Version(),
		Uptime:   int64(time.Since(node.StartTime) / time.Second),
	}
	if info.Group == "0" {
		info.Instance = "zero"
	}

	ctx := stream.Context()

	for {
		info.Uptime = int64(time.Since(node.StartTime) / time.Second)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := stream.Send(&info); err != nil {
				return err
			}
		}
	}
}
