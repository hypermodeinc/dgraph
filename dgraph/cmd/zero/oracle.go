/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package zero

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	attribute "go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"

	"github.com/dgraph-io/badger/v4/y"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/tok/hnsw"
	"github.com/hypermodeinc/dgraph/v25/x"
)

// Oracle stores and manages the transaction state and conflict detection.
type Oracle struct {
	x.SafeMutex
	commits map[uint64]uint64 // startTs -> commitTs
	// TODO: Check if we need LRU.
	keyCommit   *z.Tree // fp(key) -> commitTs. Used to detect conflict.
	maxAssigned uint64  // max transaction assigned by us.

	// All transactions with startTs < startTxnTs return true for hasConflict.
	startTxnTs  uint64
	subscribers map[int]chan pb.OracleDelta
	updates     chan *pb.OracleDelta
	doneUntil   y.WaterMark
}

// Init initializes the oracle.
func (o *Oracle) Init() {
	o.commits = make(map[uint64]uint64)
	// Remove the older btree file, before creating NewTree, as it may contain stale data leading
	// to wrong results.
	o.keyCommit = z.NewTree("oracle")
	o.subscribers = make(map[int]chan pb.OracleDelta)
	o.updates = make(chan *pb.OracleDelta, 100000) // Keeping 1 second worth of updates.
	o.doneUntil.Init(nil)
	go o.sendDeltasToSubscribers()
}

// close releases the memory associated with btree used for keycommit.
func (o *Oracle) close() {
	if err := o.keyCommit.Close(); err != nil {
		glog.Warningf("error while closing tree: %v", err)
	}
}

func (o *Oracle) updateStartTxnTs(ts uint64) {
	o.Lock()
	defer o.Unlock()
	o.startTxnTs = ts
	o.keyCommit.Reset()
}

// TODO: This should be done during proposal application for Txn status.
func (o *Oracle) hasConflict(src *api.TxnContext) bool {
	// This transaction was started before I became leader.
	if src.StartTs < o.startTxnTs {
		return true
	}
	for _, k := range src.Keys {
		ki, err := strconv.ParseUint(k, 36, 64)
		if err != nil {
			glog.Errorf("Got error while parsing conflict key %q: %v\n", k, err)
			continue
		}
		if last := o.keyCommit.Get(ki); last > src.StartTs {
			return true
		}
	}
	return false
}

func (o *Oracle) purgeBelow(minTs uint64) {
	var timer x.Timer
	timer.Start()

	o.Lock()
	defer o.Unlock()

	// Set startTxnTs so that every txn with start ts less than this, would be aborted.
	o.startTxnTs = minTs

	// Dropping would be cheaper if abort/commits map is sharded
	for ts := range o.commits {
		if ts < minTs {
			delete(o.commits, ts)
		}
	}
	timer.Record("commits")

	// There is no transaction running with startTs less than minTs
	// So we can delete everything from rowCommit whose commitTs < minTs
	stats := o.keyCommit.Stats()
	if stats.Occupancy < 50.0 {
		return
	}
	o.keyCommit.DeleteBelow(minTs)
	timer.Record("deleteBelow")
	glog.V(2).Infof("Purged below ts:%d, len(o.commits):%d, keyCommit: [before: %+v, after: %+v].\n",
		minTs, len(o.commits), stats, o.keyCommit.Stats())
	if timer.Total() > time.Second {
		glog.V(2).Infof("Purge %s\n", timer.String())
	}
}

func (o *Oracle) commit(src *api.TxnContext) error {
	o.Lock()
	defer o.Unlock()

	if o.hasConflict(src) {
		return x.ErrConflict
	}
	// We store src.Keys as string to ensure compatibility with all the various language clients we
	// have. But, really they are just uint64s encoded as strings. We use base 36 during creation of
	// these keys in FillContext in posting/mvcc.go.
	for _, k := range src.Keys {
		ki, err := strconv.ParseUint(k, 36, 64)
		if err != nil {
			glog.Errorf("Got error while parsing conflict key %q: %v\n", k, err)
			continue
		}
		o.keyCommit.Set(ki, src.CommitTs) // CommitTs is handed out before calling this func.
	}
	return nil
}

func (o *Oracle) currentState() *pb.OracleDelta {
	o.AssertRLock()
	resp := &pb.OracleDelta{}
	for start, commit := range o.commits {
		resp.Txns = append(resp.Txns,
			&pb.TxnStatus{StartTs: start, CommitTs: commit})
	}
	resp.MaxAssigned = o.maxAssigned
	return resp
}

