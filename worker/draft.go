/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.etcd.io/etcd/raft/v3"
	"go.etcd.io/etcd/raft/v3/raftpb"
	ostats "go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/badger/v4"
	bpb "github.com/dgraph-io/badger/v4/pb"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/conn"
	"github.com/hypermodeinc/dgraph/v25/posting"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/raftwal"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/x"
)

type operation struct {
	*z.Closer
	ts uint64
}

type node struct {
	// This needs to be 64 bit aligned for atomics to work on 32 bit machine.
	pendingSize int64

	// embedded struct
	*conn.Node

	// Fields which are never changed after init.
	applyCh chan []raftpb.Entry
	ctx     context.Context
	gid     uint32
	closer  *z.Closer

	checkpointTs uint64 // Timestamp corresponding to checkpoint.
	streaming    int32  // Used to avoid calculating snapshot

	// Used to track the ops going on in the system.
	ops         map[op]operation
	opsLock     sync.Mutex
	cdcTracker  *CDC
	canCampaign bool
}

type op int

func (id op) String() string {
	switch id {
	case opRollup:
		return "opRollup"
	case opSnapshot:
		return "opSnapshot"
	case opIndexing:
		return "opIndexing"
	case opRestore:
		return "opRestore"
	case opBackup:
		return "opBackup"
	case opPredMove:
		return "opPredMove"
	default:
		return "opUnknown"
	}
}

const (
	opRollup op = iota + 1
	opSnapshot
	opIndexing
	opRestore
	opBackup
	opPredMove
)

// startTask is used for the tasks that do not require tracking of timestamp.
// Currently, only the timestamps for backup and indexing needs to be tracked because they can
// run concurrently.
func (n *node) startTask(id op) (*z.Closer, error) {
	return n.startTaskAtTs(id, 0)
}

// startTaskAtTs is used to check whether an op is already running. If a rollup is running,
// it is canceled and startTask will wait until it completes before returning.
// If the same task is already running, this method returns an error.
// Restore operations have preference and cancel all other operations, not just rollups.
// You should only call Done() on the returned closer. Calling other functions (such as
// SignalAndWait) for closer could result in panics. For more details, see GitHub issue #5034.
func (n *node) startTaskAtTs(id op, ts uint64) (*z.Closer, error) {
	n.opsLock.Lock()
	defer n.opsLock.Unlock()

	stopTask := func(id op) {
		n.opsLock.Lock()
		delete(n.ops, id)
		n.opsLock.Unlock()
		glog.Infof("Operation completed with id: %s", id)

		// Resume rollups if another operation is being stopped.
		if id != opRollup {
			time.Sleep(10 * time.Second) // Wait for 10s to start rollup operation.
			// If any other operation is running, this would error out. This error can
			// be safely ignored because rollups will resume once that other task is done.
			_, _ = n.startTask(opRollup)
		}
	}

	closer := z.NewCloser(1)
	switch id {
	case opRollup:
		if len(n.ops) > 0 {
			return nil, errors.Errorf("another operation is already running")
		}
		go posting.IncrRollup.Process(closer, State.GetTimestamp)
	case opRestore:
		// Restores cancel all other operations, except for other restores since
		// only one restore operation should be active any given moment.
		for otherId, otherOp := range n.ops {
			if otherId == opRestore {
				return nil, errors.Errorf("another restore operation is already running")
			}
			// Remove from map and signal the closer to cancel the operation.
			delete(n.ops, otherId)
			otherOp.SignalAndWait()
		}
	case opBackup:
		// Backup cancels all other operations, except for other backups since
		// only one backup operation should be active any given moment. Also, indexing at higher
		// timestamp can also run concurrently with backup.
		for otherId, otherOp := range n.ops {
			if otherId == opBackup {
				return nil, errors.Errorf("another backup operation is already running")
			}
			// Remove from map and signal the closer to cancel the operation.
			delete(n.ops, otherId)
			otherOp.SignalAndWait()
		}
	case opIndexing:
		for otherId, otherOp := range n.ops {
			switch otherId {
			case opBackup:
				if otherOp.ts < ts {
					// If backup is running at higher timestamp, then indexing can't be executed.
					continue
				}
				return nil, errors.Errorf("operation %s is already running", otherId)
			case opRollup:
				// Remove from map and signal the closer to cancel the operation.
				delete(n.ops, otherId)
				otherOp.SignalAndWait()
			default:
				return nil, errors.Errorf("operation %s is already running", otherId)
			}
		}
	case opSnapshot, opPredMove:
		for otherId, otherOp := range n.ops {
			if otherId == opRollup {
				// Remove from map and signal the closer to cancel the operation.
				delete(n.ops, otherId)
				otherOp.SignalAndWait()
			} else {
				return nil, errors.Errorf("operation %s is already running", otherId)
			}
		}
	default:
		glog.Errorf("Got an unhandled operation %s. Ignoring...", id)
		return nil, nil
	}

	n.ops[id] = operation{Closer: closer, ts: ts}
	glog.Infof("Operation started with id: %s", id)
	go func(id op, closer *z.Closer) {
		closer.Wait()
		stopTask(id)
	}(id, closer)
	return closer, nil
}

func (n *node) waitForTask(id op) {
	n.opsLock.Lock()
	closer, ok := n.ops[id]
	n.opsLock.Unlock()
	if !ok {
		return
	}
	closer.Wait()
}

func (n *node) isRunningTask(id op) bool {
	n.opsLock.Lock()
	_, ok := n.ops[id]
	n.opsLock.Unlock()
	return ok
}

func (n *node) stopAllTasks() {
	defer n.closer.Done() // CLOSER:1
	<-n.closer.HasBeenClosed()

	glog.Infof("Stopping all ongoing registered tasks...")
	n.opsLock.Lock()
	defer n.opsLock.Unlock()
	for op, closer := range n.ops {
		glog.Infof("Stopping op: %s...\n", op)
		closer.SignalAndWait()
	}
	glog.Infof("Stopped all ongoing registered tasks.")
}

// GetOngoingTasks returns the list of ongoing tasks.
func GetOngoingTasks() []string {
	n := groups().Node
	if n == nil {
		return []string{}
	}

	n.opsLock.Lock()
	defer n.opsLock.Unlock()
	var tasks []string
	for id := range n.ops {
		tasks = append(tasks, id.String())
	}
	return tasks
}

// Now that we apply txn updates via Raft, waiting based on Txn timestamps is
// sufficient. We don't need to wait for proposals to be applied.

func newNode(store *raftwal.DiskStorage, gid uint32, id uint64, myAddr string) *node {
	glog.Infof("Node ID: %#x with GroupID: %d\n", id, gid)

	isLearner := x.WorkerConfig.Raft.GetBool("learner")
	rc := &pb.RaftContext{
		Addr:      myAddr,
		Group:     gid,
		Id:        id,
		IsLearner: isLearner,
	}
	glog.Infof("RaftContext: %+v\n", rc)
	m := conn.NewNode(rc, store, x.WorkerConfig.TLSClientConfig)

	n := &node{
		Node: m,
		ctx:  context.Background(),
		gid:  gid,
		// We need a generous size for applyCh, because raft.Tick happens every
		// 10ms. If we restrict the size here, then Raft goes into a loop trying
		// to maintain quorum health.
		applyCh:    make(chan []raftpb.Entry, 1000),
		closer:     z.NewCloser(4), // Matches CLOSER:1
		ops:        make(map[op]operation),
		cdcTracker: newCDC(),
	}
	return n
}

func (n *node) Ctx(key uint64) context.Context {
	if pctx := n.Proposals.Get(key); pctx != nil {
		return pctx.Ctx
	}
	return context.Background()
}

func (n *node) applyConfChange(e raftpb.Entry) {
	var cc raftpb.ConfChange
	if err := cc.Unmarshal(e.Data); err != nil {
		glog.Errorf("While unmarshalling confchange: %+v", err)
	}

	if cc.Type == raftpb.ConfChangeRemoveNode {
		n.DeletePeer(cc.NodeID)
	} else if len(cc.Context) > 0 {
		var rc pb.RaftContext
		x.Check(proto.Unmarshal(cc.Context, &rc))
		n.Connect(rc.Id, rc.Addr)
	}

	cs := n.Raft().ApplyConfChange(cc)
	n.SetConfState(cs)
	n.DoneConfChange(cc.ID, nil)
}

var errHasPendingTxns = errors.New("Pending transactions found. Please retry operation")

// We must not wait here. Previously, we used to block until we have aborted the
// transactions. We're now applying all updates serially, so blocking for one
// operation is not an option.
func detectPendingTxns(attr string) error {
	tctxs := posting.Oracle().IterateTxns(func(key []byte) bool {
		pk, err := x.Parse(key)
		if err != nil {
			glog.Errorf("error %v while parsing key %v", err, hex.EncodeToString(key))
			return false
		}
		return pk.Attr == attr
	})
	if len(tctxs) == 0 {
		return nil
	}
	go tryAbortTransactions(tctxs)
	return errHasPendingTxns
}

