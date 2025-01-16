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
	"os"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	dgoapi "github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/graphql/schema"
	"github.com/hypermodeinc/dgraph/v24/graphql/test"
	"github.com/hypermodeinc/dgraph/v24/x"
)

// Tests that result completion and GraphQL error propagation are working properly.

// All the tests work on a mocked json response, rather than a running Dgraph.
// It's better to mock the Dgraph client interface in these tests and have cases
// where one can directly see the json response and how it gets modified, than
// to try and orchestrate conditions for all these complicated tests in a live
// Dgraph instance.  Done on a real Dgraph, you also can't see the responses
// to see what the test is actually doing.

type executor struct {
	// existenceQueriesResp stores JSON response of the existence queries in case of Add
	// or Update mutations and is returned for every third Execute call.
	// counter is used to count how many times Execute function has been called.
	existenceQueriesResp string
	counter              int
	counterLock          sync.Mutex
	resp                 string
	assigned             map[string]string
	result               map[string]interface{}

	queryTouched    uint64
	mutationTouched uint64

	// start reporting Dgraph fails at this point (0 = never fail, 1 = fail on
	// first request, 2 = succeed once and then fail on 2nd request, etc.)
	failQuery    int
	failMutation int
}

type QueryCase struct {
	Name        string
	GQLQuery    string
	Explanation string
	Response    string // Dgraph json response
	Expected    string // Expected data from Resolve()
	Errors      x.GqlErrorList
}

var testGQLSchema = `
type Author {
	id: ID!
	name: String!
	dob: DateTime
	postsRequired: [Post!]!
	postsElmntRequired: [Post!]
	postsNullable: [Post]
	postsNullableListRequired: [Post]!
}

type Product {
  id: String! @id
  description: String
  title: String
  imageUrl: String
  productVector: [Float!] @embedding
}

type Post {
	id: ID!
	title: String!
	text: String
	author: Author!
}`

func (ex *executor) Execute(ctx context.Context, req *dgoapi.Request,
	field schema.Field) (*dgoapi.Response, error) {
	// In case ex.existenceQueriesResp is non empty, its an Add or an Update mutation. In this case,
	// every third call to Execute
	// query is an existence query and existenceQueriesResp is returned.
	ex.counterLock.Lock()
	ex.counter++
	defer ex.counterLock.Unlock()
	if ex.existenceQueriesResp != "" && ex.counter%3 == 1 {
		return &dgoapi.Response{
			Json: []byte(ex.existenceQueriesResp),
		}, nil
	}
	if len(req.Mutations) == 0 {
		ex.failQuery--
		if ex.failQuery == 0 {
			return nil, schema.GQLWrapf(errors.New("_bad stuff happend_"), "Dgraph query failed")
		}

		return &dgoapi.Response{
			Json: []byte(ex.resp),
			Metrics: &dgoapi.Metrics{
				NumUids: map[string]uint64{touchedUidsKey: ex.queryTouched}},
		}, nil
	}

	ex.failMutation--
	if ex.failMutation == 0 {
		return nil, schema.GQLWrapf(errors.New("_bad stuff happend_"),
			"Dgraph mutation failed")
	}

	res, err := json.Marshal(ex.result)
	x.Panic(err)

	return &dgoapi.Response{
		Json: res,
		Uids: ex.assigned,
		Metrics: &dgoapi.Metrics{
			NumUids: map[string]uint64{touchedUidsKey: ex.mutationTouched}},
	}, nil

}

func (ex *executor) CommitOrAbort(ctx context.Context,
	tc *dgoapi.TxnContext) (*dgoapi.TxnContext, error) {
	return &dgoapi.TxnContext{}, nil
}

func complete(t *testing.T, gqlSchema schema.Schema, gqlQuery, dgResponse string) *schema.Response {
	op, err := gqlSchema.Operation(&schema.Request{Query: gqlQuery})
	require.NoError(t, err)

	resp := &schema.Response{}
	var res map[string]interface{}
	err = schema.Unmarshal([]byte(dgResponse), &res)
	if err != nil {
		// TODO(abhimanyu): check if should port the test which requires this to e2e
		resp.Errors = x.GqlErrorList{x.GqlErrorf(err.Error()).WithLocations(op.Queries()[0].Location())}
	}

	// TODO(abhimanyu): completion can really be checked only for a single query,
	// so figure out tests which have more than one query and port them
	for _, query := range op.Queries() {
		b, errs := schema.CompleteObject(query.PreAllocatePathSlice(), []schema.Field{query}, res)
		addResult(resp, &Resolved{Data: b, Field: query, Err: errs})
	}

	return resp
}

