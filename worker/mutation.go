/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"bytes"
	"context"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	ostats "go.opencensus.io/stats"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/y"
	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/conn"
	"github.com/hypermodeinc/dgraph/v25/posting"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/tok/hnsw"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var (
	// ErrNonExistentTabletMessage is the error message sent when no tablet is serving a predicate.
	ErrNonExistentTabletMessage = "Requested predicate is not being served by any tablet"
	errNonExistentTablet        = errors.Errorf("%v", ErrNonExistentTabletMessage)
	errUnservedTablet           = errors.Errorf("Tablet isn't being served by this instance")
)

// Default limit on number of simultaneous open files on unix systems
const DefaultMaxOpenFileLimit = 1024

func isStarAll(v []byte) bool {
	return bytes.Equal(v, []byte(x.Star))
}

func isDeletePredicateEdge(edge *pb.DirectedEdge) bool {
	return edge.Entity == 0 && isStarAll(edge.Value)
}

func newRunMutations(ctx context.Context, edges []*pb.DirectedEdge, txn *posting.Txn) error {
	mp := posting.NewMutationPipeline(txn)
	return mp.Process(ctx, edges)
}

func newRunMutation(ctx context.Context, edge *pb.DirectedEdge, txn *posting.Txn) error {
	mp := posting.NewMutationPipeline(txn)
	return mp.Process(ctx, []*pb.DirectedEdge{edge})
}

// runMutation goes through all the edges and applies them.
func runMutation(ctx context.Context, edge *pb.DirectedEdge, txn *posting.Txn) error {
	ctx = schema.GetWriteContext(ctx)

	// We shouldn't check whether this Alpha serves this predicate or not. Membership information
	// isn't consistent across the entire cluster. We should just apply whatever is given to us.
	su, ok := schema.State().Get(ctx, edge.Attr)
	if edge.Op != pb.DirectedEdge_DEL {
		if !ok {
			return errors.Errorf("runMutation: Unable to find schema for %s", edge.Attr)
		}
	}

	if isDeletePredicateEdge(edge) {
		return errors.New("We should never reach here")
	}

	// Once mutation comes via raft we do best effort conversion
	// Type check is done before proposing mutation, in case schema is not
	// present, some invalid entries might be written initially
	if err := ValidateAndConvert(edge, &su); err != nil {
		return err
	}

	key := x.DataKey(edge.Attr, edge.Entity)
	// The following is a performance optimization which allows us to not read a posting list from
	// disk. We calculate this based on how AddMutationWithIndex works. The general idea is that if
	// we're not using the read posting list, we don't need to retrieve it. We need the posting list
	// if we're doing count index or delete operation. For scalar predicates, we just get the last item merged.
	// In other cases, we can just create a posting list facade in memory and use it to store the delta in Badger.
	// Later, the rollup operation would consolidate all these deltas into a posting list.
	isList := su.GetList()
	var getFn func(key []byte) (*posting.List, error)
	switch {
	case len(edge.Lang) == 0 && !isList:
		// Scalar Predicates, without lang
		getFn = txn.GetScalarList
	case len(edge.Lang) > 0 || su.GetCount():
		// Language or Count Index
		getFn = txn.Get
	case edge.Op == pb.DirectedEdge_DEL:
		// Covers various delete cases to keep things simple.
		getFn = txn.Get
	default:
		// Only count index needs to be read. For other indexes on list, we don't need to read any data.
		// For indexes on scalar prediactes, only the last element needs to be left.
		// Delete cases covered above.
		getFn = txn.GetFromDelta
	}

	t := time.Now()
	plist, err := getFn(key)
	if dur := time.Since(t); dur > time.Millisecond {
		span := trace.SpanFromContext(ctx)
		span.AddEvent("Slow GetLru", trace.WithAttributes(
			attribute.Bool("slow-get", true),
			attribute.String("duration", dur.String())))
	}
	if err != nil {
		return err
	}
	return plist.AddMutationWithIndex(ctx, edge, txn)
}

func undoSchemaUpdate(predicate string) {
	maxRetries := 10
	loadErr := x.RetryUntilSuccess(maxRetries, 10*time.Millisecond, func() error {
		return schema.Load(predicate)
	})

	if loadErr != nil {
		glog.Fatalf("failed to load schema after %d retries: %v", maxRetries, loadErr)
	}
}

