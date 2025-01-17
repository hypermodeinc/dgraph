/*
 * Copyright 2025 Hypermode Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/golang/glog"
	otrace "go.opencensus.io/trace"

	dgoapi "github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/dql"
	"github.com/hypermodeinc/dgraph/v24/graphql/dgraph"
	"github.com/hypermodeinc/dgraph/v24/graphql/schema"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var errNotScalar = errors.New("provided value is not a scalar, can't convert it to string")

// A QueryResolver can resolve a single query.
type QueryResolver interface {
	Resolve(ctx context.Context, query schema.Query) *Resolved
}

// A QueryRewriter can build a Dgraph dql.GraphQuery from a GraphQL query,
type QueryRewriter interface {
	Rewrite(ctx context.Context, q schema.Query) ([]*dql.GraphQuery, error)
}

// QueryResolverFunc is an adapter that allows to build a QueryResolver from
// a function.  Based on the http.HandlerFunc pattern.
type QueryResolverFunc func(ctx context.Context, query schema.Query) *Resolved

// Resolve calls qr(ctx, query)
func (qr QueryResolverFunc) Resolve(ctx context.Context, query schema.Query) *Resolved {
	return qr(ctx, query)
}

// NewQueryResolver creates a new query resolver.  The resolver runs the pipeline:
// 1) rewrite the query using qr (return error if failed)
// 2) execute the rewritten query with ex (return error if failed)
// 3) process the result with rc
func NewQueryResolver(qr QueryRewriter, ex DgraphExecutor) QueryResolver {
	return &queryResolver{queryRewriter: qr, executor: ex, resultCompleter: CompletionFunc(noopCompletion)}
}

// NewEntitiesQueryResolver creates a new query resolver for `_entities` query.
// It is introduced because result completion works little different for `_entities` query.
func NewEntitiesQueryResolver(qr QueryRewriter, ex DgraphExecutor) QueryResolver {
	return &queryResolver{queryRewriter: qr, executor: ex, resultCompleter: CompletionFunc(entitiesQueryCompletion)}
}

// a queryResolver can resolve a single GraphQL query field.
type queryResolver struct {
	queryRewriter   QueryRewriter
	executor        DgraphExecutor
	resultCompleter ResultCompleter
}

func (qr *queryResolver) Resolve(ctx context.Context, query schema.Query) *Resolved {
	span := otrace.FromContext(ctx)
	stop := x.SpanTimer(span, "resolveQuery")
	defer stop()

	resolverTrace := &schema.ResolverTrace{
		Path:       []interface{}{query.ResponseName()},
		ParentType: "Query",
		FieldName:  query.ResponseName(),
		ReturnType: query.Type().String(),
	}
	timer := newtimer(ctx, &resolverTrace.OffsetDuration)
	timer.Start()
	defer timer.Stop()

	resolved := qr.rewriteAndExecute(ctx, query)
	qr.resultCompleter.Complete(ctx, resolved)
	resolverTrace.Dgraph = resolved.Extensions.Tracing.Execution.Resolvers[0].Dgraph
	resolved.Extensions.Tracing.Execution.Resolvers[0] = resolverTrace
	return resolved
}

func (qr *queryResolver) rewriteAndExecute(ctx context.Context, query schema.Query) *Resolved {
	dgraphQueryDuration := &schema.LabeledOffsetDuration{Label: "query"}
	ext := &schema.Extensions{
		Tracing: &schema.Trace{
			Execution: &schema.ExecutionTrace{
				Resolvers: []*schema.ResolverTrace{
					{Dgraph: []*schema.LabeledOffsetDuration{dgraphQueryDuration}},
				},
			},
		},
	}

	emptyResult := func(err error) *Resolved {
		return &Resolved{
			// all the auto-generated queries are nullable, but users may define queries with
			// @custom(dql: ...) which may be non-nullable. So, we need to set the Data field
			// only if the query was nullable and keep it nil if it was non-nullable.
			// query.NullResponse() method handles that.
			Data:       query.NullResponse(),
			Field:      query,
			Err:        schema.SetPathIfEmpty(err, query.ResponseName()),
			Extensions: ext,
		}
	}

	dgQuery, err := qr.queryRewriter.Rewrite(ctx, query)
	if err != nil {
		return emptyResult(schema.GQLWrapf(err, "couldn't rewrite query %s",
			query.ResponseName()))
	}
	qry := dgraph.AsString(dgQuery)

	queryTimer := newtimer(ctx, &dgraphQueryDuration.OffsetDuration)
	queryTimer.Start()
	resp, err := qr.executor.Execute(ctx, &dgoapi.Request{Query: qry, ReadOnly: true}, query)
	queryTimer.Stop()

	if err != nil && !x.IsGqlErrorList(err) {
		err = schema.GQLWrapf(err, "Dgraph query failed")
		glog.Infof("Dgraph query execution failed : %s", err)
	}

	ext.TouchedUids = resp.GetMetrics().GetNumUids()[touchedUidsKey]
	if x.Config.GraphQL.GetBool("debug") {
		ext.DQLQuery = qry
	}
	resolved := &Resolved{
		Data:       resp.GetJson(),
		Field:      query,
		Err:        schema.SetPathIfEmpty(err, query.ResponseName()),
		Extensions: ext,
	}

	return resolved
}

func NewCustomDQLQueryResolver(ex DgraphExecutor) QueryResolver {
	return &customDQLQueryResolver{executor: ex}
}

type customDQLQueryResolver struct {
	executor DgraphExecutor
}

func (qr *customDQLQueryResolver) Resolve(ctx context.Context, query schema.Query) *Resolved {
	span := otrace.FromContext(ctx)
	stop := x.SpanTimer(span, "resolveCustomDQLQuery")
	defer stop()

	resolverTrace := &schema.ResolverTrace{
		Path:       []interface{}{query.ResponseName()},
		ParentType: "Query",
		FieldName:  query.ResponseName(),
		ReturnType: query.Type().String(),
	}
	timer := newtimer(ctx, &resolverTrace.OffsetDuration)
	timer.Start()
	defer timer.Stop()

	resolved := qr.rewriteAndExecute(ctx, query)
	resolverTrace.Dgraph = resolved.Extensions.Tracing.Execution.Resolvers[0].Dgraph
	resolved.Extensions.Tracing.Execution.Resolvers[0] = resolverTrace
	return resolved
}

func (qr *customDQLQueryResolver) rewriteAndExecute(ctx context.Context,
	query schema.Query) *Resolved {
	dgraphQueryDuration := &schema.LabeledOffsetDuration{Label: "query"}
	ext := &schema.Extensions{
		Tracing: &schema.Trace{
			Execution: &schema.ExecutionTrace{
				Resolvers: []*schema.ResolverTrace{
					{Dgraph: []*schema.LabeledOffsetDuration{dgraphQueryDuration}},
				},
			},
		},
	}

	emptyResult := func(err error) *Resolved {
		resolved := EmptyResult(query, err)
		resolved.Extensions = ext
		return resolved
	}

	dgQuery := query.DQLQuery()
	args := query.Arguments()
	vars := make(map[string]string)
	for k, v := range args {
		// dgoapi.Request{}.Vars accepts only string values for variables,
		// so need to convert all variable values to string
		vStr, err := convertScalarToString(v)
		if err != nil {
			return emptyResult(schema.GQLWrapf(err, "couldn't convert argument %s to string", k))
		}
		// the keys in dgoapi.Request{}.Vars are assumed to be prefixed with $
		vars["$"+k] = vStr
	}

	queryTimer := newtimer(ctx, &dgraphQueryDuration.OffsetDuration)
	queryTimer.Start()
	resp, err := qr.executor.Execute(ctx, &dgoapi.Request{Query: dgQuery, Vars: vars,
		ReadOnly: true}, nil)
	queryTimer.Stop()

	if err != nil {
		return emptyResult(schema.GQLWrapf(err, "Dgraph query failed"))
	}
	ext.TouchedUids = resp.GetMetrics().GetNumUids()[touchedUidsKey]

	var respJson map[string]interface{}
	if err = schema.Unmarshal(resp.Json, &respJson); err != nil {
		return emptyResult(schema.GQLWrapf(err, "couldn't unmarshal Dgraph result"))
	}

	resolved := DataResult(query, respJson, nil)
	resolved.Extensions = ext
	return resolved
}

func resolveIntrospection(ctx context.Context, q schema.Query) *Resolved {
	data, err := schema.Introspect(q)
	return &Resolved{
		Data:  data,
		Field: q,
		Err:   err,
	}
}

// converts scalar values received from GraphQL arguments to go string
// If it is a scalar only possible cases are: string, bool, int64, float64 and nil.
func convertScalarToString(val interface{}) (string, error) {
	var str string
	switch v := val.(type) {
	case string:
		str = v
	case bool:
		str = strconv.FormatBool(v)
	case int64:
		str = strconv.FormatInt(v, 10)
	case float64:
		str = strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		str = v.String()
	case nil:
		str = ""
	default:
		return "", errNotScalar
	}
	return str, nil
}