// Tests in resolver_test.yaml are about what gets into a completed result (addition
// of "null", errors and error propagation).  Exact JSON result (e.g. order) doesn't
// matter here - that makes for easier to format and read tests for these many cases.
//
// The []bytes built by Resolve() have some other properties, such as ordering of
// fields, which are tested by TestResponseOrder().
func TestGraphQLErrorPropagation(t *testing.T) {
	b, err := os.ReadFile("resolver_error_test.yaml")
	require.NoError(t, err, "Unable to read test file")

	var tests []QueryCase
	err = yaml.Unmarshal(b, &tests)
	require.NoError(t, err, "Unable to unmarshal tests to yaml.")

	gqlSchema := test.LoadSchemaFromString(t, testGQLSchema)

	for _, tcase := range tests {
		t.Run(tcase.Name, func(t *testing.T) {
			resp := complete(t, gqlSchema, tcase.GQLQuery, tcase.Response)

			if diff := cmp.Diff(tcase.Errors, resp.Errors); diff != "" {
				t.Errorf("errors mismatch (-want +got):\n%s", diff)
			}

			require.JSONEq(t, tcase.Expected, resp.Data.String(), tcase.Explanation)
		})
	}
}

// For add and update mutations, we don't need to re-test all the cases from the
// query tests.  So just test enough to demonstrate that we'll catch it if we were
// to delete the call to completeDgraphResult before adding to the response.
func TestAddMutationUsesErrorPropagation(t *testing.T) {
	t.Skipf("TODO(abhimanyu): port it to make use of completeMutationResult")
	mutation := `mutation {
		addPost(input: [{title: "A Post", text: "Some text", author: {id: "0x1"}}]) {
			post {
				title
				text
				author {
					name
					dob
				}
			}
		}
	}`

	tests := map[string]struct {
		explanation   string
		mutResponse   map[string]string
		mutQryResp    map[string]interface{}
		queryResponse string
		expected      string
		errors        x.GqlErrorList
	}{
		"Add mutation adds missing nullable fields": {
			explanation: "Field 'dob' is nullable, so null should be inserted " +
				"if the mutation's query doesn't return a value.",
			mutResponse: map[string]string{"Post1": "0x2"},
			mutQryResp: map[string]interface{}{
				"Author2": []interface{}{map[string]string{"uid": "0x1"}}},
			queryResponse: `{ "post" : [
				{ "title": "A Post",
				"text": "Some text",
				"author": { "name": "A.N. Author" } } ] }`,
			expected: `{ "addPost": { "post" :
				[{ "title": "A Post",
				"text": "Some text",
				"author": { "name": "A.N. Author", "dob": null } }] } }`,
		},
		"Add mutation triggers GraphQL error propagation": {
			explanation: "An Author's name is non-nullable, so if that's missing, " +
				"the author is squashed to null, but that's also non-nullable, so the " +
				"propagates to the query root.",
			mutResponse: map[string]string{"Post1": "0x2"},
			mutQryResp: map[string]interface{}{
				"Author2": []interface{}{map[string]string{"uid": "0x1"}}},
			queryResponse: `{ "post" : [
				{ "title": "A Post",
				"text": "Some text",
				"author": { "dob": "2000-01-01" } } ] }`,
			expected: `{ "addPost": { "post" : [null] } }`,
			errors: x.GqlErrorList{&x.GqlError{
				Message: `Non-nullable field 'name' (type String!) ` +
					`was not present in result from Dgraph.  GraphQL error propagation triggered.`,
				Locations: []x.Location{{Column: 6, Line: 7}},
				Path:      []interface{}{"addPost", "post", 0, "author", "name"}}},
		},
	}

	gqlSchema := test.LoadSchemaFromString(t, testGQLSchema)

	for name, tcase := range tests {
		t.Run(name, func(t *testing.T) {
			resp := resolveWithClient(gqlSchema, mutation, nil,
				&executor{
					existenceQueriesResp: `{ "Author_1": [{"uid":"0x1"}]}`,
					resp:                 tcase.queryResponse,
					assigned:             tcase.mutResponse,
					result:               tcase.mutQryResp,
				})

			test.RequireJSONEq(t, tcase.errors, resp.Errors)
			require.JSONEq(t, tcase.expected, resp.Data.String(), tcase.explanation)
		})
	}
}

