/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/trace"

	dgoapi "github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/edgraph"
	"github.com/hypermodeinc/dgraph/v24/graphql/api"
	"github.com/hypermodeinc/dgraph/v24/graphql/dgraph"
	"github.com/hypermodeinc/dgraph/v24/graphql/schema"
	"github.com/hypermodeinc/dgraph/v24/x"
)

type resolveCtxKey string

const (
	methodResolve = "RequestResolver.Resolve"

	resolveStartTime resolveCtxKey = "resolveStartTime"

	resolverFailed    = false
	resolverSucceeded = true

	ErrInternal = "Internal error"
)

// A ResolverFactory finds the right resolver for a query/mutation.
type ResolverFactory interface {
	queryResolverFor(query schema.Query) QueryResolver
	mutationResolverFor(mutation schema.Mutation) MutationResolver

	// WithQueryResolver adds a new query resolver.  Each time query name is resolved
	// resolver is called to create a new instance of a QueryResolver to resolve the
	// query.
	WithQueryResolver(name string, resolver func(schema.Query) QueryResolver) ResolverFactory

	// WithMutationResolver adds a new query resolver.  Each time mutation name is resolved
	// resolver is called to create a new instance of a MutationResolver to resolve the
	// mutation.
	WithMutationResolver(
		name string, resolver func(schema.Mutation) MutationResolver) ResolverFactory

	// WithConventionResolvers adds a set of our convention based resolvers to the
	// factory.  The registration happens only once.
	WithConventionResolvers(s schema.Schema, fns *ResolverFns) ResolverFactory

	// WithQueryMiddlewareConfig adds the configuration to use to apply middlewares before resolving
	// queries. The config should be a mapping of the name of query to its middlewares.
	WithQueryMiddlewareConfig(config map[string]QueryMiddlewares) ResolverFactory

	// WithMutationMiddlewareConfig adds the configuration to use to apply middlewares before
	// resolving mutations. The config should be a mapping of the name of mutation to its
	// middlewares.
	WithMutationMiddlewareConfig(config map[string]MutationMiddlewares) ResolverFactory

	// WithSchemaIntrospection adds schema introspection capabilities to the factory.
	// So __schema and __type queries can be resolved.
	WithSchemaIntrospection() ResolverFactory
}

// A ResultCompleter can take a []byte slice representing an intermediate result
// in resolving field and applies a completion step.
type ResultCompleter interface {
	Complete(ctx context.Context, resolved *Resolved)
}

// RequestResolver can process GraphQL requests and write GraphQL JSON responses.
// A schema.Request may contain any number of queries or mutations (never both).
// RequestResolver.Resolve() resolves all of them by finding the resolved answers
// of the component queries/mutations and joining into a single schema.Response.
type RequestResolver struct {
	schema    schema.Schema
	resolvers ResolverFactory
}

// A resolverFactory is the main implementation of ResolverFactory.  It stores a
// map of all the resolvers that have been registered and returns a resolver that
// just returns errors if it's asked for a resolver for a field that it doesn't
// know about.
type resolverFactory struct {
	sync.RWMutex
	queryResolvers    map[string]func(schema.Query) QueryResolver
	mutationResolvers map[string]func(schema.Mutation) MutationResolver

	queryMiddlewareConfig    map[string]QueryMiddlewares
	mutationMiddlewareConfig map[string]MutationMiddlewares

	// returned if the factory gets asked for resolver for a field that it doesn't
	// know about.
	queryError    QueryResolverFunc
	mutationError MutationResolverFunc
}

// ResolverFns is a convenience struct for passing blocks of rewriters and executors.
type ResolverFns struct {
	Qrw QueryRewriter
	Arw func() MutationRewriter
	Urw func() MutationRewriter
	Drw MutationRewriter
	Ex  DgraphExecutor
}

// dgraphExecutor is an implementation of both QueryExecutor and MutationExecutor
// that proxies query/mutation resolution through Query method in dgraph server.
type dgraphExecutor struct {
	dg *dgraph.DgraphEx
}

// adminExecutor is an implementation of both QueryExecutor and MutationExecutor
// that proxies query resolution through Query method in dgraph server, and
// it doesn't require authorization. Currently it's only used for querying
// gqlschema during init.
type adminExecutor struct {
	dg *dgraph.DgraphEx
}

// A Resolved is the result of resolving a single field - generally a query or mutation.
type Resolved struct {
	Data       []byte
	Field      schema.Field
	Err        error
	Extensions *schema.Extensions
}