func runSchemaMutation(ctx context.Context, updates []*pb.SchemaUpdate, startTs uint64) error {
	if len(updates) == 0 {
		return nil
	}
	// Wait until schema modification for all predicates is complete. There cannot be two
	// background tasks running as this is a race condition. We typically won't propose an
	// index update if one is already going on. If that's not the case, then the receiver
	// of the update had probably finished the previous index update but some follower
	// (or perhaps leader) had not finished it.
	// In other words, the proposer checks whether there is another indexing in progress.
	// If that's the case, the alter request is rejected. Otherwise, the request is accepted.
	// Before reaching here, the proposer P would have checked that no indexing is in progress
	// (could also be because proposer was done earlier than others). If P was still indexing
	// when the req was received, it would have rejected the Alter request. Only if P is
	// not indexing, it would accept and propose the request.
	// It is possible that a receiver R of the proposal is still indexing. In that case, R would
	// block here and wait for indexing to be finished.
	gr.Node.waitForTask(opIndexing)

	// done is used to ensure that we only stop the indexing task once.
	var done uint32
	start := time.Now()
	stopIndexing := func(closer *z.Closer) {
		// runSchemaMutation can return. stopIndexing could be called by goroutines.
		if !schema.State().IndexingInProgress() {
			if atomic.CompareAndSwapUint32(&done, 0, 1) {
				closer.Done()
				// Time check is here so that we do not propose snapshot too frequently.
				if time.Since(start) < 10*time.Second || !gr.Node.AmLeader() {
					return
				}
				if err := gr.Node.proposeSnapshot(); err != nil {
					glog.Errorf("error in proposing snapshot: %v", err)
				}
			}
		}
	}

	buildIndexesHelper := func(update *pb.SchemaUpdate, rebuild posting.IndexRebuild) error {
		wrtCtx := schema.GetWriteContext(context.Background())
		if err := rebuild.BuildIndexes(wrtCtx); err != nil {
			return err
		}
		if err := updateSchema(update, rebuild.StartTs); err != nil {
			return err
		}

		glog.Infof("Done schema update %+v\n", update)
		return nil
	}

	// This wg allows waiting until setup for all the predicates is complete
	// before running buildIndexes for any of those predicates.
	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Done()

	// Badger opens around 8 files for indexing per predicate.
	// The throttle limit is set to 1024 to ensure that indexing
	// does not throw "Too many open files" error.
	maxPredicateAtATime := 1024
	if len(updates) < maxPredicateAtATime {
		maxPredicateAtATime = len(updates)
	}
	throttle := y.NewThrottle(maxPredicateAtATime)

	buildIndexes := func(update *pb.SchemaUpdate, rebuild posting.IndexRebuild, c *z.Closer) {
		// In case background indexing is running, we should call it here again.
		defer stopIndexing(c)

		// We should only start building indexes once this function has returned.
		// This is in order to ensure that we do not call DropPrefix for one predicate
		// and write indexes for another predicate simultaneously. because that could
		// cause writes to badger to fail leading to undesired indexing failures.
		wg.Wait()

		x.Check(throttle.Do())
		// undo schema changes in case re-indexing fails.
		if err := buildIndexesHelper(update, rebuild); err != nil {
			glog.Errorf("error in building indexes, aborting :: %v\n", err)
			undoSchemaUpdate(update.Predicate)
		}
		throttle.Done(nil)
	}

	var closer *z.Closer
	for _, su := range updates {
		if tablet, err := groups().Tablet(su.Predicate); err != nil {
			return err
		} else if tablet.GetGroupId() != groups().groupId() {
			return errors.Errorf("Tablet isn't being served by this group. Tablet: %+v", tablet)
		}

		if err := checkSchema(su); err != nil {
			return err
		}

		old, ok := schema.State().Get(ctx, su.Predicate)
		rebuild := posting.IndexRebuild{
			Attr:          su.Predicate,
			StartTs:       startTs,
			OldSchema:     &old,
			CurrentSchema: su,
		}
		shouldRebuild := ok && rebuild.NeedIndexRebuild()

		// Start opIndexing task only if schema update needs to build the indexes.
		if shouldRebuild && !gr.Node.isRunningTask(opIndexing) {
			var err error
			closer, err = gr.Node.startTaskAtTs(opIndexing, startTs)
			if err != nil {
				return err
			}
			defer stopIndexing(closer)
		}

		querySchema := rebuild.GetQuerySchema()
		// Sets the schema only in memory. The schema is written to
		// disk only after schema mutations are successful.
		schema.State().Set(su.Predicate, querySchema)
		schema.State().SetMutSchema(su.Predicate, su)

		// TODO(Aman): If we return an error, we may not have right schema reflected.
		setup := func() error {
			if !ok {
				return nil
			}
			if err := rebuild.DropIndexes(ctx); err != nil {
				return err
			}
			return rebuild.BuildData(ctx)
		}
		if err := setup(); err != nil {
			glog.Errorf("error in building indexes, aborting :: %v\n", err)
			undoSchemaUpdate(su.Predicate)
			return err
		}

		if shouldRebuild {
			go buildIndexes(su, rebuild, closer)
		} else if err := updateSchema(su, rebuild.StartTs); err != nil {
			return err
		}
	}

	return nil
}

