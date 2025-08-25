//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

//nolint:lll
package schema

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/graphql/e2e/common"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/worker"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var (
	groupOneHTTP   = testutil.ContainerAddr("alpha1", 8080)
	groupTwoHTTP   = testutil.ContainerAddr("alpha2", 8080)
	groupThreeHTTP = testutil.ContainerAddr("alpha3", 8080)
	groupOnegRPC   = testutil.GetSockAddr()

	groupOneGraphQLServer   = "http://" + groupOneHTTP + "/graphql"
	groupTwoGraphQLServer   = "http://" + groupTwoHTTP + "/graphql"
	groupThreeGraphQLServer = "http://" + groupThreeHTTP + "/graphql"

	groupOneAdminServer = "http://" + groupOneHTTP + "/admin"
)

// This test is supposed to test the graphql schema subscribe feature. Whenever schema is updated
// in a dgraph alpha for one group, that update should also be propagated to alpha nodes in other
// groups.
func TestSchemaSubscribe(t *testing.T) {
	schema := `
	type Author {
		id: ID!
		name: String!
	}`
	groupOnePreUpdateCounter := common.RetryProbeGraphQL(t, groupOneHTTP, nil).SchemaUpdateCounter
	common.SafelyUpdateGQLSchema(t, groupOneHTTP, schema, nil)
	// since the schema has been updated on group one, the schemaUpdateCounter on all the servers
	// should have got incremented and must be the same, indicating that the schema update has
	// reached all the servers.
	common.AssertSchemaUpdateCounterIncrement(t, groupOneHTTP, groupOnePreUpdateCounter, nil)
	common.AssertSchemaUpdateCounterIncrement(t, groupTwoHTTP, groupOnePreUpdateCounter, nil)
	common.AssertSchemaUpdateCounterIncrement(t, groupThreeHTTP, groupOnePreUpdateCounter, nil)

	introspectionQuery := `
	query {
		__type(name: "Author") {
			name
			fields {
				name
			}
		}
	}`
	introspect := &common.GraphQLParams{
		Query: introspectionQuery,
	}

	expectedResult :=
		`{
			"__type": {
				"name":"Author",
				"fields": [
					{
						"name": "id"
					},
					{
						"name": "name"
					}
				]
			}
		}`

	// Also, the introspection query on all the servers should
	// give the same result as they have the same schema.
	introspectionResult := introspect.ExecuteAsPost(t, groupOneGraphQLServer)
	common.RequireNoGQLErrors(t, introspectionResult)
	testutil.CompareJSON(t, expectedResult, string(introspectionResult.Data))

	introspectionResult = introspect.ExecuteAsPost(t, groupTwoGraphQLServer)
	common.RequireNoGQLErrors(t, introspectionResult)
	testutil.CompareJSON(t, expectedResult, string(introspectionResult.Data))

	introspectionResult = introspect.ExecuteAsPost(t, groupThreeGraphQLServer)
	common.RequireNoGQLErrors(t, introspectionResult)
	testutil.CompareJSON(t, expectedResult, string(introspectionResult.Data))

	// Now update schema on an alpha node for group 3 and see if nodes in group 1 and 2 also get it.
	schema = `
	type Author {
		id: ID!
		name: String!
		posts: [Post]
	}

	interface Post {
		id: ID!
	}`
	groupThreePreUpdateCounter := groupOnePreUpdateCounter + 1
	common.SafelyUpdateGQLSchema(t, groupThreeHTTP, schema, nil)

	common.AssertSchemaUpdateCounterIncrement(t, groupOneHTTP, groupThreePreUpdateCounter, nil)
	common.AssertSchemaUpdateCounterIncrement(t, groupTwoHTTP, groupThreePreUpdateCounter, nil)
	common.AssertSchemaUpdateCounterIncrement(t, groupThreeHTTP, groupThreePreUpdateCounter, nil)

	expectedResult =
		`{
			"__type": {
				"name": "Author",
				"fields": [
					{
						"name": "id"
					},
					{
						"name": "name"
					},
					{
						"name": "posts"
					},
					{
						"name": "postsAggregate"
					}
				]
			}
		}`
	introspectionResult = introspect.ExecuteAsPost(t, groupOneGraphQLServer)
	common.RequireNoGQLErrors(t, introspectionResult)
	testutil.CompareJSON(t, expectedResult, string(introspectionResult.Data))

	introspectionResult = introspect.ExecuteAsPost(t, groupTwoGraphQLServer)
	common.RequireNoGQLErrors(t, introspectionResult)
	testutil.CompareJSON(t, expectedResult, string(introspectionResult.Data))

	introspectionResult = introspect.ExecuteAsPost(t, groupThreeGraphQLServer)
	common.RequireNoGQLErrors(t, introspectionResult)
	testutil.CompareJSON(t, expectedResult, string(introspectionResult.Data))
}

