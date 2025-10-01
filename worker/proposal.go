/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"context"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	ostats "go.opencensus.io/stats"
	tag "go.opencensus.io/tag"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/hypermodeinc/dgraph/v25/conn"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/x"
)

const baseTimeout time.Duration = 4 * time.Second

func newTimeout(retry int) time.Duration {
	timeout := baseTimeout
	for range retry {
		timeout *= 2
	}
	return timeout
}

// limiter is initialized as part of worker Init.
var limiter rateLimiter

type rateLimiter struct {
	iou int
	max int
	c   *sync.Cond
}

// Instead of using the time/rate package, we use this simple one, because that
// allows a certain number of ops per second, without taking any feedback into
// account. We however, limit solely based on feedback, allowing a certain
// number of ops to remain pending, and not anymore.
func (rl *rateLimiter) bleed() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for range tick.C {
		rl.c.L.Lock()
		iou := rl.iou
		rl.c.L.Unlock()
		// Pending proposals is tracking ious.
		ostats.Record(context.Background(), x.PendingProposals.M(int64(iou)))
		rl.c.Broadcast()
	}
}

func (rl *rateLimiter) incr(ctx context.Context, retry int) error {
	// Let's not wait here via time.Sleep or similar. Let pendingProposals
	// channel do its natural rate limiting.
	weight := 1 << uint(retry) // Use an exponentially increasing weight.
	c := rl.c
	c.L.Lock()

	for {
		if rl.iou+weight <= rl.max {
			rl.iou += weight
			c.L.Unlock()
			return nil
		}
		c.Wait()
		// We woke up after some time. Let's check if the context is done.
		select {
		case <-ctx.Done():
			c.L.Unlock()
			return ctx.Err()
		default:
		}
	}
}

// Done would slowly bleed the retries out.
func (rl *rateLimiter) decr(retry int) {
	weight := 1 << uint(retry) // Ensure that the weight calculation is a copy of incr.

	rl.c.L.Lock()
	// decr() performs opposite of incr().
	// It reduces the rl.iou by weight as incr increases it by weight.
	rl.iou -= weight
	rl.c.L.Unlock()
	rl.c.Broadcast()
}

var proposalKey uint64

// {2 bytes Node ID} {4 bytes for random} {2 bytes zero}
func initProposalKey(id uint64) error {
	x.AssertTrue(id != 0)
	var err error
	proposalKey, err = x.ProposalKey(groups().Node.Id)
	if err != nil {
		return err
	}
	return nil
}

// uniqueKey is meant to be unique across all the replicas.
// initProposalKey should be called before calling uniqueKey.
func uniqueKey() uint64 {
	return atomic.AddUint64(&proposalKey, 1)
}

var errInternalRetry = errors.New("Retry Raft proposal internally")
var errUnableToServe = errors.New("Server overloaded with pending proposals. Please retry later")

