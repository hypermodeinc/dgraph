/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package edgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	ostats "go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	apiv2 "github.com/dgraph-io/dgo/v250/protos/api.v2"
	"github.com/hypermodeinc/dgraph/v25/chunker"
	"github.com/hypermodeinc/dgraph/v25/conn"
	"github.com/hypermodeinc/dgraph/v25/dql"
	gqlSchema "github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/posting"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/query"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/tok"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/types/facets"
	"github.com/hypermodeinc/dgraph/v25/worker"
	"github.com/hypermodeinc/dgraph/v25/x"
)

const (
	methodMutate = "Server.Mutate"
	methodQuery  = "Server.Query"
)

type GraphqlContextKey int

// uniquePredMeta stores the query variable name from 'uniqueQuery'
type uniquePredMeta struct {
	queryVar string
	valVar   string
}

const (
	// IsGraphql is used to validate requests which are allowed to mutate GraphQL reserved
	// predicates, like dgraph.graphql.schema and dgraph.graphql.xid.
	IsGraphql GraphqlContextKey = iota
	// Authorize is used to set if the request requires validation.
	Authorize
)

type AuthMode int

const (
	// NeedAuthorize is used to indicate that the request needs to be authorized.
	NeedAuthorize AuthMode = iota
	// NoAuthorize is used to indicate that authorization needs to be skipped.
	// Used when ACL needs to query information for performing the authorization check.
	NoAuthorize
)

var (
	numDQL     uint64
	numGraphQL uint64
)

var (
	errIndexingInProgress = errors.New("errIndexingInProgress. Please retry")
)

// Server implements protos.DgraphServer
type Server struct {
	// embedding the api.UnimplementedZeroServer struct to ensure forward compatibility of the server.
	api.UnimplementedDgraphServer
}

// graphQLSchemaNode represents the node which contains GraphQL schema
type graphQLSchemaNode struct {
	Uid    string `json:"uid"`
	UidInt uint64
	Schema string `json:"dgraph.graphql.schema"`
}

type existingGQLSchemaQryResp struct {
	ExistingGQLSchema []graphQLSchemaNode `json:"ExistingGQLSchema"`
}

// GetGQLSchema queries for the GraphQL schema node, and returns the uid and the GraphQL schema.
// If multiple schema nodes were found, it returns an error.
func GetGQLSchema(namespace uint64) (uid, graphQLSchema string, err error) {
	ctx := context.WithValue(context.Background(), Authorize, false)
	ctx = x.AttachNamespace(ctx, namespace)
	resp, err := (&Server{}).QueryNoGrpc(ctx,
		&api.Request{
			Query: `
			query {
				ExistingGQLSchema(func: has(dgraph.graphql.schema)) {
					uid
					dgraph.graphql.schema
				  }
				}`})
	if err != nil {
		return "", "", err
	}

	var result existingGQLSchemaQryResp
	if err := json.Unmarshal(resp.GetJson(), &result); err != nil {
		return "", "", errors.Wrap(err, "Couldn't unmarshal response from Dgraph query")
	}
	res := result.ExistingGQLSchema
	if len(res) == 0 {
		// no schema has been stored yet in Dgraph
		return "", "", nil
	} else if len(res) == 1 {
		// we found an existing GraphQL schema
		gqlSchemaNode := res[0]
		return gqlSchemaNode.Uid, gqlSchemaNode.Schema, nil
	}

	// found multiple GraphQL schema nodes, this should never happen
	// returning the schema node which is added last
	for i := range res {
		iUid, err := dql.ParseUid(res[i].Uid)
		if err != nil {
			return "", "", err
		}
		res[i].UidInt = iUid
	}

	sort.Slice(res, func(i, j int) bool {
		return res[i].UidInt < res[j].UidInt
	})
	glog.Errorf("namespace: %d. Multiple schema nodes found, using the last one", namespace)
	resLast := res[len(res)-1]
	return resLast.Uid, resLast.Schema, nil
}

// UpdateGQLSchema updates the GraphQL and Dgraph schemas using the given inputs.
// It first validates and parses the dgraphSchema given in input. If that fails,
// it returns an error. All this is done on the alpha on which the update request is received.
// Then it sends an update request to the worker, which is executed only on Group-1 leader.
func UpdateGQLSchema(ctx context.Context, gqlSchema,
	dgraphSchema string) (*pb.UpdateGraphQLSchemaResponse, error) {
	var err error
	parsedDgraphSchema := &schema.ParsedSchema{}

	if !x.WorkerConfig.AclEnabled {
		ctx = x.AttachNamespace(ctx, x.RootNamespace)
	}
	// The schema could be empty if it only has custom types/queries/mutations.
	if dgraphSchema != "" {
		op := &api.Operation{Schema: dgraphSchema}
		if err = validateAlterOperation(ctx, op); err != nil {
			return nil, err
		}
		if parsedDgraphSchema, err = parseSchemaFromAlterOperation(ctx, op.Schema); err != nil {
			return nil, err
		}
	}

	return worker.UpdateGQLSchemaOverNetwork(ctx, &pb.UpdateGraphQLSchemaRequest{
		StartTs:       worker.State.GetTimestamp(false),
		GraphqlSchema: gqlSchema,
		DgraphPreds:   parsedDgraphSchema.Preds,
		DgraphTypes:   parsedDgraphSchema.Types,
	})
}

// validateAlterOperation validates the given operation for alter.
func validateAlterOperation(ctx context.Context, op *api.Operation) error {
	// The following code block checks if the operation should run or not.
	if op.Schema == "" && op.DropAttr == "" && !op.DropAll && op.DropOp == api.Operation_NONE {
		// Must have at least one field set. This helps users if they attempt
		// to set a field but use the wrong name (could be decoded from JSON).
		return errors.Errorf("Operation must have at least one field set")
	}
	if err := x.HealthCheck(); err != nil {
		return err
	}

	if isDropAll(op) && op.DropOp == api.Operation_DATA {
		return errors.Errorf("Only one of DropAll and DropData can be true")
	}

	if !isMutationAllowed(ctx) {
		return errors.Errorf("No mutations allowed by server.")
	}

	if _, err := hasAdminAuth(ctx, "Alter"); err != nil {
		glog.Warningf("Alter denied with error: %v\n", err)
		return err
	}

	if err := authorizeAlter(ctx, op); err != nil {
		glog.Warningf("Alter denied with error: %v\n", err)
		return err
	}

	return nil
}

// parseSchemaFromAlterOperation parses the string schema given in input operation to a Go
// struct, and performs some checks to make sure that the schema is valid.
func parseSchemaFromAlterOperation(ctx context.Context, sch string) (
	*schema.ParsedSchema, error) {

	// If a background task is already running, we should reject all the new alter requests.
	if schema.State().IndexingInProgress() {
		return nil, errIndexingInProgress
	}

	namespace, err := x.ExtractNamespace(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "While parsing schema")
	}

	if x.IsRootNsOperation(ctx) {
		// Only the guardian of the galaxy can do a galaxy wide query/mutation. This operation is
		// needed by live loader.
		if err := AuthSuperAdmin(ctx); err != nil {
			s := status.Convert(err)
			return nil, status.Error(s.Code(),
				"Non superadmin user cannot bypass namespaces. "+s.Message())
		}
		var err error
		namespace, err = strconv.ParseUint(x.GetForceNamespace(ctx), 0, 64)
		if err != nil {
			return nil, errors.Wrapf(err, "Valid force namespace not found in metadata")
		}
	}

	result, err := schema.ParseWithNamespace(sch, namespace)
	if err != nil {
		return nil, err
	}

	preds := make(map[string]struct{})
	for _, update := range result.Preds {
		if _, ok := preds[update.Predicate]; ok {
			return nil, errors.Errorf("predicate %s defined multiple times", x.ParseAttr(update.Predicate))
		}
		preds[update.Predicate] = struct{}{}

		// Pre-defined predicates cannot be altered but let the update go through
		// if the update is equal to the existing one.
		if schema.CheckAndModifyPreDefPredicate(update) {
			return nil, errors.Errorf("predicate %s is pre-defined and is not allowed to be"+
				" modified", x.ParseAttr(update.Predicate))
		}

		if err := validatePredName(update.Predicate); err != nil {
			return nil, err
		}
		// Users are not allowed to create a predicate under the reserved `dgraph.` namespace. But,
		// there are pre-defined predicates (subset of reserved predicates), and for them we allow
		// the schema update to go through if the update is equal to the existing one.
		// So, here we check if the predicate is reserved but not pre-defined to block users from
		// creating predicates in reserved namespace.
		if x.IsReservedPredicate(update.Predicate) && !x.IsPreDefinedPredicate(update.Predicate) {
			return nil, errors.Errorf("Can't alter predicate `%s` as it is prefixed with `dgraph.`"+
				" which is reserved as the namespace for dgraph's internal types/predicates.",
				x.ParseAttr(update.Predicate))
		}
	}

	types := make(map[string]struct{})
	for _, typ := range result.Types {
		if _, ok := types[typ.TypeName]; ok {
			return nil, errors.Errorf("type %s defined multiple times", x.ParseAttr(typ.TypeName))
		}
		types[typ.TypeName] = struct{}{}

		// Pre-defined types cannot be altered but let the update go through
		// if the update is equal to the existing one.
		if schema.IsPreDefTypeChanged(typ) {
			return nil, errors.Errorf("type %s is pre-defined and is not allowed to be modified",
				x.ParseAttr(typ.TypeName))
		}

		// Users are not allowed to create types in reserved namespace. But, there are pre-defined
		// types for which the update should go through if the update is equal to the existing one.
		if x.IsReservedType(typ.TypeName) && !x.IsPreDefinedType(typ.TypeName) {
			return nil, errors.Errorf("Can't alter type `%s` as it is prefixed with `dgraph.` "+
				"which is reserved as the namespace for dgraph's internal types/predicates.",
				x.ParseAttr(typ.TypeName))
		}
	}

	return result, nil
}