// updateSchema commits the schema to disk in blocking way, should be ok because this happens
// only during schema mutations or we see a new predicate.
func updateSchema(s *pb.SchemaUpdate, ts uint64) error {
	schema.State().Set(s.Predicate, s)
	schema.State().DeleteMutSchema(s.Predicate)
	txn := pstore.NewTransactionAt(ts, true)
	defer txn.Discard()
	data, err := proto.Marshal(s)
	x.Check(err)
	e := &badger.Entry{
		Key:      x.SchemaKey(s.Predicate),
		Value:    data,
		UserMeta: posting.BitSchemaPosting,
	}
	if err = txn.SetEntry(e.WithDiscard()); err != nil {
		return err
	}
	return txn.CommitAt(ts, nil)
}

func createSchema(attr string, typ types.TypeID, hint pb.Metadata_HintType, ts uint64) error {
	ctx := schema.GetWriteContext(context.Background())

	// Don't overwrite schema blindly, acl's might have been set even though
	// type is not present
	s, ok := schema.State().Get(ctx, attr)
	if ok {
		s.ValueType = typ.Enum()
	} else {
		s = pb.SchemaUpdate{ValueType: typ.Enum(), Predicate: attr}
		// For type UidID, set List to true. This is done because previously
		// all predicates of type UidID were implicitly considered lists.
		if typ == types.UidID {
			s.List = true
		}

		switch hint {
		case pb.Metadata_SINGLE:
			s.List = false
		case pb.Metadata_LIST:
			s.List = true
		default:
		}
	}
	if err := checkSchema(&s); err != nil {
		return err
	}
	return updateSchema(&s, ts)
}

func runTypeMutation(ctx context.Context, update *pb.TypeUpdate, ts uint64) error {
	current := proto.Clone(update).(*pb.TypeUpdate)
	schema.State().SetType(update.TypeName, current)
	return updateType(update.TypeName, update, ts)
}

// We commit schema to disk in blocking way, should be ok because this happens
// only during schema mutations or we see a new predicate.
func updateType(typeName string, t *pb.TypeUpdate, ts uint64) error {
	schema.State().SetType(typeName, t)
	txn := pstore.NewTransactionAt(ts, true)
	defer txn.Discard()
	data, err := proto.Marshal(t)
	x.Check(err)
	e := &badger.Entry{
		Key:      x.TypeKey(typeName),
		Value:    data,
		UserMeta: posting.BitSchemaPosting,
	}
	if err := txn.SetEntry(e.WithDiscard()); err != nil {
		return err
	}
	return txn.CommitAt(ts, nil)
}

