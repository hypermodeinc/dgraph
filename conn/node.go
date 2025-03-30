/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package conn

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.etcd.io/etcd/raft/v3"
	"go.etcd.io/etcd/raft/v3/raftpb"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/badger/v4/y"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/raftwal"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var (
	// ErrNoNode is returned when no node has been set up.
	ErrNoNode = errors.Errorf("No node has been set up yet")
)

// Node represents a node participating in the RAFT protocol.
type Node struct {
	x.SafeMutex

	// Applied is used to keep track of the applied RAFT proposals.
	// The stages are proposed -> committed (accepted by cluster) ->
	// applied (to PL) -> synced (to BadgerDB).
	// This needs to be 64 bit aligned for atomics to work on 32 bit machine.
	Applied y.WaterMark

	joinLock sync.Mutex

	// Used to keep track of lin read requests.
	requestCh chan linReadReq

	// SafeMutex is for fields which can be changed after init.
	_confState *raftpb.ConfState
	_raft      raft.Node

	// Fields which are never changed after init.
	StartTime       time.Time
	Cfg             *raft.Config
	MyAddr          string
	Id              uint64
	peers           map[uint64]string
	confChanges     map[uint64]chan error
	messages        chan sendmsg
	RaftContext     *pb.RaftContext
	Store           *raftwal.DiskStorage
	Rand            *rand.Rand
	tlsClientConfig *tls.Config

	Proposals proposals

	heartbeatsOut int64
	heartbeatsIn  int64
}

// NewNode returns a new Node instance.
func NewNode(rc *pb.RaftContext, store *raftwal.DiskStorage, tlsConfig *tls.Config) *Node {
	snap, err := store.Snapshot()
	x.Check(err)

	n := &Node{
		StartTime: time.Now(),
		Id:        rc.Id,
		MyAddr:    rc.Addr,
		Store:     store,
		Cfg: &raft.Config{
			ID:                       rc.Id,
			ElectionTick:             20, // 2s if we call Tick() every 100 ms.
			HeartbeatTick:            1,  // 100ms if we call Tick() every 100 ms.
			Storage:                  store,
			MaxInflightMsgs:          256,
			MaxSizePerMsg:            256 << 10, // 256 KB should allow more batching.
			MaxCommittedSizePerReady: 64 << 20,  // Avoid loading entire Raft log into memory.
			// We don't need lease based reads. They cause issues because they
			// require CheckQuorum to be true, and that causes a lot of issues
			// for us during cluster bootstrapping and later. A seemingly
			// healthy cluster would just cause leader to step down due to
			// "inactive" quorum, and then disallow anyone from becoming leader.
			// So, let's stick to default options.  Let's achieve correctness,
			// then we achieve performance. Plus, for the Dgraph alphas, we'll
			// be soon relying only on Timestamps for blocking reads and
			// achieving linearizability, than checking quorums (Zero would
			// still check quorums).
			ReadOnlyOption: raft.ReadOnlySafe,
			// When a disconnected node joins back, it forces a leader change,
			// as it starts with a higher term, as described in Raft thesis (not
			// the paper) in section 9.6. This setting can avoid that by only
			// increasing the term, if the node has a good chance of becoming
			// the leader.
			PreVote: true,

			// We can explicitly set Applied to the first index in the Raft log,
			// so it does not derive it separately, thus avoiding a crash when
			// the Applied is set to below snapshot index by Raft.
			// In case this is a new Raft log, first would be 1, and therefore
			// Applied would be zero, hence meeting the condition by the library
			// that Applied should only be set during a restart.
			//
			// Update: Set the Applied to the latest snapshot, because it seems
			// like somehow the first index can be out of sync with the latest
			// snapshot.
			Applied: snap.Metadata.Index,

			Logger: &x.ToGlog{},
		},
		// processConfChange etc are not throttled so some extra delta, so that we don't
		// block tick when applyCh is full
		Applied:     y.WaterMark{Name: "Applied watermark"},
		RaftContext: rc,
		//nolint:gosec // random node id generator does not require cryptographic precision
		Rand:            rand.New(&lockedSource{src: rand.NewSource(time.Now().UnixNano())}),
		confChanges:     make(map[uint64]chan error),
		messages:        make(chan sendmsg, 100),
		peers:           make(map[uint64]string),
		requestCh:       make(chan linReadReq, 100),
		tlsClientConfig: tlsConfig,
	}
	n.Applied.Init(nil)
	// This should match up to the Applied index set above.
	n.Applied.SetDoneUntil(n.Cfg.Applied)
	glog.Infof("Setting raft.Config to: %+v", n.Cfg)
	return n
}