// InsertDropRecord is used to insert a helper record when a DROP operation is performed.
// This helper record lets us know during backup that a DROP operation was performed and that we
// need to write this information in backup manifest. So that while restoring from a backup series,
// we can create an exact replica of the system which existed at the time the last backup was taken.
// Note that if the server crashes after the DROP operation & before this helper record is inserted,
// then restoring from the incremental backup of such a DB would restore even the dropped
// data back. This is also used to capture the delete namespace operation during backup.
func InsertDropRecord(ctx context.Context, dropOp string) error {
	_, err := (&Server{}).doQuery(context.WithValue(ctx, IsGraphql, true), &Request{
		req: &api.Request{
			Mutations: []*api.Mutation{{
				Set: []*api.NQuad{{
					Subject:     "_:r",
					Predicate:   "dgraph.drop.op",
					ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: dropOp}},
				}},
			}},
			CommitNow: true,
		}, doAuth: NoAuthorize})
	return err
}

// Alter handles requests to change the schema or remove parts or all of the data.
func (s *Server) Alter(ctx context.Context, op *api.Operation) (*api.Payload, error) {
	ctx, span := otel.Tracer("").Start(ctx, "Server.Alter")
	defer span.End()

	ctx = x.AttachJWTNamespace(ctx)
	span.AddEvent("Alter operation", trace.WithAttributes(attribute.String("op", op.String())))

	// Always print out Alter operations because they are important and rare.
	glog.Infof("Received ALTER op: %+v", op)

	// check if the operation is valid
	if err := validateAlterOperation(ctx, op); err != nil {
		return nil, err
	}

	defer glog.Infof("ALTER op: %+v done", op)

	empty := &api.Payload{}
	namespace, err := x.ExtractNamespace(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "While altering")
	}

	// StartTs is not needed if the predicate to be dropped lies on this server but is required
	// if it lies on some other machine. Let's get it for safety.
	m := &pb.Mutations{StartTs: worker.State.GetTimestamp(false)}
	if isDropAll(op) {
		if x.Config.BlockClusterWideDrop {
			glog.V(2).Info("Blocked drop-all because it is not permitted.")
			return empty, errors.New("Drop all operation is not permitted.")
		}
		if err := AuthSuperAdmin(ctx); err != nil {
			s := status.Convert(err)
			return empty, status.Error(s.Code(),
				"Drop all can only be called by the guardian of the galaxy. "+s.Message())
		}
		if len(op.DropValue) > 0 {
			return empty, errors.Errorf("If DropOp is set to ALL, DropValue must be empty")
		}

		m.DropOp = pb.Mutations_ALL
		_, err := query.ApplyMutations(ctx, m)
		if err != nil {
			return empty, err
		}

		// insert a helper record for backup & restore, indicating that drop_all was done
		err = InsertDropRecord(ctx, "DROP_ALL;")
		if err != nil {
			return empty, err
		}

		// insert empty GraphQL schema, so all alphas get notified to
		// reset their in-memory GraphQL schema
		_, err = UpdateGQLSchema(ctx, "", "")
		// recreate the admin account after a drop all operation
		InitializeAcl(nil)
		return empty, err
	}

	if op.DropOp == api.Operation_DATA {
		if len(op.DropValue) > 0 {
			return empty, errors.Errorf("If DropOp is set to DATA, DropValue must be empty")
		}

		// query the GraphQL schema and keep it in memory, so it can be inserted again
		_, graphQLSchema, err := GetGQLSchema(namespace)
		if err != nil {
			return empty, err
		}

		m.DropOp = pb.Mutations_DATA
		m.DropValue = fmt.Sprintf("%#x", namespace)
		_, err = query.ApplyMutations(ctx, m)
		if err != nil {
			return empty, err
		}

		// insert a helper record for backup & restore, indicating that drop_data was done
		err = InsertDropRecord(ctx, fmt.Sprintf("DROP_DATA;%#x", namespace))
		if err != nil {
			return empty, err
		}

		// just reinsert the GraphQL schema, no need to alter dgraph schema as this was drop_data
		_, err = UpdateGQLSchema(ctx, graphQLSchema, "")

		// Since all data has been dropped, we need to recreate the admin account in the respective namespace.
		upsertGuardianAndGroot(nil, namespace)
		return empty, err
	}

	if len(op.DropAttr) > 0 || op.DropOp == api.Operation_ATTR {
		if op.DropOp == api.Operation_ATTR && op.DropValue == "" {
			return empty, errors.Errorf("If DropOp is set to ATTR, DropValue must not be empty")
		}

		var attr string
		if len(op.DropAttr) > 0 {
			attr = op.DropAttr
		} else {
			attr = op.DropValue
		}
		attr = x.NamespaceAttr(namespace, attr)
		// Pre-defined predicates cannot be dropped.
		if x.IsPreDefinedPredicate(attr) {
			return empty, errors.Errorf("predicate %s is pre-defined and is not allowed to be"+
				" dropped", x.ParseAttr(attr))
		}

		nq := &api.NQuad{
			Subject:     x.Star,
			Predicate:   x.ParseAttr(attr),
			ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: x.Star}},
		}
		wnq := &dql.NQuad{NQuad: nq}
		edge, err := wnq.ToDeletePredEdge()
		if err != nil {
			return empty, err
		}
		edges := []*pb.DirectedEdge{edge}
		m.Edges = edges
		_, err = query.ApplyMutations(ctx, m)
		if err != nil {
			return empty, err
		}

		// insert a helper record for backup & restore, indicating that drop_attr was done
		err = InsertDropRecord(ctx, "DROP_ATTR;"+attr)
		return empty, err
	}

	if op.DropOp == api.Operation_TYPE {
		if op.DropValue == "" {
			return empty, errors.Errorf("If DropOp is set to TYPE, DropValue must not be empty")
		}

		// Pre-defined types cannot be dropped.
		dropPred := x.NamespaceAttr(namespace, op.DropValue)
		if x.IsPreDefinedType(dropPred) {
			return empty, errors.Errorf("type %s is pre-defined and is not allowed to be dropped",
				op.DropValue)
		}

		m.DropOp = pb.Mutations_TYPE
		m.DropValue = dropPred
		_, err := query.ApplyMutations(ctx, m)
		return empty, err
	}
	result, err := parseSchemaFromAlterOperation(ctx, op.Schema)
	if err == errIndexingInProgress {
		// Make the client wait a bit.
		time.Sleep(time.Second)
		return nil, err
	} else if err != nil {
		return nil, err
	}

	glog.Infof("Got schema: %+v\n", result)
	// TODO: Maybe add some checks about the schema.
	m.Schema = result.Preds
	m.Types = result.Types
	_, err = query.ApplyMutations(ctx, m)
	if err != nil {
		return empty, err
	}

	// wait for indexing to complete or context to be canceled.
	if err = worker.WaitForIndexing(ctx, !op.RunInBackground); err != nil {
		return empty, err
	}

	return empty, nil
}

func annotateNamespace(span trace.Span, ns uint64) {
	span.SetAttributes(attribute.String("ns", fmt.Sprintf("%d", ns)))
}

func annotateStartTs(span trace.Span, ts uint64) {
	span.SetAttributes(attribute.String("startTs", fmt.Sprintf("%d", ts)))
}