func (o *Oracle) newSubscriber() (<-chan pb.OracleDelta, int) {
	o.Lock()
	defer o.Unlock()
	var id int
	for {
		//nolint:gosec // random generator for node id does not require cryptographic precision
		id = rand.Int()
		if _, has := o.subscribers[id]; !has {
			break
		}
	}

	// The channel takes a delta instead of a pointer as the receiver needs to
	// modify it by setting the group checksums. Passing a pointer previously
	// resulted in a race condition.
	ch := make(chan pb.OracleDelta, 1000)
	ch <- *o.currentState() // Queue up the full state as the first entry.
	o.subscribers[id] = ch
	return ch, id
}

func (o *Oracle) removeSubscriber(id int) {
	o.Lock()
	defer o.Unlock()
	delete(o.subscribers, id)
}

// sendDeltasToSubscribers reads updates from the o.updates
// constructs a delta object containing transactions from one or more updates
// and sends the delta object to each subscriber's channel
func (o *Oracle) sendDeltasToSubscribers() {
	delta := &pb.OracleDelta{}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	// waitFor calculates the maximum value of delta.MaxAssigned and all the CommitTs of delta.Txns
	waitFor := func() uint64 {
		w := delta.MaxAssigned
		for _, txn := range delta.Txns {
			w = x.Max(w, txn.CommitTs)
		}
		return w
	}

	for {
	get_update:
		var update *pb.OracleDelta
		select {
		case update = <-o.updates:
		case <-ticker.C:
			wait := waitFor()
			if wait == 0 || o.doneUntil.DoneUntil() < wait {
				goto get_update
			}
			// Send empty update.
			update = &pb.OracleDelta{}
		}
	slurp_loop:
		for {
			delta.MaxAssigned = x.Max(delta.MaxAssigned, update.MaxAssigned)
			delta.Txns = append(delta.Txns, update.Txns...)
			select {
			case update = <-o.updates:
			default:
				break slurp_loop
			}
		}
		// No need to sort the txn updates here. Alpha would sort them before
		// applying.

		// Let's ensure that we have all the commits up until the max here.
		// Otherwise, we'll be sending commit timestamps out of order, which
		// would cause Alphas to drop some of them, during writes to Badger.

		newDelta := &pb.OracleDelta{}
		useNewDelta := false
		if o.doneUntil.DoneUntil() < waitFor() {
			if len(delta.Txns) > 10 {
				replacementTxn := []*pb.TxnStatus{}

				ts := o.doneUntil.DoneUntil()
				for _, txn := range delta.Txns {
					if txn.CommitTs > ts {
						replacementTxn = append(replacementTxn, txn)
					} else {
						newDelta.Txns = append(newDelta.Txns, txn)
						newDelta.MaxAssigned = x.Max(newDelta.MaxAssigned, txn.CommitTs)
					}
				}

				if len(newDelta.Txns) == 0 {
					continue
				}

				useNewDelta = true
				delta.Txns = replacementTxn
			} else {
				continue // The for loop doing blocking reads from o.updates.
				// We need at least one entry from the updates channel to pick up a missing update.
				// Don't goto slurp_loop, because it would break from select immediately.
			}
		}

		if glog.V(3) {
			glog.Infof("DoneUntil: %d. Sending delta: %+v\n", o.doneUntil.DoneUntil(), delta)
		}
		o.Lock()
		k := *delta
		if useNewDelta {
			k = *newDelta
		}
		for id, ch := range o.subscribers {
			select {
			case ch <- k:
			default:
				close(ch)
				delete(o.subscribers, id)
			}
		}
		o.Unlock()
		if useNewDelta {
			delta = &pb.OracleDelta{}
		}
	}
}

func (o *Oracle) updateCommitStatusHelper(index uint64, src *api.TxnContext) bool {
	o.Lock()
	defer o.Unlock()
	if _, ok := o.commits[src.StartTs]; ok {
		return false
	}
	if src.Aborted {
		o.commits[src.StartTs] = 0
	} else {
		o.commits[src.StartTs] = src.CommitTs
	}
	return true
}

func (o *Oracle) updateCommitStatus(index uint64, src *api.TxnContext) {
	// TODO: We should check if the tablet is in read-only status here.
	if o.updateCommitStatusHelper(index, src) {
		delta := new(pb.OracleDelta)
		delta.Txns = append(delta.Txns, &pb.TxnStatus{
			StartTs:  src.StartTs,
			CommitTs: o.commitTs(src.StartTs),
		})
		o.updates <- delta
	}
}