// We don't support schema mutations across nodes in a transaction.
// Wait for all transactions to either abort or complete and all write transactions
// involving the predicate are aborted until schema mutations are done.
func (n *node) applyMutations(ctx context.Context, proposal *pb.Proposal) (rerr error) {
	span := trace.SpanFromContext(ctx)

	if proposal.Mutations.DropOp == pb.Mutations_ALL_IN_NS {
		ns, err := strconv.ParseUint(proposal.Mutations.DropValue, 0, 64)
		if err != nil {
			return err
		}
		// Ensures nothing get written to disk due to commit proposals.
		posting.Oracle().ResetTxnsForNs(ns)
		if err := posting.DeleteAllForNs(ns); err != nil {
			return err
		}

		// TODO: What about multi shard cluster?
		// It should be okay to set the schema at timestamp 1 after drop all operation.
		if groups().groupId() == 1 {
			initialSchema := schema.InitialSchema(ns)
			for _, s := range initialSchema {
				if err := applySchema(s, 1); err != nil {
					return err
				}
			}
		}

		// Propose initial types as well after a drop all as they would have been cleared.
		initialTypes := schema.InitialTypes(ns)
		for _, t := range initialTypes {
			if err := updateType(t.GetTypeName(), t, 1); err != nil {
				return err
			}
		}

		// TODO: Revisit this when we work on posting cache. Don't clear entire cache.
		// We don't want to drop entire cache, just due to one namespace.
		posting.ResetCache()
		return nil
	}

	if proposal.Mutations.DropOp == pb.Mutations_DATA {
		ns, err := strconv.ParseUint(proposal.Mutations.DropValue, 0, 64)
		if err != nil {
			return err
		}
		// Ensures nothing get written to disk due to commit proposals.
		posting.Oracle().ResetTxnsForNs(ns)
		if err := posting.DeleteData(ns); err != nil {
			return err
		}

		// TODO: Revisit this when we work on posting cache. Don't clear entire cache.
		// We don't want to drop entire cache, just due to one namespace.
		posting.ResetCache()
		return nil
	}

	if proposal.Mutations.DropOp == pb.Mutations_ALL {
		// Ensures nothing get written to disk due to commit proposals.
		posting.Oracle().ResetTxns()
		schema.State().DeleteAll()

		if err := posting.DeleteAll(); err != nil {
			return err
		}

		// Clear entire cache.
		posting.ResetCache()

		// It should be okay to set the schema at timestamp 1 after drop all operation.
		if groups().groupId() == 1 {
			initialSchema := schema.InitialSchema(x.RootNamespace)
			for _, s := range initialSchema {
				if err := applySchema(s, 1); err != nil {
					return err
				}
			}
		}

		// Propose initial types as well after a drop all as they would have been cleared.
		initialTypes := schema.InitialTypes(x.RootNamespace)
		for _, t := range initialTypes {
			if err := updateType(t.GetTypeName(), t, 1); err != nil {
				return err
			}
		}

		return nil
	}

	if proposal.Mutations.DropOp == pb.Mutations_TYPE {
		return schema.State().DeleteType(proposal.Mutations.DropValue, proposal.StartTs)
	}

	if proposal.Mutations.StartTs == 0 {
		return errors.New("StartTs must be provided")
	}

	if len(proposal.Mutations.Schema) > 0 || len(proposal.Mutations.Types) > 0 {
		// MaxAssigned would ensure that everything that's committed up until this point
		// would be picked up in building indexes. Any uncommitted txns would be cancelled
		// by detectPendingTxns below.
		startTs := posting.Oracle().MaxAssigned()

		span.AddEvent("Applying schema and types")
		for _, supdate := range proposal.Mutations.Schema {
			// We should not need to check for predicate move here.
			if err := detectPendingTxns(supdate.Predicate); err != nil {
				return err
			}
		}

		if err := runSchemaMutation(ctx, proposal.Mutations.Schema, startTs); err != nil {
			return err
		}

		// Clear the entire cache if there is a schema update because the index rebuild
		// will invalidate the state.
		if len(proposal.Mutations.Schema) > 0 {
			posting.ResetCache()
		}

		for _, tupdate := range proposal.Mutations.Types {
			if err := runTypeMutation(ctx, tupdate, startTs); err != nil {
				return err
			}
		}
		return nil
	}

	// Scheduler tracks tasks at subject, predicate level, so doing
	// schema stuff here simplifies the design and we needn't worry about
	// serializing the mutations per predicate or schema mutations
	// We derive the schema here if it's not present
	// Since raft committed logs are serialized, we can derive
	// schema here without any locking

	// Stores a map of predicate and type of first mutation for each predicate.
	schemaMap := make(map[string]types.TypeID)
	for _, edge := range proposal.Mutations.Edges {
		if edge.Entity == 0 && bytes.Equal(edge.Value, []byte(x.Star)) {
			// We should only drop the predicate if there is no pending
			// transaction.
			if err := detectPendingTxns(edge.Attr); err != nil {
				span.AddEvent("Found pending transactions. Retry later.")
				return err
			}
			span.AddEvent("Deleting predicate")
			return posting.DeletePredicate(ctx, edge.Attr, proposal.StartTs)
		}
		// Don't derive schema when doing deletion.
		if edge.Op == pb.DirectedEdge_DEL {
			continue
		}
		if _, ok := schemaMap[edge.Attr]; !ok {
			schemaMap[edge.Attr] = posting.TypeID(edge)
		}
	}

	total := len(proposal.Mutations.Edges)

	// TODO: Active mutations values can go up or down but with
	// OpenCensus stats bucket boundaries start from 0, hence
	// recording negative and positive values skews up values.
	ostats.Record(ctx, x.ActiveMutations.M(int64(total)))
	defer func() {
		ostats.Record(ctx, x.ActiveMutations.M(int64(-total)))
	}()

	// Go through all the predicates and their first observed schema type. If we are unable to find
	// these predicates in the current schema state, add them to the schema state. Note that the
	// schema deduction is done by RDF/JSON chunker.
	for attr, storageType := range schemaMap {
		if _, err := schema.State().TypeOf(attr); err != nil {
			hint := pb.Metadata_DEFAULT
			if mutHint, ok := proposal.GetMutations().GetMetadata().GetPredHints()[attr]; ok {
				hint = mutHint
			}
			if err = createSchema(attr, storageType, hint, proposal.StartTs); err != nil {
				return err
			}
		}
	}

	m := proposal.Mutations

	txn := posting.Oracle().RegisterStartTs(m.StartTs)
	if txn.ShouldAbort() {
		span.AddEvent("Txn should abort.", trace.WithAttributes(
			attribute.Int64("start_ts", int64(m.StartTs)),
		))
		return x.ErrConflict
	}
	// Discard the posting lists from cache to release memory at the end.
	defer txn.Update()

	featureFlag := true
	if featureFlag {
		mp := posting.NewMutationPipeline(txn)
		return mp.Process(ctx, m.Edges)
	}

	// It is possible that the user gives us multiple versions of the same edge, one with no facets
	// and another with facets. In that case, use stable sort to maintain the ordering given to us
	// by the user.
	sort.SliceStable(m.Edges, func(i, j int) bool {
		ei := m.Edges[i]
		ej := m.Edges[j]
		if ei.GetAttr() != ej.GetAttr() {
			return ei.GetAttr() < ej.GetAttr()
		}
		return ei.GetEntity() < ej.GetEntity()
	})

	process := func(edges []*pb.DirectedEdge) error {
		var retries int
		for _, edge := range edges {
			for {
				err := runMutation(ctx, edge, txn)
				if err == nil {
					break
				}
				if err != posting.ErrRetry {
					return err
				}
				retries++
			}
		}
		if retries > 0 {
			span.AddEvent("retries=true num=%d", trace.WithAttributes(
				attribute.Int("retries", retries)))
		}
		return nil
	}
	numGo, width := x.DivideAndRule(len(m.Edges))
	span.AddEvent("To apply: %d edges. NumGo: %d. Width: %d", trace.WithAttributes(
		attribute.Int("num_edges", len(m.Edges)),
		attribute.Int("num_go", numGo),
		attribute.Int("width", width)))

	if numGo == 1 {
		return process(m.Edges)
	}

	// We need to create batches such that no two batches contains the same entry (<Entity> + <Predicate>)
	// combination. We have already sorted the edges, all we need to do is increase the batch size until we reach
	// the next different entry. New number of chans would ne less than NumGo. So we can create the chan with
	// numGo.
	sameAttrAndUid := func(i, j int) bool {
		ei := m.Edges[i]
		ej := m.Edges[j]
		if ei.GetAttr() != ej.GetAttr() {
			return false
		}
		return ei.GetEntity() == ej.GetEntity()
	}
	numChanCreated := 0

	errCh := make(chan error, numGo)
	for i := 0; i < len(m.Edges); {
		end := i + width
		if end > len(m.Edges) {
			end = len(m.Edges)
		}

		for end < len(m.Edges) && sameAttrAndUid(end, end-1) {
			end++
		}

		numChanCreated += 1
		go func(start, end int) {
			errCh <- process(m.Edges[start:end])
		}(i, end)
		i = end
	}
	// Earlier we were returning after even if one thread had an error. We should wait for
	// all the transactions to finish. We call txn.Update() when this function exists. This could cause
	// a deadlock with runMutation.
	var errs error
	for range numChanCreated {
		if err := <-errCh; err != nil {
			if errs == nil {
				errs = errors.New("Got error while running mutation")
			}
			errs = errors.Wrap(err, errs.Error())
		}
	}
	return errs
}