func (s *Server) doMutate(ctx context.Context, qc *queryContext, resp *api.Response) error {
	if len(qc.gmuList) == 0 {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	start := time.Now()
	defer func() {
		qc.latency.Processing += time.Since(start)
	}()

	if !isMutationAllowed(ctx) {
		return errors.Errorf("no mutations allowed")
	}

	// update mutations from the query results before assigning UIDs
	if err := updateMutations(qc); err != nil {
		return err
	}

	if err := verifyUniqueWithinMutation(qc); err != nil {
		return err
	}

	newUids, err := query.AssignUids(ctx, qc.gmuList)
	if err != nil {
		return err
	}

	// resp.Uids contains a map of the node name to the uid.
	// 1. For a blank node, like _:foo, the key would be foo.
	// 2. For a uid variable that is part of an upsert query,
	//    like uid(foo), the key would be uid(foo).
	resp.Uids = query.UidsToHex(query.StripBlankNode(newUids))
	edges, err := query.ToDirectedEdges(qc.gmuList, newUids)
	if err != nil {
		return err
	}

	if len(edges) > x.Config.LimitMutationsNquad {
		return errors.Errorf("NQuad count in the request: %d, is more that threshold: %d",
			len(edges), x.Config.LimitMutationsNquad)
	}

	ns, err := x.ExtractNamespace(ctx)
	if err != nil {
		return errors.Wrapf(err, "While doing mutations:")
	}
	predHints := make(map[string]pb.Metadata_HintType)
	for _, gmu := range qc.gmuList {
		for pred, hint := range gmu.Metadata.GetPredHints() {
			pred = x.NamespaceAttr(ns, pred)
			if oldHint := predHints[pred]; oldHint == pb.Metadata_LIST {
				continue
			}
			predHints[pred] = hint
		}
	}
	m := &pb.Mutations{
		Edges:   edges,
		StartTs: qc.req.StartTs,
		Metadata: &pb.Metadata{
			PredHints: predHints,
		},
	}

	// ensure that we do not insert very large (> 64 KB) value
	if err := validateMutation(ctx, edges); err != nil {
		return err
	}

	qc.span.AddEvent("Applying mutations",
		trace.WithAttributes(attribute.String("m", fmt.Sprintf("%+v", m))))
	resp.Txn, err = query.ApplyMutations(ctx, m)
	qc.span.AddEvent("Txn Context",
		trace.WithAttributes(attribute.String("txn", fmt.Sprintf("%+v", resp.Txn))))
	if err != nil {
		qc.span.AddEvent("Error",
			trace.WithAttributes(attribute.String("err", err.Error())))
	}

	// calculateMutationMetrics calculate cost for the mutation.
	calculateMutationMetrics := func() {
		cost := uint64(len(newUids) + len(edges))
		resp.Metrics.NumUids["mutation_cost"] = cost
		resp.Metrics.NumUids["_total"] = resp.Metrics.NumUids["_total"] + cost
	}
	if !qc.req.CommitNow {
		calculateMutationMetrics()
		if err == x.ErrConflict {
			err = status.Error(codes.FailedPrecondition, err.Error())
		}

		return err
	}

	// The following logic is for committing immediately.
	if err != nil {
		// ApplyMutations failed. We now want to abort the transaction,
		// ignoring any error that might occur during the abort (the user would
		// care more about the previous error).
		if resp.Txn == nil {
			resp.Txn = &api.TxnContext{StartTs: qc.req.StartTs}
		}

		resp.Txn.Aborted = true
		_, _ = worker.CommitOverNetwork(ctx, resp.Txn)

		if err == x.ErrConflict {
			// We have already aborted the transaction, so the error message should reflect that.
			return dgo.ErrAborted
		}

		return err
	}

	ctxn := resp.Txn
	// zero would assign the CommitTs
	cts, err := worker.CommitOverNetwork(ctx, ctxn)
	if err != nil {
		qc.span.AddEvent("Status of commit at ts",
			trace.WithAttributes(attribute.String("err", err.Error())))

		if err == dgo.ErrAborted {
			err = status.Error(codes.Aborted, err.Error())
			resp.Txn.Aborted = true
		}

		return err
	}

	// CommitNow was true, no need to send keys.
	resp.Txn.Keys = resp.Txn.Keys[:0]
	resp.Txn.CommitTs = cts
	calculateMutationMetrics()
	return nil
}

// validateMutation ensures that the value in the edge is not too big.
// The challange here is that the keys in badger have a limitation on their size (< 2<<16).
// We need to ensure that no key, either primary or secondary index key is bigger than that.
// See here for more details: https://github.com/dgraph-io/projects/issues/73
func validateMutation(ctx context.Context, edges []*pb.DirectedEdge) error {
	errValueTooBigForIndex := errors.New("value in the mutation is too large for the index")

	// key = meta data + predicate + actual key, this all needs to fit into 64 KB
	// we are keeping 536 bytes aside for meta information we put into the key and we
	// use 65000 bytes for the rest, that is predicate and the actual key.
	const maxKeySize = 65000

	for _, e := range edges {
		maxSizeForDataKey := maxKeySize - len(e.Attr)

		// seems reasonable to assume, the tokens for indexes won't be bigger than the value itself
		if len(e.Value) <= maxSizeForDataKey {
			continue
		}
		pred := x.NamespaceAttr(e.Namespace, e.Attr)
		update, ok := schema.State().Get(ctx, pred)
		if !ok {
			continue
		}
		// only string type can have large values that could cause us issues later
		if update.GetValueType() != pb.Posting_STRING {
			continue
		}

		storageVal := types.Val{Tid: types.TypeID(e.GetValueType()), Value: e.GetValue()}
		schemaVal, err := types.Convert(storageVal, types.TypeID(update.GetValueType()))
		if err != nil {
			return err
		}

		for _, tokenizer := range schema.State().Tokenizer(ctx, pred) {
			toks, err := tok.BuildTokens(schemaVal.Value, tok.GetTokenizerForLang(tokenizer, e.Lang))
			if err != nil {
				return fmt.Errorf("error while building index tokens: %w", err)
			}

			for _, tok := range toks {
				if len(tok) > maxSizeForDataKey {
					return errValueTooBigForIndex
				}
			}
		}
	}

	return nil
}

// buildUpsertQuery modifies the query to evaluate the
// @if condition defined in Conditional Upsert.
func buildUpsertQuery(qc *queryContext) string {
	if qc.req.Query == "" || len(qc.gmuList) == 0 {
		return qc.req.Query
	}

	qc.condVars = make([]string, len(qc.req.Mutations))

	var upsertQB strings.Builder
	x.Check2(upsertQB.WriteString(strings.TrimSuffix(qc.req.Query, "}")))

	for i, gmu := range qc.gmuList {
		isCondUpsert := strings.TrimSpace(gmu.Cond) != ""
		if isCondUpsert {
			qc.condVars[i] = fmt.Sprintf("__dgraph_upsertcheck_%v__", strconv.Itoa(i))
			qc.uidRes[qc.condVars[i]] = nil
			// @if in upsert is same as @filter in the query
			cond := strings.Replace(gmu.Cond, "@if", "@filter", 1)

			// Add dummy query to evaluate the @if directive, ok to use uid(0) because
			// dgraph doesn't check for existence of UIDs until we query for other predicates.
			// Here, we are only querying for uid predicate in the dummy query.
			//
			// For example if - mu.Query = {
			//      me(...) {...}
			//   }
			//
			// Then, upsertQuery = {
			//      me(...) {...}
			//      __dgraph_0__ as var(func: uid(0)) @filter(...)
			//   }
			//
			// The variable __dgraph_0__ will -
			//      * be empty if the condition is true
			//      * have 1 UID (the 0 UID) if the condition is false
			x.Check2(upsertQB.WriteString(qc.condVars[i]))
			x.Check2(upsertQB.WriteString(` as var(func: uid(0)) `))
			x.Check2(upsertQB.WriteString(cond))
			x.Check2(upsertQB.WriteString("\n"))
		}
	}

	x.Check2(upsertQB.WriteString(`}`))
	return upsertQB.String()
}

// updateMutations updates the mutation and replaces uid(var) and val(var) with
// their values or a blank node, in case of an upsert.
// We use the values stored in qc.uidRes and qc.valRes to update the mutation.
func updateMutations(qc *queryContext) error {
	for i, condVar := range qc.condVars {
		gmu := qc.gmuList[i]
		if condVar != "" {
			uids, ok := qc.uidRes[condVar]
			if !(ok && len(uids) == 1) {
				gmu.Set = nil
				gmu.Del = nil
				continue
			}
		}

		if err := updateUIDInMutations(gmu, qc); err != nil {
			return err
		}
		if err := updateValInMutations(gmu, qc); err != nil {
			return err
		}
	}

	return nil
}

// findMutationVars finds all the variables used in mutation block and stores them
// qc.uidRes and qc.valRes so that we only look for these variables in query results.
func findMutationVars(qc *queryContext) []string {
	updateVars := func(s string) {
		if strings.HasPrefix(s, "uid(") {
			varName := s[4 : len(s)-1]
			qc.uidRes[varName] = nil
		} else if strings.HasPrefix(s, "val(") {
			varName := s[4 : len(s)-1]
			qc.valRes[varName] = nil
		}
	}

	for _, gmu := range qc.gmuList {
		for _, nq := range gmu.Set {
			updateVars(nq.Subject)
			updateVars(nq.ObjectId)
		}
		for _, nq := range gmu.Del {
			updateVars(nq.Subject)
			updateVars(nq.ObjectId)
		}
	}

	varsList := make([]string, 0, len(qc.uidRes)+len(qc.valRes))
	for v := range qc.uidRes {
		varsList = append(varsList, v)
	}
	for v := range qc.valRes {
		varsList = append(varsList, v)
	}

	// We use certain variables to check uniqueness. To prevent errors related to unused
	// variables, we explicitly add them to the 'varsList' since they are not used in the mutation.
	for _, uniqueQueryVar := range qc.uniqueVars {
		varsList = append(varsList, uniqueQueryVar.queryVar)
		// If the triple contains a "val()" in objectId, we need to add
		// one more variable to the unique query. So, we explicitly add it to the varList.
		if uniqueQueryVar.valVar != "" {
			varsList = append(varsList, uniqueQueryVar.valVar)
		}
	}

	return varsList
}

// updateValInNQuads picks the val() from object and replaces it with its value
// Assumption is that Subject can contain UID, whereas Object can contain Val
// If val(variable) exists in a query, but the values are not there for the variable,
// it will ignore the mutation silently.
func updateValInNQuads(nquads []*api.NQuad, qc *queryContext, isSet bool) []*api.NQuad {
	getNewVals := func(s string) (*types.ShardedMap, bool) {
		if strings.HasPrefix(s, "val(") {
			varName := s[4 : len(s)-1]
			if v, ok := qc.valRes[varName]; ok && v != nil {
				return v, true
			}
			return nil, true
		}
		return nil, false
	}

	getValue := func(key uint64, uidToVal *types.ShardedMap) (types.Val, bool) {
		val, ok := uidToVal.Get(key)
		if ok {
			return val, true
		}

		// Check if the variable is aggregate variable
		// Only 0 key would exist for aggregate variable
		val, ok = uidToVal.Get(0)
		return val, ok
	}

	newNQuads := nquads[:0]
	for _, nq := range nquads {
		// Check if the nquad contains a val() in Object or not.
		// If not then, keep the mutation and continue
		uidToVal, found := getNewVals(nq.ObjectId)
		if !found {
			newNQuads = append(newNQuads, nq)
			continue
		}

		// uid(u) <amount> val(amt)
		// For each NQuad, we need to convert the val(variable_name)
		// to *api.Value before applying the mutation. For that, first
		// we convert key to uint64 and get the UID to Value map from
		// the result of the query.
		var key uint64
		var err error
		switch {
		case nq.Subject[0] == '_' && isSet:
			// in case aggregate val(var) is there, that should work with blank node.
			key = 0
		case nq.Subject[0] == '_' && !isSet:
			// UID is of format "_:uid(u)". Ignore the delete silently
			continue
		default:
			key, err = strconv.ParseUint(nq.Subject, 0, 64)
			if err != nil {
				// Key conversion failed, ignoring the nquad. Ideally,
				// it shouldn't happen as this is the result of a query.
				glog.Errorf("Conversion of subject %s failed. Error: %s",
					nq.Subject, err.Error())
				continue
			}
		}

		// Get the value to the corresponding UID(key) from the query result
		nq.ObjectId = ""
		val, ok := getValue(key, uidToVal)
		if !ok {
			continue
		}

		// Convert the value from types.Val to *api.Value
		nq.ObjectValue, err = types.ObjectValue(val.Tid, val.Value)
		if err != nil {
			// Value conversion failed, ignoring the nquad. Ideally,
			// it shouldn't happen as this is the result of a query.
			glog.Errorf("Conversion of %s failed for %d subject. Error: %s",
				nq.ObjectId, key, err.Error())
			continue
		}

		newNQuads = append(newNQuads, nq)
	}
	qc.nquadsCount += len(newNQuads)
	return newNQuads
}

// updateValInMutations does following transformations:
// 0x123 <amount> val(v) -> 0x123 <amount> 13.0
func updateValInMutations(gmu *dql.Mutation, qc *queryContext) error {
	gmu.Del = updateValInNQuads(gmu.Del, qc, false)
	gmu.Set = updateValInNQuads(gmu.Set, qc, true)
	if qc.nquadsCount > x.Config.LimitMutationsNquad {
		return errors.Errorf("NQuad count in the request: %d, is more that threshold: %d",
			qc.nquadsCount, x.Config.LimitMutationsNquad)
	}
	return nil
}

// updateUIDInMutations does following transformations:
//   - uid(v) -> 0x123     -- If v is defined in query block
//   - uid(v) -> _:uid(v)  -- Otherwise
func updateUIDInMutations(gmu *dql.Mutation, qc *queryContext) error {
	// usedMutationVars keeps track of variables that are used in mutations.
	getNewVals := func(s string) []string {
		if strings.HasPrefix(s, "uid(") {
			varName := s[4 : len(s)-1]
			if uids, ok := qc.uidRes[varName]; ok && len(uids) != 0 {
				return uids
			}

			return []string{"_:" + s}
		}

		return []string{s}
	}

	getNewNQuad := func(nq *api.NQuad, s, o string) *api.NQuad {
		// The following copy is fine because we only modify Subject and ObjectId.
		// The pointer values are not modified across different copies of NQuad.
		n := *nq

		n.Subject = s
		n.ObjectId = o
		return &n
	}

	// Remove the mutations from gmu.Del when no UID was found.
	gmuDel := make([]*api.NQuad, 0, len(gmu.Del))
	for _, nq := range gmu.Del {
		// if Subject or/and Object are variables, each NQuad can result
		// in multiple NQuads if any variable stores more than one UIDs.
		newSubs := getNewVals(nq.Subject)
		newObs := getNewVals(nq.ObjectId)

		for _, s := range newSubs {
			for _, o := range newObs {
				// Blank node has no meaning in case of deletion.
				if strings.HasPrefix(s, "_:uid(") ||
					strings.HasPrefix(o, "_:uid(") {
					continue
				}

				gmuDel = append(gmuDel, getNewNQuad(nq, s, o))
				qc.nquadsCount++
			}
			if qc.nquadsCount > x.Config.LimitMutationsNquad {
				return errors.Errorf("NQuad count in the request: %d, is more that threshold: %d",
					qc.nquadsCount, x.Config.LimitMutationsNquad)
			}
		}
	}

	gmu.Del = gmuDel

	// Update the values in mutation block from the query block.
	gmuSet := make([]*api.NQuad, 0, len(gmu.Set))
	for _, nq := range gmu.Set {
		newSubs := getNewVals(nq.Subject)
		newObs := getNewVals(nq.ObjectId)

		qc.nquadsCount += len(newSubs) * len(newObs)
		if qc.nquadsCount > int(x.Config.LimitQueryEdge) {
			return errors.Errorf("NQuad count in the request: %d, is more that threshold: %d",
				qc.nquadsCount, int(x.Config.LimitQueryEdge))
		}

		for _, s := range newSubs {
			for _, o := range newObs {
				gmuSet = append(gmuSet, getNewNQuad(nq, s, o))
			}
		}
	}
	gmu.Set = gmuSet
	return nil
}

// queryContext is used to pass around all the variables needed
// to process a request for query, mutation or upsert.
type queryContext struct {
	// req is the incoming, not yet parsed request containing
	// a query or more than one mutations or both (in case of upsert)
	req *api.Request
	// gmuList is the list of mutations after parsing req.Mutations
	gmuList []*dql.Mutation
	// dqlRes contains result of parsing the req.Query
	dqlRes dql.Result
	// condVars are conditional variables used in the (modified) query to figure out
	// whether the condition in Conditional Upsert is true. The string would be empty
	// if the corresponding mutation is not a conditional upsert.
	// Note that, len(condVars) == len(gmuList).
	condVars []string
	// uidRes stores mapping from variable names to UIDs for UID variables.
	// These variables are either dummy variables used for Conditional
	// Upsert or variables used in the mutation block in the incoming request.
	uidRes map[string][]string
	// valRes stores mapping from variable names to values for value
	// variables used in the mutation block of incoming request.
	valRes map[string]*types.ShardedMap
	// l stores latency numbers
	latency *query.Latency
	// span stores a opencensus span used throughout the query processing
	span trace.Span
	// graphql indicates whether the given request is from graphql admin or not.
	graphql bool
	// gqlField stores the GraphQL field for which the query is being processed.
	// This would be set only if the request is a query from GraphQL layer,
	// otherwise it would be nil. (Eg. nil cases: in case of a DQL query,
	// a mutation being executed from GraphQL layer).
	gqlField gqlSchema.Field
	// nquadsCount maintains numbers of nquads which would be inserted as part of this request.
	// In some cases(mostly upserts), numbers of nquads to be inserted can to huge(we have seen upto
	// 1B) and resulting in OOM. We are limiting number of nquads which can be inserted in
	// a single request.
	nquadsCount int
	// uniqueVar stores the mapping between the indexes of gmuList and gmu.Set,
	// along with their respective uniqueQueryVariables.
	uniqueVars map[uint64]uniquePredMeta
}

// Request represents a query request sent to the doQuery() method on the Server.
// It contains all the metadata required to execute a query.
type Request struct {
	// req is the incoming gRPC request
	req *api.Request
	// gqlField is the GraphQL field for which the request is being sent
	gqlField gqlSchema.Field
	// doAuth tells whether this request needs ACL authorization or not
	doAuth AuthMode
}

// Health handles /health and /health?all requests.
func (s *Server) Health(ctx context.Context, all bool) (*api.Response, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var healthAll []pb.HealthInfo
	if all {
		if err := AuthorizeGuardians(ctx); err != nil {
			return nil, err
		}
		pool := conn.GetPools().GetAll()
		for _, p := range pool {
			if p.Addr == x.WorkerConfig.MyAddr {
				continue
			}
			healthAll = append(healthAll, p.HealthInfo())
		}
	}

	// Append self.
	healthAll = append(healthAll, pb.HealthInfo{
		Instance:    "alpha",
		Address:     x.WorkerConfig.MyAddr,
		Status:      "healthy",
		Group:       strconv.Itoa(int(worker.GroupId())),
		Version:     x.Version(),
		Uptime:      int64(time.Since(x.WorkerConfig.StartTime) / time.Second),
		LastEcho:    time.Now().Unix(),
		Ongoing:     worker.GetOngoingTasks(),
		Indexing:    schema.GetIndexingPredicates(),
		EeFeatures:  worker.GetFeaturesList(),
		MaxAssigned: posting.Oracle().MaxAssigned(),
	})

	var err error
	var jsonOut []byte
	if jsonOut, err = json.Marshal(healthAll); err != nil {
		return nil, errors.Errorf("Unable to Marshal. Err %v", err)
	}
	return &api.Response{Json: jsonOut}, nil
}

// Filter out the tablets that do not belong to the requestor's namespace.
func filterTablets(ctx context.Context, ms *pb.MembershipState) error {
	if !x.WorkerConfig.AclEnabled {
		return nil
	}
	namespace, err := x.ExtractNamespaceFrom(ctx)
	if err != nil {
		return errors.Errorf("Namespace not found in JWT.")
	}
	if namespace == x.RootNamespace {
		// For galaxy namespace, we don't want to filter out the predicates.
		return nil
	}
	for _, group := range ms.GetGroups() {
		tablets := make(map[string]*pb.Tablet)
		for pred, tablet := range group.GetTablets() {
			if ns, attr := x.ParseNamespaceAttr(pred); namespace == ns {
				tablets[attr] = tablet
				tablets[attr].Predicate = attr
			}
		}
		group.Tablets = tablets
	}
	return nil
}

// State handles state requests
func (s *Server) State(ctx context.Context) (*api.Response, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if err := AuthorizeGuardians(ctx); err != nil {
		return nil, err
	}

	ms := worker.GetMembershipState()
	if ms == nil {
		return nil, errors.Errorf("No membership state found")
	}

	if err := filterTablets(ctx, ms); err != nil {
		return nil, err
	}

	m := protojson.MarshalOptions{EmitUnpopulated: true}
	jsonState, err := m.Marshal(ms)
	if err != nil {
		return nil, errors.Errorf("Error marshalling state information to JSON")
	}

	return &api.Response{Json: jsonState}, nil
}

func getAuthMode(ctx context.Context) AuthMode {
	if auth := ctx.Value(Authorize); auth == nil || auth.(bool) {
		return NeedAuthorize
	}
	return NoAuthorize
}

// QueryGraphQL handles only GraphQL queries, neither mutations nor DQL.
func (s *Server) QueryGraphQL(ctx context.Context, req *api.Request,
	field gqlSchema.Field) (*api.Response, error) {
	// Add a timeout for queries which don't have a deadline set. We don't want to
	// apply a timeout if it's a mutation, that's currently handled by flag
	// "txn-abort-after".
	if req.GetMutations() == nil && x.Config.QueryTimeout != 0 {
		if d, _ := ctx.Deadline(); d.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, x.Config.QueryTimeout)
			defer cancel()
		}
	}
	// no need to attach namespace here, it is already done by GraphQL layer
	return s.doQuery(ctx, &Request{req: req, gqlField: field, doAuth: getAuthMode(ctx)})
}