func TestUpdateMutationUsesErrorPropagation(t *testing.T) {
	t.Skipf("TODO(abhimanyu): port it to make use of completeMutationResult")
	mutation := `mutation {
		updatePost(input: { filter: { id: ["0x1"] }, set: { text: "Some more text" } }) {
			post {
				title
				text
				author {
					name
					dob
				}
			}
		}
	}`

	// There's no need to have mocks for the mutation part here because with nil results all the
	// rewriting and rewriting from results will silently succeed.  All we care about the is the
	// result from the query that follows the mutation.  In that add case we have to satisfy
	// the type checking, but that's not required here.

	tests := map[string]struct {
		explanation   string
		mutResponse   map[string]string
		queryResponse string
		expected      string
		errors        x.GqlErrorList
	}{
		"Update Mutation adds missing nullable fields": {
			explanation: "Field 'dob' is nullable, so null should be inserted " +
				"if the mutation's query doesn't return a value.",
			queryResponse: `{ "post" : [
				{ "title": "A Post",
				"text": "Some text",
				"author": { "name": "A.N. Author" } } ] }`,
			expected: `{ "updatePost": { "post" :
				[{ "title": "A Post",
				"text": "Some text",
				"author": { "name": "A.N. Author", "dob": null } }] } }`,
		},
		"Update Mutation triggers GraphQL error propagation": {
			explanation: "An Author's name is non-nullable, so if that's missing, " +
				"the author is squashed to null, but that's also non-nullable, so the error " +
				"propagates to the query root.",
			queryResponse: `{ "post" : [ {
				"title": "A Post",
				"text": "Some text",
				"author": { "dob": "2000-01-01" } } ] }`,
			expected: `{ "updatePost": { "post" : [null] } }`,
			errors: x.GqlErrorList{&x.GqlError{
				Message: `Non-nullable field 'name' (type String!) ` +
					`was not present in result from Dgraph.  GraphQL error propagation triggered.`,
				Locations: []x.Location{{Column: 6, Line: 7}},
				Path:      []interface{}{"updatePost", "post", 0, "author", "name"}}},
		},
	}

	gqlSchema := test.LoadSchemaFromString(t, testGQLSchema)

	for name, tcase := range tests {
		t.Run(name, func(t *testing.T) {
			resp := resolveWithClient(gqlSchema, mutation, nil,
				&executor{resp: tcase.queryResponse, assigned: tcase.mutResponse})

			test.RequireJSONEq(t, tcase.errors, resp.Errors)
			require.JSONEq(t, tcase.expected, resp.Data.String(), tcase.explanation)
		})
	}
}