// TestConcurrentSchemaUpdates checks that if there are too many concurrent requests to update the
// GraphQL schema, then the system works as expected by either:
//  1. failing the schema update because there is another one in progress, OR
//  2. if the schema update succeeds, then the last successful schema update is reflected by both
//     Dgraph and GraphQL schema
//
// It also tests that only one node exists for GraphQL schema in Dgraph after all the
// concurrent requests have executed.
func TestConcurrentSchemaUpdates(t *testing.T) {
	common.SafelyDropAll(t)
	dg, err := testutil.DgraphClient(groupOnegRPC)
	require.NoError(t, err)

	tcases := []struct {
		graphQLSchema string
		dgraphSchema  string
		authority     string
	}{
		{
			graphQLSchema: `
			type A {
				b: String!
			}`,
			dgraphSchema: `{
				"predicate": "A.b",
				"type": "string"
			}`,
			authority: groupOneHTTP,
		},
		{
			graphQLSchema: `
			type A {
				b: String! @search(by: [term])
			}`,
			dgraphSchema: `{
				"predicate": "A.b",
				"type": "string",
				"index": true,
				"tokenizer": [
					"term"
				]
			}`,
			authority: groupTwoHTTP,
		},
		{
			graphQLSchema: `
			type A {
				b: String! @search(by: [exact])
			}`,
			dgraphSchema: `{
				"predicate": "A.b",
				"type": "string",
				"index": true,
				"tokenizer": [
					"exact"
				]
			}`,
			authority: groupThreeHTTP,
		},
	}

	numTcases := len(tcases)
	numRequests := 100
	var lastSuccessReqTimestamp int64 = -1
	lastSuccessTcaseIdx := -1

	mux := sync.Mutex{}
	wg := sync.WaitGroup{}

	// send too many concurrent schema update requests to different servers
	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(reqIdx int) {
			tcaseIdx := reqIdx % numTcases
			// if the update succeeded, save the success request timestamp and tcase index
			if updateGQLSchemaConcurrent(t, tcases[tcaseIdx].graphQLSchema, tcases[tcaseIdx].authority) {
				now := time.Now().UnixNano()
				mux.Lock()
				if now > lastSuccessReqTimestamp {
					lastSuccessReqTimestamp = now
					lastSuccessTcaseIdx = tcaseIdx
				}
				mux.Unlock()
			}
			wg.Done()
		}(i)
	}

	// wait for all of them to finish
	wg.Wait()

	// make sure at least one update request succeeded
	require.GreaterOrEqual(t, lastSuccessReqTimestamp, int64(0))
	require.GreaterOrEqual(t, lastSuccessTcaseIdx, 0)

	// find final GraphQL & Dgraph schemas
	finalGraphQLSchema := tcases[lastSuccessTcaseIdx].graphQLSchema
	finalDgraphPreds := tcases[lastSuccessTcaseIdx].dgraphSchema
	finalDgraphTypes := `
	{
		"fields": [
			{
				"name": "A.b"
			}
		],
		"name": "A"
	}`

	// now check that both the final GraphQL schema and Dgraph schema are the ones we expect
	require.Equal(t, finalGraphQLSchema, common.AssertGetGQLSchemaRequireId(t, groupOneHTTP, nil).Schema)
	testutil.VerifySchema(t, dg, testutil.SchemaOptions{
		UserPreds:        finalDgraphPreds,
		UserTypes:        finalDgraphTypes,
		ExcludeAclSchema: true,
	})

	// now check that there is exactly one node for GraphQL schema in Dgraph,
	// and that contains the same schema as the one we expect
	res, err := dg.NewReadOnlyTxn().Query(context.Background(), `
	query {
		gqlSchema(func: has(dgraph.graphql.schema)) {
			uid
			dgraph.graphql.schema
		}
	}`)
	require.NoError(t, err)

	var resp struct {
		GqlSchema []struct {
			Uid    string
			Schema string `json:"dgraph.graphql.schema"`
		}
	}
	require.NoError(t, json.Unmarshal(res.GetJson(), &resp))
	require.Len(t, resp.GqlSchema, 1)
	require.Equal(t, finalGraphQLSchema, resp.GqlSchema[0].Schema)
}