func (s *Server) Query(ctx context.Context, req *api.Request) (*api.Response, error) {
	resp, err := s.QueryNoGrpc(ctx, req)
	if err != nil {
		return resp, err
	}
	md := metadata.Pairs(x.DgraphCostHeader, fmt.Sprint(resp.Metrics.NumUids["_total"]))
	if err := grpc.SendHeader(ctx, md); err != nil {
		glog.Warningf("error in sending grpc headers: %v", err)
	}
	return resp, nil
}

// Query handles queries or mutations
func (s *Server) QueryNoGrpc(ctx context.Context, req *api.Request) (*api.Response, error) {
	ctx = x.AttachJWTNamespace(ctx)
	if x.WorkerConfig.AclEnabled && req.GetStartTs() != 0 {
		// A fresh StartTs is assigned if it is 0.
		ns, err := x.ExtractNamespace(ctx)
		if err != nil {
			return nil, err
		}
		if req.GetHash() != getHash(ns, req.GetStartTs()) {
			return nil, x.ErrHashMismatch
		}
	}
	// Add a timeout for queries which don't have a deadline set. We don't want to
	// apply a timeout if it's a mutation, that's currently handled by flag
	// "txn-abort-after".
	if req.GetMutations() == nil && x.Config.QueryTimeout != 0 {
		if d, _ := ctx.Deadline(); d.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, x.Config.QueryTimeout)
			defer cancel()
		}
	}
	return s.doQuery(ctx, &Request{req: req, doAuth: getAuthMode(ctx)})
}