func (n *node) applyCommitted(proposal *pb.Proposal, key uint64) error {
	start := time.Now()
	defer func() {
		since := time.Since(start)
		if since > 30*time.Second {
			glog.Warningf("applyCh entry took [%v] to handle: %#v", since, proposal)
		}
	}()

	ctx := n.Ctx(key)
	span := trace.SpanFromContext(ctx)
	span.AddEvent("Node.applyCommited", trace.WithAttributes(
		attribute.Int64("node id", int64(n.Id)),
		attribute.Int64("Group Id", int64(n.gid)),
		attribute.Int64("proposal key", int64(key))))

	if proposal.Mutations != nil {
		// syncmarks for this shouldn't be marked done until it's committed.
		span.AddEvent("Applying mutations")
		if err := n.applyMutations(ctx, proposal); err != nil {
			span.AddEvent("While applying mutations", trace.WithAttributes(
				attribute.String("error", err.Error())))
			return err
		}

		span.AddEvent("Done")
		return nil
	}

	switch {
	case len(proposal.Kv) > 0:
		return populateKeyValues(ctx, proposal.Kv)

	case proposal.State != nil:
		span.AddEvent("Applying state for key: %s", trace.WithAttributes(
			attribute.Int64("key", int64(key))))
		// This state needn't be snapshotted in this group, on restart we would fetch
		// a state which is latest or equal to this.
		groups().applyState(groups().Node.Id, proposal.State)
		return nil

	case len(proposal.CleanPredicate) > 0:
		span.AddEvent("Cleaning predicate: %s", trace.WithAttributes(
			attribute.String("predicate", proposal.CleanPredicate)))
		end := time.Now().Add(10 * time.Second)
		for proposal.ExpectedChecksum > 0 && time.Now().Before(end) {
			cur := atomic.LoadUint64(&groups().membershipChecksum)
			if proposal.ExpectedChecksum == cur {
				break
			}
			time.Sleep(100 * time.Millisecond)
			glog.Infof("Waiting for checksums to match. Expected: %d. Current: %d\n",
				proposal.ExpectedChecksum, cur)
		}
		if time.Now().After(end) {
			glog.Warningf(
				"Giving up on predicate deletion: %q due to timeout. Wanted checksum: %d.",
				proposal.CleanPredicate, proposal.ExpectedChecksum)
			return nil
		}
		err := posting.DeletePredicate(ctx, proposal.CleanPredicate, proposal.StartTs)
		if err == badger.ErrBannedKey {
			// Zero might send the delete predicate instruction to alpha when updating the
			// membership state. This can happen for predicates from banned namespaces too.
			glog.Warningf("Couldn't clean the predicate %s as it is already banned.",
				proposal.CleanPredicate)
			return nil
		}
		return err

	case proposal.Delta != nil:
		span.AddEvent("Applying Oracle Delta for key: %d", trace.WithAttributes(
			attribute.Int64("key", int64(key))))
		return n.commitOrAbort(key, proposal.Delta)

	case proposal.ExtSnapshotState != nil:
		// Handle ExtSnapshotState proposal with two distinct scenarios:
		// 1. Node was offline during streaming and is now rejoining so this we are figuring out using
		//    x.ExtSnapshotStreamingState if that is true then we can say node is rejoined because
		//    we are disabling it after completion of streaming:
		//     - We request and populate snapshot from the group leader
		//     - After receiving the snapshot, we perform post-stream processing to ensure data consistency
		//
		// 2. Node was online during streaming:
		//     - If the node was online during streaming, we only need to disable drain mode
		//     - No additional data synchronization is required since the node was already receiving updates
		//
		// Drop Data Handling:
		// - When DropData is true, we clear all data from the node
		// - This is used as a safety measure when:
		//   - Something went wrong with other groups/nodes
		//   - The node needs to be brought back in sync with the cluster
		//   - Ensures the node reaches a clean, consistent state
		switch {
		case proposal.ExtSnapshotState.Start:
			x.ExtSnapshotStreamingState(true)
			return nil
		case proposal.ExtSnapshotState.DropData:
			// When DropData is true, it implies that there was some issue with import (e.g. some group
			// may have started streaming but not finished or streaming never started because p directory
			// was not there). We can't be certain if we have disabled x.UpdateExtSnapshotStreamingState or
			// not, so we are disabling it here:
			x.ExtSnapshotStreamingState(false)
			if err := posting.DeleteAll(); err != nil {
				glog.Errorf("[import] failed to delete all data: %v", err)
				return err
			}
			return nil
		case proposal.ExtSnapshotState.Finish && x.IsExtSnapshotStreamingStateTrue():
			lastApplied := n.Applied.LastIndex()
			pl := groups().Leader(n.gid)
			if pl == nil {
				glog.Errorf("[import] failed to get connection pool for group %d", n.gid)
				return errors.Errorf("failed to get connection pool for group %d", n.gid)
			}
			glog.Infof("[import] requesting snapshot from leader")
			snap := &pb.Snapshot{
				Context: n.RaftContext,
				Index:   lastApplied,
				ReadTs:  State.GetTimestamp(false),
			}
			// Request and populate snapshot
			if err := n.populateSnapshot(snap, pl); err != nil {
				glog.Errorf("[import] failed to populate snapshot for rejoining node: %v", err)
				return errors.Wrapf(err, "failed to populate snapshot for rejoining node")
			}

			if err := postStreamProcessing(ctx); err != nil {
				glog.Errorf("[import] failed to post stream processing for rejoining node: %v", err)
				return errors.Wrapf(err, "failed to post stream processing for rejoining node")
			}
			glog.Infof("[import] successfully rejoined node")
			x.ExtSnapshotStreamingState(false)
		}

		return nil

	case proposal.Snapshot != nil:
		existing, err := n.Store.Snapshot()
		if err != nil {
			return err
		}
		snap := proposal.Snapshot
		if existing.Metadata.Index >= snap.Index {
			span.AddEvent("Skipping snapshot at %d, because found one at %d",
				trace.WithAttributes(
					attribute.Int64("snapshot index", int64(snap.Index)),
					attribute.Int64("existing index", int64(existing.Metadata.Index))))
			log := fmt.Sprintf("Skipping snapshot at %d, because found one at %d",
				snap.Index, existing.Metadata.Index)
			glog.Info(log)
			return nil
		}
		span.AddEvent("Creating snapshot: %+v", trace.WithAttributes(
			attribute.Int64("snapshot index", int64(snap.Index)),
			attribute.Int64("read ts", int64(snap.ReadTs))))
		glog.Infof("Creating snapshot at Index: %d, ReadTs: %d\n", snap.Index, snap.ReadTs)

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
		// We can now discard all invalid versions of keys below this ts.
		pstore.SetDiscardTs(snap.ReadTs)
		return nil
	case proposal.Restore != nil:
		// Enable draining mode for the duration of the restore processing.
		x.UpdateDrainingMode(true)
		if !proposal.Restore.IsPartial {
			defer x.UpdateDrainingMode(false)
		}

		var err error
		var closer *z.Closer
		closer, err = n.startTask(opRestore)
		if err != nil {
			return errors.Wrapf(err, "cannot start restore task")
		}
		defer closer.Done()

		glog.Infof("Got restore proposal at Index: %d, ReadTs: %d",
			proposal.Index, proposal.Restore.RestoreTs)
		if err := handleRestoreProposal(ctx, proposal.Restore, proposal.Index); err != nil {
			return err
		}

		// Call commitOrAbort to update the group checksums.
		ts := proposal.Restore.RestoreTs
		return n.commitOrAbort(key, &pb.OracleDelta{
			Txns: []*pb.TxnStatus{
				{StartTs: ts, CommitTs: ts},
			},
		})

	case proposal.DeleteNs != nil:
		x.AssertTrue(proposal.DeleteNs.Namespace != x.RootNamespace)
		span.AddEvent("Deleting namespace: %d", trace.WithAttributes(
			attribute.Int64("namespace", int64(proposal.DeleteNs.Namespace))))
		return posting.DeleteNamespace(proposal.DeleteNs.Namespace)

	case proposal.CdcState != nil:
		n.cdcTracker.updateCDCState(proposal.CdcState)
		return nil
	}
	x.Fatalf("Unknown proposal: %+v", proposal)
	return nil
}

func (n *node) processTabletSizes() {
	defer n.closer.Done()                   // CLOSER:1
	tick := time.NewTicker(5 * time.Minute) // Once every 5 minutes seems alright.
	defer tick.Stop()

	for {
		select {
		case <-n.closer.HasBeenClosed():
			return
		case <-tick.C:
			n.calculateTabletSizes()
		}
	}
}