// CompletionFunc is an adapter that allows us to compose completions and build a
// ResultCompleter from a function.  Based on the http.HandlerFunc pattern.
type CompletionFunc func(ctx context.Context, resolved *Resolved)

// Complete calls cf(ctx, resolved)
func (cf CompletionFunc) Complete(ctx context.Context, resolved *Resolved) {
	cf(ctx, resolved)
}

// NewDgraphExecutor builds a DgraphExecutor for proxying requests through dgraph.
func NewDgraphExecutor() DgraphExecutor {
	return newDgraphExecutor(&dgraph.DgraphEx{})
}

func newDgraphExecutor(dg *dgraph.DgraphEx) DgraphExecutor {
	return &dgraphExecutor{dg: dg}
}

// NewAdminExecutor builds a DgraphExecutor for proxying requests through dgraph.
func NewAdminExecutor() DgraphExecutor {
	return &adminExecutor{dg: &dgraph.DgraphEx{}}
}

func (aex *adminExecutor) Execute(ctx context.Context, req *dgoapi.Request, field schema.Field) (
	*dgoapi.Response, error) {
	ctx = context.WithValue(ctx, edgraph.Authorize, false)
	return aex.dg.Execute(ctx, req, field)
}

func (aex *adminExecutor) CommitOrAbort(ctx context.Context,
	tc *dgoapi.TxnContext) (*dgoapi.TxnContext, error) {
	return aex.dg.CommitOrAbort(ctx, tc)
}

func (de *dgraphExecutor) Execute(ctx context.Context, req *dgoapi.Request, field schema.Field) (
	*dgoapi.Response, error) {
	return de.dg.Execute(ctx, req, field)
}

func (de *dgraphExecutor) CommitOrAbort(ctx context.Context,
	tc *dgoapi.TxnContext) (*dgoapi.TxnContext, error) {
	return de.dg.CommitOrAbort(ctx, tc)
}

func (rf *resolverFactory) WithQueryResolver(
	name string, resolver func(schema.Query) QueryResolver) ResolverFactory {
	rf.Lock()
	defer rf.Unlock()
	rf.queryResolvers[name] = resolver
	return rf
}

func (rf *resolverFactory) WithMutationResolver(
	name string, resolver func(schema.Mutation) MutationResolver) ResolverFactory {
	rf.Lock()
	defer rf.Unlock()
	rf.mutationResolvers[name] = resolver
	return rf
}

func (rf *resolverFactory) WithSchemaIntrospection() ResolverFactory {
	return rf.
		WithQueryResolver("__schema",
			func(q schema.Query) QueryResolver {
				return QueryResolverFunc(resolveIntrospection)
			}).
		WithQueryResolver("__type",
			func(q schema.Query) QueryResolver {
				return QueryResolverFunc(resolveIntrospection)
			}).
		WithQueryResolver("__typename",
			func(q schema.Query) QueryResolver {
				return QueryResolverFunc(resolveIntrospection)
			}).
		WithMutationResolver("__typename",
			func(m schema.Mutation) MutationResolver {
				return MutationResolverFunc(func(ctx context.Context, m schema.Mutation) (*Resolved, bool) {
					return DataResult(m, map[string]interface{}{"__typename": "Mutation"}, nil),
						resolverSucceeded
				})
			})
}