func (s *Server) QueryNoAuth(ctx context.Context, req *api.Request) (*api.Response, error) {
	return s.doQuery(ctx, &Request{req: req, doAuth: NoAuthorize})
}

var pendingQueries int64
var maxPendingQueries int64
var serverOverloadErr = errors.New("429 Too Many Requests. Please throttle your requests")

func Init() {
	maxPendingQueries = x.Config.Limit.GetInt64("max-pending-queries")
}

func (s *Server) doQuery(ctx context.Context, req *Request) (resp *api.Response, rerr error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	defer atomic.AddInt64(&pendingQueries, -1)
	if val := atomic.AddInt64(&pendingQueries, 1); val > maxPendingQueries {
		return nil, serverOverloadErr
	}

	isGraphQL, _ := ctx.Value(IsGraphql).(bool)
	if isGraphQL {
		atomic.AddUint64(&numGraphQL, 1)
	} else {
		atomic.AddUint64(&numDQL, 1)
	}
	l := &query.Latency{}
	l.Start = time.Now()

	if bool(glog.V(3)) || worker.LogDQLRequestEnabled() {
		glog.Infof("Got a query, DQL form: %+v %+v at %+v",
			req.req.Query, req.req.Mutations, l.Start.Format(time.RFC3339))
	}

	isMutation := len(req.req.Mutations) > 0
	methodRequest := methodQuery
	if isMutation {
		methodRequest = methodMutate
	}

	var measurements []ostats.Measurement
	ctx, span := otel.Tracer("").Start(ctx, methodRequest)
	if ns, err := x.ExtractNamespace(ctx); err == nil {
		annotateNamespace(span, ns)
	}

	ctx = x.WithMethod(ctx, methodRequest)
	defer func() {
		span.End()
		v := x.TagValueStatusOK
		if rerr != nil {
			v = x.TagValueStatusError
		}
		ctx, _ = tag.New(ctx, tag.Upsert(x.KeyStatus, v))
		timeSpentMs := x.SinceMs(l.Start)
		measurements = append(measurements, x.LatencyMs.M(timeSpentMs))
		ostats.Record(ctx, measurements...)
	}()

	if rerr = x.HealthCheck(); rerr != nil {
		return
	}

	req.req.Query = strings.TrimSpace(req.req.Query)
	isQuery := len(req.req.Query) != 0
	if !isQuery && !isMutation {
		span.AddEvent("empty request")
		return nil, errors.Errorf("empty request")
	}

	span.AddEvent("Request received",
		trace.WithAttributes(attribute.String("Query", req.req.Query)))
	if isQuery {
		ostats.Record(ctx, x.PendingQueries.M(1), x.NumQueries.M(1))
		defer func() {
			measurements = append(measurements, x.PendingQueries.M(-1))
		}()
	}
	if isMutation {
		ostats.Record(ctx, x.NumMutations.M(1))
	}

	if req.doAuth == NeedAuthorize && x.IsRootNsOperation(ctx) {
		// Only the guardian of the galaxy can do a galaxy wide query/mutation. This operation is
		// needed by live loader.
		if err := AuthSuperAdmin(ctx); err != nil {
			s := status.Convert(err)
			return nil, status.Error(s.Code(),
				"Non superadmin user cannot bypass namespaces. "+s.Message())
		}
	}

	qc := &queryContext{
		req:      req.req,
		latency:  l,
		span:     span,
		graphql:  isGraphQL,
		gqlField: req.gqlField,
	}
	if rerr = parseRequest(ctx, qc); rerr != nil {
		return
	}

	if req.doAuth == NeedAuthorize {
		if rerr = authorizeRequest(ctx, qc); rerr != nil {
			return
		}
	}

	// We use defer here because for queries, startTs will be
	// assigned in the processQuery function called below.
	defer annotateStartTs(qc.span, qc.req.StartTs)
	// For mutations, we update the startTs if necessary.
	if isMutation && req.req.StartTs == 0 {
		start := time.Now()
		req.req.StartTs = worker.State.GetTimestamp(false)
		qc.latency.AssignTimestamp = time.Since(start)
	}
	if x.WorkerConfig.AclEnabled {
		ns, err := x.ExtractNamespace(ctx)
		if err != nil {
			return nil, err
		}
		defer func() {
			if resp != nil && resp.Txn != nil {
				// attach the hash, user must send this hash when further operating on this startTs.
				resp.Txn.Hash = getHash(ns, resp.Txn.StartTs)
			}
		}()
	}

	var gqlErrs error
	if resp, rerr = processQuery(ctx, qc); rerr != nil {
		// if rerr is just some error from GraphQL encoding, then we need to continue the normal
		// execution ignoring the error as we still need to assign latency info to resp. If we can
		// change the api.Response proto to have a field to contain GraphQL errors, that would be
		// great. Otherwise, we will have to do such checks a lot and that would make code ugly.
		if qc.gqlField != nil && x.IsGqlErrorList(rerr) {
			gqlErrs = rerr
		} else {
			return
		}
	}
	// if it were a mutation, simple or upsert, in any case gqlErrs would be empty as GraphQL JSON
	// is formed only for queries. So, gqlErrs can have something only in the case of a pure query.
	// So, safe to ignore gqlErrs and not return that here.
	if rerr = s.doMutate(ctx, qc, resp); rerr != nil {
		return
	}

	// TODO(Ahsan): resp.Txn.Preds contain predicates of form gid-namespace|attr.
	// Remove the namespace from the response.
	// resp.Txn.Preds = x.ParseAttrList(resp.Txn.Preds)

	// TODO(martinmr): Include Transport as part of the latency. Need to do
	// this separately since it involves modifying the API protos.
	resp.Latency = &api.Latency{
		AssignTimestampNs: uint64(l.AssignTimestamp.Nanoseconds()),
		ParsingNs:         uint64(l.Parsing.Nanoseconds()),
		ProcessingNs:      uint64(l.Processing.Nanoseconds()),
		EncodingNs:        uint64(l.Json.Nanoseconds()),
		TotalNs:           uint64((time.Since(l.Start)).Nanoseconds()),
	}
	return resp, gqlErrs
}