func hasEdges(attr string, startTs uint64) bool {
	pk := x.ParsedKey{Attr: attr}
	iterOpt := badger.DefaultIteratorOptions
	iterOpt.PrefetchValues = false
	iterOpt.Prefix = pk.DataPrefix()

	txn := pstore.NewTransactionAt(startTs, false)
	defer txn.Discard()

	it := txn.NewIterator(iterOpt)
	defer it.Close()

	for it.Rewind(); it.Valid(); it.Next() {
		// NOTE: This is NOT correct.
		// An incorrect, but efficient way to quickly check if we have at least one non-empty
		// posting. This does NOT consider those posting lists which can have multiple deltas
		// summing up to an empty posting list. I'm leaving it as it is for now. But, this could
		// cause issues because of this inaccuracy.
		if it.Item().UserMeta()&posting.BitEmptyPosting == 0 {
			return true
		}
	}
	return false
}
func checkSchema(s *pb.SchemaUpdate) error {
	if s == nil {
		return errors.Errorf("Nil schema")
	}

	if x.ParseAttr(s.Predicate) == "" {
		return errors.Errorf("No predicate specified in schema mutation")
	}

	if x.IsInternalPredicate(s.Predicate) {
		return errors.Errorf("Cannot create user-defined predicate with internal name %s",
			x.ParseAttr(s.Predicate))
	}

	if s.Directive == pb.SchemaUpdate_INDEX && !schema.HasTokenizerOrVectorIndexSpec(s) {
		return errors.Errorf("Tokenizer must be specified while indexing a predicate: %+v", s)
	}

	if schema.HasTokenizerOrVectorIndexSpec(s) && s.Directive != pb.SchemaUpdate_INDEX {
		return errors.Errorf("Directive must be SchemaUpdate_INDEX when a tokenizer is specified")
	}

	typ := types.TypeID(s.ValueType)
	if typ == types.UidID && s.Directive == pb.SchemaUpdate_INDEX {
		// index on uid type
		return errors.Errorf("Index not allowed on predicate of type uid on predicate %s",
			x.ParseAttr(s.Predicate))
	} else if typ != types.UidID && s.Directive == pb.SchemaUpdate_REVERSE {
		// reverse on non-uid type
		return errors.Errorf("Cannot reverse for non-uid type on predicate %s",
			x.ParseAttr(s.Predicate))
	}

	// If schema update has upsert directive, it should have index directive.
	if s.Upsert && len(s.Tokenizer) == 0 && !s.Unique {
		return errors.Errorf("Index tokenizer is mandatory for: [%s] when specifying @upsert directive",
			x.ParseAttr(s.Predicate))
	}

	if s.Unique {
		ctx := context.WithValue(context.Background(), schema.IsWrite, false)
		prevSchema, _ := schema.State().Get(ctx, s.Predicate)
		if err := validateSchemaForUnique(prevSchema, s); err != nil {
			return err
		}
	}

	t, err := schema.State().TypeOf(s.Predicate)
	if err != nil {
		// No schema previously defined, so no need to do checks about schema conversions.
		return nil
	}

	// schema was defined already
	switch {
	case t.IsScalar() && (t.Enum() == pb.Posting_PASSWORD || s.ValueType == pb.Posting_PASSWORD):
		// can't change password -> x, x -> password
		if t.Enum() != s.ValueType {
			return errors.Errorf("Schema change not allowed from %s to %s",
				t.Enum(), typ.Enum())
		}

	case t.IsScalar() == typ.IsScalar():
		// If old type was list and new type is non-list, we don't allow it until user
		// has data.
		if schema.State().IsList(s.Predicate) && !s.List && hasEdges(s.Predicate, math.MaxUint64) {
			return errors.Errorf("Schema change not allowed from [%s] => %s without"+
				" deleting pred: %s", t.Name(), typ.Name(), x.ParseAttr(s.Predicate))
		}

	default:
		// uid => scalar or scalar => uid. Check that there shouldn't be any data.
		if hasEdges(s.Predicate, math.MaxUint64) {
			return errors.Errorf("Schema change not allowed from scalar to uid or vice versa"+
				" while there is data for pred: %s", x.ParseAttr(s.Predicate))
		}
	}
	return nil
}

