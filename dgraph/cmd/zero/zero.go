/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package zero

import (
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v24/conn"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var (
	emptyConnectionState pb.ConnectionState
	errServerShutDown    = errors.New("Server is being shut down")
)

// Server implements the zero server.
type Server struct {
	x.SafeMutex
	Node *node
	orc  *Oracle

	NumReplicas int
	state       *pb.MembershipState
	nextRaftId  uint64

	// nextUint is the uint64 which we can hand out next. See maxLease for the
	// max ID leased via Zero quorum.
	nextUint    map[pb.NumLeaseType]uint64
	readOnlyTs  uint64
	leaseLock   sync.Mutex // protects nextUID, nextTxnTs, nextNsID and corresponding proposals.
	rateLimiter *x.RateLimiter

	// groupMap    map[uint32]*Group
	nextGroup      uint32
	leaderChangeCh chan struct{}
	closer         *z.Closer  // Used to tell stream to close.
	connectLock    sync.Mutex // Used to serialize connect requests from servers.

	// tls client config used to connect with zero internally
	tlsClientConfig *tls.Config

	moveOngoing    chan struct{}
	blockCommitsOn *sync.Map

	checkpointPerGroup map[uint32]uint64
	// embedding the pb.UnimplementedZeroServer struct to ensure forward compatibility of the server.
	pb.UnimplementedZeroServer
}

// Init initializes the zero server.
func (s *Server) Init() {
	s.Lock()
	defer s.Unlock()

	s.orc = &Oracle{}
	s.orc.Init()
	s.state = &pb.MembershipState{
		Groups: make(map[uint32]*pb.Group),
		Zeros:  make(map[uint64]*pb.Member),
	}
	s.nextUint = make(map[pb.NumLeaseType]uint64)
	s.nextRaftId = 1
	s.nextUint[pb.Num_UID] = 1
	s.nextUint[pb.Num_TXN_TS] = 1
	s.nextUint[pb.Num_NS_ID] = 1
	s.nextGroup = 1
	s.leaderChangeCh = make(chan struct{}, 1)
	s.closer = z.NewCloser(2) // grpc and http
	s.blockCommitsOn = new(sync.Map)
	s.moveOngoing = make(chan struct{}, 1)
	s.checkpointPerGroup = make(map[uint32]uint64)
	if opts.limiterConfig.UidLeaseLimit > 0 {
		// rate limiting is not enabled when lease limit is set to zero.
		s.rateLimiter = x.NewRateLimiter(int64(opts.limiterConfig.UidLeaseLimit),
			opts.limiterConfig.RefillAfter, s.closer)
	}

	go s.rebalanceTablets()
}

func (s *Server) triggerLeaderChange() {
	s.Lock()
	defer s.Unlock()
	close(s.leaderChangeCh)
	s.leaderChangeCh = make(chan struct{}, 1)
}

func (s *Server) leaderChangeChannel() chan struct{} {
	s.RLock()
	defer s.RUnlock()
	return s.leaderChangeCh
}

func (s *Server) member(addr string) *pb.Member {
	s.AssertRLock()
	for _, m := range s.state.Zeros {
		if m.Addr == addr {
			return m
		}
	}
	for _, g := range s.state.Groups {
		for _, m := range g.Members {
			if m.Addr == addr {
				return m
			}
		}
	}
	return nil
}

// Leader returns a connection pool to the zero leader.
func (s *Server) Leader(gid uint32) *conn.Pool {
	s.RLock()
	defer s.RUnlock()
	if s.state == nil {
		return nil
	}
	var members map[uint64]*pb.Member
	if gid == 0 {
		members = s.state.Zeros
	} else {
		group := s.state.Groups[gid]
		if group == nil {
			return nil
		}
		members = group.Members
	}
	var healthyPool *conn.Pool
	for _, m := range members {
		if pl, err := conn.GetPools().Get(m.Addr); err == nil {
			healthyPool = pl
			if m.Leader {
				return pl
			}
		}
	}
	return healthyPool
}

// KnownGroups returns a list of the known groups.
func (s *Server) KnownGroups() []uint32 {
	var groups []uint32
	s.RLock()
	defer s.RUnlock()
	for group := range s.state.Groups {
		groups = append(groups, group)
	}
	return groups
}