func updateStartTs(p *pb.Proposal) {
	switch {
	case p.Mutations != nil:
		p.StartTs = p.Mutations.StartTs
	case p.Snapshot != nil:
		p.StartTs = p.Snapshot.ReadTs
	case p.Delta != nil:
		p.StartTs = 0 // Run this asap.
	case len(p.CleanPredicate) > 0 && p.StartTs == 0:
		// proposal.StartTs did not exist earlier. So, it would be zero when reading an old
		// proposal. This startTs is used for writing the schema.
		// Hence, use the ts=1 for such proposals.
		p.StartTs = 1
	default:
		// For now, not covering everything.
	}
}

func (n *node) processApplyCh() {
	defer n.closer.Done() // CLOSER:1

	type P struct {
		err  error
		size int
		seen time.Time
	}
	previous := make(map[uint64]*P)

	// This function must be run serially.
	handle := func(entries []raftpb.Entry) {
		glog.V(3).Infof("handling element in applyCh with #entries %v", len(entries))
		defer glog.V(3).Infof("done handling element in applyCh")

		var totalSize int64
		for _, entry := range entries {
			x.AssertTrue(len(entry.Data) > 0)

			// We use the size as a double check to ensure that we're
			// working with the same proposal as before.
			psz := entry.Size()
			totalSize += int64(psz)

			var proposal pb.Proposal
			key := binary.BigEndian.Uint64(entry.Data[:8])
			x.Check(proto.Unmarshal(entry.Data[8:], &proposal))
			proposal.Index = entry.Index
			updateStartTs(&proposal)

			var perr error
			p, ok := previous[key]
			if ok && p.err == nil && p.size == psz {
				msg := fmt.Sprintf("Proposal with key: %d already applied. Skipping index: %d."+
					" Delta: %+v Snapshot: %+v.\n",
					key, proposal.Index, proposal.Delta, proposal.Snapshot)
				glog.Infof(msg)
				previous[key].seen = time.Now() // Update the ts.
				// Don't break here. We still need to call the Done below.

			} else {
				// if this applyCommitted fails, how do we ensure
				start := time.Now()
				perr = n.applyCommitted(&proposal, key)
				if key != 0 {
					p := &P{err: perr, size: psz, seen: time.Now()}
					previous[key] = p
				}
				span := trace.SpanFromContext(n.ctx)
				if perr != nil {
					glog.Errorf("Applying proposal. Error: %v. Proposal: %q.", perr, &proposal)
					span.AddEvent(fmt.Sprintf("Applying proposal failed. Error: %v Proposal: %q", perr, &proposal))
				}
				span.AddEvent("Applied proposal with key: %d, index: %d. Err: %v",
					trace.WithAttributes(
						attribute.Int64("key", int64(key)),
						attribute.Int64("index", int64(proposal.Index)),
					))

				var tags []tag.Mutator
				switch {
				case proposal.Mutations != nil:
					tags = append(tags, tag.Upsert(x.KeyMethod, "apply.Mutations"))
				case proposal.Delta != nil:
					tags = append(tags, tag.Upsert(x.KeyMethod, "apply.Delta"))
				}
				ms := x.SinceMs(start)
				_ = ostats.RecordWithTags(context.Background(), tags, x.LatencyMs.M(ms))
			}

			n.Proposals.Done(key, perr)
			n.Applied.Done(proposal.Index)
			ostats.Record(context.Background(), x.RaftAppliedIndex.M(int64(n.Applied.DoneUntil())))
		}
		if sz := atomic.AddInt64(&n.pendingSize, -totalSize); sz < 0 {
			glog.Warningf("Pending size should remain above zero: %d", sz)
		}
	}

	maxAge := 10 * time.Minute
	tick := time.NewTicker(maxAge / 2)
	defer tick.Stop()

	for {
		select {
		case entries, ok := <-n.applyCh:
			if !ok {
				return
			}
			handle(entries)
		case <-tick.C:
			// We use this ticker to clear out previous map.
			now := time.Now()
			for key, p := range previous {
				if now.Sub(p.seen) > maxAge {
					delete(previous, key)
				}
			}
			glog.V(3).Infof("Size of previous map: %d", len(previous))
		}
	}
}

// TODO(Anurag - 4 May 2020): Are we using pkey? Remove if unused.
func (n *node) commitOrAbort(pkey uint64, delta *pb.OracleDelta) error {
	x.PrintOracleDelta(delta)
	// First let's commit all mutations to disk.
	writer := posting.NewTxnWriter(pstore)
	toDisk := func(start, commit uint64) {
		txn := posting.Oracle().GetTxn(start)
		if txn == nil || commit == 0 {
			return
		}
		// If the transaction has failed, we dont need to update it.
		if commit != 0 {
			txn.Update()
		}
		// We start with 20 ms, so that we end up waiting 5 mins by the end.
		// If there is any transient issue, it should get fixed within that timeframe.
		err := x.ExponentialRetry(int(x.Config.MaxRetries),
			20*time.Millisecond, func() error {
				err := txn.CommitToDisk(writer, commit)
				if err == badger.ErrBannedKey {
					glog.Errorf("Error while writing to banned namespace.")
					return nil
				}
				return err
			})

		if err != nil {
			glog.Errorf("Error while applying txn status to disk (%d -> %d): %v",
				start, commit, err)
			panic(err)
		}
	}

	for _, status := range delta.Txns {
		toDisk(status.StartTs, status.CommitTs)
	}
	if err := writer.Flush(); err != nil {
		return errors.Wrapf(err, "while flushing to disk")
	}

	if x.WorkerConfig.HardSync {
		if err := pstore.Sync(); err != nil {
			glog.Errorf("Error while calling Sync while commitOrAbort: %v", err)
		}
	}

	g := groups()
	if delta.GroupChecksums != nil && delta.GroupChecksums[g.groupId()] > 0 {
		atomic.StoreUint64(&g.deltaChecksum, delta.GroupChecksums[g.groupId()])
	}

	// Clear all the cached lists that were touched by this transaction.
	for _, status := range delta.Txns {
		txn := posting.Oracle().GetTxn(status.StartTs)
		txn.UpdateCachedKeys(status.CommitTs)
	}

	// Now advance Oracle(), so we can service waiting reads.
	posting.Oracle().ProcessDelta(delta)
	return nil
}

func (n *node) leaderBlocking() (*conn.Pool, error) {
	pool := groups().Leader(groups().groupId())
	if pool == nil {
		// Functions like retrieveSnapshot and joinPeers are blocking at initial start and
		// leader election for a group might not have happened when it is called. If we can't
		// find a leader, get latest state from Zero.
		if err := UpdateMembershipState(context.Background()); err != nil {
			return nil, errors.Errorf("Error while trying to update membership state: %+v", err)
		}
		return nil, errors.Errorf("Unable to reach leader in group %d", n.gid)
	}
	return pool, nil
}

func (n *node) Snapshot() (*pb.Snapshot, error) {
	if n == nil || n.Store == nil {
		return nil, conn.ErrNoNode
	}
	snap, err := n.Store.Snapshot()
	if err != nil {
		return nil, err
	}
	res := &pb.Snapshot{}
	if err := proto.Unmarshal(snap.Data, res); err != nil {
		return nil, err
	}
	return res, nil
}

func (n *node) retrieveSnapshot(snap *pb.Snapshot) error {
	closer, err := n.startTask(opSnapshot)
	if err != nil {
		return err
	}
	defer closer.Done()

	// In some edge cases, the Zero leader might not have been able to update
	// the status of Alpha leader. So, instead of blocking forever on waiting
	// for Zero to send us the updates info about the leader, we can just use
	// the Snapshot RaftContext, which contains the address of the leader.
	var pool *conn.Pool
	addr := snap.Context.GetAddr()
	glog.V(2).Infof("Snapshot.RaftContext.Addr: %q", addr)
	if len(addr) > 0 {
		p, err := conn.GetPools().Get(addr)
		if err != nil {
			glog.V(2).Infof("conn.Get(%q) Error: %v", addr, err)
		} else {
			pool = p
			glog.V(2).Infof("Leader connection picked from RaftContext")
		}
	}
	if pool == nil {
		glog.V(2).Infof("No leader conn from RaftContext. Using membership state.")
		p, err := n.leaderBlocking()
		if err != nil {
			return err
		}
		pool = p
	}

	// Need to clear pl's stored in memory for the case when retrieving snapshot with
	// index greater than this node's last index
	// Should invalidate/remove pl's to this group only ideally
	//
	// We can safely evict posting lists from memory. Because, all the updates corresponding to txn
	// commits up until then have already been written to pstore. And the way we take snapshots, we
	// keep all the pre-writes for a pending transaction, so they will come back to memory, as Raft
	// logs are replayed.
	if err := n.populateSnapshot(snap, pool); err != nil {
		return errors.Wrapf(err, "cannot retrieve snapshot from peer")
	}
	// Populate shard stores the streamed data directly into db, so we need to refresh
	// schema for current group id
	if err := schema.LoadFromDb(closer.Ctx()); err != nil {
		return errors.Wrapf(err, "while initializing schema")
	}
	groups().triggerMembershipSync()
	return nil
}