func validateSchemaForUnique(prevSchema pb.SchemaUpdate, currentSchema *pb.SchemaUpdate) error {
	validTokenizer := func(tokenizers []string) bool {
		for _, value := range tokenizers {
			if value == "hash" || value == "exact" || value == "int" {
				return true
			}
		}
		return false
	}
	if prevSchema.Predicate != "" {
		if prevSchema.Upsert && !currentSchema.Upsert {
			return errors.Errorf("could not drop @upsert from [%v] predicate when @unique directive specified",
				x.ParseAttr(currentSchema.Predicate))
		}

		if prevSchema.Unique && !validTokenizer(currentSchema.Tokenizer) {
			return errors.Errorf("could not drop index %v from [%v] predicate when @unique directive specified",
				prevSchema.Tokenizer, x.ParseAttr(currentSchema.Predicate))
		}
	}
	if !currentSchema.Upsert {
		currentSchema.Upsert = true
	}
	switch currentSchema.ValueType {
	case pb.Posting_STRING:
		if len(currentSchema.Tokenizer) == 0 ||
			(len(currentSchema.Tokenizer) > 0 && !validTokenizer(currentSchema.Tokenizer)) {

			return errors.Errorf("index for predicate [%v] is missing, add either hash or exact index with @unique",
				x.ParseAttr(currentSchema.Predicate))
		} else {
			return nil
		}

	case pb.Posting_INT:
		if len(currentSchema.Tokenizer) == 0 {
			return errors.Errorf("index for predicate [%v] is missing, add int index with @unique",
				x.ParseAttr(currentSchema.Predicate))
		} else {
			return nil
		}
	}

	return errors.Errorf("@unique directive not supported on [%v] type predicate", currentSchema.ValueType.String())
}

// ValidateAndConvert checks compatibility or converts to the schema type if the storage type is
// specified. If no storage type is specified then it converts to the schema type.
func ValidateAndConvert(edge *pb.DirectedEdge, su *pb.SchemaUpdate) error {

	if isDeletePredicateEdge(edge) {
		return nil
	}
	if types.TypeID(edge.ValueType) == types.DefaultID && isStarAll(edge.Value) {
		return nil
	}

	if strings.Contains(su.Predicate, hnsw.VecKeyword) {
		return errors.Errorf("Not allowed to insert mutations in vector index keys, edge: [%v]", edge)
	}

	storageType := posting.TypeID(edge)
	schemaType := types.TypeID(su.ValueType)

	// type checks
	switch {
	case edge.Lang != "" && !su.GetLang():
		return errors.Errorf("Attr: [%v] should have @lang directive in schema to mutate edge: [%v]",
			x.ParseAttr(edge.Attr), edge)

	case !schemaType.IsScalar() && !storageType.IsScalar():
		return nil

	case !schemaType.IsScalar() && storageType.IsScalar():
		return errors.Errorf("Input for predicate %q of type uid is scalar. Edge: %v",
			x.ParseAttr(edge.Attr), edge)

	case schemaType.IsScalar() && !storageType.IsScalar():
		return errors.Errorf("Input for predicate %q of type scalar is uid. Edge: %v",
			x.ParseAttr(edge.Attr), edge)

	case schemaType == types.TypeID(pb.Posting_VFLOAT):
		if !(storageType == types.TypeID(pb.Posting_VFLOAT) || storageType == types.TypeID(pb.Posting_STRING) ||
			storageType == types.TypeID(pb.Posting_DEFAULT)) {
			return errors.Errorf("Input for predicate %q of type vector is not vector."+
				" Did you forget to add quotes before []?. Edge: %v", x.ParseAttr(edge.Attr), edge)
		}

	// The suggested storage type matches the schema, OK! (Nothing to do ...)
	case storageType == schemaType && schemaType != types.DefaultID:
		return nil

	// We accept the storage type iff we don't have a schema type and a storage type is specified.
	case schemaType == types.DefaultID:
		schemaType = storageType
	}

	var (
		dst types.Val
		err error
	)

	src := types.Val{Tid: types.TypeID(edge.ValueType), Value: edge.Value}
	// check compatibility of schema type and storage type
	// The goal is to convert value on edge to value type defined by schema.
	if dst, err = types.Convert(src, schemaType); err != nil {
		return err
	}

	// convert to schema type
	b := types.ValueForType(types.BinaryID)
	if err = types.Marshal(dst, &b); err != nil {
		return err
	}

	if x.WorkerConfig.AclEnabled && x.ParseAttr(edge.GetAttr()) == "dgraph.rule.permission" {
		perm, ok := dst.Value.(int64)
		if !ok {
			return errors.Errorf("Value for predicate <dgraph.rule.permission> should be of type int")
		}
		if perm < 0 || perm > 7 {
			return errors.Errorf("Can't set <dgraph.rule.permission> to %d, Value for this"+
				" predicate should be between 0 and 7", perm)
		}
	}

	// TODO: Figure out why this is Enum. It really seems like an odd choice -- rather than
	//       specifying it as the same type as presented in su.
	edge.ValueType = schemaType.Enum()
	var ok bool
	edge.Value, ok = b.Value.([]byte)
	if !ok {
		return errors.Errorf("failure to convert edge type: '%+v' to schema type: '%+v'",
			storageType, schemaType)
	}

	return nil
}