func (s *Server) hasLeader(gid uint32) bool {
	s.AssertRLock()
	if s.state == nil {
		return false
	}
	group := s.state.Groups[gid]
	if group == nil {
		return false
	}
	for _, m := range group.Members {
		if m.Leader {
			return true
		}
	}
	return false
}

// SetMembershipState updates the membership state to the given one.
func (s *Server) SetMembershipState(state *pb.MembershipState) {
	s.Lock()
	defer s.Unlock()

	s.state = state
	s.nextRaftId = x.Max(s.nextRaftId, s.state.MaxRaftId+1)

	if state.Zeros == nil {
		state.Zeros = make(map[uint64]*pb.Member)
	}
	if state.Groups == nil {
		state.Groups = make(map[uint32]*pb.Group)
	}

	// Create connections to all members.
	for _, g := range state.Groups {
		for _, m := range g.Members {
			conn.GetPools().Connect(m.Addr, s.tlsClientConfig)
		}

		if g.Tablets == nil {
			g.Tablets = make(map[string]*pb.Tablet)
		}
	}

	s.nextGroup = uint32(len(state.Groups) + 1)
}

// MarshalMembershipState returns the marshaled membership state.
func (s *Server) MarshalMembershipState() ([]byte, error) {
	s.Lock()
	defer s.Unlock()
	return proto.Marshal(s.state)
}

func (s *Server) membershipState() *pb.MembershipState {
	s.RLock()
	defer s.RUnlock()
	return proto.Clone(s.state).(*pb.MembershipState)
}

func (s *Server) groupChecksums() map[uint32]uint64 {
	s.RLock()
	defer s.RUnlock()
	m := make(map[uint32]uint64)
	for gid, g := range s.state.GetGroups() {
		m[gid] = g.Checksum
	}
	return m
}

func (s *Server) storeZero(m *pb.Member) {
	s.Lock()
	defer s.Unlock()

	s.state.Zeros[m.Id] = m
}

func (s *Server) updateZeroLeader() {
	s.Lock()
	defer s.Unlock()
	leader := s.Node.Raft().Status().Lead
	for _, m := range s.state.Zeros {
		m.Leader = m.Id == leader
	}
}

func (s *Server) removeZero(nodeId uint64) {
	s.Lock()
	defer s.Unlock()
	m, has := s.state.Zeros[nodeId]
	if !has {
		return
	}
	delete(s.state.Zeros, nodeId)
	s.state.Removed = append(s.state.Removed, m)
}

// ServingTablet returns the Tablet called tablet.
func (s *Server) ServingTablet(tablet string) *pb.Tablet {
	s.RLock()
	defer s.RUnlock()

	for _, group := range s.state.Groups {
		if tab, ok := group.Tablets[tablet]; ok {
			return tab
		}
	}
	return nil
}

func (s *Server) blockTablet(pred string) func() {
	s.blockCommitsOn.Store(pred, struct{}{})
	return func() {
		s.blockCommitsOn.Delete(pred)
	}
}

func (s *Server) isBlocked(pred string) bool {
	_, blocked := s.blockCommitsOn.Load(pred)
	return blocked
}

func (s *Server) servingTablet(tablet string) *pb.Tablet {
	s.AssertRLock()

	for _, group := range s.state.Groups {
		if tab, ok := group.Tablets[tablet]; ok {
			return tab
		}
	}
	return nil
}