// TestIntrospectionQueryAfterDropAll make sure that Introspection query after drop_all doesn't give any internal error
func TestIntrospectionQueryAfterDropAll(t *testing.T) {
	common.SafelyDropAll(t)

	introspectionQuery := `
	query{
		__schema{
		   types{
			 name
		   }
		}
	}`
	introspect := &common.GraphQLParams{
		Query: introspectionQuery,
	}

	// On doing Introspection Query Now, We should get the Expected Error Message, not the Internal Error.
	introspectionResult := introspect.ExecuteAsPost(t, groupOneGraphQLServer)
	require.Len(t, introspectionResult.Errors, 1)
	gotErrorMessage := introspectionResult.Errors[0].Message
	expectedErrorMessage := "Not resolving __schema. There's no GraphQL schema in Dgraph. Use the /admin API to add a GraphQL schema"
	require.Equal(t, expectedErrorMessage, gotErrorMessage)
}

// TestUpdateGQLSchemaAfterDropAll makes sure that updating the GraphQL schema after drop_all works
func TestUpdateGQLSchemaAfterDropAll(t *testing.T) {
	common.SafelyUpdateGQLSchema(t, groupOneHTTP, `
	type A {
		b: String!
	}`, nil)
	oldCounter := common.RetryProbeGraphQL(t, groupOneHTTP, nil).SchemaUpdateCounter

	// now do drop_all
	dg, err := testutil.DgraphClient(groupOnegRPC)
	require.NoError(t, err)
	testutil.DropAll(t, dg)

	// need to wait a bit, because the update notification takes time to reach the alpha
	common.AssertSchemaUpdateCounterIncrement(t, groupOneHTTP, oldCounter, nil)
	// now retrieving the GraphQL schema should report no schema
	require.Empty(t, common.AssertGetGQLSchemaRequireId(t, groupOneHTTP, nil).Schema)

	// updating the schema now should work
	schema := `
			type A {
				b: String! @id
			}`
	common.SafelyUpdateGQLSchema(t, groupOneHTTP, schema, nil)
	// we should get the schema we expect
	require.Equal(t, schema, common.AssertGetGQLSchemaRequireId(t, groupOneHTTP, nil).Schema)
}

// TestGQLSchemaAfterDropData checks if the schema still exists after drop_data
func TestGQLSchemaAfterDropData(t *testing.T) {
	schema := `
			type A {
				b: String!
			}`
	common.SafelyUpdateGQLSchema(t, groupOneHTTP, schema, nil)
	oldCounter := common.RetryProbeGraphQL(t, groupOneHTTP, nil).SchemaUpdateCounter

	// now do drop_data
	dg, err := testutil.DgraphClient(groupOnegRPC)
	require.NoError(t, err)
	require.NoError(t, dg.Alter(context.Background(), &api.Operation{DropOp: api.Operation_DATA}))

	// lets wait a bit to be sure that the update notification has reached the alpha,
	// otherwise we are anyways gonna get the previous schema from the in-memory schema
	time.Sleep(5 * time.Second)
	// drop_data should not increment the schema update counter
	newCounter := common.RetryProbeGraphQL(t, groupOneHTTP, nil).SchemaUpdateCounter
	require.Equal(t, oldCounter, newCounter)
	// we should still get the schema we inserted earlier
	require.Equal(t, schema, common.AssertGetGQLSchemaRequireId(t, groupOneHTTP, nil).Schema)

}

