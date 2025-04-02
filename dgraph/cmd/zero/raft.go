/*
 * SPDX-FileCopyrightText:  Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package zero

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	farm "github.com/dgryski/go-farm"
	"github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"go.etcd.io/etcd/raft/v3"
	"go.etcd.io/etcd/raft/v3/raftpb"
	ostats "go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.opentelemetry.io/otel"
	attribute "go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v24/conn"
	"github.com/hypermodeinc/dgraph/v24/ee/audit"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/x"
)

const (
	raftDefaults = "idx=1; learner=false;"
)

var proposalKey uint64

type node struct {
	*conn.Node
	server *Server
	ctx    context.Context
	closer *z.Closer // to stop Run.

	// The last timestamp when this Zero was able to reach quorum.
	mu         sync.RWMutex
	lastQuorum time.Time
}

func (n *node) amLeader() bool {
	if n.Raft() == nil {
		return false
	}
	r := n.Raft()
	return r.Status().Lead == r.Status().ID
}

func (n *node) AmLeader() bool {
	// Return false if the node is not the leader. Otherwise, check the lastQuorum as well.
	if !n.amLeader() {
		return false
	}
	// This node must be the leader, but must also be an active member of
	// the cluster, and not hidden behind a partition. Basically, if this
	// node was the leader and goes behind a partition, it would still
	// think that it is indeed the leader for the duration mentioned below.
	n.mu.RLock()
	defer n.mu.RUnlock()
	return time.Since(n.lastQuorum) <= 5*time.Second
}

// {2 bytes Node ID} {4 bytes for random} {2 bytes zero}
func (n *node) initProposalKey(id uint64) error {
	x.AssertTrue(id != 0)
	var err error
	proposalKey, err = x.ProposalKey(n.Id)
	if err != nil {
		return err
	}
	return nil
}

func (n *node) uniqueKey() uint64 {
	return atomic.AddUint64(&proposalKey, 1)
}

var errInternalRetry = errors.New("Retry Raft proposal internally")

// proposeAndWait makes a proposal to the quorum for Group Zero and waits for it to be accepted by
// the group before returning. It is safe to call concurrently.
func (n *node) proposeAndWait(ctx context.Context, proposal *pb.ZeroProposal) error {
	switch {
	case n.Raft() == nil:
		return errors.Errorf("Raft isn't initialized yet.")
	case ctx.Err() != nil:
		return ctx.Err()
	case !n.AmLeader():
		// Do this check upfront. Don't do this inside propose for reasons explained below.
		return errors.Errorf("Not Zero leader. Aborting proposal: %+v", proposal)
	}

	// We could consider adding a wrapper around the user proposal, so we can access any key-values.
	// Something like this:
	// https://github.com/golang/go/commit/5d39260079b5170e6b4263adb4022cc4b54153c4
	span := trace.SpanFromContext(ctx)
	// Overwrite ctx, so we no longer enforce the timeouts or cancels from ctx.
	ctx = trace.ContextWithSpan(context.Background(), span)

	stop := x.SpanTimer(span, "n.proposeAndWait")
	defer stop()

	// propose runs in a loop. So, we should not do any checks inside, including n.AmLeader. This is
	// to avoid the scenario where the first proposal times out and the second one gets returned
	// due to node no longer being the leader. In this scenario, the first proposal can still get
	// accepted by Raft, causing a txn violation later for us, because we assumed that the proposal
	// did not go through.
	propose := func(timeout time.Duration) error {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		errCh := make(chan error, 1)
		pctx := &conn.ProposalCtx{
			ErrCh: errCh,
			// Don't use the original context, because that's not what we're passing to Raft.
			Ctx: cctx,
		}
		key := n.uniqueKey()
		// unique key is randomly generated key and could have collision.
		// This is to ensure that even if collision occurs, we retry.
		for !n.Proposals.Store(key, pctx) {
			glog.Warningf("Found existing proposal with key: [%v]", key)
			key = n.uniqueKey()
		}
		defer n.Proposals.Delete(key)
		span.AddEvent("Proposing with key: %d. Timeout: %v", trace.WithAttributes(
			attribute.Int64("key", int64(key)),
			attribute.Int64("timeout", int64(timeout))))

		sz := proto.Size(proposal)
		data := make([]byte, 8+sz)
		binary.BigEndian.PutUint64(data[:8], key)
		_, err := x.MarshalToSizedBuffer(data[8:], proposal)
		if err != nil {
			return err
		}
		data = data[:8+sz]
		// Propose the change.
		if err := n.Raft().Propose(cctx, data); err != nil {
			span.AddEvent("Error while proposing via Raft: %v", trace.WithAttributes(
				attribute.String("error", err.Error())))
			return errors.Wrapf(err, "While proposing")
		}

		// Wait for proposal to be applied or timeout.
		select {
		case err := <-errCh:
			// We arrived here by a call to n.props.Done().
			return err
		case <-cctx.Done():
			span.AddEvent(fmt.Sprintf("Internal context timeout %s. Will retry...", timeout))
			return errInternalRetry
		}
	}

	// Some proposals can be stuck if leader change happens. For e.g. MsgProp message from follower
	// to leader can be dropped/end up appearing with empty Data in CommittedEntries.
	// Having a timeout here prevents the mutation being stuck forever in case they don't have a
	// timeout. We should always try with a timeout and optionally retry.
	err := errInternalRetry
	timeout := 4 * time.Second
	for err == errInternalRetry {
		err = propose(timeout)
		timeout *= 2 // Exponential backoff
		if timeout > time.Minute {
			timeout = 32 * time.Second
		}
	}
	return err
}

var (
	errInvalidProposal     = errors.New("Invalid group proposal")
	errTabletAlreadyServed = errors.New("Tablet is already being served")
)

func newGroup() *pb.Group {
	return &pb.Group{
		Members: make(map[uint64]*pb.Member),
		Tablets: make(map[string]*pb.Tablet),
	}
}

func (n *node) handleMemberProposal(member *pb.Member) error {
	n.server.AssertLock()
	state := n.server.state

	m := n.server.member(member.Addr)
	// Ensures that different nodes don't have same address.
	if m != nil && (m.Id != member.Id || m.GroupId != member.GroupId) {
		return errors.Errorf("Found another member %d with same address: %v", m.Id, m.Addr)
	}
	if member.GroupId == 0 {
		state.Zeros[member.Id] = member
		if member.Leader {
			// Unset leader flag for other nodes, there can be only one
			// leader at a time.
			for _, m := range state.Zeros {
				if m.Id != member.Id {
					m.Leader = false
				}
			}
		}
		return nil
	}
	group := state.Groups[member.GroupId]
	if group == nil {
		group = newGroup()
		state.Groups[member.GroupId] = group
	}
	m, has := group.Members[member.Id]
	if member.AmDead {
		if has {
			delete(group.Members, member.Id)
			state.Removed = append(state.Removed, m)
		}
		return nil
	}
	var numReplicas int
	for _, gm := range group.Members {
		if !gm.Learner {
			numReplicas++
		}
	}
	switch {
	case has || member.GetLearner():
		// pass
	case numReplicas >= n.server.NumReplicas:
		// We shouldn't allow more members than the number of replicas.
		return errors.Errorf("Group reached replication level. Can't add another member: %+v", member)
	}

	// Create a connection to this server.
	go conn.GetPools().Connect(member.Addr, n.server.tlsClientConfig)

	group.Members[member.Id] = member
	// Increment nextGroup when we have enough replicas
	if member.GroupId == n.server.nextGroup && numReplicas >= n.server.NumReplicas {
		n.server.nextGroup++
	}
	if member.Leader {
		// Unset leader flag for other nodes, there can be only one
		// leader at a time.
		for _, m := range group.Members {
			if m.Id != member.Id {
				m.Leader = false
			}
		}
	}
	// On replay of logs on restart we need to set nextGroup.
	if n.server.nextGroup <= member.GroupId {
		n.server.nextGroup = member.GroupId + 1
	}
	return nil
}

func (n *node) regenerateChecksum() {
	n.server.AssertLock()
	state := n.server.state
	// Regenerate group checksums. These checksums are solely based on which tablets are being
	// served by the group. If the tablets that a group is serving changes, and the Alpha does
	// not know about these changes, then the read request must fail.
	for _, g := range state.GetGroups() {
		preds := make([]string, 0, len(g.GetTablets()))
		for pred := range g.GetTablets() {
			preds = append(preds, pred)
		}
		sort.Strings(preds)
		g.Checksum = farm.Fingerprint64([]byte(strings.Join(preds, "")))
	}

	if n.AmLeader() {
		// It is important to push something to Oracle updates channel, so the subscribers would
		// get the latest checksum that we calculated above. Otherwise, if all the queries are
		// best effort queries which don't create any transaction, then the OracleDelta never
		// gets sent to Alphas, causing their group checksum to mismatch and never converge.
		n.server.orc.updates <- &pb.OracleDelta{}
	}
}

func (n *node) handleBulkTabletProposal(tablets []*pb.Tablet) error {
	n.server.AssertLock()
	defer n.regenerateChecksum()
	for _, tablet := range tablets {
		if err := n.handleTablet(tablet); err != nil {
			glog.Warningf("not able to handle tablet %s. Got err: %+v", tablet.GetPredicate(), err)
		}
	}

	return nil
}

// handleTablet will check if the given tablet is served by any group.
// If not the tablet will be added to the current group predicate list
//
// This function doesn't take any locks.
// It is the calling functions responsibility to manage the concurrency.
func (n *node) handleTablet(tablet *pb.Tablet) error {
	state := n.server.state
	if tablet.GroupId == 0 {
		return errors.Errorf("Tablet group id is zero: %+v", tablet)
	}
	group := state.Groups[tablet.GroupId]
	if tablet.Remove {
		glog.Infof("Removing tablet for attr: [%v], gid: [%v]\n", tablet.Predicate, tablet.GroupId)
		if group != nil {
			delete(group.Tablets, tablet.Predicate)
		}
		return nil
	}
	if group == nil {
		group = newGroup()
		state.Groups[tablet.GroupId] = group
	}

	// There's a edge case that we're handling.
	// Two servers ask to serve the same tablet, then we need to ensure that
	// only the first one succeeds.
	if prev := n.server.servingTablet(tablet.Predicate); prev != nil {
		if tablet.Force {
			originalGroup := state.Groups[prev.GroupId]
			delete(originalGroup.Tablets, tablet.Predicate)
		} else if prev.GroupId != tablet.GroupId {
			glog.Infof(
				"Tablet for attr: [%s], gid: [%d] already served by group: [%d]\n",
				prev.Predicate, tablet.GroupId, prev.GroupId)
			return errTabletAlreadyServed
		}
	}
	tablet.Force = false
	group.Tablets[tablet.Predicate] = tablet
	return nil
}

func (n *node) handleTabletProposal(tablet *pb.Tablet) error {
	n.server.AssertLock()
	defer n.regenerateChecksum()
	return n.handleTablet(tablet)
}

func (n *node) deleteNamespace(delNs uint64) error {
	n.server.AssertLock()
	state := n.server.state
	glog.Infof("Deleting namespace %d", delNs)
	defer n.regenerateChecksum()

	for _, group := range state.Groups {
		for pred := range group.Tablets {
			ns := x.ParseNamespace(pred)
			if ns == delNs {
				delete(group.Tablets, pred)
			}
		}
	}
	return nil
}

func (n *node) applySnapshot(snap *pb.ZeroSnapshot) error {
	existing, err := n.Store.Snapshot()
	if err != nil {
		return err
	}
	if existing.Metadata.Index >= snap.Index {
		glog.V(2).Infof("Skipping snapshot at %d, because found one at %d\n",
			snap.Index, existing.Metadata.Index)
		return nil
	}
	n.server.orc.purgeBelow(snap.CheckpointTs)

	data, err := proto.Marshal(snap)
	x.Check(err)

	for {
		// We should never let CreateSnapshot have an error.
		err := n.Store.CreateSnapshot(snap.Index, n.ConfState(), data)
		if err == nil {
			break
		}
		glog.Warningf("Error while calling CreateSnapshot: %v. Retrying...", err)
	}
	return nil
}

func (n *node) applyProposal(e raftpb.Entry) (uint64, error) {
	x.AssertTrue(len(e.Data) > 0)

	var p pb.ZeroProposal
	key := binary.BigEndian.Uint64(e.Data[:8])
	if err := proto.Unmarshal(e.Data[8:], &p); err != nil {
		return key, err
	}
	span := trace.SpanFromContext(n.Proposals.Ctx(key))

	n.server.Lock()
	defer n.server.Unlock()

	state := n.server.state
	state.Counter = e.Index
	if len(p.Cid) > 0 {
		if len(state.Cid) > 0 {
			return key, errInvalidProposal
		}
		state.Cid = p.Cid
	}
	if p.MaxRaftId > 0 {
		if p.MaxRaftId <= state.MaxRaftId {
			return key, errInvalidProposal
		}
		state.MaxRaftId = p.MaxRaftId
		n.server.nextRaftId = x.Max(n.server.nextRaftId, p.MaxRaftId+1)
	}
	if p.SnapshotTs != nil {
		for gid, ts := range p.SnapshotTs {
			if group, ok := state.Groups[gid]; ok {
				group.SnapshotTs = x.Max(group.SnapshotTs, ts)
			}
		}
	}
	if p.Member != nil {
		if err := n.handleMemberProposal(p.Member); err != nil {
			span.AddEvent(fmt.Sprintf("While applying membership proposal: %+v", err))
			glog.Errorf("While applying membership proposal: %+v", err)
			return key, err
		}
	}
	if p.Tablet != nil {
		if err := n.handleTabletProposal(p.Tablet); err != nil {
			span.AddEvent(fmt.Sprintf("While applying tablet proposal: %v", err))
			glog.Errorf("While applying tablet proposal: %v", err)
			return key, err
		}
	}

	if len(p.Tablets) > 0 {
		if err := n.handleBulkTabletProposal(p.Tablets); err != nil {
			span.AddEvent(fmt.Sprintf("While applying bulk tablet proposal: %v", err))
			glog.Errorf("While applying bulk tablet proposal: %v", err)
			return key, err
		}
	}

	if p.License != nil {
		// Check that the number of nodes in the cluster should be less than MaxNodes, otherwise
		// reject the proposal.
		numNodes := len(state.GetZeros())
		for _, group := range state.GetGroups() {
			numNodes += len(group.GetMembers())
		}
		if uint64(numNodes) > p.GetLicense().GetMaxNodes() {
			return key, errInvalidProposal
		}
		state.License = p.License
		// Check expiry and set enabled accordingly.
		expiry := time.Unix(state.License.ExpiryTs, 0).UTC()
		state.License.Enabled = time.Now().UTC().Before(expiry)
		if state.License.Enabled && opts.audit != nil {
			if err := audit.InitAuditor(opts.audit, 0, n.Id); err != nil {
				glog.Errorf("error while initializing audit logs %+v", err)
			}
		}
	}
	if p.Snapshot != nil {
		if err := n.applySnapshot(p.Snapshot); err != nil {
			glog.Errorf("While applying snapshot: %v\n", err)
		}
	}
	if p.DeleteNs != nil {
		if err := n.deleteNamespace(p.DeleteNs.Namespace); err != nil {
			glog.Errorf("While deleting namespace %+v", err)
			return key, err
		}
	}

	switch {
	case p.MaxUID > state.MaxUID:
		state.MaxUID = p.MaxUID
	case p.MaxTxnTs > state.MaxTxnTs:
		state.MaxTxnTs = p.MaxTxnTs
	case p.MaxNsID > state.MaxNsID:
		state.MaxNsID = p.MaxNsID
	case p.MaxUID != 0 || p.MaxTxnTs != 0 || p.MaxNsID != 0:
		// Could happen after restart when some entries were there in WAL and did not get
		// snapshotted.
		glog.Infof("Could not apply proposal, ignoring: p.MaxUID=%v, p.MaxTxnTs=%v"+
			"p.MaxNsID=%v, maxUID=%d maxTxnTs=%d maxNsID=%d\n",
			p.MaxUID, p.MaxTxnTs, p.MaxNsID, state.MaxUID, state.MaxTxnTs, state.MaxNsID)
	}
	if p.Txn != nil {
		n.server.orc.updateCommitStatus(e.Index, p.Txn)
	}

	return key, nil
}

func (n *node) applyConfChange(e raftpb.Entry) {
	var cc raftpb.ConfChange
	if err := cc.Unmarshal(e.Data); err != nil {
		glog.Errorf("While unmarshalling confchange: %+v", err)
	}

	if cc.Type == raftpb.ConfChangeRemoveNode {
		if cc.NodeID == n.Id {
			glog.Fatalf("I [id:%#x group:0] have been removed. Goodbye!", n.Id)
		}
		n.DeletePeer(cc.NodeID)
		n.server.removeZero(cc.NodeID)

	} else if len(cc.Context) > 0 {
		var rc pb.RaftContext
		x.Check(proto.Unmarshal(cc.Context, &rc))
		go n.Connect(rc.Id, rc.Addr)

		m := &pb.Member{
			Id:      rc.Id,
			Addr:    rc.Addr,
			GroupId: 0,
			Learner: rc.IsLearner,
		}
		for _, member := range n.server.membershipState().Removed {
			// It is not recommended to reuse RAFT ids.
			if member.GroupId == 0 && m.Id == member.Id {
				err := errors.Errorf("REUSE_RAFTID: Reusing removed id: %d.\n", m.Id)
				n.DoneConfChange(cc.ID, err)
				// Cancel configuration change.
				cc.NodeID = raft.None
				n.Raft().ApplyConfChange(cc)
				return
			}
		}

		n.server.storeZero(m)
	}

	cs := n.Raft().ApplyConfChange(cc)
	n.SetConfState(cs)
	n.DoneConfChange(cc.ID, nil)

	// The following doesn't really trigger leader change. It's just capturing a leader change
	// event. The naming is poor. TODO: Fix naming, and see if we can simplify this leader change
	// logic.
	n.triggerLeaderChange()
}

func (n *node) triggerLeaderChange() {
	n.server.triggerLeaderChange()
	// We update leader information on each node without proposal. This
	// function is called on all nodes on leader change.
	n.server.updateZeroLeader()
}

func (n *node) proposeNewCID() {
	// Either this is a new cluster or can't find a CID in the entries. So, propose a new ID for the cluster.
	// CID check is needed for the case when a leader assigns a CID to the new node and the new node is proposing a CID
	for n.server.membershipState().Cid == "" {
		id := uuid.New().String()

		if zeroCid := Zero.Conf.GetString("cid"); len(zeroCid) > 0 {
			id = zeroCid
		}

		err := n.proposeAndWait(context.Background(), &pb.ZeroProposal{Cid: id})
		if err == nil {
			glog.Infof("CID set for cluster: %v", id)
			break
		}
		if err == errInvalidProposal {
			glog.Errorf("invalid proposal error while proposing cluster id")
			return
		}
		glog.Errorf("While proposing CID: %v. Retrying...", err)
		time.Sleep(3 * time.Second)
	}

	// Apply trial license only if not already licensed and no enterprise license provided.
	if n.server.license() == nil && Zero.Conf.GetString("enterprise_license") == "" {
		if err := n.proposeTrialLicense(); err != nil {
			glog.Errorf("while proposing trial license to cluster: %v", err)
		}
	}
}

func (n *node) checkForCIDInEntries() (bool, error) {
	first, err := n.Store.FirstIndex()
	if err != nil {
		return false, err
	}
	last, err := n.Store.LastIndex()
	if err != nil {
		return false, err
	}

	for batch := first; batch <= last; {
		entries, err := n.Store.Entries(batch, last+1, 64<<20)
		if err != nil {
			return false, err
		}

		// Exit early from the loop if no entries were found.
		if len(entries) == 0 {
			break
		}

		// increment the iterator to the next batch
		batch = entries[len(entries)-1].Index + 1

		for _, entry := range entries {
			if entry.Type != raftpb.EntryNormal || len(entry.Data) == 0 {
				continue
			}
			var proposal pb.ZeroProposal
			if err = proto.Unmarshal(entry.Data[8:], &proposal); err != nil {
				return false, errors.Wrapf(err, "error unmarshlling wal entry: [%x]", entry.Data[8:])
			}
			if len(proposal.Cid) > 0 {
				return true, err
			}
		}
	}
	return false, err
}

func (n *node) initAndStartNode() error {
	x.Check(n.initProposalKey(n.Id))
	_, restart, err := n.PastLife()
	x.Check(err)

	switch {
	case restart:
		glog.Infoln("Restarting node for dgraphzero")
		sp, err := n.Store.Snapshot()
		x.Checkf(err, "Unable to get existing snapshot")
		if !raft.IsEmptySnap(sp) {
			// It is important that we pick up the conf state here.
			n.SetConfState(&sp.Metadata.ConfState)

			var zs pb.ZeroSnapshot
			x.Check(proto.Unmarshal(sp.Data, &zs))
			n.server.SetMembershipState(zs.State)
			for _, id := range sp.Metadata.ConfState.Voters {
				n.Connect(id, zs.State.Zeros[id].Addr)
			}
		}

		n.SetRaft(raft.RestartNode(n.Cfg))
		foundCID, err := n.checkForCIDInEntries()
		if err != nil {
			return err
		}
		if !foundCID {
			go n.proposeNewCID()
		}

	case len(opts.peer) > 0:
		p := conn.GetPools().Connect(opts.peer, opts.tlsClientConfig)
		if p == nil {
			return errors.Errorf("Unhealthy connection to %v", opts.peer)
		}

		timeout := 8 * time.Second
		for {
			c := pb.NewRaftClient(p.Get())
			ctx, cancel := context.WithTimeout(n.ctx, timeout)
			// JoinCluster can block indefinitely, raft ignores conf change proposal
			// if it has pending configuration.
			_, err := c.JoinCluster(ctx, n.RaftContext)
			if err == nil {
				cancel()
				break
			}
			if x.ShouldCrash(err) {
				cancel()
				log.Fatalf("Error while joining cluster: %v", err)
			}
			glog.Errorf("Error while joining cluster: %v\n", err)
			timeout *= 2
			if timeout > 32*time.Second {
				timeout = 32 * time.Second
			}
			time.Sleep(timeout) // This is useful because JoinCluster can exit immediately.
			cancel()
		}
		glog.Infof("[%#x] Starting node\n", n.Id)
		// We call RestartNode here because nodes already exists in the cluster and
		// we setup the state by calling c.JoinCluster above. We can't call StartNode
		// here because we only have peer's IP addresses.
		n.SetRaft(raft.RestartNode(n.Cfg))

	default:
		glog.Infof("Starting a brand new node")
		data, err := proto.Marshal(n.RaftContext)
		x.Check(err)
		peers := []raft.Peer{{ID: n.Id, Context: data}}
		n.SetRaft(raft.StartNode(n.Cfg, peers))
		go n.proposeNewCID()
	}

	go n.Run()
	go n.BatchAndSendMessages()
	go n.ReportRaftComms()
	return nil
}

func (n *node) updateZeroMembershipPeriodically(closer *z.Closer) {
	defer closer.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			n.server.updateZeroLeader()
		case <-closer.HasBeenClosed():
			return
		}
	}
}

func (n *node) checkQuorum(closer *z.Closer) {
	defer closer.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	quorum := func() {
		// Make this timeout 1.5x the timeout on RunReadIndexLoop.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		ctx, span := otel.Tracer("").Start(ctx, "Zero.checkQuorum")
		defer span.End()
		span.SetAttributes(attribute.String("Node id", fmt.Sprintf("%d", n.Id)))

		if state, err := n.server.latestMembershipState(ctx); err == nil {
			n.mu.Lock()
			n.lastQuorum = time.Now()
			n.mu.Unlock()
			// Also do some connection cleanup.
			conn.GetPools().RemoveInvalid(state)
			span.AddEvent("Updated lastQuorum")

		} else if glog.V(1) {
			span.AddEvent(fmt.Sprintf("Got error: %v", err))
			glog.Warningf("Zero node: %#x unable to reach quorum. Error: %v", n.Id, err)
		}
	}

	for {
		select {
		case <-ticker.C:
			// Only the leader needs to check for the quorum. The quorum is
			// used by a leader to identify if it is behind a network partition.
			if n.amLeader() {
				quorum()
			}
		case <-closer.HasBeenClosed():
			return
		}
	}
}

func (n *node) snapshotPeriodically(closer *z.Closer) {
	defer closer.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := n.calculateAndProposeSnapshot(); err != nil {
				glog.Errorf("While calculateAndProposeSnapshot: %v", err)
			}

		case <-closer.HasBeenClosed():
			return
		}
	}
}

// calculateAndProposeSnapshot works by tracking Alpha group leaders' checkpoint timestamps. It then
// finds the minimum checkpoint ts across these groups, say Tmin.  And then, iterates over Zero Raft
// logs to determine what all entries we could discard which are below Tmin. It uses that
// information to calculate a snapshot, which it proposes to other Zeros. When the proposal arrives
// via Raft, all Zeros apply it to themselves via applySnapshot in raft.Ready.
func (n *node) calculateAndProposeSnapshot() error {
	// Only run this on the leader.
	if !n.AmLeader() {
		return nil
	}

	_, span := otel.Tracer("").Start(n.ctx, "Calculate.Snapshot")
	defer span.End()

	// We calculate the minimum timestamp from all the group's maxAssigned.
	discardBelow := uint64(math.MaxUint64)
	{
		s := n.server
		s.RLock()
		if len(s.state.Groups) != len(s.checkpointPerGroup) {
			log := fmt.Sprintf("Skipping creating a snapshot."+
				" Num groups: %d, Num checkpoints: %d\n",
				len(s.state.Groups), len(s.checkpointPerGroup))
			s.RUnlock()
			span.AddEvent(log)
			glog.Infof(log)
			return nil
		}
		for gid, ts := range s.checkpointPerGroup {
			span.AddEvent(fmt.Sprintf("Group: %d Checkpoint Ts: %d", gid, ts))
			discardBelow = x.Min(discardBelow, ts)
		}
		s.RUnlock()
	}

	first, err := n.Store.FirstIndex()
	if err != nil {
		span.AddEvent(fmt.Sprintf("FirstIndex error: %v", err))
		return err
	}
	last, err := n.Store.LastIndex()
	if err != nil {
		span.AddEvent(fmt.Sprintf("LastIndex error: %v", err))
		return err
	}

	span.AddEvent(fmt.Sprintf("First index: %d. Last index: %d. Discard Below Ts: %d",
		first, last, discardBelow))

	var snapshotIndex uint64
	for batchFirst := first; batchFirst <= last; {
		entries, err := n.Store.Entries(batchFirst, last+1, 256<<20)
		if err != nil {
			span.AddEvent(fmt.Sprintf("Error: %v", err))
			return err
		}
		// Exit early from the loop if no entries were found.
		if len(entries) == 0 {
			break
		}
		for _, entry := range entries {
			if entry.Type != raftpb.EntryNormal || len(entry.Data) == 0 {
				continue
			}
			var p pb.ZeroProposal
			if err := proto.Unmarshal(entry.Data[8:], &p); err != nil {
				span.AddEvent(fmt.Sprintf("Error: %v", err))
				return err
			}
			if txn := p.Txn; txn != nil {
				if txn.CommitTs > 0 && txn.CommitTs < discardBelow {
					snapshotIndex = entry.Index
				}
			}
		}
		batchFirst = entries[len(entries)-1].Index + 1
	}
	if snapshotIndex == 0 {
		return nil
	}
	span.AddEvent(fmt.Sprintf("Taking snapshot at index: %d", snapshotIndex))
	state := n.server.membershipState()

	zs := &pb.ZeroSnapshot{
		Index:        snapshotIndex,
		CheckpointTs: discardBelow,
		State:        state,
	}
	glog.V(2).Infof("Proposing snapshot at index: %d, checkpoint ts: %d\n",
		zs.Index, zs.CheckpointTs)
	zp := &pb.ZeroProposal{Snapshot: zs}
	if err = n.proposeAndWait(n.ctx, zp); err != nil {
		glog.Errorf("Error while proposing snapshot: %v\n", err)
		span.AddEvent(fmt.Sprintf("Error while proposing snapshot: %v", err))
		return err
	}
	span.AddEvent("Snapshot proposed: Done")
	return nil
}

const tickDur = 100 * time.Millisecond

func (n *node) Run() {
	// lastLead is for detecting leadership changes
	//
	// etcd has a similar mechanism for tracking leader changes, with their
	// raftReadyHandler.getLead() function that returns the previous leader
	lastLead := uint64(math.MaxUint64)

	var leader bool
	licenseApplied := false
	ticker := time.NewTicker(tickDur)
	defer ticker.Stop()

	// snapshot can cause select loop to block while deleting entries, so run
	// it in goroutine
	readStateCh := make(chan raft.ReadState, 100)
	closer := z.NewCloser(5)
	defer func() {
		closer.SignalAndWait()
		n.closer.Done()
		glog.Infof("Zero Node.Run finished.")
	}()

	go n.snapshotPeriodically(closer)
	go n.updateEnterpriseState(closer)
	go n.updateZeroMembershipPeriodically(closer)
	go n.checkQuorum(closer)
	go n.RunReadIndexLoop(closer, readStateCh)
	if !x.WorkerConfig.HardSync {
		closer.AddRunning(1)
		go x.StoreSync(n.Store, closer)
	}
	// We only stop runReadIndexLoop after the for loop below has finished interacting with it.
	// That way we know sending to readStateCh will not deadlock.

	var timer x.Timer
	for {
		select {
		case <-n.closer.HasBeenClosed():
			n.Raft().Stop()
			return
		case <-ticker.C:
			n.Raft().Tick()
		case rd := <-n.Raft().Ready():
			timer.Start()
			_, span := otel.Tracer("").Start(n.ctx, "Zero.RunLoop")
			for _, rs := range rd.ReadStates {
				// No need to use select-case-default on pushing to readStateCh. It is typically
				// empty.
				readStateCh <- rs
			}
			span.AddEvent(fmt.Sprintf("Pushed %d readstates", len(rd.ReadStates)))

			if rd.SoftState != nil {
				if rd.RaftState == raft.StateLeader && !leader {
					glog.Infoln("I've become the leader, updating leases.")
					n.server.updateLeases()
				}
				leader = rd.RaftState == raft.StateLeader
				// group id hardcoded as 0
				ctx, _ := tag.New(n.ctx, tag.Upsert(x.KeyGroup, "0"))
				if rd.SoftState.Lead != lastLead {
					lastLead = rd.SoftState.Lead
					ostats.Record(ctx, x.RaftLeaderChanges.M(1))
				}
				if rd.SoftState.Lead != raft.None {
					ostats.Record(ctx, x.RaftHasLeader.M(1))
				} else {
					ostats.Record(ctx, x.RaftHasLeader.M(0))
				}
				if leader {
					ostats.Record(ctx, x.RaftIsLeader.M(1))
				} else {
					ostats.Record(ctx, x.RaftIsLeader.M(0))
				}
				// Oracle stream would close the stream once it steps down as leader
				// predicate move would cancel any in progress move on stepping down.
				n.triggerLeaderChange()
			}
			if leader {
				// Leader can send messages in parallel with writing to disk.
				for i := range rd.Messages {
					n.Send(&rd.Messages[i])
				}
			}
			n.SaveToStorage(&rd.HardState, rd.Entries, &rd.Snapshot)
			timer.Record("disk")
			span.AddEvent("Saved to storage")
			for x.WorkerConfig.HardSync && rd.MustSync {
				if err := n.Store.Sync(); err != nil {
					glog.Errorf("Error while calling Store.Sync: %v", err)
					time.Sleep(10 * time.Millisecond)
					continue
				}
				timer.Record("sync")
				break
			}

			if !raft.IsEmptySnap(rd.Snapshot) {
				var zs pb.ZeroSnapshot
				x.Check(proto.Unmarshal(rd.Snapshot.Data, &zs))
				n.server.SetMembershipState(zs.State)
			}

			for _, entry := range rd.CommittedEntries {
				n.Applied.Begin(entry.Index)
				switch {
				case entry.Type == raftpb.EntryConfChange:
					n.applyConfChange(entry)
					glog.Infof("Done applying conf change at %#x", n.Id)

				case len(entry.Data) == 0:
					// Raft commits empty entry on becoming a leader.
					// Do nothing.

				case entry.Type == raftpb.EntryNormal:
					start := time.Now()
					key, err := n.applyProposal(entry)
					if err != nil {
						glog.Errorf("While applying proposal: %v\n", err)
					}
					n.Proposals.Done(key, err)
					if took := time.Since(start); took > time.Second {
						var p pb.ZeroProposal
						// Raft commits empty entry on becoming a leader.
						if err := proto.Unmarshal(entry.Data[8:], &p); err == nil {
							glog.V(2).Infof("Proposal took %s to apply: %+v\n",
								took.Round(time.Second), p)
						}
					}

				default:
					glog.Infof("Unhandled entry: %+v\n", entry)
				}
				n.Applied.Done(entry.Index)
			}
			span.AddEvent(fmt.Sprintf("Applied %d CommittedEntries", len(rd.CommittedEntries)))

			if !leader {
				// Followers should send messages later.
				for i := range rd.Messages {
					n.Send(&rd.Messages[i])
				}
			}
			span.AddEvent("Sent messages")
			timer.Record("proposals")

			n.Raft().Advance()
			span.AddEvent("Advanced Raft")
			timer.Record("advance")

			span.SetAttributes(attribute.String("Zero.RunLoop", "Running"))
			span.SetAttributes(attribute.String("Zero.RunLoop", "Done"))
			span.End()
			if timer.Total() > 5*tickDur {
				glog.Warningf(
					"Raft.Ready took too long to process: %s."+
						" Num entries: %d. Num committed entries: %d. MustSync: %v",
					timer.String(), len(rd.Entries), len(rd.CommittedEntries), rd.MustSync)
			}

			// Apply license when I am the leader.
			if !licenseApplied && n.AmLeader() {
				licenseApplied = true
				// Apply the EE License given on CLI which may over-ride previous
				// license, if present. That is an intended behavior to allow customers
				// to apply new/renewed licenses.
				if license := Zero.Conf.GetString("enterprise_license"); len(license) > 0 {
					go n.server.applyLicenseFile(license)
				}
			}
		}
	}
}