func (o *Oracle) commitTs(startTs uint64) uint64 {
	o.RLock()
	defer o.RUnlock()
	return o.commits[startTs]
}

func (o *Oracle) storePending(ids *pb.AssignedIds) {
	// Wait to finish up processing everything before start id.
	max := x.Max(ids.EndId, ids.ReadOnly)
	if err := o.doneUntil.WaitForMark(context.Background(), max); err != nil {
		glog.Errorf("Error while waiting for mark: %+v", err)
	}

	// Now send it out to updates.
	o.updates <- &pb.OracleDelta{MaxAssigned: max}

	o.Lock()
	defer o.Unlock()
	o.maxAssigned = x.Max(o.maxAssigned, max)
}

// MaxPending returns the maximum assigned timestamp.
func (o *Oracle) MaxPending() uint64 {
	o.RLock()
	defer o.RUnlock()
	return o.maxAssigned
}

// proposeTxn proposes a txn update, and then updates src to reflect the state
// of the commit after proposal is run.
func (s *Server) proposeTxn(ctx context.Context, src *api.TxnContext) error {
	var zp pb.ZeroProposal
	zp.Txn = &api.TxnContext{
		StartTs:  src.StartTs,
		CommitTs: src.CommitTs,
		Aborted:  src.Aborted,
	}

	// NOTE: It is important that we continue retrying proposeTxn until we succeed. This should
	// happen, irrespective of what the user context timeout might be. We check for it before
	// reaching this stage, but now that we're here, we have to ensure that the commit proposal goes
	// through. Otherwise, we should block here forever. If we don't do this, we'll see txn
	// violations in Jepsen, because we'll send out a MaxAssigned higher than a commit, which would
	// cause newer txns to see older data.

	// If this node stops being the leader, we want this proposal to not be forwarded to the leader,
	// and get aborted.
	if err := s.Node.proposeAndWait(ctx, &zp); err != nil {
		return err
	}

	// There might be race between this proposal trying to commit and predicate
	// move aborting it. A predicate move, triggered by Zero, would abort all
	// pending transactions.  At the same time, a client which has already done
	// mutations, can proceed to commit it. A race condition can happen here,
	// with both proposing their respective states, only one can succeed after
	// the proposal is done. So, check again to see the fate of the transaction
	// here.
	src.CommitTs = s.orc.commitTs(src.StartTs)
	if src.CommitTs == 0 {
		src.Aborted = true
	}
	return nil
}

func (s *Server) commit(ctx context.Context, src *api.TxnContext) error {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.Int64("startTs", int64(src.StartTs)))
	if src.Aborted {
		return s.proposeTxn(ctx, src)
	}

	// Use the start timestamp to check if we have a conflict, before we need to assign a commit ts.
	s.orc.RLock()
	conflict := s.orc.hasConflict(src)
	s.orc.RUnlock()
	if conflict {
		span.SetAttributes(attribute.Bool("abort", true))
		src.Aborted = true
		return s.proposeTxn(ctx, src)
	}

	checkPreds := func() error {
		// Check if any of these tablets is being moved. If so, abort the transaction.
		for _, pkey := range src.Preds {
			splits := strings.SplitN(pkey, "-", 2)
			if len(splits) < 2 {
				return errors.Errorf("Unable to find group id in %s", pkey)
			}
			gid, err := strconv.Atoi(splits[0])
			if err != nil {
				return errors.Wrapf(err, "unable to parse group id from %s", pkey)
			}
			pred := splits[1]
			if strings.Contains(pred, hnsw.VecKeyword) {
				pred = pred[0:strings.Index(pred, hnsw.VecKeyword)]
			}
			tablet := s.ServingTablet(pred)
			if tablet == nil {
				return errors.Errorf("Tablet for %s is nil", pred)
			}
			if tablet.GroupId != uint32(gid) {
				return errors.Errorf("Mutation done in group: %d. Predicate %s assigned to %d",
					gid, pred, tablet.GroupId)
			}
			if s.isBlocked(pred) {
				return errors.Errorf("Commits on predicate %s are blocked due to predicate move", pred)
			}
		}
		return nil
	}
	if err := checkPreds(); err != nil {
		span.SetAttributes(attribute.Bool("abort", true))
		src.Aborted = true
		return s.proposeTxn(ctx, src)
	}

	num := pb.Num{Val: 1, Type: pb.Num_TXN_TS}
	assigned, err := s.lease(ctx, &num)
	if err != nil {
		return err
	}
	src.CommitTs = assigned.StartId
	// Mark the transaction as done, irrespective of whether the proposal succeeded or not.
	defer s.orc.doneUntil.Done(src.CommitTs)
	span.SetAttributes(attribute.Int64("commitTs", int64(src.CommitTs)))
	span.SetAttributes(attribute.Int64("nodeId", int64(s.Node.Id)))
	span.AddEvent(fmt.Sprintf("TXN Context: %+v", src))

	if err := s.orc.commit(src); err != nil {
		span.SetAttributes(attribute.Bool("abort", true))
		src.Aborted = true
	}
	if err := ctx.Err(); err != nil {
		span.SetAttributes(attribute.Bool("abort", true))
		src.Aborted = true
	}
	// Propose txn should be used to set watermark as done.
	return s.proposeTxn(ctx, src)
}