func (s *Server) createProposals(dst *pb.Group) ([]*pb.ZeroProposal, error) {
	var res []*pb.ZeroProposal
	if len(dst.Members) > 1 {
		return res, errors.Errorf("Create Proposal: Invalid group: %+v", dst)
	}

	s.RLock()
	defer s.RUnlock()
	// There is only one member. We use for loop because we don't know what the mid is.
	for mid, dstMember := range dst.Members {
		group, has := s.state.Groups[dstMember.GroupId]
		if !has {
			return res, errors.Errorf("Unknown group for member: %+v", dstMember)
		}
		srcMember, has := group.Members[mid]
		if !has {
			return res, errors.Errorf("Unknown member: %+v", dstMember)
		}
		if srcMember.Addr != dstMember.Addr ||
			srcMember.Leader != dstMember.Leader {

			proposal := &pb.ZeroProposal{
				Member: dstMember,
			}
			res = append(res, proposal)
		}
		if !dstMember.Leader {
			// Don't continue to tablets if request is not from the leader.
			return res, nil
		}
		if dst.SnapshotTs > group.SnapshotTs {
			res = append(res, &pb.ZeroProposal{
				SnapshotTs: map[uint32]uint64{dstMember.GroupId: dst.SnapshotTs},
			})
		}
	}

	var tablets []*pb.Tablet
	for key, dstTablet := range dst.Tablets {
		group, has := s.state.Groups[dstTablet.GroupId]
		if !has {
			return res, errors.Errorf("Unknown group for tablet: %+v", dstTablet)
		}
		srcTablet, has := group.Tablets[key]
		if !has {
			// Tablet moved to new group
			continue
		}

		s := float64(srcTablet.OnDiskBytes)
		d := float64(dstTablet.OnDiskBytes)
		if dstTablet.Remove || (s == 0 && d > 0) || (s > 0 && math.Abs(d/s-1) > 0.1) {
			dstTablet.Force = false
			tablets = append(tablets, dstTablet)
		}
	}

	if len(tablets) > 0 {
		res = append(res, &pb.ZeroProposal{Tablets: tablets})
	}
	return res, nil
}

func (s *Server) Inform(ctx context.Context, req *pb.TabletRequest) (*pb.TabletResponse, error) {
	ctx, span := otel.Tracer("").Start(ctx, "Zero.Inform")
	defer span.End()
	if req == nil || len(req.Tablets) == 0 {
		return nil, errors.Errorf("Tablets are empty in %+v", req)
	}

	if req.GroupId == 0 {
		return nil, errors.Errorf("Group ID is Zero in %+v", req)
	}

	tablets := make([]*pb.Tablet, 0)
	unknownTablets := make([]*pb.Tablet, 0)
	for _, t := range req.Tablets {
		tab := s.ServingTablet(t.Predicate)
		span.SetAttributes(attribute.String("tablet_predicate", t.Predicate))
		switch {
		case tab != nil && !t.Force:
			tablets = append(tablets, t)
		case t.ReadOnly:
			tablets = append(tablets, &pb.Tablet{})
		default:
			unknownTablets = append(unknownTablets, t)
		}
	}

	if len(unknownTablets) == 0 {
		return &pb.TabletResponse{
			Tablets: tablets,
		}, nil
	}

	// Set the tablet to be served by this server's group.
	var proposal pb.ZeroProposal
	proposal.Tablets = make([]*pb.Tablet, 0)
	for _, t := range unknownTablets {
		if x.IsReservedPredicate(t.Predicate) {
			// Force all the reserved predicates to be allocated to group 1.
			// This is to make it easier to stream ACL updates to all alpha servers
			// since they only need to open one pipeline to receive updates for all
			// ACL predicates.
			// This will also make it easier to restore the reserved predicates after
			// a DropAll operation.
			t.GroupId = 1
		}
		proposal.Tablets = append(proposal.Tablets, t)
	}

	if err := s.Node.proposeAndWait(ctx, &proposal); err != nil && err != errTabletAlreadyServed {
		span.AddEvent(fmt.Sprintf("Error proposing tablet: %+v. Error: %v", &proposal, err))
		return nil, err
	}

	for _, t := range unknownTablets {
		tab := s.ServingTablet(t.Predicate)
		x.AssertTrue(tab != nil)
		span.AddEvent(fmt.Sprintf("Tablet served: %+v", tab))
		tablets = append(tablets, tab)
	}

	return &pb.TabletResponse{
		Tablets: tablets,
	}, nil
}