func (rf *resolverFactory) WithConventionResolvers(
	s schema.Schema, fns *ResolverFns) ResolverFactory {

	queries := append(s.Queries(schema.GetQuery), s.Queries(schema.FilterQuery)...)
	queries = append(queries, s.Queries(schema.SimilarByIdQuery)...)
	queries = append(queries, s.Queries(schema.SimilarByEmbeddingQuery)...)
	queries = append(queries, s.Queries(schema.PasswordQuery)...)
	queries = append(queries, s.Queries(schema.AggregateQuery)...)
	for _, q := range queries {
		rf.WithQueryResolver(q, func(q schema.Query) QueryResolver {
			return NewQueryResolver(fns.Qrw, fns.Ex)
		})
	}

	for _, q := range s.Queries(schema.EntitiesQuery) {
		rf.WithQueryResolver(q, func(q schema.Query) QueryResolver {
			return NewEntitiesQueryResolver(fns.Qrw, fns.Ex)
		})
	}

	for _, q := range s.Queries(schema.HTTPQuery) {
		rf.WithQueryResolver(q, func(q schema.Query) QueryResolver {
			return NewHTTPQueryResolver(nil)
		})
	}

	for _, q := range s.Queries(schema.DQLQuery) {
		rf.WithQueryResolver(q, func(q schema.Query) QueryResolver {
			// DQL queries don't need any QueryRewriter
			return NewCustomDQLQueryResolver(fns.Ex)
		})
	}

	for _, m := range s.Mutations(schema.AddMutation) {
		rf.WithMutationResolver(m, func(m schema.Mutation) MutationResolver {
			return NewDgraphResolver(fns.Arw(), fns.Ex)
		})
	}

	for _, m := range s.Mutations(schema.UpdateMutation) {
		rf.WithMutationResolver(m, func(m schema.Mutation) MutationResolver {
			return NewDgraphResolver(fns.Urw(), fns.Ex)
		})
	}

	for _, m := range s.Mutations(schema.DeleteMutation) {
		rf.WithMutationResolver(m, func(m schema.Mutation) MutationResolver {
			return NewDgraphResolver(fns.Drw, fns.Ex)
		})
	}

	for _, m := range s.Mutations(schema.HTTPMutation) {
		rf.WithMutationResolver(m, func(m schema.Mutation) MutationResolver {
			return NewHTTPMutationResolver(nil)
		})
	}

	return rf
}

func (rf *resolverFactory) WithQueryMiddlewareConfig(
	config map[string]QueryMiddlewares) ResolverFactory {
	if len(config) != 0 {
		rf.queryMiddlewareConfig = config
	}
	return rf
}

func (rf *resolverFactory) WithMutationMiddlewareConfig(
	config map[string]MutationMiddlewares) ResolverFactory {
	if len(config) != 0 {
		rf.mutationMiddlewareConfig = config
	}
	return rf
}

// NewResolverFactory returns a ResolverFactory that resolves requests via
// query/mutation rewriting and execution through Dgraph.  If the factory gets asked
// to resolve a query/mutation it doesn't know how to rewrite, it uses
// the queryError/mutationError to build an error result.
func NewResolverFactory(
	queryError QueryResolverFunc, mutationError MutationResolverFunc) ResolverFactory {

	return &resolverFactory{
		queryResolvers:    make(map[string]func(schema.Query) QueryResolver),
		mutationResolvers: make(map[string]func(schema.Mutation) MutationResolver),

		queryMiddlewareConfig:    make(map[string]QueryMiddlewares),
		mutationMiddlewareConfig: make(map[string]MutationMiddlewares),

		queryError:    queryError,
		mutationError: mutationError,
	}
}

// entitiesQueryCompletion transform the result of the `_entities` query.
// It changes the order of the result to the order of keyField in the
// `_representations` argument.
func entitiesQueryCompletion(ctx context.Context, resolved *Resolved) {
	// return if Data is not present
	if len(resolved.Data) == 0 {
		return
	}
	query, ok := resolved.Field.(schema.Query)
	if !ok {
		// this function shouldn't be called for anything other than a query
		return
	}

	var data map[string][]interface{}
	err := schema.Unmarshal(resolved.Data, &data)
	if err != nil {
		resolved.Err = schema.AppendGQLErrs(resolved.Err, err)
		return
	}

	// fetch the keyFieldValueList from the query arguments.
	repr, err := query.RepresentationsArg()
	if err != nil {
		resolved.Err = schema.AppendGQLErrs(resolved.Err, err)
		return
	}
	keyFieldType := repr.KeyField.Type().Name()

	// store the index of the keyField Values present in the argument in a map.
	// key in the map is of type interface because there are multiple types like String,
	// Int, Int64 allowed as @id. There could be duplicate keys in the representations
	// so the value of map is a list of integers containing all the indices for a key.
	indexMap := make(map[interface{}][]int)
	uniqueKeyList := make([]interface{}, 0)
	for i, key := range repr.KeyVals {
		indexMap[key] = append(indexMap[key], i)
	}

	// Create a list containing unique keys and then sort in ascending order because this
	// will be the order in which the data is received.
	// for eg: for keys: {1, 2, 4, 1, 3} is converted into {1, 2, 4, 3} and then {1, 2, 3, 4}
	// this will be the order of received data from the dgraph.
	for k := range indexMap {
		uniqueKeyList = append(uniqueKeyList, k)
	}
	sort.Slice(uniqueKeyList, func(i, j int) bool {
		switch val := uniqueKeyList[i].(type) {
		case string:
			return val < uniqueKeyList[j].(string)
		case json.Number:
			switch keyFieldType {
			case "Int", "Int64":
				val1, _ := val.Int64()
				val2, _ := uniqueKeyList[j].(json.Number).Int64()
				return val1 < val2
			case "Float":
				val1, _ := val.Float64()
				val2, _ := uniqueKeyList[j].(json.Number).Float64()
				return val1 < val2
			}
		case int64:
			return val < uniqueKeyList[j].(int64)
		case float64:
			return val < uniqueKeyList[j].(float64)
		}
		return false
	})

	// create the new output according to the index of the keyFields present in the argument.
	entitiesQryResp := data["_entities"]

	// if `entitiesQueryResp` contains less number of elements than the number of unique keys
	// which is because the object related to certain key is not present in the dgraph.
	// This will end into an error at the Gateway, so no need to order the result here.
	if len(entitiesQryResp) < len(uniqueKeyList) {
		return
	}

	// Reorder the output response according to the order of the keys in the representations argument.
	output := make([]interface{}, len(repr.KeyVals))
	for i, key := range uniqueKeyList {
		for _, idx := range indexMap[key] {
			output[idx] = entitiesQryResp[i]
		}
	}

	// replace the result obtained from the dgraph and marshal back.
	data["_entities"] = output
	resolved.Data, err = json.Marshal(data)
	if err != nil {
		resolved.Err = schema.AppendGQLErrs(resolved.Err, err)
	}

}