// AssignNsIdsOverNetwork sends a request to assign Namespace IDs to the current zero leader.
func AssignNsIdsOverNetwork(ctx context.Context, num *pb.Num) (*pb.AssignedIds, error) {
	pl := groups().Leader(0)
	if pl == nil {
		return nil, conn.ErrNoConnection
	}

	con := pl.Get()
	c := pb.NewZeroClient(con)
	num.Type = pb.Num_NS_ID
	return c.AssignIds(ctx, num)
}

// AssignUidsOverNetwork sends a request to assign UIDs from the current zero leader.
func AssignUidsOverNetwork(ctx context.Context, num *pb.Num) (*pb.AssignedIds, error) {
	// Pass on the incoming metadata to the zero. Namespace from the metadata is required by zero.
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		ctx = metadata.NewOutgoingContext(ctx, md)
	}
	pl := groups().Leader(0)
	if pl == nil {
		return nil, conn.ErrNoConnection
	}

	con := pl.Get()
	c := pb.NewZeroClient(con)
	num.Type = pb.Num_UID
	return c.AssignIds(ctx, num)
}

// Timestamps sends a request to assign startTs for a new transaction to the current zero leader.
func Timestamps(ctx context.Context, num *pb.Num) (*pb.AssignedIds, error) {
	pl := groups().connToZeroLeader()
	if pl == nil {
		return nil, conn.ErrNoConnection
	}

	con := pl.Get()
	c := pb.NewZeroClient(con)
	return c.Timestamps(ctx, num)
}

func fillTxnContext(tctx *api.TxnContext, startTs uint64, isErrored bool) {
	if txn := posting.Oracle().GetTxn(startTs); txn != nil {
		txn.FillContext(tctx, groups().groupId(), isErrored)
	}
	// We do not need to fill linread mechanism anymore, because transaction
	// start ts is sufficient to wait for, to achieve lin reads.
}

// proposeOrSend either proposes the mutation if the node serves the group gid or sends it to
// the leader of the group gid for proposing.
func proposeOrSend(ctx context.Context, gid uint32, m *pb.Mutations, chr chan res) {
	res := res{}
	if groups().ServesGroup(gid) {
		res.ctx = &api.TxnContext{}
		res.err = (&grpcWorker{}).proposeAndWait(ctx, res.ctx, m)
		chr <- res
		return
	}

	pl := groups().Leader(gid)
	if pl == nil {
		res.err = conn.ErrNoConnection
		chr <- res
		return
	}

	var tc *api.TxnContext
	c := pb.NewWorkerClient(pl.Get())

	ch := make(chan error, 1)
	go func() {
		var err error
		tc, err = c.Mutate(ctx, m)
		ch <- err
	}()

	select {
	case <-ctx.Done():
		res.err = ctx.Err()
		res.ctx = nil
	case err := <-ch:
		res.err = err
		res.ctx = tc
	}
	chr <- res
}

// populateMutationMap populates a map from group id to the mutation that
// should be sent to that group.
func populateMutationMap(src *pb.Mutations) (map[uint32]*pb.Mutations, error) {
	mm := make(map[uint32]*pb.Mutations)
	for _, edge := range src.Edges {
		gid, err := groups().BelongsTo(edge.Attr)
		if err != nil {
			return nil, err
		}

		mu := mm[gid]
		if mu == nil {
			mu = &pb.Mutations{GroupId: gid}
			mm[gid] = mu
		}
		mu.Edges = append(mu.Edges, edge)
		mu.Metadata = src.Metadata
	}

	for _, schema := range src.Schema {
		gid, err := groups().BelongsTo(schema.Predicate)
		if err != nil {
			return nil, err
		}

		mu := mm[gid]
		if mu == nil {
			mu = &pb.Mutations{GroupId: gid}
			mm[gid] = mu
		}
		mu.Schema = append(mu.Schema, schema)
	}

	if src.DropOp > 0 {
		for _, gid := range groups().KnownGroups() {
			mu := mm[gid]
			if mu == nil {
				mu = &pb.Mutations{GroupId: gid}
				mm[gid] = mu
			}
			mu.DropOp = src.DropOp
			mu.DropValue = src.DropValue
		}
	}

	// Type definitions are sent to all groups.
	if len(src.Types) > 0 {
		for _, gid := range groups().KnownGroups() {
			mu := mm[gid]
			if mu == nil {
				mu = &pb.Mutations{GroupId: gid}
				mm[gid] = mu
			}
			mu.Types = src.Types
		}
	}

	return mm, nil
}