// RemoveNode removes the given node from the given group.
// It's the user's responsibility to ensure that node doesn't come back again
// before calling the api.
func (s *Server) RemoveNode(ctx context.Context, req *pb.RemoveNodeRequest) (*pb.Status, error) {
	if req.GroupId == 0 {
		return nil, s.Node.ProposePeerRemoval(ctx, req.NodeId)
	}
	zp := &pb.ZeroProposal{}
	zp.Member = &pb.Member{Id: req.NodeId, GroupId: req.GroupId, AmDead: true}
	if _, ok := s.state.Groups[req.GroupId]; !ok {
		return nil, errors.Errorf("No group with groupId %d found", req.GroupId)
	}
	if _, ok := s.state.Groups[req.GroupId].Members[req.NodeId]; !ok {
		return nil, errors.Errorf("No node with nodeId %d found in group %d", req.NodeId,
			req.GroupId)
	}
	if len(s.state.Groups[req.GroupId].Members) == 1 && len(s.state.Groups[req.GroupId].
		Tablets) > 0 {
		return nil, errors.Errorf("Move all tablets from group %d before removing the last node",
			req.GroupId)
	}
	if err := s.Node.proposeAndWait(ctx, zp); err != nil {
		return nil, err
	}

	return &pb.Status{}, nil
}

// Connect is used by Alpha nodes to connect the very first time with group zero.
func (s *Server) Connect(ctx context.Context,
	m *pb.Member) (resp *pb.ConnectionState, err error) {
	// Ensures that connect requests are always serialized
	s.connectLock.Lock()
	defer s.connectLock.Unlock()
	glog.Infof("Got connection request: %+v\n", m)
	defer glog.Infof("Connected: %+v\n", m)

	if ctx.Err() != nil {
		err := errors.Errorf("Context has error: %v\n", ctx.Err())
		return &emptyConnectionState, err
	}
	ms, err := s.latestMembershipState(ctx)
	if err != nil {
		return nil, err
	}

	if m.ClusterInfoOnly {
		// This request only wants to access the membership state, and nothing else. Most likely
		// from our clients.
		cs := &pb.ConnectionState{
			State:      ms,
			MaxPending: s.orc.MaxPending(),
		}
		return cs, err
	}
	if m.Addr == "" {
		return &emptyConnectionState, errors.Errorf("NO_ADDR: No address provided: %+v", m)
	}

	for _, member := range ms.Removed {
		// It is not recommended to reuse RAFT ids.
		if member.GroupId != 0 && m.Id == member.Id {
			return &emptyConnectionState, errors.Errorf(
				"REUSE_RAFTID: Duplicate Raft ID %d to removed member: %+v", m.Id, member)
		}
	}

	numberOfNodes := len(ms.Zeros)
	for _, group := range ms.Groups {
		for _, member := range group.Members {
			switch {
			case member.Addr == m.Addr && m.Id == 0:
				glog.Infof("Found a member with the same address. Returning: %+v", member)
				conn.GetPools().Connect(m.Addr, s.tlsClientConfig)
				return &pb.ConnectionState{
					State:  ms,
					Member: member,
				}, nil

			case member.Addr == m.Addr && member.Id != m.Id:
				// Same address. Different Id. If Id is zero, then it might be trying to connect for
				// the first time. We can just directly return the membership information.
				return nil, errors.Errorf("REUSE_ADDR: Duplicate address to existing member: %+v."+
					" Self: +%v", member, m)

			case member.Addr != m.Addr && member.Id == m.Id:
				// Same Id. Different address.
				if pl, err := conn.GetPools().Get(member.Addr); err == nil && pl.IsHealthy() {
					// Found a healthy connection.
					return nil, errors.Errorf("REUSE_RAFTID: Healthy connection to a member"+
						" with same ID: %+v", member)
				}
			}
			numberOfNodes++
		}
	}

	// Create a connection and check validity of the address by doing an Echo.
	conn.GetPools().Connect(m.Addr, s.tlsClientConfig)

	createProposal := func() *pb.ZeroProposal {
		s.Lock()
		defer s.Unlock()

		proposal := new(pb.ZeroProposal)
		// Check if we already have this member.
		for _, group := range s.state.Groups {
			if _, has := group.Members[m.Id]; has {
				return nil
			}
		}
		if m.Id == 0 {
			// In certain situations, the proposal can be sent and return with an error.
			// However,  Dgraph will keep retrying the proposal. To avoid assigning duplicating
			// IDs, the counter is incremented every time a proposal is created.
			m.Id = s.nextRaftId
			s.nextRaftId += 1
			proposal.MaxRaftId = m.Id
		} else if m.Id >= s.nextRaftId {
			s.nextRaftId = m.Id + 1
			proposal.MaxRaftId = m.Id
		}

		// We don't have this member. So, let's see if it has preference for a group.
		if m.GroupId > 0 {
			group, has := s.state.Groups[m.GroupId]
			if !has {
				// We don't have this group. Add the server to this group.
				proposal.Member = m
				return proposal
			}

			if _, has := group.Members[m.Id]; has {
				proposal.Member = m // Update in case some fields have changed, like address.
				return proposal
			}

			if m.Learner {
				// Give it the group it wants.
				proposal.Member = m
				return proposal
			}

			// We don't have this server in the list.
			if len(group.Members) < s.NumReplicas {
				// We need more servers here, so let's add it.
				proposal.Member = m
				return proposal
			} else if m.ForceGroupId {
				// If the group ID was taken from the group_id file, force the member
				// to be in this group even if the group is at capacity. This should
				// not happen if users properly initialize a cluster after a bulk load.
				proposal.Member = m
				return proposal
			}
			// Already have plenty of servers serving this group.
		}
		// Let's assign this server to a new group.
		for gid, group := range s.state.Groups {
			if len(group.Members) < s.NumReplicas {
				m.GroupId = gid
				proposal.Member = m
				return proposal
			}
		}
		// We either don't have any groups, or don't have any groups which need another member.
		m.GroupId = s.nextGroup
		// We shouldn't increase nextGroup here as we don't know whether we have enough
		// replicas until proposal is committed and can cause issues due to race.
		proposal.Member = m
		return proposal
	}

	proposal := createProposal()
	if proposal == nil {
		return &pb.ConnectionState{
			State: ms, Member: m,
		}, nil
	}

	if err := s.Node.proposeAndWait(ctx, proposal); err != nil {
		return &emptyConnectionState, err
	}
	resp = &pb.ConnectionState{
		State:  s.membershipState(),
		Member: m,
	}
	return resp, nil
}