func processQuery(ctx context.Context, qc *queryContext) (*api.Response, error) {
	resp := &api.Response{}
	if qc.req.Query == "" {
		// No query, so make the query cost 0.
		resp.Metrics = &api.Metrics{
			NumUids: map[string]uint64{"_total": 0},
		}
		return resp, nil
	}
	if ctx.Err() != nil {
		return resp, ctx.Err()
	}
	qr := query.Request{
		Latency:  qc.latency,
		DqlQuery: &qc.dqlRes,
	}

	// Here we try our best effort to not contact Zero for a timestamp. If we succeed,
	// then we use the max known transaction ts value (from ProcessDelta) for a read-only query.
	// If we haven't processed any updates yet then fall back to getting TS from Zero.
	switch {
	case qc.req.BestEffort:
		qc.span.AddEvent("", trace.WithAttributes(attribute.Bool("be", true)))
	case qc.req.ReadOnly:
		qc.span.AddEvent("", trace.WithAttributes(attribute.Bool("ro", true)))
	default:
		qc.span.AddEvent("", trace.WithAttributes(attribute.Bool("no", true)))
	}

	if qc.req.BestEffort {
		// Sanity: check that request is read-only too.
		if !qc.req.ReadOnly {
			return resp, errors.Errorf("A best effort query must be read-only.")
		}
		if qc.req.StartTs == 0 {
			qc.req.StartTs = posting.Oracle().MaxAssigned()
		}
		qr.Cache = worker.NoCache
	}

	if qc.req.StartTs == 0 {
		assignTimestampStart := time.Now()
		qc.req.StartTs = worker.State.GetTimestamp(qc.req.ReadOnly)
		qc.latency.AssignTimestamp = time.Since(assignTimestampStart)
	}

	qr.ReadTs = qc.req.StartTs
	resp.Txn = &api.TxnContext{StartTs: qc.req.StartTs}

	// Core processing happens here.
	er, err := qr.Process(ctx)

	if bool(glog.V(3)) || worker.LogDQLRequestEnabled() {
		glog.Infof("Finished a query that started at: %+v",
			qr.Latency.Start.Format(time.RFC3339))
	}

	if err != nil {
		if bool(glog.V(3)) {
			glog.Infof("Error processing query: %+v\n", err.Error())
		}
		return resp, errors.Wrap(err, "")
	}

	if len(er.SchemaNode) > 0 || len(er.Types) > 0 {
		if err = authorizeSchemaQuery(ctx, &er); err != nil {
			return resp, err
		}
		sort.Slice(er.SchemaNode, func(i, j int) bool {
			return er.SchemaNode[i].Predicate < er.SchemaNode[j].Predicate
		})
		sort.Slice(er.Types, func(i, j int) bool {
			return er.Types[i].TypeName < er.Types[j].TypeName
		})

		respMap := make(map[string]interface{})
		if len(er.SchemaNode) > 0 {
			respMap["schema"] = er.SchemaNode
		}
		if len(er.Types) > 0 {
			respMap["types"] = formatTypes(er.Types)
		}
		resp.Json, err = json.Marshal(respMap)
	} else if qc.req.RespFormat == api.Request_RDF {
		resp.Rdf, err = query.ToRDF(qc.latency, er.Subgraphs)
	} else {
		resp.Json, err = query.ToJson(ctx, qc.latency, er.Subgraphs, qc.gqlField)
	}
	// if err is just some error from GraphQL encoding, then we need to continue the normal
	// execution ignoring the error as we still need to assign metrics and latency info to resp.
	if err != nil && (qc.gqlField == nil || !x.IsGqlErrorList(err)) {
		return resp, err
	}
	qc.span.AddEvent("Response",
		trace.WithAttributes(attribute.String("response", string(resp.Json))))

	// varToUID contains a map of variable name to the uids corresponding to it.
	// It is used later for constructing set and delete mutations by replacing
	// variables with the actual uids they correspond to.
	// If a variable doesn't have any UID, we generate one ourselves later.
	for name := range qc.uidRes {
		v := qr.Vars[name]

		// If the list of UIDs is empty but the map of values is not,
		// we need to get the UIDs from the keys in the map.
		var uidList []uint64
		if v.Uids != nil && len(v.Uids.Uids) > 0 {
			uidList = v.Uids.Uids
		} else {
			uidList = make([]uint64, 0, v.Vals.Len())
			v.Vals.Iterate(func(uid uint64, val types.Val) error {
				uidList = append(uidList, uid)
				return nil
			})
		}
		if len(uidList) == 0 {
			continue
		}

		// We support maximum 1 million UIDs per variable to ensure that we
		// don't do bad things to alpha and mutation doesn't become too big.
		if len(uidList) > 1e6 {
			return resp, errors.Errorf("var [%v] has over million UIDs", name)
		}

		uids := make([]string, len(uidList))
		for i, u := range uidList {
			// We use base 10 here because the RDF mutations expect the uid to be in base 10.
			uids[i] = strconv.FormatUint(u, 10)
		}
		qc.uidRes[name] = uids
	}

	// look for values for value variables
	for name := range qc.valRes {
		v := qr.Vars[name]
		qc.valRes[name] = v.Vals
	}

	if err := verifyUnique(qc, qr); err != nil {
		return resp, err
	}

	resp.Metrics = &api.Metrics{
		NumUids: er.Metrics,
	}
	var total uint64
	for _, num := range resp.Metrics.NumUids {
		total += num
	}
	resp.Metrics.NumUids["_total"] = total

	return resp, err
}

// parseRequest parses the incoming request
func parseRequest(ctx context.Context, qc *queryContext) error {
	start := time.Now()
	defer func() {
		qc.latency.Parsing = time.Since(start)
	}()

	var needVars []string
	upsertQuery := qc.req.Query
	if len(qc.req.Mutations) > 0 {
		// parsing mutations
		qc.gmuList = make([]*dql.Mutation, 0, len(qc.req.Mutations))
		for _, mu := range qc.req.Mutations {
			gmu, err := ParseMutationObject(mu, qc.graphql)
			if err != nil {
				return err
			}

			qc.gmuList = append(qc.gmuList, gmu)
		}

		if err := addQueryIfUnique(ctx, qc); err != nil {
			return err
		}

		qc.uidRes = make(map[string][]string)
		qc.valRes = make(map[string]*types.ShardedMap)
		upsertQuery = buildUpsertQuery(qc)
		needVars = findMutationVars(qc)
		if upsertQuery == "" {
			if len(needVars) > 0 {
				return errors.Errorf("variables %v not defined", needVars)
			}

			return nil
		}
	}

	// parsing the updated query
	var err error
	qc.dqlRes, err = dql.ParseWithNeedVars(dql.Request{
		Str:       upsertQuery,
		Variables: qc.req.Vars,
	}, needVars)
	if err != nil {
		return err
	}
	return validateQuery(qc.dqlRes.Query)
}

// verifyUnique verifies uniqueness of mutation
func verifyUnique(qc *queryContext, qr query.Request) error {
	if len(qc.uniqueVars) == 0 {
		return nil
	}

	for i, queryVar := range qc.uniqueVars {
		gmuIndex, rdfIndex := decodeIndex(i)
		pred := qc.gmuList[gmuIndex].Set[rdfIndex]
		queryResult := qr.Vars[queryVar.queryVar]
		if !isUpsertCondTrue(qc, int(gmuIndex)) {
			continue
		}
		isEmpty := func(l *pb.List) bool {
			return l == nil || len(l.Uids) == 0
		}

		var subjectUid uint64
		if strings.HasPrefix(pred.Subject, "uid(") {
			varName := qr.Vars[pred.Subject[4:len(pred.Subject)-1]]
			if isEmpty(varName.Uids) {
				subjectUid = 0 // blank node
			} else if len(varName.Uids.Uids) == 1 {
				subjectUid = varName.Uids.Uids[0]
			} else {
				return errors.Errorf("unique constraint violated for predicate [%v]", pred.Predicate)
			}
		} else {
			var err error
			subjectUid, err = parseSubject(pred.Subject)
			if err != nil {
				return errors.Wrapf(err, "error while parsing [%v]", pred.Subject)
			}
		}

		var predValue interface{}
		if strings.HasPrefix(pred.ObjectId, "val(") {
			varName := qr.Vars[pred.ObjectId[4:len(pred.ObjectId)-1]]
			val, ok := varName.Vals.Get(0)
			if !ok {
				_, isValueGoingtoSet := varName.Vals.Get(subjectUid)
				if !isValueGoingtoSet {
					continue
				}

				results := qr.Vars[queryVar.valVar]
				err := results.Vals.Iterate(func(uidOfv uint64, v types.Val) error {
					varNameVal, _ := varName.Vals.Get(subjectUid)
					if v.Value == varNameVal.Value && uidOfv != subjectUid {
						return errors.Errorf("could not insert duplicate value [%v] for predicate [%v]",
							v.Value, pred.Predicate)
					}
					return nil
				})
				if err != nil {
					return err
				}
				continue
			} else {
				predValue = val.Value
			}
		} else {
			predValue = dql.TypeValFrom(pred.ObjectValue).Value
		}

		// Here, we check the uniqueness of the triple by comparing the result of the uniqueQuery with the triple.
		if !isEmpty(queryResult.Uids) {
			if len(queryResult.Uids.Uids) > 1 {
				glog.Errorf("unique constraint violated for predicate [%v].uids: [%v].namespace: [%v]",
					pred.Predicate, queryResult.Uids.Uids, pred.Namespace)
				return errors.Errorf("there are duplicates in existing data for predicate [%v]."+
					"Please drop the unique constraint and re-add it after fixing the predicate data", pred.Predicate)
			} else if queryResult.Uids.Uids[0] != subjectUid {
				// Determine whether the mutation is a swap mutation
				isSwap, err := isSwap(qc, queryResult.Uids.Uids[0], pred.Predicate)
				if err != nil {
					return err
				}
				if !isSwap {
					return errors.Errorf("could not insert duplicate value [%v] for predicate [%v]",
						predValue, pred.Predicate)
				}
			}
		}
	}
	return nil
}