// CommitOrAbort either commits a transaction or aborts it.
// The abortion can happen under the following conditions
// 1) the api.TxnContext.Aborted flag is set in the src argument
// 2) if there's an error (e.g server is not the leader or there's a conflicting transaction)
func (s *Server) CommitOrAbort(ctx context.Context, src *api.TxnContext) (*api.TxnContext, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	ctx, span := otel.Tracer("").Start(ctx, "Zero.CommitOrAbort")
	defer span.End()

	if !s.Node.AmLeader() {
		return nil, errors.Errorf("Only leader can decide to commit or abort")
	}
	err := s.commit(ctx, src)
	if err != nil {
		span.SetAttributes(attribute.Bool("error", true))
	}
	return src, err
}

var errClosed = errors.New("Streaming closed by oracle")
var errNotLeader = errors.New("Node is no longer leader")

// Oracle streams the oracle state to the alphas.
// The first entry sent by Zero contains the entire state of transactions. Zero periodically
// confirms receipt from the group, and truncates its state. This 2-way acknowledgement is a
// safe way to get the status of all the transactions.
func (s *Server) Oracle(_ *api.Payload, server pb.Zero_OracleServer) error {
	if !s.Node.AmLeader() {
		return errNotLeader
	}
	ch, id := s.orc.newSubscriber()
	defer s.orc.removeSubscriber(id)

	ctx := server.Context()
	leaderChangeCh := s.leaderChangeChannel()
	for {
		select {
		case <-leaderChangeCh:
			return errNotLeader
		case delta, open := <-ch:
			if !open {
				return errClosed
			}
			// Pass in the latest group checksum as well, so the Alpha can use that to determine
			// when not to service a read.
			delta.GroupChecksums = s.groupChecksums()
			if err := server.Send(&delta); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-s.closer.HasBeenClosed():
			return errServerShutDown
		}
	}
}

// TryAbort attempts to abort the given transactions which are not already committed..
func (s *Server) TryAbort(ctx context.Context,
	txns *pb.TxnTimestamps) (*pb.OracleDelta, error) {
	delta := &pb.OracleDelta{}
	for _, startTs := range txns.Ts {
		// Do via proposals to avoid race
		tctx := &api.TxnContext{StartTs: startTs, Aborted: true}
		if err := s.proposeTxn(ctx, tctx); err != nil {
			return delta, err
		}
		// Txn should be aborted if not already committed.
		delta.Txns = append(delta.Txns, &pb.TxnStatus{
			StartTs:  startTs,
			CommitTs: s.orc.commitTs(startTs)})
	}
	return delta, nil
}

// Timestamps is used to assign startTs for a new transaction
func (s *Server) Timestamps(ctx context.Context, num *pb.Num) (*pb.AssignedIds, error) {
	ctx, span := otel.Tracer("").Start(ctx, "Zero.Timestamps")
	defer span.End()

	span.SetAttributes(attribute.Int64("zeroId", int64(s.Node.Id)))
	span.SetAttributes(attribute.String("timestampRequest", fmt.Sprintf("%+v", num)))
	if ctx.Err() != nil {
		return &emptyAssignedIds, ctx.Err()
	}

	num.Type = pb.Num_TXN_TS
	reply, err := s.lease(ctx, num)
	span.AddEvent(fmt.Sprintf("Response: %+v, Error: %v", reply, err))

	switch err {
	case nil:
		s.orc.doneUntil.Done(x.Max(reply.EndId, reply.ReadOnly))
		go s.orc.storePending(reply)
	case errServedFromMemory:
		// Avoid calling doneUntil.Done, and storePending.
		err = nil
	default:
		glog.Errorf("Got error: %v while leasing timestamps: %+v", err, num)
	}
	return reply, err
}