// DeleteNamespace removes the tablets for deleted namespace from the membership state.
func (s *Server) DeleteNamespace(ctx context.Context, in *pb.DeleteNsRequest) (*pb.Status, error) {
	err := s.Node.proposeAndWait(ctx, &pb.ZeroProposal{DeleteNs: in})
	return &pb.Status{}, err
}

// ShouldServe returns the tablet serving the predicate passed in the request.
func (s *Server) ShouldServe(
	ctx context.Context, tablet *pb.Tablet) (resp *pb.Tablet, err error) {
	ctx, span := otel.Tracer("").Start(ctx, "Zero.ShouldServe")
	defer span.End()

	if tablet.Predicate == "" {
		return resp, errors.Errorf("Tablet predicate is empty in %+v", tablet)
	}
	if tablet.GroupId == 0 && !tablet.ReadOnly {
		return resp, errors.Errorf("Group ID is Zero in %+v", tablet)
	}

	// Check who is serving this tablet.
	tab := s.ServingTablet(tablet.Predicate)
	span.SetAttributes(attribute.String("tablet_predicate", tablet.Predicate))
	if tab != nil && !tablet.Force {
		// Someone is serving this tablet. Could be the caller as well.
		// The caller should compare the returned group against the group it holds to check who's
		// serving.
		return tab, nil
	}

	// Read-only requests should return an empty tablet instead of asking zero
	// to serve the predicate.
	if tablet.ReadOnly {
		return &pb.Tablet{}, nil
	}

	// Set the tablet to be served by this server's group.
	var proposal pb.ZeroProposal

	if x.IsReservedPredicate(tablet.Predicate) {
		// Force all the reserved predicates to be allocated to group 1.
		// This is to make it easier to stream ACL updates to all alpha servers
		// since they only need to open one pipeline to receive updates for all
		// ACL predicates.
		// This will also make it easier to restore the reserved predicates after
		// a DropAll operation.
		tablet.GroupId = 1
	}
	proposal.Tablet = tablet
	if err := s.Node.proposeAndWait(ctx, &proposal); err != nil && err != errTabletAlreadyServed {
		span.AddEvent(fmt.Sprintf("Error proposing tablet: %+v. Error: %v", &proposal, err))
		return tablet, err
	}
	tab = s.ServingTablet(tablet.Predicate)
	x.AssertTrue(tab != nil)
	span.SetAttributes(attribute.String("tablet_predicate_served", tablet.Predicate))
	return tab, nil
}