func (n *node) proposeCDCState(ts uint64) error {
	proposal := &pb.Proposal{
		CdcState: &pb.CDCState{
			SentTs: ts,
		},
	}
	glog.V(2).Infof("Proposing new CDC state ts: %d\n", ts)
	sz := proto.Size(proposal)
	data := make([]byte, 8+sz)
	x.Check2(x.MarshalToSizedBuffer(data[8:], proposal))
	data = data[:8+sz]
	return n.Raft().Propose(n.ctx, data)
}

func (n *node) proposeSnapshot() error {
	lastIdx := x.Min(n.Applied.DoneUntil(), n.cdcTracker.getSeenIndex())
	// We can't rely upon the Raft entries to determine the minPendingStart,
	// because there are many cases during mutations where we don't commit or
	// abort the transaction. This might happen due to an early error thrown.
	// Only the mutations which make it to Zero for a commit/abort decision have
	// corresponding Delta entries. So, instead of replicating all that logic
	// here, we just use the MinPendingStartTs tracked by the Oracle, and look
	// for that in the logs.
	//
	// So, we iterate over logs. If we hit MinPendingStartTs, that generates our
	// snapshotIdx. In any case, we continue picking up txn updates, to generate
	// a maxCommitTs, which would become the readTs for the snapshot.
	minPendingStart := x.Min(posting.Oracle().MinPendingStartTs(), n.cdcTracker.getTs())
	snap, err := n.calculateSnapshot(0, lastIdx, minPendingStart)
	if err != nil {
		return err
	}
	if snap == nil {
		return nil
	}
	proposal := &pb.Proposal{
		Snapshot: snap,
	}
	glog.V(2).Infof("Proposing snapshot: %+v\n", snap)
	sz := proto.Size(proposal)
	data := make([]byte, 8+sz)
	x.Check2(x.MarshalToSizedBuffer(data[8:], proposal))
	data = data[:8+sz]
	return n.Raft().Propose(n.ctx, data)
}

const (
	maxPendingSize int64 = 256 << 20 // in bytes.
	nodeApplyChan        = "pushing to raft node applyCh"
)