// TestCORS checks that all the CORS headers are correctly set in the response.
func TestCORS(t *testing.T) {
	// initially setting a schema without any Dgraph.Allow-Origin and forwardHeaders
	testCORS(t, `
	type TestCORS {
		name: String
	}`, "", "*", x.AccessControlAllowedHeaders)

	// forwardHeaders should be part of allowed CORS headers
	testCORS(t, `
	type TestCORS {
		id: ID!
		name: String
		cf: String @custom(http:{
			url: "https://play.dgraph.io",
			method: GET,
			forwardHeaders: ["Test-CORS"]
		})
	}`, "", "*", strings.Join([]string{x.AccessControlAllowedHeaders, "Test-CORS"}, ","))

	// setting Dgraph.Allow-Origin and sending request from correct Origin should return the
	// same origin back
	testCORS(t, `
	type TestCORS {
		name: String
	}
	# Dgraph.Allow-Origin "https://play.dgraph.io"
	`, "https://play.dgraph.io", "https://play.dgraph.io", x.AccessControlAllowedHeaders)

	// setting Dgraph.Allow-Origin and sending request from incorrect Origin should not return any
	// origin back
	testCORS(t, `
	type TestCORS {
		name: String
	}
	# Dgraph.Allow-Origin "https://dgraph.io"
	`, "https://play.dgraph.io", "", x.AccessControlAllowedHeaders)

	// setting auth, forwardHeaders and Dgraph.Allow-Origin should work as expected
	testCORS(t, `
	type TestCORS {
		id: ID!
		name: String
		cf: String @custom(http:{
			url: "https://play.dgraph.io",
			method: GET,
			forwardHeaders: ["Test-CORS"]
		})
	}
	# Dgraph.Allow-Origin "https://play.dgraph.io"
	# Dgraph.Authorization  {"VerificationKey":"secret","Header":"X-Test-Dgraph","Namespace":"https://dgraph.io/jwt/claims","Algo":"HS256"}
	`, "https://play.dgraph.io", "https://play.dgraph.io",
		strings.Join([]string{x.AccessControlAllowedHeaders, "Test-CORS", "X-Test-Dgraph"}, ","))
}

func testCORS(t *testing.T, schema, reqOrigin, expectedAllowedOrigin,
	expectedAllowedHeaders string) {
	common.SafelyUpdateGQLSchema(t, groupOneHTTP, schema, nil)

	params := &common.GraphQLParams{Query: `query {	queryTestCORS { name } }`}
	req, err := params.CreateGQLPost(groupOneGraphQLServer)
	require.NoError(t, err)

	if reqOrigin != "" {
		req.Header.Set("Origin", reqOrigin)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)

	// GraphQL server should always return OK and JSON content, even when there are errors
	require.Equal(t, resp.StatusCode, http.StatusOK)
	require.Equal(t, strings.ToLower(resp.Header.Get("Content-Type")), "application/json")
	// assert that the CORS headers are there as expected
	require.Equal(t, resp.Header.Get("Access-Control-Allow-Origin"), expectedAllowedOrigin)
	require.Equal(t, resp.Header.Get("Access-Control-Allow-Methods"), "POST, OPTIONS")
	require.Equal(t, resp.Header.Get("Access-Control-Allow-Headers"), expectedAllowedHeaders)
	require.Equal(t, resp.Header.Get("Access-Control-Allow-Credentials"), "true")

	gqlRes := &common.GraphQLResponse{}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, gqlRes))
	common.RequireNoGQLErrors(t, gqlRes)
	testutil.CompareJSON(t, `{"queryTestCORS":[]}`, string(gqlRes.Data))
}