// noopCompletion just passes back it's result and err arguments
func noopCompletion(ctx context.Context, resolved *Resolved) {}

func (rf *resolverFactory) queryResolverFor(query schema.Query) QueryResolver {
	rf.RLock()
	defer rf.RUnlock()
	mws := rf.queryMiddlewareConfig[query.Name()]
	if resolver, ok := rf.queryResolvers[query.Name()]; ok {
		return mws.Then(resolver(query))
	}
	return rf.queryError
}

func (rf *resolverFactory) mutationResolverFor(mutation schema.Mutation) MutationResolver {
	rf.RLock()
	defer rf.RUnlock()
	mws := rf.mutationMiddlewareConfig[mutation.Name()]
	if resolver, ok := rf.mutationResolvers[mutation.Name()]; ok {
		return mws.Then(resolver(mutation))
	}
	return rf.mutationError
}

// New creates a new RequestResolver.
func New(s schema.Schema, resolverFactory ResolverFactory) *RequestResolver {
	return &RequestResolver{
		schema:    s,
		resolvers: resolverFactory,
	}
}

// Resolve processes r.GqlReq and returns a GraphQL response.
// r.GqlReq should be set with a request before Resolve is called
// and a schema and backend Dgraph should have been added.
// Resolve records any errors in the response's error field.
func (r *RequestResolver) Resolve(ctx context.Context, gqlReq *schema.Request) (resp *schema.Response) {
	span := trace.SpanFromContext(ctx)
	stop := x.SpanTimer(span, methodResolve)
	defer stop()

	if r == nil {
		glog.Errorf("Call to Resolve with nil RequestResolver")
		return schema.ErrorResponse(errors.New(ErrInternal))
	}

	if r.schema == nil {
		glog.Errorf("Call to Resolve with no schema")
		return schema.ErrorResponse(errors.New(ErrInternal))
	}

	startTime := time.Now()
	resp = &schema.Response{
		Extensions: &schema.Extensions{
			Tracing: &schema.Trace{
				Version:   1,
				StartTime: startTime.Format(time.RFC3339Nano),
			},
		},
	}
	// Panic Handler for mutation. This ensures that the mutation which causes panic
	// gets logged in Alpha logs. This panic handler overrides the default Panic Handler
	// used in recoveryHandler in admin/http.go
	defer api.PanicHandler(
		func(err error) {
			resp.Errors = schema.AsGQLErrors(schema.AppendGQLErrs(resp.Errors, err))
		}, gqlReq.Query)

	defer func() {
		endTime := time.Now()
		resp.Extensions.Tracing.EndTime = endTime.Format(time.RFC3339Nano)
		resp.Extensions.Tracing.Duration = endTime.Sub(startTime).Nanoseconds()
	}()
	ctx = context.WithValue(ctx, resolveStartTime, startTime)

	// Pass in GraphQL @auth information
	ctx, err := r.schema.Meta().AuthMeta().AttachAuthorizationJwt(ctx, gqlReq.Header)
	if err != nil {
		resp.Errors = schema.AsGQLErrors(err)
		return
	}

	ctx = x.AttachJWTNamespace(ctx)
	op, err := r.schema.Operation(gqlReq)
	if err != nil {
		resp.Errors = schema.AsGQLErrors(err)
		return
	}

	if glog.V(3) {
		// don't log the introspection queries they are sent too frequently
		// by GraphQL dev tools
		if !op.IsQuery() ||
			(op.IsQuery() && !strings.HasPrefix(op.Queries()[0].Name(), "__")) {
			b, err := json.Marshal(gqlReq.Variables)
			if err != nil {
				glog.Infof("Failed to marshal variables for logging : %s", err)
			}
			glog.Infof("Resolving GQL request: \n%s\nWith Variables: \n%s\n",
				gqlReq.Query, string(b))
		}
	}

	// resolveQueries will resolve user's queries.
	resolveQueries := func() {
		// Queries run in parallel and are independent of each other: e.g.
		// an error in one query, doesn't affect the others.

		var wg sync.WaitGroup
		allResolved := make([]*Resolved, len(op.Queries()))

		for i, q := range op.Queries() {
			wg.Add(1)

			go func(q schema.Query, storeAt int) {
				defer wg.Done()
				defer api.PanicHandler(
					func(err error) {
						allResolved[storeAt] = &Resolved{
							Data:  nil,
							Field: q,
							Err:   err,
						}
					}, gqlReq.Query)
				allResolved[storeAt] = r.resolvers.queryResolverFor(q).Resolve(ctx, q)
			}(q, i)
		}
		wg.Wait()

		// The GraphQL data response needs to be written in the same order as the
		// queries in the request.
		for _, res := range allResolved {
			// Errors and data in the same response is valid.  Both WithError and
			// AddData handle nil cases.
			addResult(resp, res)

		}
	}
	// A single request can contain either queries or mutations - not both.
	// GraphQL validation on the request would have caught that error case
	// before we get here.  At this point, we know it's valid, it's passed
	// GraphQL validation and any additional validation we've added.  So here,
	// we can just execute it.
	switch {
	case op.IsQuery():
		if op.CacheControl() != "" {
			resp.Header = make(map[string][]string)
			resp.Header.Set(schema.CacheControlHeader, op.CacheControl())
			resp.Header.Set("Vary", "Accept-Encoding")
		}
		resolveQueries()
	case op.IsMutation():
		// A mutation operation can contain any number of mutation fields.  Those should be executed
		// serially.
		// (spec https://graphql.github.io/graphql-spec/June2018/#sec-Normal-and-Serial-Execution)
		//
		// The spec is ambiguous about what to do in the case of errors during that serial execution
		// - apparently deliberately so; see this comment from Lee Byron:
		// https://github.com/graphql/graphql-spec/issues/277#issuecomment-385588590
		// and clarification
		// https://github.com/graphql/graphql-spec/pull/438
		//
		// A reasonable interpretation of that is to stop a list of mutations after the first error -
		// which seems like the natural semantics and is what we enforce here.
		allSuccessful := true

		for _, m := range op.Mutations() {
			if !allSuccessful {
				resp.WithError(x.GqlErrorf(
					"Mutation %s was not executed because of a previous error.",
					m.ResponseName()).
					WithLocations(m.Location()).
					WithPath([]interface{}{m.ResponseName()}))

				continue
			}

			var res *Resolved
			res, allSuccessful = r.resolvers.mutationResolverFor(m).Resolve(ctx, m)
			addResult(resp, res)
		}
	case op.IsSubscription():
		resolveQueries()
	}

	return resp
}