func rampMeter(address *int64, maxSize int64, component string) {
	start := time.Now()
	defer func() {
		if dur := time.Since(start); dur > time.Second {
			glog.Infof("Blocked %s for %v", component, dur.Round(time.Millisecond))
		}
	}()
	for {
		if atomic.LoadInt64(address) <= maxSize {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
}

func (n *node) updateRaftProgress() error {
	// Both leader and followers can independently update their Raft progress. We don't store
	// this in Raft WAL. Instead, this is used to just skip over log records that this Alpha
	// has already applied, to speed up things on a restart.
	//
	// Let's check what we already have. And only update if the new snap.Index is ahead of the last
	// stored applied.
	applied := n.Store.Uint(raftwal.CheckpointIndex)

	snap, err := n.calculateSnapshot(applied, n.Applied.DoneUntil(),
		posting.Oracle().MinPendingStartTs())
	if err != nil || snap == nil || snap.Index <= applied {
		return err
	}
	atomic.StoreUint64(&n.checkpointTs, snap.ReadTs)

	n.Store.SetUint(raftwal.CheckpointIndex, snap.GetIndex())
	glog.V(2).Infof("[%#x] Set Raft progress to index: %d, ts: %d.", n.Id, snap.Index, snap.ReadTs)
	return nil
}

func (n *node) checkpointAndClose(done chan struct{}) {
	slowTicker := time.NewTicker(time.Minute)
	lastSnapshotTime := time.Now()
	defer slowTicker.Stop()

	snapshotAfterEntries := x.WorkerConfig.Raft.GetUint64("snapshot-after-entries")
	x.AssertTruef(snapshotAfterEntries > 10, "raft.snapshot-after must be a number greater than 10")

	snapshotFrequency := x.WorkerConfig.Raft.GetDuration("snapshot-after-duration")

	for {
		select {
		case <-slowTicker.C:
			// Do these operations asynchronously away from the main Run loop to allow heartbeats to
			// be sent on time. Otherwise, followers would just keep running elections.

			glog.V(3).Infof("Size of applyCh: %d", len(n.applyCh))
			if err := n.updateRaftProgress(); err != nil {
				glog.Errorf("While updating Raft progress: %v", err)
			}

			if n.AmLeader() {
				// If leader doesn't have a snapshot, we should create one immediately. This is very
				// useful when you bring up the cluster from bulk loader. If you remove an alpha and
				// add a new alpha, the new follower won't get a snapshot if the leader doesn't have
				// one.
				snap, err := n.Store.Snapshot()
				if err != nil {
					glog.Errorf("While retrieving snapshot from Store: %v\n", err)
					continue
				}

				// If we don't have a snapshot, or if there are too many log files in Raft,
				// calculate a new snapshot.
				calculate := raft.IsEmptySnap(snap) || n.Store.NumLogFiles() > 4

				// Only take snapshot if both snapshotFrequency and
				// snapshotAfterEntries requirements are met. If set to 0,
				// we consider duration condition to be disabled.
				if snapshotFrequency == 0 || time.Since(lastSnapshotTime) > snapshotFrequency {
					if chk, err := n.Store.Checkpoint(); err == nil {
						if first, err := n.Store.FirstIndex(); err == nil {
							// Save some cycles by only calculating snapshot if the checkpoint
							// has gone quite a bit further than the first index.
							calculate = calculate || chk >= first+snapshotAfterEntries
							glog.V(3).Infof("Evaluating snapshot first:%d chk:%d (chk-first:%d) "+
								"snapshotAfterEntries:%d snap:%v", first, chk, chk-first,
								snapshotAfterEntries, calculate)
						}
					}
				}

				// We keep track of the applied index in the p directory. Even if we don't take
				// snapshot for a while and let the Raft logs grow and restart, we would not have to
				// run all the log entries, because we can tell Raft.Config to set Applied to that
				// index.
				// This applied index tracking also covers the case when we have a big index
				// rebuild. The rebuild would be tracked just like others and would not need to be
				// replayed after a restart, because the Applied config would let us skip right
				// through it.
				// We use disk based storage for Raft. So, we're not too concerned about
				// snapshotting.  We just need to do enough, so that we don't have a huge backlog of
				// entries to process on a restart.
				if calculate {
					// We can set discardN argument to zero, because we already know that calculate
					// would be true if either we absolutely needed to calculate the snapshot,
					// or our checkpoint already crossed the SnapshotAfter threshold.
					if err := n.proposeSnapshot(); err != nil {
						glog.Errorf("While calculating and proposing snapshot: %v", err)
					} else {
						lastSnapshotTime = time.Now()
					}
				}
				go n.abortOldTransactions()
			}

		case <-n.closer.HasBeenClosed():
			glog.Infof("Stopping node.Run")
			if peerId, has := groups().MyPeer(); has && n.AmLeader() {
				n.Raft().TransferLeadership(n.ctx, n.Id, peerId)
				time.Sleep(time.Second) // Let transfer happen.
			}
			n.Raft().Stop()
			close(done)
			return
		}
	}
}

func (n *node) drainApplyChan() {
	numDrained := 0
	for {
		select {
		case entries := <-n.applyCh:
			numDrained += len(entries)
			for _, entry := range entries {
				key := binary.BigEndian.Uint64(entry.Data[:8])
				n.Proposals.Done(key, nil)
				n.Applied.Done(entry.Index)
			}
		default:
			glog.Infof("Drained %d proposals\n", numDrained)
			return
		}
	}
}

const tickDur = 100 * time.Millisecond

func (n *node) Run() {
	defer n.closer.Done() // CLOSER:1

	// lastLead is for detecting leadership changes
	//
	// etcd has a similar mechanism for tracking leader changes, with their
	// raftReadyHandler.getLead() function that returns the previous leader
	lastLead := uint64(math.MaxUint64)

	firstRun := true
	var leader bool
	// See also our configuration of HeartbeatTick and ElectionTick.
	// Before we used to have 20ms ticks, but they would overload the Raft tick channel, causing
	// "tick missed to fire" logs. Etcd uses 100ms and they haven't seen those issues.
	// Additionally, using 100ms for ticks does not cause proposals to slow down, because they get
	// sent out asap and don't rely on ticks. So, setting this to 100ms instead of 20ms is a NOOP.
	ticker := time.NewTicker(tickDur)
	defer ticker.Stop()

	done := make(chan struct{})
	go n.checkpointAndClose(done)
	go n.ReportRaftComms()

	if !x.WorkerConfig.HardSync {
		closer := z.NewCloser(2)
		defer closer.SignalAndWait()
		go x.StoreSync(n.Store, closer)
		go x.StoreSync(pstore, closer)
	}

	applied, err := n.Store.Checkpoint()
	if err != nil {
		glog.Errorf("While trying to find raft progress: %v", err)
	} else {
		glog.Infof("Found Raft progress: %d", applied)
	}

	var timer x.Timer
	for {
		select {
		case <-done:
			// We use done channel here instead of closer.HasBeenClosed so that we can transfer
			// leadership in a goroutine. The push to n.applyCh happens in this loop, so the close
			// should happen here too. Otherwise, race condition between push and close happens.
			close(n.applyCh)
			glog.Infoln("Raft node done.")
			return

			// Slow ticker can't be placed here because figuring out checkpoints and snapshots takes
			// time and if the leader does not send heartbeats out during this time, the followers
			// start an election process. And that election process would just continue to happen
			// indefinitely because checkpoints and snapshots are being calculated indefinitely.
		case <-ticker.C:
			n.Raft().Tick()

		case rd := <-n.Raft().Ready():
			// TODO(Aman): Based on the code here https://github.com/etcd-io/etcd/tree/raft/v3.5.9/raft,
			// n.SaveToStorage should be called first before doing anything else.

			timer.Start()
			_, span := otel.Tracer("").Start(n.ctx, "Alpha.RunLoop")

			if rd.SoftState != nil {
				groups().triggerMembershipSync()
				leader = rd.RaftState == raft.StateLeader
				// create context with group id
				ctx, _ := tag.New(n.ctx, tag.Upsert(x.KeyGroup, fmt.Sprintf("%d", n.gid)))
				// detect leadership changes
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
				timer.Record("updating soft state")
			}
			if leader {
				// Leader can send messages in parallel with writing to disk.
				for i := range rd.Messages {
					// When there is a `raftpb.EntryConfChange` after creating the snapshot,
					// then the confState included in the snapshot is out of date. so We need
					// to update the confState before sending a snapshot to a follower.
					if rd.Messages[i].Type == raftpb.MsgSnap {
						rd.Messages[i].Snapshot.Metadata.ConfState = *n.ConfState()
					}

					// NOTE: We can do some optimizations here to drop messages.
					n.Send(&rd.Messages[i])
				}
				timer.Record("leader sending message")
			}
			if span != nil {
				span.AddEvent("Handled ReadStates and SoftState.")
			}

			// We move the retrieval of snapshot before we store the rd.Snapshot, so that in case
			// this node fails to get the snapshot, the Raft state would reflect that by not having
			// the snapshot on a future probe. This is different from the recommended order in Raft
			// docs where they assume that the Snapshot contains the full data, so even on a crash
			// between n.SaveToStorage and n.retrieveSnapshot, that Snapshot can be applied by the
			// node on a restart. In our case, we don't store the full data in snapshot, only the
			// metadata.  So, we should only store the snapshot received in Raft, iff we actually
			// were able to update the state.
			if !raft.IsEmptySnap(rd.Snapshot) {
				// We don't send snapshots to other nodes. But, if we get one, that means
				// either the leader is trying to bring us up to state; or this is the
				// snapshot that I created. Only the former case should be handled.
				snap := &pb.Snapshot{}
				x.Check(proto.Unmarshal(rd.Snapshot.Data, snap))
				rc := snap.GetContext()
				x.AssertTrue(rc.GetGroup() == n.gid)
				if rc.Id != n.Id {
					// Set node to unhealthy state here while it applies the snapshot.
					x.UpdateHealthStatus(false)

					// We are getting a new snapshot from leader. We need to wait for the applyCh to
					// finish applying the updates, otherwise, we'll end up overwriting the data
					// from the new snapshot that we retrieved.

					// Drain the apply channel. Snapshot will be retrieved next.
					maxIndex := n.Applied.LastIndex()
					glog.Infof("Drain applyCh by reaching %d before"+
						" retrieving snapshot\n", maxIndex)
					n.drainApplyChan()

					if err := n.Applied.WaitForMark(context.Background(), maxIndex); err != nil {
						glog.Errorf("Error waiting for mark for index %d: %+v", maxIndex, err)
					}

					if currSnap, err := n.Snapshot(); err != nil {
						// Retrieve entire snapshot from leader if node does not have
						// a current snapshot.
						glog.Errorf("Could not retrieve previous snapshot. Setting SinceTs to 0.")
						snap.SinceTs = 0
					} else {
						snap.SinceTs = currSnap.ReadTs
					}

					// It's ok to block ticks while retrieving snapshot, since it's a follower.
					glog.Infof("---> SNAPSHOT: %+v. Group %d from node id %#x\n",
						snap, n.gid, rc.Id)

					for {
						err := n.retrieveSnapshot(snap)
						if err == nil {
							glog.Infoln("---> Retrieve snapshot: OK.")
							break
						}
						glog.Errorf("While retrieving snapshot, error: %v. Retrying...", err)
						time.Sleep(time.Second) // Wait for a bit.
					}
					glog.Infof("---> SNAPSHOT: %+v. Group %d. DONE.\n", snap, n.gid)

					// Set node to healthy state here.
					x.UpdateHealthStatus(true)
				} else {
					glog.Infof("---> SNAPSHOT: %+v. Group %d from node id %#x [SELF]. Ignoring.\n",
						snap, n.gid, rc.Id)
				}
				if span != nil {
					span.AddEvent("Applied or retrieved snapshot.")
				}
				timer.Record("got snapshot")
			}

			// Store the hardstate and entries. Note that these are not CommittedEntries.
			n.SaveToStorage(&rd.HardState, rd.Entries, &rd.Snapshot)
			timer.Record("disk")
			span.AddEvent(fmt.Sprintf("Saved %d entries. Snapshot, HardState empty? (%v, %v)",
				len(rd.Entries),
				raft.IsEmptySnap(rd.Snapshot),
				raft.IsEmptyHardState(rd.HardState)))

			for x.WorkerConfig.HardSync && rd.MustSync {
				if err := n.Store.Sync(); err != nil {
					glog.Errorf("Error while calling Store.Sync: %+v", err)
					time.Sleep(10 * time.Millisecond)
					continue
				}
				timer.Record("sync")
				break
			}

			// Now schedule or apply committed entries.
			var entries []raftpb.Entry
			for _, entry := range rd.CommittedEntries {
				// Need applied watermarks for schema mutation also for read linearazibility
				// Applied watermarks needs to be emitted as soon as possible sequentially.
				// If we emit Mark{4, false} and Mark{4, true} before emitting Mark{3, false}
				// then doneUntil would be set as 4 as soon as Mark{4,true} is done and before
				// Mark{3, false} is emitted. So it's safer to emit watermarks as soon as
				// possible sequentially
				n.Applied.Begin(entry.Index)

				switch {
				case entry.Type == raftpb.EntryConfChange:
					n.applyConfChange(entry)
					// Not present in proposal map.
					n.Applied.Done(entry.Index)
					groups().triggerMembershipSync()
				case len(entry.Data) == 0:
					glog.V(3).Infof("Found empty data at index: %d", entry.Index)
					n.Applied.Done(entry.Index)
				case entry.Index < applied:
					glog.V(3).Infof("Skipping over already applied entry: %d", entry.Index)
					n.Applied.Done(entry.Index)
				default:
					key := binary.BigEndian.Uint64(entry.Data[:8])
					if pctx := n.Proposals.Get(key); pctx != nil {
						atomic.AddUint32(&pctx.Found, 1)
						if span := trace.SpanFromContext(pctx.Ctx); span != nil {
							span.AddEvent("Proposal found in CommittedEntries")
						}
					}
					entries = append(entries, entry)
				}
			}
			// Send the whole lot to applyCh in one go, instead of sending proposals one by one.
			if len(entries) > 0 {
				// Apply the meter this before adding size to pending size so some crazy big
				// proposal can be pushed to applyCh. If we do this after adding its size to
				// pending size, we could block forever in rampMeter.
				rampMeter(&n.pendingSize, maxPendingSize, nodeApplyChan)
				var pendingSize int64
				for _, e := range entries {
					pendingSize += int64(e.Size())
				}
				if sz := atomic.AddInt64(&n.pendingSize, pendingSize); sz > 2*maxPendingSize {
					glog.Warningf("Inflight proposal size: %d. There would be some throttling.", sz)
				}
				n.applyCh <- entries
			}

			if span != nil {
				span.AddEvent("Handled %d committed entries.",
					trace.WithAttributes(attribute.Int("count", len(rd.CommittedEntries))))
			}

			if !leader {
				// Followers should send messages later.
				for i := range rd.Messages {
					// When there is a `raftpb.EntryConfChange` after creating the snapshot,
					// then the confState included in the snapshot is out of date. so We need
					// to update the confState before sending a snapshot to a follower.
					if rd.Messages[i].Type == raftpb.MsgSnap {
						rd.Messages[i].Snapshot.Metadata.ConfState = *n.ConfState()
					}
					// NOTE: We can do some optimizations here to drop messages.
					n.Send(&rd.Messages[i])
				}
			}
			if span != nil {
				span.AddEvent("Followed queued messages.")
			}
			timer.Record("proposals")

			n.Raft().Advance()
			timer.Record("advance")

			if firstRun && n.canCampaign {
				go func() {
					if err := n.Raft().Campaign(n.ctx); err != nil {
						glog.Errorf("Error starting campaign for node %v: %+v", n.gid, err)
					}
				}()
				firstRun = false
			}
			if span != nil {
				span.AddEvent("Advanced Raft. Done.")
				span.End()
				if err := ostats.RecordWithTags(context.Background(),
					[]tag.Mutator{tag.Upsert(x.KeyMethod, "alpha.RunLoop")},
					x.LatencyMs.M(float64(timer.Total())/1e6)); err != nil {
					glog.Errorf("Error recording stats: %+v", err)
				}
			}
			if timer.Total() > 5*tickDur {
				glog.Warningf(
					"Raft.Ready took too long to process: %s"+
						" Num entries: %d. MustSync: %v",
					timer.String(), len(rd.Entries), rd.MustSync)
			}
		}
	}
}

func listWrap(kv *bpb.KV) *bpb.KVList {
	return &bpb.KVList{Kv: []*bpb.KV{kv}}
}

// calculateTabletSizes updates the tablet sizes for the keys.
func (n *node) calculateTabletSizes() {
	if !n.AmLeader() {
		// Only leader sends the tablet size updates to Zero. No one else does.
		return
	}
	var total int64
	tablets := make(map[string]*pb.Tablet)
	updateSize := func(tinfo badger.TableInfo) {
		// The error has already been checked by caller.
		left, _ := x.Parse(tinfo.Left)
		pred := left.Attr
		if pred == "" {
			return
		}
		if tablet, ok := tablets[pred]; ok {
			tablet.OnDiskBytes += int64(tinfo.OnDiskSize)
			tablet.UncompressedBytes += int64(tinfo.UncompressedSize)
		} else {
			tablets[pred] = &pb.Tablet{
				GroupId:           n.gid,
				Predicate:         pred,
				OnDiskBytes:       int64(tinfo.OnDiskSize),
				UncompressedBytes: int64(tinfo.UncompressedSize),
			}
		}
		total += int64(tinfo.OnDiskSize)
	}

	tableInfos := pstore.Tables()
	glog.V(2).Infof("Calculating tablet sizes. Found %d tables\n", len(tableInfos))
	for _, tinfo := range tableInfos {
		left, err := x.Parse(tinfo.Left)
		if err != nil {
			glog.V(3).Infof("Unable to parse key: %v", err)
			continue
		}
		right, err := x.Parse(tinfo.Right)
		if err != nil {
			glog.V(3).Infof("Unable to parse key: %v", err)
			continue
		}

		// Count the table only if it is occupied by a single predicate.
		if left.Attr == right.Attr {
			updateSize(tinfo)
		} else {
			glog.V(3).Info("Skipping table not owned by one predicate")
		}
	}

	if len(tablets) == 0 {
		glog.V(2).Infof("No tablets found.")
		return
	}
	// Update Zero with the tablet sizes. If Zero sees a tablet which does not belong to
	// this group, it would send instruction to delete that tablet. There's an edge case
	// here if the followers are still running Rollup, and happen to read a key before and
	// write after the tablet deletion, causing that tablet key to resurface. Then, only the
	// follower would have that key, not the leader.
	// However, if the follower then becomes the leader, we'd be able to get rid of that
	// key then. Alternatively, we could look into cancelling the Rollup if we see a
	// predicate deletion.
	if err := groups().doSendMembership(tablets); err != nil {
		glog.Warningf("While sending membership to Zero. Error: %v", err)
	} else {
		glog.V(2).Infof("Sent tablet size update to Zero. Total size: %s",
			humanize.Bytes(uint64(total)))
	}
}

var errNoConnection = errors.New("No connection exists")

func (n *node) blockingAbort(req *pb.TxnTimestamps) error {
	pl := groups().Leader(0)
	if pl == nil {
		return errNoConnection
	}
	zc := pb.NewZeroClient(pl.Get())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	delta, err := zc.TryAbort(ctx, req)
	glog.Infof("TryAbort %d txns with start ts. Error: %v\n", len(req.Ts), err)
	if err != nil || len(delta.Txns) == 0 {
		return err
	}

	// Let's propose the txn updates received from Zero. This is important because there are edge
	// cases where a txn status might have been missed by the group.
	aborted := &pb.OracleDelta{}
	for _, txn := range delta.Txns {
		// Only pick the aborts. DO NOT propose the commits. They must come in the right order via
		// oracle delta stream, otherwise, we'll end up losing some committed txns.
		if txn.CommitTs == 0 {
			aborted.Txns = append(aborted.Txns, txn)
		}
	}
	if len(aborted.Txns) == 0 {
		glog.Infoln("TryAbort: No aborts found. Quitting.")
		return nil
	}

	// We choose not to store the MaxAssigned, because it would cause our Oracle to move ahead
	// artificially. The Oracle delta stream moves that ahead in the right order, and we shouldn't
	// muck with that order here.
	glog.Infof("TryAbort selectively proposing only aborted txns: %+v\n", aborted)
	proposal := &pb.Proposal{Delta: aborted}
	return n.proposeAndWait(n.ctx, proposal)
}

// abortOldTransactions would find txns which have done pre-writes, but have been pending for a
// while. The time that is used is based on the last pre-write seen, so if a txn is doing a
// pre-write multiple times, we'll pick the timestamp of the last pre-write. Thus, this function
// would only act on the txns which have not been active in the last N minutes, and send them for
// abort. Note that only the leader runs this function.
func (n *node) abortOldTransactions() {
	// Aborts if not already committed.
	starts := posting.Oracle().TxnOlderThan(x.WorkerConfig.AbortOlderThan)
	if len(starts) == 0 {
		return
	}
	glog.Infof("Found %d old transactions. Acting to abort them.\n", len(starts))
	req := &pb.TxnTimestamps{Ts: starts}
	err := n.blockingAbort(req)
	glog.Infof("Done abortOldTransactions for %d txns. Error: %v\n", len(req.Ts), err)
}

// calculateSnapshot would calculate a snapshot index, considering these factors:
// - We only start discarding once we have at least discardN entries.
// - We are not overshooting the max applied entry. That is, we're not removing
// Raft entries before they get applied.
// - We are considering the minimum start ts that has yet to be committed or
// aborted. This way, we still keep all the mutations corresponding to this
// start ts in the Raft logs. This is important, because we don't persist
// pre-writes to disk in pstore.
// - In simple terms, this means we MUST keep all pending transactions in the Raft logs.
// - Find the maximum commit timestamp that we have seen.
// That would tell us about the maximum timestamp used to do any commits. This
// ts is what we can use for future reads of this snapshot.
// - Finally, this function would propose this snapshot index, so the entire
// group can apply it to their Raft stores.
//
// Txn0  | S0 |    |    | C0 |    |    |
// Txn1  |    | S1 |    |    |    | C1 |
// Txn2  |    |    | S2 | C2 |    |    |
// Txn3  |    |    |    |    | S3 |    |
// Txn4  |    |    |    |    |    |    | S4
// Index | i1 | i2 | i3 | i4 | i5 | i6 | i7
//
// At i7, min pending start ts = S3, therefore snapshotIdx = i5 - 1 = i4.
// At i7, max commit ts = C1, therefore readTs = C1.
//
// This function also takes a startIdx, which can be used an optimization to skip over Raft entries.
// This is useful when we already have a previous snapshot checkpoint (all txns have concluded up
// until that last checkpoint) that we can use as a new start point for the snapshot calculation.
func (n *node) calculateSnapshot(startIdx, lastIdx, minPendingStart uint64) (*pb.Snapshot, error) {
	_, span := otel.Tracer("").Start(n.ctx, "Calculate.Snapshot")
	defer span.End()
	discardN := 1

	// We do not need to block snapshot calculation because of a pending stream. Badger would have
	// pending iterators which would ensure that the data above their read ts would not be
	// discarded. Secondly, if a new snapshot does get calculated and applied, the follower can just
	// ask for the new snapshot. Blocking snapshot calculation has caused us issues when a follower
	// somehow kept streaming forever. Then, the leader didn't calculate snapshot, instead it
	// kept appending to Raft logs forever causing group wide issues.

	first, err := n.Store.FirstIndex()
	if err != nil {
		span.AddEvent("Error", trace.WithAttributes(
			attribute.String("error", err.Error())))
		return nil, err
	}
	span.AddEvent("First index", trace.WithAttributes(
		attribute.Int64("first", int64(first))))
	if startIdx > first {
		// If we're starting from a higher index, set first to that.
		first = startIdx
		span.AddEvent("Setting first to", trace.WithAttributes(
			attribute.Int64("first", int64(first))))
	}

	rsnap, err := n.Store.Snapshot()
	if err != nil {
		return nil, err
	}
	snap := &pb.Snapshot{}
	if len(rsnap.Data) > 0 {
		if err := proto.Unmarshal(rsnap.Data, snap); err != nil {
			return nil, err
		}
	}
	span.AddEvent("Last snapshot", trace.WithAttributes(
		attribute.String("snapshot", snap.String())))

	if int(lastIdx-first) < discardN {
		span.AddEvent("Skipping due to insufficient entries")
		return nil, nil
	}
	span.AddEvent("Found Raft entries", trace.WithAttributes(
		attribute.Int64("count", int64(lastIdx-first))))

	if num := posting.Oracle().NumPendingTxns(); num > 0 {
		// TODO (Damon): this is associated with stuck alphas. Is there anything else we should log here
		// such as the transaction IDs so we can see in logs if a specific tx is stuck for a long time and how long?
		glog.V(2).Infof("Num pending txns: %d", num)
	}

	maxCommitTs := snap.ReadTs
	var snapshotIdx uint64

	// Trying to retrieve all entries at once might cause out-of-memory issues in
	// cases where the raft log is too big to fit into memory. Instead of retrieving
	// all entries at once, retrieve it in batches of 64MB.
	var lastEntry raftpb.Entry
	for batchFirst := first; batchFirst <= lastIdx; {
		entries, err := n.Store.Entries(batchFirst, lastIdx+1, 256<<20)
		if err != nil {
			span.AddEvent("Error", trace.WithAttributes(
				attribute.String("error", err.Error())))
			return nil, err
		}
		// Exit early from the loop if no entries were found.
		if len(entries) == 0 {
			break
		}

		// Store the last entry (as it might be needed outside the loop) and set the
		// start of the new batch at the entry following it. Also set foundEntries to
		// true to indicate to the code outside the loop that entries were retrieved.
		lastEntry = entries[len(entries)-1]
		batchFirst = lastEntry.Index + 1

		for _, entry := range entries {
			if entry.Type != raftpb.EntryNormal || len(entry.Data) == 0 {
				continue
			}
			var proposal pb.Proposal
			if err := proto.Unmarshal(entry.Data[8:], &proposal); err != nil {
				span.AddEvent("Error", trace.WithAttributes(
					attribute.String("error", err.Error())))
				return nil, err
			}

			var start uint64
			if proposal.Mutations != nil {
				start = proposal.Mutations.StartTs
				if start >= minPendingStart && snapshotIdx == 0 {
					snapshotIdx = entry.Index - 1
				}
			}
			if proposal.Delta != nil {
				for _, txn := range proposal.Delta.GetTxns() {
					maxCommitTs = x.Max(maxCommitTs, txn.CommitTs)
				}
			}
		}
	}

	if maxCommitTs == 0 {
		span.AddEvent("maxCommitTs is zero")
		return nil, nil
	}
	if snapshotIdx == 0 {
		// It is possible that there are no pending transactions. In that case,
		// snapshotIdx would be zero.
		snapshotIdx = lastEntry.Index
		span.AddEvent("snapshotIdx is zero. Using last entry's index", trace.WithAttributes(
			attribute.Int64("index", int64(snapshotIdx))))
	}

	numDiscarding := snapshotIdx - first + 1
	span.AddEvent("Got snapshotIdx", trace.WithAttributes(
		attribute.Int64("snapshotIdx", int64(snapshotIdx)),
		attribute.Int64("maxCommitTs", int64(maxCommitTs)),
		attribute.Int64("numDiscarding", int64(numDiscarding)),
		attribute.Int64("minPendingStart", int64(minPendingStart))))

	if int(numDiscarding) < discardN {
		span.AddEvent("Skipping snapshot because insufficient discard entries")
		glog.Infof("Skipping snapshot at index: %d. Insufficient discard entries: %d."+
			" MinPendingStartTs: %d\n", snapshotIdx, numDiscarding, minPendingStart)
		return nil, nil
	}

	result := &pb.Snapshot{
		Context: n.RaftContext,
		Index:   snapshotIdx,
		ReadTs:  maxCommitTs,
	}
	span.AddEvent("Got snapshot", trace.WithAttributes(
		attribute.Stringer("snapshot", result)))
	return result, nil
}

func (n *node) joinPeers() error {
	pl, err := n.leaderBlocking()
	if err != nil {
		return err
	}

	c := pb.NewRaftClient(pl.Get())
	glog.Infof("Calling JoinCluster via leader: %s", pl.Addr)
	if _, err := c.JoinCluster(n.ctx, n.RaftContext); err != nil {
		return errors.Wrapf(err, "error while joining cluster")
	}
	glog.Infof("Done with JoinCluster call\n")
	return nil
}

// Checks if its a peer from the leader of the group.
func (n *node) isMember() (bool, error) {
	pl, err := n.leaderBlocking()
	if err != nil {
		return false, err
	}

	gconn := pl.Get()
	c := pb.NewRaftClient(gconn)
	glog.Infof("Calling IsPeer")
	pr, err := c.IsPeer(n.ctx, n.RaftContext)
	if err != nil {
		return false, errors.Wrapf(err, "error while joining cluster")
	}
	glog.Infof("Done with IsPeer call\n")
	return pr.Status, nil
}

func (n *node) retryUntilSuccess(fn func() error, pause time.Duration) {
	var err error
	for {
		if err = fn(); err == nil {
			break
		}
		glog.Errorf("Error while calling fn: %v. Retrying...\n", err)
		time.Sleep(pause)
	}
}

// InitAndStartNode gets called after having at least one membership sync with the cluster.
func (n *node) InitAndStartNode() {
	x.Check(initProposalKey(n.Id))
	_, restart, err := n.PastLife()
	x.Check(err)

	_, hasPeer := groups().MyPeer()
	if !restart && hasPeer {
		// The node has other peers, it might have crashed after joining the cluster and before
		// writing a snapshot. Check from leader, if it is part of the cluster. Consider this a
		// restart if it is part of the cluster, else start a new node.
		for {
			if restart, err = n.isMember(); err == nil {
				break
			}
			glog.Errorf("Error while calling hasPeer: %v. Retrying...\n", err)
			time.Sleep(time.Second)
		}
	}

	if n.RaftContext.IsLearner && !hasPeer {
		glog.Fatal("Cannot start a learner node without peer alpha nodes")
	}

	if restart {
		glog.Infof("Restarting node for group: %d\n", n.gid)
		sp, err := n.Store.Snapshot()
		x.Checkf(err, "Unable to get existing snapshot")
		if !raft.IsEmptySnap(sp) {
			// It is important that we pick up the conf state here.
			// Otherwise, we'll lose the store conf state, and it would get
			// overwritten with an empty state when a new snapshot is taken.
			// This causes a node to just hang on restart, because it finds a
			// zero-member Raft group.
			n.SetConfState(&sp.Metadata.ConfState)

			// TODO: Making connections here seems unnecessary, evaluate.
			members := groups().members(n.gid)
			for _, id := range sp.Metadata.ConfState.Voters {
				m, ok := members[id]
				if ok {
					n.Connect(id, m.Addr)
				}
			}
			for _, id := range sp.Metadata.ConfState.Learners {
				m, ok := members[id]
				if ok {
					n.Connect(id, m.Addr)
				}
			}
		}
		n.SetRaft(raft.RestartNode(n.Cfg))
		glog.V(2).Infoln("Restart node complete")

	} else {
		glog.Infof("New Node for group: %d\n", n.gid)
		if _, hasPeer := groups().MyPeer(); hasPeer {
			// Get snapshot before joining peers as it can take time to retrieve it and we dont
			// want the quorum to be inactive when it happens.
			// Update: This is an optimization, which adds complexity because it requires us to
			// understand the Raft state of the node. Let's instead have the node retrieve the
			// snapshot as needed after joining the group, instead of us forcing one upfront.
			glog.Infoln("Trying to join peers.")
			n.retryUntilSuccess(n.joinPeers, time.Second)
			// We call RestartNode here because nodes already exists in the cluster and
			// we setup the state by calling c.JoinCluster above. We can't call StartNode
			// here because we only have peer's IP addresses.
			n.SetRaft(raft.RestartNode(n.Cfg))
		} else {
			peers := []raft.Peer{{ID: n.Id}}
			n.SetRaft(raft.StartNode(n.Cfg, peers))
			// Trigger election, so this node can become the leader of this single-node cluster.
			n.canCampaign = true
		}
	}
	go n.processTabletSizes()
	go n.processApplyCh()
	go n.BatchAndSendMessages()
	go n.monitorRaftMetrics()
	go n.cdcTracker.processCDCEvents()
	// Ignoring the error since InitAndStartNode does not return an error and using x.Check would
	// not be the right thing to do.
	_, _ = n.startTask(opRollup)
	go n.stopAllTasks()
	go n.Run()
}

func (n *node) AmLeader() bool {
	if n.Raft() == nil {
		return false
	}
	r := n.Raft()
	return r.Status().Lead == r.Status().ID
}

func (n *node) monitorRaftMetrics() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		curPendingSize := atomic.LoadInt64(&n.pendingSize)
		ostats.Record(n.ctx, x.RaftPendingSize.M(curPendingSize))
		ostats.Record(n.ctx, x.RaftApplyCh.M(int64(len(n.applyCh))))
	}
}