func TestGQLSchemaValidate(t *testing.T) {
	common.SafelyDropAll(t)

	testCases := []struct {
		schema string
		errors x.GqlErrorList
		valid  bool
	}{
		{
			schema: `
				type Task @auth(
					query: { rule: "{$USERROLE: { eq: \"USER\"}}" }
				) {
					id: ID!
					name: String!
					occurrences: [TaskOccurrence] @hasInverse(field: task)
				}

				type TaskOccurrence @auth(
					query: { rule: "query { queryTaskOccurrence { task { id } } }" }
				) {
					id: ID!
					due: DateTime
					comp: DateTime
					task: Task @hasInverse(field: occurrences)
				}
			`,
			valid: true,
		},
		{
			schema: `
				type X {
					id: ID @dgraph(pred: "X.id")
					name: String
				}
				type Y {
					f1: String! @dgraph(pred:"~movie")
				}
			`,
			errors: x.GqlErrorList{
				{Message: "input:3: Type X; Field id: has the @dgraph directive " +
					"but fields of type ID can't have the @dgraph directive."},
				{Message: "input:7: Type Y; Field f1 is of type String, " +
					"but reverse predicate in @dgraph directive only applies to fields with object types."}},
			valid: false,
		},
	}

	validateUrl := groupOneAdminServer + "/schema/validate"
	var response x.QueryResWithData
	for _, tcase := range testCases {
		resp, err := http.Post(validateUrl, "text/plain", bytes.NewBuffer([]byte(tcase.schema)))
		require.NoError(t, err)

		decoder := json.NewDecoder(resp.Body)
		require.NoError(t, decoder.Decode(&response))

		// Verify that we only validate the schema and not set it.
		require.Empty(t, common.AssertGetGQLSchema(t, groupOneHTTP, nil).Schema)

		if tcase.valid {
			require.Equal(t, resp.StatusCode, http.StatusOK)
			continue
		}
		require.Equal(t, resp.StatusCode, http.StatusBadRequest)
		require.NotNil(t, response.Errors)
		require.Equal(t, len(response.Errors), len(tcase.errors))
		for idx, err := range response.Errors {
			require.Equal(t, err.Message, tcase.errors[idx].Message)
		}
	}
}

// TestUpdateGQLSchemaFields makes sure that all the fields in the updateGQLSchema mutation response
// are correctly set.
func TestUpdateGQLSchemaFields(t *testing.T) {
	schema := `
	type Author {
		id: ID!
		name: String!
	}`

	generatedSchema, err := os.ReadFile("generatedSchema.graphql")
	require.NoError(t, err)
	require.Equal(t, string(generatedSchema), common.SafelyUpdateGQLSchema(t, groupOneHTTP,
		schema, nil).GeneratedSchema)
}

// TestLargeSchemaUpdate makes sure that updating large schemas (4000 fields with indexes) does not
// throw any error
func TestLargeSchemaUpdate(t *testing.T) {
	numFields := 250

	schema := "type LargeSchema {"
	for i := 1; i <= numFields; i++ {
		schema = schema + "\n" + fmt.Sprintf("field%d: String! @search(by: [\"regexp\"])", i)
	}
	schema = schema + "\n}"

	common.SafelyUpdateGQLSchema(t, groupOneHTTP, schema, nil)
}

func TestIntrospection(t *testing.T) {
	// note that both the types implement the same interface and have a field called `name`, which
	// has exact same name as a field in full introspection query.
	schema := `
	interface Node {
		id: ID!
	}

	type Human implements Node {
		name: String
	}

	type Dog implements Node {
		name: String
	}`
	common.SafelyUpdateGQLSchema(t, groupOneHTTP, schema, nil)
	query, err := os.ReadFile("../../schema/testdata/introspection/input/full_query.graphql")
	require.NoError(t, err)

	introspectionParams := &common.GraphQLParams{Query: string(query)}
	resp := introspectionParams.ExecuteAsPost(t, groupOneGraphQLServer)

	// checking that there are no errors in the response, i.e., we always get some data in the
	// introspection response.
	common.RequireNoGQLErrors(t, resp)
	require.NotEmpty(t, resp.Data)
	// TODO: we should actually compare data here, but there seems to be some issue with either the
	// introspection response or the JSON comparison. Needs deeper looking.
}