// ValidateSubscription will check the given subscription query is valid or not.
func (r *RequestResolver) ValidateSubscription(req *schema.Request) error {
	op, err := r.schema.Operation(req)
	if err != nil {
		return err
	}

	if !op.IsSubscription() {
		return errors.New("given GraphQL operation is not a subscription")
	}

	for _, q := range op.Queries() {
		for _, field := range q.SelectionSet() {
			if err := validateCustomFieldsRecursively(field); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *RequestResolver) Schema() schema.Schema {
	return r.schema
}

// validateCustomFieldsRecursively will return err if the given field is custom or any of its
// children is type of a custom field.
func validateCustomFieldsRecursively(field schema.Field) error {
	if field.IsCustomHTTP() {
		return x.GqlErrorf("Custom field `%s` is not supported in graphql subscription",
			field.Name()).WithLocations(field.Location())
	}
	for _, f := range field.SelectionSet() {
		err := validateCustomFieldsRecursively(f)
		if err != nil {
			return err
		}
	}
	return nil
}

func addResult(resp *schema.Response, res *Resolved) {
	// Errors should report the "path" into the result where the error was found.
	//
	// The definition of a path in a GraphQL error is here:
	// https://graphql.github.io/graphql-spec/June2018/#sec-Errors
	// For a query like (assuming field f is of a list type and g is a scalar type):
	// - q { f { g } }
	// a path to the 3rd item in the f list would look like:
	// - [ "q", "f", 2, "g" ]
	if res.Data == nil && !res.Field.Type().Nullable() {
		// According to GraphQL spec, out of all the queries in the request, if any one query
		// returns null but expected return type is non-nullable then we set root data to null.
		resp.SetDataNull()
	} else {
		resp.AddData(res.Data)
	}

	resp.WithError(res.Err)
	resp.MergeExtensions(res.Extensions)
}

// a httpResolver can resolve a single GraphQL field from an HTTP endpoint
type httpResolver struct {
	*http.Client
}

type httpQueryResolver httpResolver
type httpMutationResolver httpResolver

// NewHTTPQueryResolver creates a resolver that can resolve GraphQL query from an HTTP endpoint
func NewHTTPQueryResolver(hc *http.Client) QueryResolver {
	return &httpQueryResolver{hc}
}

// NewHTTPMutationResolver creates a resolver that resolves GraphQL mutation from an HTTP endpoint
func NewHTTPMutationResolver(hc *http.Client) MutationResolver {
	return &httpMutationResolver{hc}
}

func (hr *httpResolver) Resolve(ctx context.Context, field schema.Field) *Resolved {
	span := trace.SpanFromContext(ctx)
	stop := x.SpanTimer(span, "resolveHTTP")
	defer stop()

	resolved := hr.rewriteAndExecute(ctx, field)
	return resolved
}

func (hr *httpResolver) rewriteAndExecute(ctx context.Context, field schema.Field) *Resolved {
	hrc, err := field.CustomHTTPConfig()
	if err != nil {
		return EmptyResult(field, err)
	}

	// If this is a lambda field, it will always have a body template.
	// Just convert that into a lambda template.
	if field.HasLambdaDirective() {
		hrc.Template = schema.GetBodyForLambda(ctx, field, nil, hrc.Template)
	}

	fieldData, errs, hardErrs := hrc.MakeAndDecodeHTTPRequest(hr.Client, hrc.URL, hrc.Template,
		field)
	if hardErrs != nil {
		// Not using EmptyResult() here as we don't want to wrap the errors returned from remote
		// endpoints
		return &Resolved{
			Data:  field.NullResponse(),
			Field: field,
			Err:   hardErrs,
		}
	}

	return DataResult(field, map[string]interface{}{field.Name(): fieldData}, errs)
}

func (h *httpQueryResolver) Resolve(ctx context.Context, query schema.Query) *Resolved {
	return (*httpResolver)(h).Resolve(ctx, query)
}

func (h *httpMutationResolver) Resolve(ctx context.Context, mutation schema.Mutation) (*Resolved,
	bool) {
	resolved := (*httpResolver)(h).Resolve(ctx, mutation)
	return resolved, resolved.Err == nil || resolved.Err.Error() == ""
}

func EmptyResult(f schema.Field, err error) *Resolved {
	return &Resolved{
		Data:  f.NullResponse(),
		Field: f,
		Err:   schema.GQLWrapLocationf(err, f.Location(), "resolving %s failed", f.Name()),
	}
}

func DataResult(f schema.Field, data map[string]interface{}, err error) *Resolved {
	b, errs := schema.CompleteObject(f.PreAllocatePathSlice(), []schema.Field{f}, data)

	return &Resolved{
		Data:  b,
		Field: f,
		Err:   schema.AppendGQLErrs(err, errs),
	}
}

func newtimer(ctx context.Context, Duration *schema.OffsetDuration) schema.OffsetTimer {
	resolveStartTime, _ := ctx.Value(resolveStartTime).(time.Time)
	tf := schema.NewOffsetTimerFactory(resolveStartTime)
	return tf.NewOffsetTimer(Duration)
}