type res struct {
	err error
	ctx *api.TxnContext
}

// MutateOverNetwork checks which group should be running the mutations
// according to the group config and sends it to that instance.
func MutateOverNetwork(ctx context.Context, m *pb.Mutations) (*api.TxnContext, error) {
	ctx, span := otel.Tracer("").Start(ctx, "worker.MutateOverNetwork")
	defer span.End()

	tctx := &api.TxnContext{StartTs: m.StartTs}
	if err := verifyTypes(ctx, m); err != nil {
		return tctx, err
	}
	mutationMap, err := populateMutationMap(m)
	if err != nil {
		return tctx, err
	}

	resCh := make(chan res, len(mutationMap))
	for gid, mu := range mutationMap {
		if gid == 0 {
			span.AddEvent("State information", trace.WithAttributes(
				attribute.String("state", groups().state.String())))
			span.AddEvent("Group id zero for mutation", trace.WithAttributes(
				attribute.String("mutation", mu.String())))
			return tctx, errNonExistentTablet
		}
		mu.StartTs = m.StartTs
		go proposeOrSend(ctx, gid, mu, resCh)
	}

	// Wait for all the goroutines to reply back.
	// We return if an error was returned or the parent called ctx.Done()
	var e error
	for range mutationMap {
		res := <-resCh
		if res.err != nil {
			e = res.err
		}
		if res.ctx != nil {
			tctx.Keys = append(tctx.Keys, res.ctx.Keys...)
			tctx.Preds = append(tctx.Preds, res.ctx.Preds...)
		}
	}
	close(resCh)
	return tctx, e
}

func verifyTypes(ctx context.Context, m *pb.Mutations) error {
	// Create a set of all the predicates included in this schema request.
	reqPredSet := make(map[string]struct{}, len(m.Schema))
	for _, schemaUpdate := range m.Schema {
		reqPredSet[schemaUpdate.Predicate] = struct{}{}
	}

	// Create a set of all the predicates already present in the schema.
	var fields []string
	for _, t := range m.Types {
		if t.TypeName == "" {
			return errors.Errorf("Type name must be specified in type update")
		}

		if err := typeSanityCheck(t); err != nil {
			return err
		}

		for _, field := range t.Fields {
			fieldName := field.Predicate
			ns, attr := x.ParseNamespaceAttr(fieldName)
			if attr[0] == '~' {
				fieldName = x.NamespaceAttr(ns, attr[1:])
			}

			if _, ok := reqPredSet[fieldName]; !ok {
				fields = append(fields, fieldName)
			}
		}
	}

	// Retrieve the schema for those predicates.
	schemas, err := GetSchemaOverNetwork(ctx, &pb.SchemaRequest{Predicates: fields})
	if err != nil {
		return errors.Wrapf(err, "cannot retrieve predicate information")
	}
	schemaSet := make(map[string]struct{})
	for _, schemaNode := range schemas {
		schemaSet[schemaNode.Predicate] = struct{}{}
	}

	for _, t := range m.Types {
		// Verify all the fields in the type are already on the schema or come included in
		// this request.
		for _, field := range t.Fields {
			fieldName := field.Predicate
			ns, attr := x.ParseNamespaceAttr(fieldName)
			if attr[0] == '~' {
				fieldName = x.NamespaceAttr(ns, attr[1:])
			}

			_, inSchema := schemaSet[fieldName]
			_, inRequest := reqPredSet[fieldName]
			if !inSchema && !inRequest {
				return errors.Errorf(
					"Schema does not contain a matching predicate for field %s in type %s",
					field.Predicate, t.TypeName)
			}
		}
	}

	return nil
}