// addQueryIfUnique adds dummy queries in the request for checking whether predicate is unique in the db
func addQueryIfUnique(qctx context.Context, qc *queryContext) error {
	if len(qc.gmuList) == 0 {
		return nil
	}

	ctx := context.WithValue(qctx, schema.IsWrite, false)
	namespace, err := x.ExtractNamespace(ctx)
	if err != nil {
		// It's okay to ignore this here. If namespace is not set, it could mean either there is no
		// authorization or it's trying to be bypassed. So the namespace is either 0 or the mutation would fail.
		glog.Errorf("Error while extracting namespace, assuming default %s", err)
		namespace = 0
	}
	isGalaxyQuery := x.IsRootNsOperation(ctx)

	qc.uniqueVars = map[uint64]uniquePredMeta{}
	for gmuIndex, gmu := range qc.gmuList {
		var buildQuery strings.Builder
		for rdfIndex, pred := range gmu.Set {
			if isGalaxyQuery {
				// The caller should make sure that the directed edges contain the namespace we want
				// to insert into.
				namespace = pred.Namespace
			}
			if pred.Predicate != "dgraph.xid" {
				// [TODO] Don't check if it's dgraph.xid. It's a bug as this node might not be aware
				// of the schema for the given predicate. This is a bug issue for dgraph.xid hence
				// we are bypassing it manually until the bug is fixed.
				predSchema, ok := schema.State().Get(ctx, x.NamespaceAttr(namespace, pred.Predicate))
				if !ok || !predSchema.Unique {
					continue
				}
			}

			// Wrapping predicateName with angle brackets ensures that if the predicate contains any non-Latin letters,
			// the unique query will not fail. Additionally,
			// it helps ensure that non-Latin predicate names are properly formatted
			// during the automatic serialization of a structure into JSON.
			predicateName := fmt.Sprintf("<%v>", pred.Predicate)
			if pred.Lang != "" {
				predicateName = fmt.Sprintf("%v@%v", predicateName, pred.Lang)
			}

			uniqueVarMapKey := encodeIndex(gmuIndex, rdfIndex)
			queryVar := fmt.Sprintf("__dgraph_uniquecheck_%v__", uniqueVarMapKey)
			// Now, we add a query for a predicate to check if the value of the
			// predicate sent in the mutation already exists in the DB.
			// For example, schema => email: string @unique @index(exact) .\
			//
			// To ensure uniqueness of values of email predicate, we will query for the value
			// to ensure that we are not adding a duplicate value.
			//
			// There are following use-cases of mutations which we need to handle:
			//   1. _:a <email> "example@email.com"  .
			//   2. _:a <email> val(queryVariable)  .
			// In the code below, we construct respective query for the above use-cases viz.
			//   __dgraph_uniquecheck_1__ as var(func: eq(email,"example@email.com"))
			//   __dgraph_uniquecheck_2__ as var(func: eq(email, val(queryVariable))){
			//       uid
			//       __dgraph_uniquecheck_val_2__ as email
			//   }
			// Each of the above query will check if there is already an existing value.
			// We can be sure that we may get at most one UID in return.
			// If the returned UID is different than the UID we have
			// in the mutation, then we reject the mutation.

			if !strings.HasPrefix(pred.ObjectId, "val(") {
				val := strconv.Quote(fmt.Sprintf("%v", dql.TypeValFrom(pred.ObjectValue).Value))
				query := fmt.Sprintf(`%v as var(func: eq(%v,"%v"))`, queryVar, predicateName, val[1:len(val)-1])
				if _, err := buildQuery.WriteString(query); err != nil {
					return errors.Wrapf(err, "error while writing string")
				}
				qc.uniqueVars[uniqueVarMapKey] = uniquePredMeta{queryVar: queryVar}
			} else {
				valQueryVar := fmt.Sprintf("__dgraph_uniquecheck_val_%v__", uniqueVarMapKey)
				query := fmt.Sprintf(`%v as var(func: eq(%v,%v)){
					                             uid
					                             %v as %v
				                 }`, queryVar, predicateName, pred.ObjectId, valQueryVar, predicateName)
				if _, err := buildQuery.WriteString(query); err != nil {
					return errors.Wrapf(err, "error while building unique query")
				}
				qc.uniqueVars[uniqueVarMapKey] = uniquePredMeta{valVar: valQueryVar, queryVar: queryVar}
			}
		}

		if buildQuery.Len() > 0 {
			if _, err := buildQuery.WriteString("}"); err != nil {
				return errors.Wrapf(err, "error while writing string")
			}
			if qc.req.Query == "" {
				qc.req.Query = "{" + buildQuery.String()
			} else {
				qc.req.Query = strings.TrimSuffix(qc.req.Query, "}")
				qc.req.Query = qc.req.Query + buildQuery.String()
			}
		}
	}
	return nil
}

func authorizeRequest(ctx context.Context, qc *queryContext) error {
	if err := authorizeQuery(ctx, &qc.dqlRes, qc.graphql); err != nil {
		return err
	}

	// TODO(Aman): can be optimized to do the authorization in just one func call
	for _, gmu := range qc.gmuList {
		if err := authorizeMutation(ctx, gmu); err != nil {
			return err
		}
	}

	return nil
}

func getHash(ns, startTs uint64) string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%#x%#x%#x", ns, startTs, []byte(worker.Config.AclSecretKeyBytes))))
	return hex.EncodeToString(h.Sum(nil))
}

func validateNamespace(ctx context.Context, tc *api.TxnContext) error {
	if !x.WorkerConfig.AclEnabled {
		return nil
	}

	ns, err := x.ExtractNamespaceFrom(ctx)
	if err != nil {
		return err
	}
	if tc.Hash != getHash(ns, tc.StartTs) {
		return x.ErrHashMismatch
	}
	return nil
}

func (s *ServerV25) UpdateExtSnapshotStreamingState(ctx context.Context,
	req *apiv2.UpdateExtSnapshotStreamingStateRequest) (v *apiv2.UpdateExtSnapshotStreamingStateResponse, err error) {

	if req == nil {
		return nil, errors.New("UpdateExtSnapshotStreamingStateRequest must not be nil")
	}

	if req.Start && req.Finish {
		return nil, errors.New("UpdateExtSnapshotStreamingStateRequest cannot have both Start and Finish set to true")
	}

	groups, err := worker.ProposeDrain(ctx, req)
	if err != nil {
		return nil, err
	}

	resp := &apiv2.UpdateExtSnapshotStreamingStateResponse{Groups: groups}

	return resp, nil
}

func (s *ServerV25) StreamExtSnapshot(stream apiv2.Dgraph_StreamExtSnapshotServer) error {
	defer x.ExtSnapshotStreamingState(false)

	return worker.InStream(stream)
}

// CommitOrAbort commits or aborts a transaction.
func (s *Server) CommitOrAbort(ctx context.Context, tc *api.TxnContext) (*api.TxnContext, error) {
	ctx, span := otel.Tracer("").Start(ctx, "Server.CommitOrAbort")
	defer span.End()

	if err := x.HealthCheck(); err != nil {
		return &api.TxnContext{}, err
	}

	tctx := &api.TxnContext{}
	if tc.StartTs == 0 {
		return &api.TxnContext{}, errors.Errorf(
			"StartTs cannot be zero while committing a transaction")
	}
	if ns, err := x.ExtractNamespaceFrom(ctx); err == nil {
		annotateNamespace(span, ns)
	}
	annotateStartTs(span, tc.StartTs)

	if err := validateNamespace(ctx, tc); err != nil {
		return &api.TxnContext{}, err
	}

	span.AddEvent("Txn Context received", trace.WithAttributes(attribute.Stringer("txn", tc)))
	commitTs, err := worker.CommitOverNetwork(ctx, tc)
	if err == dgo.ErrAborted {
		// If err returned is dgo.ErrAborted and tc.Aborted was set, that means the client has
		// aborted the transaction by calling txn.Discard(). Hence return a nil error.
		tctx.Aborted = true
		if tc.Aborted {
			return tctx, nil
		}

		return tctx, status.Error(codes.Aborted, err.Error())
	}
	tctx.StartTs = tc.StartTs
	tctx.CommitTs = commitTs
	return tctx, err
}

// CheckVersion returns the version of this Dgraph instance.
func (s *Server) CheckVersion(ctx context.Context, c *api.Check) (v *api.Version, err error) {
	if err := x.HealthCheck(); err != nil {
		return v, err
	}

	v = new(api.Version)
	v.Tag = x.Version()
	return v, nil
}

// -------------------------------------------------------------------------------------------------
// HELPER FUNCTIONS
// -------------------------------------------------------------------------------------------------
func isMutationAllowed(ctx context.Context) bool {
	if worker.Config.MutationsMode != worker.DisallowMutations {
		return true
	}
	shareAllowed, ok := ctx.Value("_share_").(bool)
	if !ok || !shareAllowed {
		return false
	}
	return true
}

var errNoAuth = errors.Errorf("No Auth Token found. Token needed for Admin operations.")

func hasAdminAuth(ctx context.Context, tag string) (net.Addr, error) {
	ipAddr, err := x.HasWhitelistedIP(ctx)
	if err != nil {
		return nil, err
	}
	glog.Infof("Got %s request from: %q\n", tag, ipAddr)
	if err = hasPoormansAuth(ctx); err != nil {
		return nil, err
	}
	return ipAddr, nil
}

func hasPoormansAuth(ctx context.Context) error {
	if worker.Config.AuthToken == "" {
		return nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return errNoAuth
	}
	tokens := md.Get("auth-token")
	if len(tokens) == 0 {
		return errNoAuth
	}
	if tokens[0] != worker.Config.AuthToken {
		return errors.Errorf("Provided auth token [%s] does not match. Permission denied.", tokens[0])
	}
	return nil
}

