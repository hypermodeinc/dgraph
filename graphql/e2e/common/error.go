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

package common

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"gopkg.in/yaml.v3"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	admin2 "github.com/hypermodeinc/dgraph/v24/graphql/admin"
	"github.com/hypermodeinc/dgraph/v24/graphql/resolve"
	"github.com/hypermodeinc/dgraph/v24/graphql/schema"
	"github.com/hypermodeinc/dgraph/v24/graphql/test"
	"github.com/hypermodeinc/dgraph/v24/x"
)

const (
	panicMsg = "\n****\nthis test should trap this panic.\n" +
		"It's working as expected if this message is logged with a stack trace\n****"
)

type ErrorCase struct {
	Name         string
	GQLRequest   string
	GQLVariables string
	Errors       x.GqlErrorList
}

func graphQLCompletionOn(t *testing.T) {
	newCountry := addCountry(t, postExecutor)

	// delete the country's name.
	// The schema states type Country `{ ... name: String! ... }`
	// so a query error will be raised if we ask for the country's name in a
	// query.  Don't think a GraphQL update can do this ATM, so do through Dgraph.
	d, err := grpc.Dial(Alpha1gRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	client := dgo.NewDgraphClient(api.NewDgraphClient(d))
	mu := &api.Mutation{
		CommitNow: true,
		DelNquads: []byte(fmt.Sprintf("<%s> <Country.name> * .", newCountry.ID)),
	}
	_, err = client.NewTxn().Mutate(context.Background(), mu)
	require.NoError(t, err)

	tests := [2]string{"name", "id name"}
	for _, test := range tests {
		t.Run(test, func(t *testing.T) {
			queryCountry := &GraphQLParams{
				Query: fmt.Sprintf(`query {queryCountry {%s}}`, test),
			}

			// Check that the error is valid
			gqlResponse := queryCountry.ExecuteAsPost(t, GraphqlURL)
			require.NotNil(t, gqlResponse.Errors)
			require.Equal(t, 1, len(gqlResponse.Errors))
			require.Contains(t, gqlResponse.Errors[0].Error(),
				"Non-nullable field 'name' (type String!) was not present"+
					" in result from Dgraph.")

			// Check that the result is valid
			var result, expected struct {
				QueryCountry []*country
			}
			err := json.Unmarshal([]byte(gqlResponse.Data), &result)
			require.NoError(t, err)
			require.Equal(t, 5, len(result.QueryCountry))
			expected.QueryCountry = []*country{
				{Name: "Angola"},
				{Name: "Bangladesh"},
				{Name: "India"},
				{Name: "Mozambique"},
				nil,
			}

			sort.Slice(result.QueryCountry, func(i, j int) bool {
				if result.QueryCountry[i] == nil {
					return false
				}
				return result.QueryCountry[i].Name < result.QueryCountry[j].Name
			})

			for i := range 4 {
				require.NotNil(t, result.QueryCountry[i])
				require.Equal(t, result.QueryCountry[i].Name, expected.QueryCountry[i].Name)
			}
			require.Nil(t, result.QueryCountry[4])
		})
	}

	cleanUp(t,
		[]*country{newCountry},
		[]*author{},
		[]*post{},
	)
}

func deepMutationErrors(t *testing.T) {
	executeRequest := postExecutor

	newCountry := addCountry(t, postExecutor)

	tcases := map[string]struct {
		set *country
		exp string
	}{
		"missing ID and XID": {
			set: &country{States: []*state{{Name: "NOT A VALID STATE"}}},
			exp: "couldn't rewrite mutation updateCountry because failed to rewrite" +
				" mutation payload because field xcode cannot be empty",
		},
		"ID not valid": {
			set: &country{States: []*state{{ID: "HI"}}},
			exp: "couldn't rewrite mutation updateCountry because failed to rewrite " +
				"mutation payload because ID argument (HI) was not able to be parsed",
		},
		"ID not found": {
			set: &country{States: []*state{{ID: "0x1"}}},
			exp: "couldn't rewrite mutation updateCountry because failed to rewrite mutation" +
				" payload because ID \"0x1\" isn't a State",
		},
		"XID not found": {
			set: &country{States: []*state{{Code: "NOT A VALID CODE"}}},
			exp: "couldn't rewrite mutation updateCountry because failed to rewrite mutation" +
				" payload because type State requires a value for field name, but no value" +
				" present",
		},
	}

	for name, tcase := range tcases {
		t.Run(name, func(t *testing.T) {
			updateCountryParams := &GraphQLParams{
				Query: `mutation updateCountry($id: ID!, $set: CountryPatch!) {
					updateCountry(input: {filter: {id: [$id]}, set: $set}) {
						country { id }
					}
				}`,
				Variables: map[string]interface{}{
					"id":  newCountry.ID,
					"set": tcase.set,
				},
			}

			gqlResponse := executeRequest(t, GraphqlURL, updateCountryParams)
			require.NotNil(t, gqlResponse.Errors)
			require.Equal(t, 1, len(gqlResponse.Errors))
			require.EqualError(t, gqlResponse.Errors[0], tcase.exp)
		})
	}

	cleanUp(t, []*country{newCountry}, []*author{}, []*post{})
}

// requestValidationErrors just makes sure we are catching validation failures.
// Mostly this is provided by an external lib, so just checking we hit common cases.
func requestValidationErrors(t *testing.T) {
	b, err := os.ReadFile("../common/error_test.yaml")
	require.NoError(t, err, "Unable to read test file")

	var tests []ErrorCase
	err = yaml.Unmarshal(b, &tests)
	require.NoError(t, err, "Unable to unmarshal test cases from yaml.")

	for _, tcase := range tests {
		t.Run(tcase.Name, func(t *testing.T) {
			// -- Arrange --
			var vars map[string]interface{}
			if tcase.GQLVariables != "" {
				d := json.NewDecoder(strings.NewReader(tcase.GQLVariables))
				d.UseNumber()
				err := d.Decode(&vars)
				require.NoError(t, err)
			}
			test := &GraphQLParams{
				Query:     tcase.GQLRequest,
				Variables: vars,
			}
			gqlResponse := test.ExecuteAsPost(t, GraphqlURL)
			require.Nil(t, gqlResponse.Data)
			if diff := cmp.Diff(tcase.Errors, gqlResponse.Errors); diff != "" {
				t.Errorf("errors mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// notGeneratedAPIErrors check that the mutations and queries explicitly asked to be not
// generated using the generate directive are indeed not generated
func notGeneratedAPIErrors(t *testing.T) {
	// Add and update university
	universityID := addUniversity(t)
	updateUniversity(t, universityID)

	// Try querying, should throw error as query API is not generated
	query := `
		query {
			queryUniversity {
				name
			}
		}`
	params := &GraphQLParams{Query: query}
	gqlResponse := params.ExecuteAsPost(t, GraphqlURL)
	require.NotNil(t, gqlResponse.Errors)
	require.Nil(t, gqlResponse.Data, string(gqlResponse.Data))
	require.Equal(t, 1, len(gqlResponse.Errors))
	require.True(t, strings.Contains(gqlResponse.Errors[0].Message,
		"Cannot query field \"queryUniversity\" on type \"Query\"."))

	// Try deleting university, should throw error as delete API does not exist
	mutation := `
		mutation {
			deleteUniversity(filter: {}) {
				name
			}
		}`
	params = &GraphQLParams{Query: mutation}
	gqlResponse = params.ExecuteAsPost(t, GraphqlURL)
	require.NotNil(t, gqlResponse.Errors)
	require.Nil(t, gqlResponse.Data, string(gqlResponse.Data))
	require.Equal(t, 1, len(gqlResponse.Errors))
	require.True(t, strings.Contains(gqlResponse.Errors[0].Message,
		"Cannot query field \"deleteUniversity\" on type \"Mutation\"."))
}

// panicCatcher tests that the GraphQL server behaves properly when an internal
// bug triggers a panic.  Here, this is mocked up with httptest and a dgraph package
// that just panics.
//
// Not really an e2e test cause it uses httptest and mocks up a panicing Dgraph, but
// uses all the e2e infrastructure.
func panicCatcher(t *testing.T) {

	// queries and mutations have different panic paths.
	//
	// Because queries run concurrently in their own goroutine, any panics are
	// caught by a panic handler deferred when starting those goroutines.
	//
	// Mutations run serially in the same goroutine as the original http handler,
	// so a panic here is caught by the panic catching http handler that wraps
	// the http stack.

	tests := map[string]*GraphQLParams{
		"query": {Query: `query { queryCountry { name } }`},
		"mutation": {
			Query: `mutation {
						addCountry(input: [{ name: "A Country" }]) { country { id } }
					}`,
		},
	}

	gqlSchema := test.LoadSchemaFromFile(t, "schema.graphql")

	fns := &resolve.ResolverFns{
		Qrw: resolve.NewQueryRewriter(),
		Arw: resolve.NewAddRewriter,
		Urw: resolve.NewUpdateRewriter,
		Drw: resolve.NewDeleteRewriter(),
		Ex:  &panicClient{}}

	resolverFactory := resolve.NewResolverFactory(nil, nil).
		WithConventionResolvers(gqlSchema, fns)
	schemaEpoch := uint64(0)
	resolvers := resolve.New(gqlSchema, resolverFactory)
	server := admin2.NewServer()
	server.Set(x.GalaxyNamespace, &schemaEpoch, resolvers)

	ts := httptest.NewServer(server.HTTPHandler())
	defer ts.Close()

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			gqlResponse := test.ExecuteAsPost(t, ts.URL)

			require.Equal(t, x.GqlErrorList{
				{Message: fmt.Sprintf("Internal Server Error - a panic was trapped.  " +
					"This indicates a bug in the GraphQL server.  A stack trace was logged.  " +
					"Please let us know by filing an issue with the stack trace.")}},
				gqlResponse.Errors)

			require.Nil(t, gqlResponse.Data, string(gqlResponse.Data))
		})
	}
}

type panicClient struct{}

func (dg *panicClient) Execute(ctx context.Context, req *api.Request,
	field schema.Field) (*api.Response, error) {
	x.Panic(errors.New(panicMsg))
	return nil, nil
}

func (dg *panicClient) CommitOrAbort(ctx context.Context,
	tc *api.TxnContext) (*api.TxnContext, error) {
	return &api.TxnContext{}, nil
}

// clientInfoLogin check whether the client info(IP address) is propagated in the request.
// It mocks Dgraph like panicCatcher.
func clientInfoLogin(t *testing.T) {
	loginQuery := &GraphQLParams{
		Query: `mutation {
					login(userId: "groot", password: "password") {
						response {
							accessJWT
						}
					}
				}`,
	}

	gqlSchema := test.LoadSchemaFromFile(t, "schema.graphql")

	fns := &resolve.ResolverFns{}
	var loginCtx context.Context
	errFunc := func(name string) error { return nil }
	mErr := resolve.MutationResolverFunc(
		func(ctx context.Context, mutation schema.Mutation) (*resolve.Resolved, bool) {
			loginCtx = ctx
			return &resolve.Resolved{Err: errFunc(mutation.ResponseName()), Field: mutation}, false
		})

	resolverFactory := resolve.NewResolverFactory(nil, mErr).
		WithConventionResolvers(gqlSchema, fns)
	schemaEpoch := uint64(0)
	resolvers := resolve.New(gqlSchema, resolverFactory)
	server := admin2.NewServer()
	server.Set(x.GalaxyNamespace, &schemaEpoch, resolvers)

	ts := httptest.NewServer(server.HTTPHandler())
	defer ts.Close()

	_ = loginQuery.ExecuteAsPost(t, ts.URL)
	require.NotNil(t, loginCtx)
	peerInfo, found := peer.FromContext(loginCtx)
	require.True(t, found)
	require.NotNil(t, peerInfo.Addr.String())
}