// proposeAndWait sends a proposal through RAFT. It waits on a channel for the proposal
// to be applied(written to WAL) to all the nodes in the group.
func (n *node) proposeAndWait(ctx context.Context, proposal *pb.Proposal) (perr error) {
	startTime := time.Now()
	ctx = x.WithMethod(ctx, "n.proposeAndWait")
	defer func() {
		v := x.TagValueStatusOK
		if perr != nil {
			v = x.TagValueStatusError
		}
		ctx, _ = tag.New(ctx, tag.Upsert(x.KeyStatus, v))
		timeMs := x.SinceMs(startTime)
		ostats.Record(ctx, x.LatencyMs.M(timeMs))
	}()

	if n.Raft() == nil {
		return errors.Errorf("Raft isn't initialized yet")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Set this to disable retrying mechanism, and using the user-specified
	// timeout.
	var noTimeout bool

	checkTablet := func(pred string) error {
		tablet, err := groups().Tablet(pred)
		switch {
		case err != nil:
			return err
		case tablet == nil || tablet.GroupId == 0:
			return errNonExistentTablet
		case tablet.GroupId != groups().groupId():
			return errUnservedTablet
		default:
			return nil
		}
	}

	// Do a type check here if schema is present
	// In very rare cases invalid entries might pass through raft, which would
	// be persisted, we do best effort schema check while writing
	ctx = schema.GetWriteContext(ctx)
	if proposal.Mutations != nil {
		for _, edge := range proposal.Mutations.Edges {
			if err := checkTablet(edge.Attr); err != nil {
				return err
			}
			su, ok := schema.State().Get(ctx, edge.Attr)
			if !ok {
				// We don't allow mutations for reserved predicates if the schema for them doesn't
				// already exist.
				if x.IsReservedPredicate(edge.Attr) {
					return errors.Errorf("Can't store predicate `%s` as it is prefixed with "+
						"`dgraph.` which is reserved as the namespace for dgraph's internal "+
						"types/predicates.",
						x.ParseAttr(edge.Attr))
				}
				continue
			} else if err := ValidateAndConvert(edge, &su); err != nil {
				return err
			}
		}

		for _, schema := range proposal.Mutations.Schema {
			if err := checkTablet(schema.Predicate); err != nil {
				return err
			}
			if err := checkSchema(schema); err != nil {
				return err
			}
			noTimeout = true
		}
	}

	// Let's keep the same key, so multiple retries of the same proposal would
	// have this shared key. Thus, each server in the group can identify
	// whether it has already done this work, and if so, skip it.
	key := uniqueKey()
	sz := proto.Size(proposal)
	data := make([]byte, 8+sz)
	binary.BigEndian.PutUint64(data, key)
	_, err := x.MarshalToSizedBuffer(data[8:], proposal)
	if err != nil {
		return err
	}

	// Trim data to the new size after Marshal.
	data = data[:8+sz]

	span := trace.SpanFromContext(ctx)
	stop := x.SpanTimer(span, "n.proposeAndWait")
	defer stop()

	propose := func(timeout time.Duration) error {
		// We don't need to extend from base ctx as it might have a timeout. This timeout
		// is only to find the proposal back via Raft.
		cctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cctx = trace.ContextWithSpan(cctx, span)

		errCh := make(chan error, 1)
		pctx := &conn.ProposalCtx{
			ErrCh: errCh,
			Ctx:   cctx,
		}
		x.AssertTruef(n.Proposals.Store(key, pctx), "Found existing proposal with key: [%x]", key)
		defer n.Proposals.Delete(key) // Ensure that it gets deleted on return.

		span.AddEvent("Proposing", trace.WithAttributes(
			attribute.Int64("key", int64(key)),
			attribute.String("timeout", timeout.String())))

		if err = n.Raft().Propose(cctx, data); err != nil {
			return errors.Wrapf(err, "While proposing")
		}

		timer := time.NewTimer(timeout)
		defer timer.Stop()

		for {
			select {
			case err = <-errCh:
				// We arrived here by a call to n.Proposals.Done().
				return err
			case <-ctx.Done():
				glog.Warningf("Context expired while processing proposal %v", ctx.Err())
				return ctx.Err()
			case <-timer.C:
				if atomic.LoadUint32(&pctx.Found) > 0 {
					// We found the proposal in CommittedEntries. No need to retry.
				} else {
					span.AddEvent("Timeout reached", trace.WithAttributes(
						attribute.String("timeout", timeout.String())))
					cancel()
				}
			case <-cctx.Done():
				glog.Warningf("Internal context expired while processing proposal %v", cctx.Err())
				return errInternalRetry
			}
		}
	}

	// Some proposals, like schema updates are very expensive to retry. So, let's
	// not do the retry mechanism on them. Instead, we can set a long timeout.
	//
	// Note that timeout only affects how long it takes us to find the proposal back via Raft logs.
	// It does not consider the amount of time it takes to actually apply the proposal.
	//
	// Based on updated logic, once we find the proposal in the raft log, we would not cancel it
	// anyways. Instead, we'd let the proposal run its course.
	if noTimeout {
		return propose(3 * time.Minute)
	}

	// Some proposals can be stuck if leader change happens. For e.g. MsgProp message from follower
	// to leader can be dropped/end up appearing with empty Data in CommittedEntries.
	// Having a timeout here prevents the mutation being stuck forever in case they don't have a
	// timeout. We should always try with a timeout and optionally retry.
	//
	// Let's try 3 times before giving up.

	proposeWithLimit := func(i int) error {
		// Each retry creates a new proposal, which adds to the number of pending proposals. We
		// should consider this into account, when adding new proposals to the system.
		switch {
		case proposal.Delta != nil: // Is a delta.
			// If a proposal is important (like delta updates), let's not run it via the limiter
			// below. We should always propose it irrespective of how many pending proposals there
			// might be.
		default:
			span.AddEvent("Incrementing limiter", trace.WithAttributes(
				attribute.Int64("retry", int64(i))))
			if err := limiter.incr(ctx, i); err != nil {
				return err
			}
			// We have now acquired slots in limiter. We MUST release them before we retry this
			// proposal, otherwise we end up with dining philosopher problem.
			defer limiter.decr(i)
		}
		return propose(newTimeout(i))
	}

	for i := range 3 {
		if err := proposeWithLimit(i); err != errInternalRetry {
			return err
		}
	}
	return errUnableToServe
}