func TestApolloServiceResolver(t *testing.T) {
	schema := `
	type Mission {
		id: ID!
		crew: [Astronaut]
		designation: String!
		startDate: String
		endDate: String
	}

	type Astronaut @key(fields: "id") @extends {
		id: ID! @external
		missions: [Mission]
	}

	type User @remote {
		id: ID!
		name: String!
	}

	type Car @auth(
		password: { rule: "{$ROLE: { eq: \"Admin\" } }"}
	){
		id: ID!
		name: String!
	}

	type Query {
		getMyFavoriteUsers(id: ID!): [User] @custom(http: {
			url: "http://my-api.com",
			method: "GET"
		})
	}
	`
	common.SafelyUpdateGQLSchema(t, groupOneHTTP, schema, nil)
	serviceQueryParams := &common.GraphQLParams{Query: `
	query {
		_service {
			s: sdl
		}
	}`}
	resp := serviceQueryParams.ExecuteAsPost(t, groupOneGraphQLServer)
	common.RequireNoGQLErrors(t, resp)
	var gqlRes struct {
		Service struct {
			S string
		} `json:"_service"`
	}
	require.NoError(t, json.Unmarshal(resp.Data, &gqlRes))

	sdl, err := os.ReadFile("apollo_service_response.graphql")
	require.NoError(t, err)

	require.Equal(t, string(sdl), gqlRes.Service.S)
}

func TestDeleteSchemaAndExport(t *testing.T) {
	// first apply a schema
	schema := `
	type Person {
		name: String
	}`
	schemaResp := common.SafelyUpdateGQLSchema(t, groupOneHTTP, schema, nil)

	// now delete it with S * * delete mutation
	dg, err := testutil.DgraphClient(groupOnegRPC)
	require.NoError(t, err)
	txn := dg.NewTxn()
	_, err = txn.Mutate(context.Background(), &api.Mutation{
		DelNquads: []byte(fmt.Sprintf("<%s> * * .", schemaResp.Id)),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(context.Background()))

	// running an export shouldn't give any errors
	exportReq := &common.GraphQLParams{
		Query: `mutation {
		  export(input: {format: "rdf"}) {
			response { code }
			taskId
		  }
		}`,
	}
	exportGqlResp := exportReq.ExecuteAsPost(t, groupOneAdminServer)
	common.RequireNoGQLErrors(t, exportGqlResp)

	var data interface{}
	require.NoError(t, json.Unmarshal(exportGqlResp.Data, &data))

	require.Equal(t, "Success", testutil.JsonGet(data, "export", "response", "code").(string))
	taskId := testutil.JsonGet(data, "export", "taskId").(string)
	testutil.WaitForTask(t, taskId, false, testutil.GetSockAddrHttp())

	// applying a new schema should still work
	newSchemaResp := common.AssertUpdateGQLSchemaSuccess(t, groupOneHTTP, schema, nil)
	// we can assert that the uid allocated to new schema isn't same as the uid for old schema
	require.NotEqual(t, schemaResp.Id, newSchemaResp.Id)
}

func updateGQLSchemaConcurrent(t *testing.T, schema, authority string) bool {
	res := common.RetryUpdateGQLSchema(t, authority, schema, nil)
	err := res.Errors.Error()
	require.NotContains(t, err, worker.ErrMultipleGraphQLSchemaNodes)
	require.NotContains(t, err, worker.ErrGraphQLSchemaAlterFailed)

	return res.Errors == nil
}

func TestMain(m *testing.M) {
	x.Panic(common.CheckGraphQLStarted(common.GraphqlAdminURL))
	m.Run()
}