// typeSanityCheck performs basic sanity checks on the given type update.
func typeSanityCheck(t *pb.TypeUpdate) error {
	for _, field := range t.Fields {
		if x.ParseAttr(field.Predicate) == "" {
			return errors.Errorf("Field in type definition must have a name")
		}

		if field.ValueType == pb.Posting_OBJECT && field.ObjectTypeName == "" {
			return errors.Errorf(
				"Field with value type OBJECT must specify the name of the object type")
		}

		if field.Directive != pb.SchemaUpdate_NONE {
			return errors.Errorf("Field in type definition cannot have a directive")
		}

		if len(field.Tokenizer) > 0 {
			return errors.Errorf("Field in type definition cannot have tokenizers")
		}
	}

	return nil
}

// CommitOverNetwork makes a proxy call to Zero to commit or abort a transaction.
func CommitOverNetwork(ctx context.Context, tc *api.TxnContext) (uint64, error) {
	ctx, span := otel.Tracer("").Start(ctx, "worker.CommitOverNetwork")
	defer span.End()

	clientDiscard := false
	if tc.Aborted {
		// The client called Discard
		ostats.Record(ctx, x.TxnDiscards.M(1))
		clientDiscard = true
	}

	pl := groups().Leader(0)
	if pl == nil {
		return 0, conn.ErrNoConnection
	}

	// Do de-duplication before sending the request to zero.
	tc.Keys = x.Unique(tc.Keys)
	tc.Preds = x.Unique(tc.Preds)

	zc := pb.NewZeroClient(pl.Get())
	tctx, err := zc.CommitOrAbort(ctx, tc)

	if err != nil {
		span.AddEvent("Error in CommitOrAbort", trace.WithAttributes(
			attribute.String("error", err.Error())))
		return 0, err
	}
	span.AddEvent("Commit status", trace.WithAttributes(
		attribute.Int64("commitTs", int64(tctx.CommitTs)),
		attribute.Bool("committed", tctx.CommitTs > 0)))

	if tctx.Aborted || tctx.CommitTs == 0 {
		if !clientDiscard {
			// The server aborted the txn (not the client)
			ostats.Record(ctx, x.TxnAborts.M(1))
		}
		return 0, dgo.ErrAborted
	}
	ostats.Record(ctx, x.TxnCommits.M(1))
	return tctx.CommitTs, nil
}

func (w *grpcWorker) proposeAndWait(ctx context.Context, txnCtx *api.TxnContext,
	m *pb.Mutations) error {
	if x.WorkerConfig.StrictMutations {
		for _, edge := range m.Edges {
			if _, err := schema.State().TypeOf(edge.Attr); err != nil {
				return err
			}
		}
	}

	// We should wait to ensure that we have seen all the updates until the StartTs of this mutation
	// transaction. Otherwise, when we read the posting list value for calculating the indices, we
	// might be wrong because we might be missing out a commit which has updated the value. This
	// wait here ensures that the proposal would only be registered after seeing txn status of all
	// pending transactions. Thus, the ordering would be correct.
	if err := posting.Oracle().WaitForTs(ctx, m.StartTs); err != nil {
		return err
	}

	node := groups().Node
	err := node.proposeAndWait(ctx, &pb.Proposal{Mutations: m})
	// When we are filling txn context, we don't need to update latest delta if the transaction has failed.
	fillTxnContext(txnCtx, m.StartTs, err != nil)
	return err
}

// Mutate is used to apply mutations over the network on other instances.
func (w *grpcWorker) Mutate(ctx context.Context, m *pb.Mutations) (*api.TxnContext, error) {
	ctx, span := otel.Tracer("").Start(ctx, "worker.Mutate")
	defer span.End()

	txnCtx := &api.TxnContext{}
	if ctx.Err() != nil {
		return txnCtx, ctx.Err()
	}
	if !groups().ServesGroup(m.GroupId) {
		return txnCtx, errors.Errorf("This server doesn't serve group id: %v", m.GroupId)
	}

	return txnCtx, w.proposeAndWait(ctx, txnCtx, m)
}

func tryAbortTransactions(startTimestamps []uint64) {
	// Aborts if not already committed.
	req := &pb.TxnTimestamps{Ts: startTimestamps}

	err := groups().Node.blockingAbort(req)
	glog.Infof("tryAbortTransactions for %d txns. Error: %+v\n", len(req.Ts), err)
}