// UpdateMembership updates the membership of the given group.
func (s *Server) UpdateMembership(ctx context.Context, group *pb.Group) (*api.Payload, error) {
	// Only Zero leader would get these membership updates.
	if ts := group.GetCheckpointTs(); ts > 0 {
		for _, m := range group.GetMembers() {
			s.Lock()
			s.checkpointPerGroup[m.GetGroupId()] = ts
			s.Unlock()
		}
	}
	proposals, err := s.createProposals(group)
	if err != nil {
		// Sleep here so the caller doesn't keep on retrying indefinitely, creating a busy
		// wait.
		time.Sleep(time.Second)
		glog.Errorf("Error while creating proposals in Update: %v\n", err)
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, len(proposals))
	for _, pr := range proposals {
		go func(pr *pb.ZeroProposal) {
			errCh <- s.Node.proposeAndWait(ctx, pr)
		}(pr)
	}

	for range proposals {
		// We Don't care about these errors
		// Ideally shouldn't error out.
		if err := <-errCh; err != nil {
			glog.Errorf("Error while applying proposal in Update stream: %v\n", err)
			return nil, err
		}
	}

	if len(group.Members) == 0 {
		return &api.Payload{Data: []byte("OK")}, nil
	}
	select {
	case s.moveOngoing <- struct{}{}:
	default:
		// If a move is going on, don't do the next steps of deleting predicates.
		return &api.Payload{Data: []byte("OK")}, nil
	}
	defer func() {
		<-s.moveOngoing
	}()

	if err := s.deletePredicates(ctx, group); err != nil {
		glog.Warningf("While deleting predicates: %v", err)
	}
	return &api.Payload{Data: []byte("OK")}, nil
}

func (s *Server) deletePredicates(ctx context.Context, group *pb.Group) error {
	if group == nil || group.Tablets == nil {
		return nil
	}
	var gid uint32
	for _, tablet := range group.Tablets {
		gid = tablet.GroupId
		break
	}
	if gid == 0 {
		return errors.Errorf("Unable to find group")
	}
	state, err := s.latestMembershipState(ctx)
	if err != nil {
		return err
	}
	sg, ok := state.Groups[gid]
	if !ok {
		return errors.Errorf("Unable to find group: %d", gid)
	}

	pl := s.Leader(gid)
	if pl == nil {
		return errors.Errorf("Unable to reach leader of group: %d", gid)
	}
	wc := pb.NewWorkerClient(pl.Get())

	for pred := range group.Tablets {
		if _, found := sg.Tablets[pred]; found {
			continue
		}
		glog.Infof("Tablet: %v does not belong to group: %d. Sending delete instruction.",
			pred, gid)
		in := &pb.MovePredicatePayload{
			Predicate: pred,
			SourceGid: gid,
			DestGid:   0,
		}
		if _, err := wc.MovePredicate(ctx, in); err != nil {
			return err
		}
	}
	return nil
}

// StreamMembership periodically streams the membership state to the given stream.
func (s *Server) StreamMembership(_ *api.Payload, stream pb.Zero_StreamMembershipServer) error {
	// Send MembershipState right away. So, the connection is correctly established.
	ctx := stream.Context()
	ms, err := s.latestMembershipState(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(ms); err != nil {
		return err
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// Send an update every second.
			ms, err := s.latestMembershipState(ctx)
			if err != nil {
				return err
			}
			if err := stream.Send(ms); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-s.closer.HasBeenClosed():
			return errServerShutDown
		}
	}
}

func (s *Server) latestMembershipState(ctx context.Context) (*pb.MembershipState, error) {
	if err := s.Node.WaitLinearizableRead(ctx); err != nil {
		return nil, err
	}
	ms := s.membershipState()
	if ms == nil {
		return &pb.MembershipState{}, nil
	}
	return ms, nil
}