// ParseMutationObject tries to consolidate fields of the api.Mutation into the
// corresponding field of the returned dql.Mutation. For example, the 3 fields,
// api.Mutation#SetJson, api.Mutation#SetNquads and api.Mutation#Set are consolidated into the
// dql.Mutation.Set field. Similarly the 3 fields api.Mutation#DeleteJson, api.Mutation#DelNquads
// and api.Mutation#Del are merged into the dql.Mutation#Del field.
func ParseMutationObject(mu *api.Mutation, isGraphql bool) (*dql.Mutation, error) {
	res := &dql.Mutation{Cond: mu.Cond}

	if len(mu.SetJson) > 0 {
		nqs, md, err := chunker.ParseJSON(mu.SetJson, chunker.SetNquads)
		if err != nil {
			return nil, err
		}
		res.Set = append(res.Set, nqs...)
		res.Metadata = md
	}
	if len(mu.DeleteJson) > 0 {
		// The metadata is not currently needed for delete operations so it can be safely ignored.
		nqs, _, err := chunker.ParseJSON(mu.DeleteJson, chunker.DeleteNquads)
		if err != nil {
			return nil, err
		}
		res.Del = append(res.Del, nqs...)
	}
	if len(mu.SetNquads) > 0 {
		nqs, md, err := chunker.ParseRDFs(mu.SetNquads)
		if err != nil {
			return nil, err
		}
		res.Set = append(res.Set, nqs...)
		res.Metadata = md
	}
	if len(mu.DelNquads) > 0 {
		nqs, _, err := chunker.ParseRDFs(mu.DelNquads)
		if err != nil {
			return nil, err
		}
		res.Del = append(res.Del, nqs...)
	}

	res.Set = append(res.Set, mu.Set...)
	res.Del = append(res.Del, mu.Del...)
	// parse facets and convert to the binary format so that
	// a field of type datetime like "2017-01-01" can be correctly encoded in the
	// marshaled binary format as done in the time.Marshal method
	if err := validateAndConvertFacets(res.Set); err != nil {
		return nil, err
	}

	if err := validateNQuads(res.Set, res.Del, isGraphql); err != nil {
		return nil, err
	}
	return res, nil
}

func validateAndConvertFacets(nquads []*api.NQuad) error {
	for _, m := range nquads {
		encodedFacets := make([]*api.Facet, 0, len(m.Facets))
		for _, f := range m.Facets {
			// try to interpret the value as binary first
			if _, err := facets.ValFor(f); err == nil {
				encodedFacets = append(encodedFacets, f)
			} else {
				encodedFacet, err := facets.FacetFor(f.Key, string(f.Value))
				if err != nil {
					return err
				}
				encodedFacets = append(encodedFacets, encodedFacet)
			}
		}

		m.Facets = encodedFacets
	}
	return nil
}

// validateForOtherReserved validate nquads for other reserved predicates
func validateForOtherReserved(nq *api.NQuad, isGraphql bool) error {
	// Check whether the incoming predicate is other reserved predicate.
	if !isGraphql && x.IsOtherReservedPredicate(nq.Predicate) {
		return errors.Errorf("Cannot mutate graphql reserved predicate %s", nq.Predicate)
	}
	return nil
}

func validateNQuads(set, del []*api.NQuad, isGraphql bool) error {
	for _, nq := range set {
		if err := validatePredName(nq.Predicate); err != nil {
			return err
		}
		var ostar bool
		if o, ok := nq.ObjectValue.GetVal().(*api.Value_DefaultVal); ok {
			ostar = o.DefaultVal == x.Star
		}
		if nq.Subject == x.Star || nq.Predicate == x.Star || ostar {
			return errors.Errorf("Cannot use star in set n-quad: %+v", nq)
		}
		if err := validateKeys(nq); err != nil {
			return errors.Wrapf(err, "key error: %+v", nq)
		}
		if err := validateForOtherReserved(nq, isGraphql); err != nil {
			return err
		}
	}
	for _, nq := range del {
		if err := validatePredName(nq.Predicate); err != nil {
			return err
		}
		var ostar bool
		if o, ok := nq.ObjectValue.GetVal().(*api.Value_DefaultVal); ok {
			ostar = o.DefaultVal == x.Star
		}
		if nq.Subject == x.Star || (nq.Predicate == x.Star && !ostar) {
			return errors.Errorf("Only valid wildcard delete patterns are 'S * *' and 'S P *': %v", nq)
		}
		if err := validateForOtherReserved(nq, isGraphql); err != nil {
			return err
		}
		// NOTE: we dont validateKeys() with delete to let users fix existing mistakes
		// with bad predicate forms. ex: foo@bar ~something
	}
	return nil
}

func validateKey(key string) error {
	switch {
	case key == "":
		return errors.Errorf("Has zero length")
	case strings.ContainsAny(key, "~@"):
		return errors.Errorf("Has invalid characters")
	case strings.IndexFunc(key, unicode.IsSpace) != -1:
		return errors.Errorf("Must not contain spaces")
	}
	return nil
}

// validateKeys checks predicate and facet keys in N-Quad for syntax errors.
func validateKeys(nq *api.NQuad) error {
	if err := validateKey(nq.Predicate); err != nil {
		return errors.Wrapf(err, "predicate %q", nq.Predicate)
	}
	for i := range nq.Facets {
		if nq.Facets[i] == nil {
			continue
		}
		if err := validateKey(nq.Facets[i].Key); err != nil {
			return errors.Errorf("Facet %q, %s", nq.Facets[i].Key, err)
		}
	}
	return nil
}

// validateQuery verifies that the query does not contain any preds that
// are longer than the limit (2^16).
func validateQuery(queries []*dql.GraphQuery) error {
	for _, q := range queries {
		if err := validatePredName(q.Attr); err != nil {
			return err
		}

		if err := validateQuery(q.Children); err != nil {
			return err
		}
	}

	return nil
}

func validatePredName(name string) error {
	if len(name) > math.MaxUint16 {
		return errors.Errorf("Predicate name length cannot be bigger than 2^16. Predicate: %v",
			name[:80])
	}
	return nil
}

// formatTypes takes a list of TypeUpdates and converts them in to a list of
// maps in a format that is human-readable to be marshaled into JSON.
func formatTypes(typeList []*pb.TypeUpdate) []map[string]interface{} {
	var res []map[string]interface{}
	for _, typ := range typeList {
		typeMap := make(map[string]interface{})
		typeMap["name"] = typ.TypeName
		fields := make([]map[string]string, len(typ.Fields))

		for i, field := range typ.Fields {
			m := make(map[string]string, 1)
			m["name"] = field.Predicate
			fields[i] = m
		}
		typeMap["fields"] = fields

		res = append(res, typeMap)
	}
	return res
}

func isDropAll(op *api.Operation) bool {
	if op.DropAll || op.DropOp == api.Operation_ALL {
		return true
	}
	return false
}

func verifyUniqueWithinMutation(qc *queryContext) error {
	if len(qc.uniqueVars) == 0 {
		return nil
	}

	for i := range qc.uniqueVars {
		gmuIndex, rdfIndex := decodeIndex(i)
		// handles cases where the mutation was pruned in updateMutations
		if int(gmuIndex) >= len(qc.gmuList) || qc.gmuList[gmuIndex] == nil || int(rdfIndex) >= len(qc.gmuList[gmuIndex].Set) {
			continue
		}
		pred1 := qc.gmuList[gmuIndex].Set[rdfIndex]
		pred1Value := dql.TypeValFrom(pred1.ObjectValue).Value
		for j := range qc.uniqueVars {
			if i == j {
				continue
			}
			gmuIndex2, rdfIndex2 := decodeIndex(j)
			// bounds check for the second predicate, which could also have been pruned
			if int(gmuIndex2) >= len(qc.gmuList) || qc.gmuList[gmuIndex2] == nil || int(rdfIndex2) >= len(qc.gmuList[gmuIndex2].Set) {
				continue
			}
			pred2 := qc.gmuList[gmuIndex2].Set[rdfIndex2]
			if pred1.Predicate == pred2.Predicate && dql.TypeValFrom(pred2.ObjectValue).Value == pred1Value &&
				pred1.Subject != pred2.Subject {
				return errors.Errorf("could not insert duplicate value [%v] for predicate [%v]",
					pred1Value, pred1.Predicate)
			}
		}
	}
	return nil
}

func isUpsertCondTrue(qc *queryContext, gmuIndex int) bool {
	condVar := qc.condVars[gmuIndex]
	if condVar == "" {
		return true
	}

	uids, ok := qc.uidRes[condVar]
	return ok && len(uids) == 1
}

func isSwap(qc *queryContext, pred1SubjectUid uint64, pred1Predicate string) (bool, error) {
	for i := range qc.uniqueVars {
		gmuIndex, rdfIndex := decodeIndex(i)
		pred2 := qc.gmuList[gmuIndex].Set[rdfIndex]
		var pred2SubjectUid uint64
		if !strings.HasPrefix(pred2.Subject, "uid(") {
			var err error
			pred2SubjectUid, err = parseSubject(pred2.Subject)
			if err != nil {
				return false, errors.Wrapf(err, "error while parsing [%v]", pred2.Subject)
			}
		} else {
			pred2SubjectUid = 0
		}

		if pred2SubjectUid == pred1SubjectUid && pred1Predicate == pred2.Predicate {
			return true, nil
		}
	}
	return false, nil
}

// encodeBit two uint32 numbers by bit.
// First 32 bits store k1 and last 32 bits store k2.
func encodeIndex(k1, k2 int) uint64 {
	safeToConvert := func(num int) {
		if num > math.MaxUint32 {
			panic("unsafe conversion: integer value is too large for uint32")
		}
	}
	safeToConvert(k1)
	safeToConvert(k2)
	return uint64(uint32(k1))<<32 | uint64(uint32(k2))
}

func decodeIndex(pair uint64) (uint32, uint32) {
	k1 := uint32(pair >> 32)
	k2 := uint32(pair) & 0xFFFFFFFF
	return k1, k2
}

func parseSubject(predSubject string) (uint64, error) {
	if strings.HasPrefix(predSubject, "_:") {
		return 0, nil // blank node
	} else {
		return dql.ParseUid(predSubject)
	}
}
