/*
 * SPDX-FileCopyrightText:  Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package zero

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	attribute "go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc/metadata"

	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var emptyAssignedIds pb.AssignedIds

const (
	leaseBandwidth = uint64(10000)
)

func (s *Server) updateLeases() {
	var startTs uint64
	s.Lock()
	s.nextUint[pb.Num_UID] = s.state.MaxUID + 1
	s.nextUint[pb.Num_TXN_TS] = s.state.MaxTxnTs + 1
	s.nextUint[pb.Num_NS_ID] = s.state.MaxNsID + 1

	startTs = s.nextUint[pb.Num_TXN_TS]
	glog.Infof("Updated UID: %d. Txn Ts: %d. NsID: %d.",
		s.nextUint[pb.Num_UID], s.nextUint[pb.Num_TXN_TS], s.nextUint[pb.Num_NS_ID])
	s.Unlock()
	s.orc.updateStartTxnTs(startTs)
}

// maxLease keeps track of the various ID leases that we have already achieved
// quorum on. This Server can hand out IDs <= maxLease, without the need for any
// more quorum. If a new server becomes Zero leader, they'd renew this lease and
// advance maxLease before handing out new IDs.
func (s *Server) maxLease(typ pb.NumLeaseType) uint64 {
	s.RLock()
	defer s.RUnlock()
	var maxlease uint64
	switch typ {
	case pb.Num_UID:
		maxlease = s.state.MaxUID
	case pb.Num_TXN_TS:
		maxlease = s.state.MaxTxnTs
	case pb.Num_NS_ID:
		maxlease = s.state.MaxNsID
	}
	return maxlease
}

var errServedFromMemory = errors.New("Lease was served from memory")

// lease would either allocate ids or timestamps.
// This function is triggered by an RPC call. We ensure that only leader can assign new UIDs,
// so we can tackle any collisions that might happen with the leasemanager
// In essence, we just want one server to be handing out new uids.
func (s *Server) lease(ctx context.Context, num *pb.Num) (*pb.AssignedIds, error) {
	typ := num.GetType()
	node := s.Node
	// TODO: Fix when we move to linearizable reads, need to check if we are the leader, might be
	// based on leader leases. If this node gets partitioned and unless checkquorum is enabled, this
	// node would still think that it's the leader.
	if !node.AmLeader() {
		return &emptyAssignedIds, errors.Errorf("Assigning IDs is only allowed on leader.")
	}

	if num.Val == 0 && !num.ReadOnly {
		return &emptyAssignedIds, errors.Errorf("Nothing to be leased")
	}
	if glog.V(3) {
		glog.Infof("Got lease request for Type: %v. Num: %+v\n", typ, num)
	}

	s.leaseLock.Lock()
	defer s.leaseLock.Unlock()

	if typ == pb.Num_TXN_TS {
		if num.Val == 0 && num.ReadOnly {
			// If we're only asking for a readonly timestamp, we can potentially
			// service it directly.
			if glog.V(3) {
				glog.Infof("Attempting to serve read only txn ts [%d, %d]",
					s.readOnlyTs, s.nextUint[pb.Num_TXN_TS])
			}
			if s.readOnlyTs > 0 && s.readOnlyTs == s.nextUint[pb.Num_TXN_TS]-1 {
				return &pb.AssignedIds{ReadOnly: s.readOnlyTs}, errServedFromMemory
			}
		}
		// We couldn't service it. So, let's request an extra timestamp for
		// readonly transactions, if needed.
	}
	if s.nextUint[pb.Num_UID] == 0 || s.nextUint[pb.Num_TXN_TS] == 0 ||
		s.nextUint[pb.Num_NS_ID] == 0 {
		return nil, errors.New("Server not initialized")
	}

	// Calculate how many ids do we have available in memory, before we need to
	// renew our lease.
	maxLease := s.maxLease(typ)
	available := maxLease - s.nextUint[typ] + 1

	// If we have less available than what we need, we need to renew our lease.
	if available < num.Val+1 { // +1 for a potential readonly ts.
		// If we're asking for more ids than the standard lease bandwidth, then we
		// should set howMany generously, so we can service future requests from
		// memory, without asking for another lease. Only used if we need to renew
		// our lease.
		howMany := leaseBandwidth
		if num.Val > leaseBandwidth {
			howMany = num.Val + leaseBandwidth
		}
		if howMany < num.Val || maxLease+howMany < maxLease { // check for overflow.
			return &emptyAssignedIds, errors.Errorf("Cannot lease %s as the limit has reached."+
				" currMax:%d", typ, s.nextUint[typ]-1)
		}

		var proposal pb.ZeroProposal
		switch typ {
		case pb.Num_TXN_TS:
			proposal.MaxTxnTs = maxLease + howMany
		case pb.Num_UID:
			proposal.MaxUID = maxLease + howMany
		case pb.Num_NS_ID:
			proposal.MaxNsID = maxLease + howMany
		}
		// Blocking propose to get more ids or timestamps.
		if err := s.Node.proposeAndWait(ctx, &proposal); err != nil {
			return nil, err
		}
	}

	out := &pb.AssignedIds{}
	if typ == pb.Num_TXN_TS {
		if num.Val > 0 {
			out.StartId = s.nextUint[pb.Num_TXN_TS]
			out.EndId = out.StartId + num.Val - 1
			s.nextUint[pb.Num_TXN_TS] = out.EndId + 1
		}
		if num.ReadOnly {
			s.readOnlyTs = s.nextUint[pb.Num_TXN_TS]
			s.nextUint[pb.Num_TXN_TS]++
			out.ReadOnly = s.readOnlyTs
		}
		s.orc.doneUntil.Begin(x.Max(out.EndId, out.ReadOnly))
	} else if typ == pb.Num_UID {
		out.StartId = s.nextUint[pb.Num_UID]
		out.EndId = out.StartId + num.Val - 1
		s.nextUint[pb.Num_UID] = out.EndId + 1
	} else if typ == pb.Num_NS_ID {
		out.StartId = s.nextUint[pb.Num_NS_ID]
		out.EndId = out.StartId + num.Val - 1
		s.nextUint[pb.Num_NS_ID] = out.EndId + 1

	} else {
		return out, errors.Errorf("Unknown lease type: %v\n", typ)
	}
	return out, nil
}

// AssignIds is used to assign new ids (UIDs, NsIDs) by communicating with the leader of the
// RAFT group responsible for handing out ids. If bump is set to true in the request then the
// lease for the given id type is bumped to num.Val and {startId, endId} of the newly leased ids
// in the process of bump is returned.
func (s *Server) AssignIds(ctx context.Context, num *pb.Num) (*pb.AssignedIds, error) {
	if ctx.Err() != nil {
		return &emptyAssignedIds, ctx.Err()
	}
	ctx, span := otel.Tracer("").Start(ctx, "Zero.AssignIds")
	defer span.End()

	rateLimit := func() error {
		if s.rateLimiter == nil {
			return nil
		}
		if num.GetType() != pb.Num_UID {
			// We only rate limit lease of UIDs.
			return nil
		}
		ns, err := x.ExtractNamespace(ctx)
		if err != nil || ns == x.GalaxyNamespace {
			// There is no rate limiting for GalaxyNamespace. Also, we allow the requests which do
			// not contain namespace into context.
			return nil
		}
		if num.Val > opts.limiterConfig.UidLeaseLimit {
			return errors.Errorf("Requested UID lease(%d) is greater than allowed(%d).",
				num.Val, opts.limiterConfig.UidLeaseLimit)
		}

		if !s.rateLimiter.Allow(ns, int64(num.Val)) {
			// Return error after random delay.
			//nolint:gosec // random generator in closed set does not require cryptographic precision
			delay := rand.Intn(int(opts.limiterConfig.RefillAfter))
			time.Sleep(time.Duration(delay))
			return errors.Errorf("Cannot lease UID because UID lease for the namespace %#x is "+
				"exhausted. Please retry after some time.", ns)
		}
		return nil
	}

	reply := &emptyAssignedIds
	lease := func() error {
		var err error
		if s.Node.AmLeader() {
			if err := rateLimit(); err != nil {
				return err
			}
			span.SetAttributes(attribute.String("tablet", "predicate"))
			span.AddEvent(fmt.Sprintf("Zero leader leasing %d ids", num.GetVal()))
			reply, err = s.lease(ctx, num)
			return err
		}
		span.SetAttributes(attribute.String("Not Zero leader", "true"))
		span.AddEvent("Not Zero leader")
		// I'm not the leader and this request was forwarded to me by a peer, who thought I'm the
		// leader.
		if num.Forwarded {
			return errors.Errorf("Invalid Zero received AssignIds request forward. Please retry")
		}
		// This is an original request. Forward it to the leader.
		pl := s.Leader(0)
		if pl == nil {
			return errors.Errorf("No healthy connection found to Leader of group zero")
		}
		span.AddEvent(fmt.Sprintf("Sending request to %v", pl.Addr))
		zc := pb.NewZeroClient(pl.Get())
		num.Forwarded = true
		// pass on the incoming metadata to the zero leader.
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			ctx = metadata.NewOutgoingContext(ctx, md)
		}
		reply, err = zc.AssignIds(ctx, num)
		return err
	}

	// If this is a bump request and the current node is the leader then we create a normal lease
	// request based on the number of required ids to reach the asked bump value. If the current
	// node is not the leader then the bump request will be forwarded to the leader by lease().
	if num.GetBump() && s.Node.AmLeader() {
		s.leaseLock.Lock()
		cur := s.nextUint[num.GetType()] - 1
		s.leaseLock.Unlock()

		// We need to lease more UIDs if bump request is more than current max lease.
		req := num.GetVal()
		if cur >= req {
			return &emptyAssignedIds, errors.Errorf("Nothing to be leased")
		}
		num.Val = req - cur

		// Set bump to false because we want to lease the required ids in the following request.
		num.Bump = false
	}

	c := make(chan error, 1)
	go func() {
		c <- lease()
	}()

	select {
	case <-ctx.Done():
		return &emptyAssignedIds, ctx.Err()
	case err := <-c:
		span.AddEvent(fmt.Sprintf("Error while leasing %+v: %v", num, err))
		return reply, err
	}
}