// TestManyMutationsWithError : Multiple mutations run serially (queries would
// run in parallel) and, in GraphQL, if an error is encountered in a request with
// multiple mutations, the mutations following the error are not run.  The mutations
// that have succeeded are permanent - i.e. not rolled back.
//
// There's no real way to test this E2E against a live instance because the only
// real fails during a mutation are either failure to communicate with Dgraph, or
// a bug that causes a query rewriting that Dgraph rejects.  There are some other
// cases: e.g. a delete that doesn't end up deleting anything (but we interpret
// that as not an error, it just deleted 0 things), and a mutation with some error
// in the input data/query (but that gets caught by validation before any mutations
// are executed).
//
// So this mocks a failing mutation and tests that we behave correctly in the case
// of multiple mutations.
func TestManyMutationsWithError(t *testing.T) {
	// add1 - should succeed
	// add2 - should fail
	// add3 - is never executed
	multiMutation := `mutation multipleMutations($id: ID!) {
			add1: addPost(input: [{title: "A Post", text: "Some text", author: {id: "0x1"}}]) {
				post { title }
			}

			add2: addPost(input: [{title: "A Post", text: "Some text", author: {id: $id}}]) {
				post { title }
			}

			add3: addPost(input: [{title: "A Post", text: "Some text", author: {id: "0x1"}}]) {
				post { title }
			}
		}`

	tests := map[string]struct {
		explanation   string
		idValue       string
		mutResponse   map[string]string
		mutQryResp    map[string]interface{}
		queryResponse string
		expected      string
		errors        x.GqlErrorList
	}{
		"Dgraph fail": {
			explanation: "a Dgraph, network or error in rewritten query failed the mutation",
			idValue:     "0x1",
			mutResponse: map[string]string{"Post_2": "0x2"},
			mutQryResp: map[string]interface{}{
				"Author1": []interface{}{map[string]string{"uid": "0x1"}}},
			queryResponse: `{"post": [{ "title": "A Post" } ] }`,
			expected: `{
				"add1": { "post": [{ "title": "A Post" }] },
				"add2" : null
			}`,
			errors: x.GqlErrorList{
				&x.GqlError{Message: `mutation addPost failed because ` +
					`Dgraph mutation failed because _bad stuff happend_`,
					Locations: []x.Location{{Line: 6, Column: 4}},
					Path:      []interface{}{"add2"}},
				&x.GqlError{Message: `Mutation add3 was not executed because of ` +
					`a previous error.`,
					Locations: []x.Location{{Line: 10, Column: 4}},
					Path:      []interface{}{"add3"}}},
		},
		"Rewriting error": {
			explanation: "The reference ID is not a uint64, so can't be converted to a uid",
			idValue:     "hi",
			mutResponse: map[string]string{"Post_2": "0x2"},
			mutQryResp: map[string]interface{}{
				"Author1": []interface{}{map[string]string{"uid": "0x1"}}},
			queryResponse: `{"post": [{ "title": "A Post" } ] }`,
			expected: `{
				"add1": { "post": [{ "title": "A Post" }] },
				"add2" : null
			}`,
			errors: x.GqlErrorList{
				&x.GqlError{Message: `couldn't rewrite mutation addPost because ` +
					`failed to rewrite mutation payload because ` +
					`ID argument (hi) was not able to be parsed`,
					Path: []interface{}{"add2"}},
				&x.GqlError{Message: `Mutation add3 was not executed because of ` +
					`a previous error.`,
					Locations: []x.Location{{Line: 10, Column: 4}},
					Path:      []interface{}{"add3"}}},
		},
	}

	gqlSchema := test.LoadSchemaFromString(t, testGQLSchema)

	for name, tcase := range tests {
		t.Run(name, func(t *testing.T) {

			resp := resolveWithClient(
				gqlSchema,
				multiMutation,
				map[string]interface{}{"id": tcase.idValue},
				&executor{
					existenceQueriesResp: `{ "Author_1": [{"uid":"0x1", "dgraph.type":["Author"]}]}`,
					resp:                 tcase.queryResponse,
					assigned:             tcase.mutResponse,
					failMutation:         2})

			if diff := cmp.Diff(tcase.errors, resp.Errors); diff != "" {
				t.Errorf("errors mismatch (-want +got):\n%s", diff)
			}
			require.JSONEq(t, tcase.expected, resp.Data.String())
		})
	}
}

func TestSubscriptionErrorWhenNoneDefined(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, testGQLSchema)
	resp := resolveWithClient(gqlSchema, `subscription { foo }`, nil, nil)
	test.RequireJSONEq(t, x.GqlErrorList{{Message: "Not resolving subscription because schema" +
		" doesn't have any fields defined for subscription operation."}}, resp.Errors)
}

func resolveWithClient(
	gqlSchema schema.Schema,
	gqlQuery string,
	vars map[string]interface{},
	ex DgraphExecutor) *schema.Response {
	resolver := New(
		gqlSchema,
		NewResolverFactory(nil, nil).WithConventionResolvers(gqlSchema, &ResolverFns{
			Qrw: NewQueryRewriter(),
			Arw: NewAddRewriter,
			Urw: NewUpdateRewriter,
			Ex:  ex,
		}))

	return resolver.Resolve(context.Background(), &schema.Request{Query: gqlQuery, Variables: vars})
}