// ReportRaftComms periodically prints the state of the node (heartbeats in and out).
func (n *Node) ReportRaftComms() {
	if !glog.V(3) {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for range ticker.C {
		out := atomic.SwapInt64(&n.heartbeatsOut, 0)
		in := atomic.SwapInt64(&n.heartbeatsIn, 0)
		glog.Infof("RaftComm: [%#x] Heartbeats out: %d, in: %d", n.Id, out, in)
	}
}

// SetRaft would set the provided raft.Node to this node.
// It would check fail if the node is already set.
func (n *Node) SetRaft(r raft.Node) {
	n.Lock()
	defer n.Unlock()
	x.AssertTrue(n._raft == nil)
	n._raft = r
}

// Raft would return back the raft.Node stored in the node.
func (n *Node) Raft() raft.Node {
	n.RLock()
	defer n.RUnlock()
	return n._raft
}

// SetConfState would store the latest ConfState generated by ApplyConfChange.
func (n *Node) SetConfState(cs *raftpb.ConfState) {
	glog.Infof("Setting conf state to %+v\n", cs)
	n.Lock()
	defer n.Unlock()
	n._confState = cs
}

// DoneConfChange marks a configuration change as done and sends the given error to the
// config channel.
func (n *Node) DoneConfChange(id uint64, err error) {
	n.Lock()
	defer n.Unlock()
	ch, has := n.confChanges[id]
	if !has {
		return
	}
	delete(n.confChanges, id)
	ch <- err
}

//nolint:gosec // random node id generator does not require cryptographic precision
func (n *Node) storeConfChange(che chan error) uint64 {
	n.Lock()
	defer n.Unlock()
	id := rand.Uint64()
	_, has := n.confChanges[id]
	for has {
		id = rand.Uint64()
		_, has = n.confChanges[id]
	}
	n.confChanges[id] = che
	return id
}

// ConfState would return the latest ConfState stored in node.
func (n *Node) ConfState() *raftpb.ConfState {
	n.RLock()
	defer n.RUnlock()
	return n._confState
}

// Peer returns the address of the peer with the given id.
func (n *Node) Peer(pid uint64) (string, bool) {
	n.RLock()
	defer n.RUnlock()
	addr, ok := n.peers[pid]
	return addr, ok
}

// SetPeer sets the address of the peer with the given id. The address must not be empty.
func (n *Node) SetPeer(pid uint64, addr string) {
	x.AssertTruef(addr != "", "SetPeer for peer %d has empty addr.", pid)
	n.Lock()
	defer n.Unlock()
	n.peers[pid] = addr
}

// Send sends the given RAFT message from this node.
func (n *Node) Send(msg *raftpb.Message) {
	x.AssertTruef(n.Id != msg.To, "Sending message to itself")
	data, err := msg.Marshal()
	x.Check(err)

	if glog.V(2) {
		switch msg.Type {
		case raftpb.MsgHeartbeat, raftpb.MsgHeartbeatResp:
			atomic.AddInt64(&n.heartbeatsOut, 1)
		case raftpb.MsgReadIndex, raftpb.MsgReadIndexResp:
		case raftpb.MsgApp, raftpb.MsgAppResp:
		case raftpb.MsgProp:
		default:
			glog.Infof("RaftComm: [%#x] Sending message of type %s to %#x", msg.From, msg.Type, msg.To)
		}
	}
	// As long as leadership is stable, any attempted Propose() calls should be reflected in the
	// next raft.Ready.Messages. Leaders will send MsgApps to the followers; followers will send
	// MsgProp to the leader. It is up to the transport layer to get those messages to their
	// destination. If a MsgApp gets dropped by the transport layer, it will get retried by raft
	// (i.e. it will appear in a future Ready.Messages), but MsgProp will only be sent once. During
	// leadership transitions, proposals may get dropped even if the network is reliable.
	//
	// We can't do a select default here. The messages must be sent to the channel, otherwise we
	// should block until the channel can accept these messages. BatchAndSendMessages would take
	// care of dropping messages which can't be sent due to network issues to the corresponding
	// node. But, we shouldn't take the liberty to do that here. It would take us more time to
	// repropose these dropped messages anyway, than to block here a bit waiting for the messages
	// channel to clear out.
	n.messages <- sendmsg{to: msg.To, data: data}
}

// Snapshot returns the current snapshot.
func (n *Node) Snapshot() (raftpb.Snapshot, error) {
	if n == nil || n.Store == nil {
		return raftpb.Snapshot{}, errors.New("Uninitialized node or raft store")
	}
	return n.Store.Snapshot()
}

// SaveToStorage saves the hard state, entries, and snapshot to persistent storage, in that order.
func (n *Node) SaveToStorage(h *raftpb.HardState, es []raftpb.Entry, s *raftpb.Snapshot) {
	for {
		if err := n.Store.Save(h, es, s); err != nil {
			glog.Errorf("While trying to save Raft update: %v. Retrying...", err)
		} else {
			return
		}
	}
}

// PastLife returns the index of the snapshot before the restart (if any) and whether there was
// a previous state that should be recovered after a restart.
func (n *Node) PastLife() (uint64, bool, error) {
	var (
		sp      raftpb.Snapshot
		idx     uint64
		restart bool
		rerr    error
	)
	sp, rerr = n.Store.Snapshot()
	if rerr != nil {
		return 0, false, rerr
	}
	if !raft.IsEmptySnap(sp) {
		glog.Infof("Found Snapshot.Metadata: %+v\n", sp.Metadata)
		restart = true
		idx = sp.Metadata.Index
	}

	var hd raftpb.HardState
	hd, rerr = n.Store.HardState()
	if rerr != nil {
		return 0, false, rerr
	}
	if !raft.IsEmptyHardState(hd) {
		glog.Infof("Found hardstate: %+v\n", hd)
		restart = true
	}

	num := n.Store.NumEntries()
	glog.Infof("Group %d found %d entries\n", n.RaftContext.Group, num)
	// We'll always have at least one entry.
	if num > 1 {
		restart = true
	}
	return idx, restart, nil
}

const (
	messageBatchSoftLimit = 10e6
)

type stream struct {
	msgCh chan []byte
	alive int32
}

// BatchAndSendMessages sends messages in batches.
func (n *Node) BatchAndSendMessages() {
	batches := make(map[uint64]*bytes.Buffer)
	streams := make(map[uint64]*stream)

	for {
		totalSize := 0
		sm := <-n.messages
	slurp_loop:
		for {
			var buf *bytes.Buffer
			if b, ok := batches[sm.to]; !ok {
				buf = new(bytes.Buffer)
				batches[sm.to] = buf
			} else {
				buf = b
			}
			totalSize += 4 + len(sm.data)
			x.Check(binary.Write(buf, binary.LittleEndian, uint32(len(sm.data))))
			x.Check2(buf.Write(sm.data))

			if totalSize > messageBatchSoftLimit {
				// We limit the batch size, but we aren't pushing back on
				// n.messages, because the loop below spawns a goroutine
				// to do its dirty work.  This is good because right now
				// (*node).send fails(!) if the channel is full.
				break
			}

			select {
			case sm = <-n.messages:
			default:
				break slurp_loop
			}
		}

		for to, buf := range batches {
			if buf.Len() == 0 {
				continue
			}
			s, ok := streams[to]
			if !ok || atomic.LoadInt32(&s.alive) <= 0 {
				s = &stream{
					msgCh: make(chan []byte, 100),
					alive: 1,
				}
				go n.streamMessages(to, s)
				streams[to] = s
			}
			data := make([]byte, buf.Len())
			copy(data, buf.Bytes())
			buf.Reset()

			select {
			case s.msgCh <- data:
			default:
			}
		}
	}
}

func (n *Node) streamMessages(to uint64, s *stream) {
	defer atomic.StoreInt32(&s.alive, 0)

	// Exit after this deadline. Let BatchAndSendMessages create another goroutine, if needed.
	// Let's set the deadline to 10s because if we increase it, then it takes longer to recover from
	// a partition and get a new leader.
	deadline := time.Now().Add(10 * time.Second)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var logged int
	for range ticker.C { // Don't do this in an busy-wait loop, use a ticker.
		// doSendMessage would block doing a stream. So, time.Now().After is
		// only there to avoid a busy-wait.
		if err := n.doSendMessage(to, s.msgCh); err != nil {
			// Update lastLog so we print error only a few times if we are not able to connect.
			// Otherwise, the log is polluted with repeated errors.
			if logged == 0 {
				glog.Warningf("Unable to send message to peer: %#x. Error: %v", to, err)
				logged++
			}
		}
		if time.Now().After(deadline) {
			return
		}
	}
}

func (n *Node) doSendMessage(to uint64, msgCh chan []byte) error {
	addr, has := n.Peer(to)
	if !has {
		return errors.Errorf("Do not have address of peer %#x", to)
	}
	pool, err := GetPools().Get(addr)
	if err != nil {
		return err
	}

	c := pb.NewRaftClient(pool.Get())
	ctx, span := otel.Tracer("").Start(context.Background(),
		fmt.Sprintf("RaftMessage-%d-to-%d", n.Id, to))
	defer span.End()

	mc, err := c.RaftMessage(ctx)
	if err != nil {
		return err
	}

	var packets, lastPackets uint64
	slurp := func(batch *pb.RaftBatch) {
		for {
			if len(batch.Payload.Data) > messageBatchSoftLimit {
				return
			}
			select {
			case data := <-msgCh:
				batch.Payload.Data = append(batch.Payload.Data, data...)
				packets++
			default:
				return
			}
		}
	}

	ctx = mc.Context()

	fastTick := time.NewTicker(5 * time.Second)
	defer fastTick.Stop()

	ticker := time.NewTicker(3 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case data := <-msgCh:
			batch := &pb.RaftBatch{
				Context: n.RaftContext,
				Payload: &api.Payload{Data: data},
			}
			slurp(batch) // Pick up more entries from msgCh, if present.
			span.AddEvent(fmt.Sprintf("[to: %x] [Packets: %d] Sending data of length: %d.",
				to, packets, len(batch.Payload.Data)))
			if packets%10000 == 0 {
				glog.V(2).Infof("[to: %x] [Packets: %d] Sending data of length: %d.",
					to, packets, len(batch.Payload.Data))
			}
			packets++
			if err := mc.Send(batch); err != nil {
				span.AddEvent(fmt.Sprintf("Error while mc.Send: %v", err))
				glog.Errorf("[to: %x] Error while mc.Send: %v", to, err)
				switch {
				case strings.Contains(err.Error(), "TransientFailure"):
					glog.Warningf("Reporting node: %d addr: %s as unreachable.", to, pool.Addr)
					n.Raft().ReportUnreachable(to)
					pool.SetUnhealthy()
				default:
				}
				// We don't need to do anything if we receive any error while sending message.
				// RAFT would automatically retry.
				return err
			}
		case <-fastTick.C:
			// We use this ticker, because during network partitions, mc.Send is
			// unable to actually send packets, and also does not complain about
			// them. We could have potentially used the separately tracked
			// heartbeats to check this, but what we have observed is that
			// incoming traffic might be OK, but outgoing might not be. So, this
			// is a better way for us to verify whether this particular outbound
			// connection is valid or not.
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_, err := c.IsPeer(ctx, n.RaftContext)
			cancel()
			if err != nil {
				glog.Errorf("Error while calling IsPeer %v. Reporting %x as unreachable.", err, to)
				n.Raft().ReportUnreachable(to)
				pool.SetUnhealthy()
				return errors.Wrapf(err, "while calling IsPeer %x", to)
			}
		case <-ticker.C:
			if lastPackets == packets {
				span.AddEvent(fmt.Sprintf("No activity for a while [Packets == %d]. Closing connection.", packets))
				return mc.CloseSend()
			}
			lastPackets = packets
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Connect connects the node and makes its peerPool refer to the constructed pool and address
// (possibly updating ourselves from the old address.)  (Unless pid is ourselves, in which
// case this does nothing.)
func (n *Node) Connect(pid uint64, addr string) {
	if pid == n.Id {
		return
	}
	if paddr, ok := n.Peer(pid); ok && paddr == addr {
		// Already connected.
		return
	}
	// Here's what we do.  Right now peerPool maps peer node id's to addr values.  If
	// a *pool can be created, good, but if not, we still create a peerPoolEntry with
	// a nil *pool.
	if addr == n.MyAddr {
		// TODO: Note this fact in more general peer health info somehow.
		glog.Infof("Peer %d claims same host as me\n", pid)
		n.SetPeer(pid, addr)
		return
	}
	GetPools().Connect(addr, n.tlsClientConfig)
	n.SetPeer(pid, addr)
}

// DeletePeer deletes the record of the peer with the given id.
func (n *Node) DeletePeer(pid uint64) {
	if pid == n.Id {
		return
	}
	n.Lock()
	defer n.Unlock()
	delete(n.peers, pid)
}

var errInternalRetry = errors.New("Retry proposal again")

func (n *Node) proposeConfChange(ctx context.Context, conf raftpb.ConfChange) error {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	ch := make(chan error, 1)
	id := n.storeConfChange(ch)
	// TODO: Delete id from the map.
	conf.ID = id
	if err := n.Raft().ProposeConfChange(cctx, conf); err != nil {
		if cctx.Err() != nil {
			return errInternalRetry
		}
		glog.Warningf("Error while proposing conf change: %v", err)
		return err
	}
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-cctx.Done():
		return errInternalRetry
	}
}

func (n *Node) addToCluster(ctx context.Context, rc *pb.RaftContext) error {
	pid := rc.Id
	rc.SnapshotTs = 0
	rcBytes, err := proto.Marshal(rc)
	x.Check(err)

	cc := raftpb.ConfChange{
		Type:    raftpb.ConfChangeAddNode,
		NodeID:  pid,
		Context: rcBytes,
	}
	if rc.IsLearner {
		cc.Type = raftpb.ConfChangeAddLearnerNode
	}

	err = errInternalRetry
	for err == errInternalRetry {
		glog.Infof("Trying to add %#x to cluster. Addr: %v\n", pid, rc.Addr)
		glog.Infof("Current confstate at %#x: %+v\n", n.Id, n.ConfState())
		err = n.proposeConfChange(ctx, cc)
	}
	return err
}

// ProposePeerRemoval proposes a new configuration with the peer with the given id removed.
func (n *Node) ProposePeerRemoval(ctx context.Context, id uint64) error {
	if n.Raft() == nil {
		return ErrNoNode
	}
	if _, ok := n.Peer(id); !ok && id != n.RaftContext.Id {
		return errors.Errorf("Node %#x not part of group", id)
	}
	cc := raftpb.ConfChange{
		Type:   raftpb.ConfChangeRemoveNode,
		NodeID: id,
	}
	err := errInternalRetry
	for err == errInternalRetry {
		err = n.proposeConfChange(ctx, cc)
	}
	return err
}

type linReadReq struct {
	// A one-shot chan which we send a raft index upon.
	indexCh chan<- uint64
}

var errReadIndex = errors.Errorf(
	"Cannot get linearized read (time expired or no configured leader)")

var readIndexOk, readIndexTotal uint64

// WaitLinearizableRead waits until a linearizable read can be performed.
func (n *Node) WaitLinearizableRead(ctx context.Context) error {
	span := trace.SpanFromContext(ctx)
	span.AddEvent("WaitLinearizableRead")

	if num := atomic.AddUint64(&readIndexTotal, 1); num%1000 == 0 {
		glog.V(2).Infof("ReadIndex Total: %d\n", num)
	}
	indexCh := make(chan uint64, 1)
	select {
	case n.requestCh <- linReadReq{indexCh: indexCh}:
		span.AddEvent("Pushed to requestCh")
	case <-ctx.Done():
		span.AddEvent("Context expired")
		return ctx.Err()
	}

	select {
	case index := <-indexCh:
		span.AddEvent(fmt.Sprintf("Received index %d", index))
		if index == 0 {
			return errReadIndex
		} else if num := atomic.AddUint64(&readIndexOk, 1); num%1000 == 0 {
			glog.V(2).Infof("ReadIndex OK: %d\n", num)
		}
		err := n.Applied.WaitForMark(ctx, index)
		span.AddEvent(fmt.Sprintf("Error from Applied.WaitForMark: %v", err))
		return err
	case <-ctx.Done():
		span.AddEvent("Context expired")
		return ctx.Err()
	}
}

// RunReadIndexLoop runs the RAFT index in a loop.
func (n *Node) RunReadIndexLoop(closer *z.Closer, readStateCh <-chan raft.ReadState) {
	defer closer.Done()
	readIndex := func(activeRctx []byte) (uint64, error) {
		// Read Request can get rejected then we would wait indefinitely on the channel
		// so have a timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := n.Raft().ReadIndex(ctx, activeRctx); err != nil {
			glog.Errorf("Error while trying to call ReadIndex: %v\n", err)
			return 0, err
		}

	again:
		select {
		case <-closer.HasBeenClosed():
			return 0, errors.New("Closer has been called")
		case rs := <-readStateCh:
			if !bytes.Equal(activeRctx, rs.RequestCtx) {
				glog.V(3).Infof("Read state: %x != requested %x", rs.RequestCtx, activeRctx)
				goto again
			}
			return rs.Index, nil
		case <-ctx.Done():
			glog.Warningf("[%#x] Read index context timed out\n", n.Id)
			return 0, errInternalRetry
		}
	} // end of readIndex func

	// We maintain one linearizable ReadIndex request at a time.  Others wait queued behind
	// requestCh.
	requests := []linReadReq{}
	for {
		select {
		case <-closer.HasBeenClosed():
			return
		case <-readStateCh:
			// Do nothing, discard ReadState as we don't have any pending ReadIndex requests.
		case req := <-n.requestCh:
		slurpLoop:
			for {
				requests = append(requests, req)
				select {
				case req = <-n.requestCh:
				default:
					break slurpLoop
				}
			}
			// Create one activeRctx slice for the read index, even if we have to call readIndex
			// repeatedly. That way, we can process the requests as soon as we encounter the first
			// activeRctx. This is better than flooding readIndex with a new activeRctx on each
			// call, causing more unique traffic and further delays in request processing.
			activeRctx := make([]byte, 8)
			x.Check2(n.Rand.Read(activeRctx))
			glog.V(4).Infof("Request readctx: %#x", activeRctx)
			for {
				index, err := readIndex(activeRctx)
				if err == errInternalRetry {
					continue
				}
				if err != nil {
					index = 0
					glog.Errorf("[%#x] While trying to do lin read index: %v", n.Id, err)
				}
				for _, req := range requests {
					req.indexCh <- index
				}
				break
			}
			requests = requests[:0]
		}
	}
}

func (n *Node) joinCluster(ctx context.Context, rc *pb.RaftContext) (*api.Payload, error) {
	// Only process one JoinCluster request at a time.
	n.joinLock.Lock()
	defer n.joinLock.Unlock()

	// Check that the new node is from the same group as me.
	if rc.Group != n.RaftContext.Group {
		return nil, errors.Errorf("Raft group mismatch")
	}
	// Also check that the new node is not me.
	if rc.Id == n.RaftContext.Id {
		return nil, errors.Errorf("REUSE_RAFTID: Raft ID duplicates mine: %+v", rc)
	}

	// Check that the new node is not already part of the group.
	if addr, ok := n.Peer(rc.Id); ok && rc.Addr != addr {
		// There exists a healthy connection to server with same id.
		if _, err := GetPools().Get(addr); err == nil {
			return &api.Payload{}, errors.Errorf(
				"REUSE_ADDR: IP Address same as existing peer: %s", addr)
		}
	}
	n.Connect(rc.Id, rc.Addr)

	err := n.addToCluster(context.Background(), rc)
	glog.Infof("[%#x] Done joining cluster with err: %v", rc.Id, err)
	return &api.Payload{}, err
}
