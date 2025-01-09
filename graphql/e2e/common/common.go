/*
 *    Copyright 2025 Hypermode Inc. and Contributors
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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/graphql/schema"
	"github.com/hypermodeinc/dgraph/v24/testutil"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var (
	Alpha1HTTP = testutil.ContainerAddr("alpha1", 8080)
	Alpha1gRPC = testutil.ContainerAddr("alpha1", 9080)

	GraphqlURL      = "http://" + Alpha1HTTP + "/graphql"
	GraphqlAdminURL = "http://" + Alpha1HTTP + "/admin"

	dgraphHealthURL = "http://" + Alpha1HTTP + "/health?all"
	dgraphStateURL  = "http://" + Alpha1HTTP + "/state"

	// this port is used on the host machine to spin up a test HTTP server
	lambdaHookServerAddr = ":8888"

	retryableUpdateGQLSchemaErrors = []string{
		"errIndexingInProgress",
		"is already running",
		"retry again, server is not ready", // given by Dgraph while applying the snapshot
		"Unavailable: Server not ready",    // given by GraphQL layer, during init on admin server
	}

	retryableCreateNamespaceErrors = append(retryableUpdateGQLSchemaErrors,
		"is not indexed",
	)

	safelyUpdateGQLSchemaErr = "New Counter: %v, Old Counter: %v.\n" +
		"Schema update counter didn't increment, " +
		"indicating that the GraphQL layer didn't get the updated schema even after 10" +
		" retries. The most probable cause is the new GraphQL schema is same as the old" +
		" GraphQL schema."
)

// GraphQLParams is parameters for constructing a GraphQL query - that's
// http POST with this body, or http GET with this in the query string.
//
// https://graphql.org/learn/serving-over-http/ says:
//
// POST
// ----
// 'A standard GraphQL POST request should use the application/json content type,
// and include a JSON-encoded body of the following form:
// {
// 	  "query": "...",
// 	  "operationName": "...",
// 	  "variables": { "myVariable": "someValue", ... }
// }
// operationName and variables are optional fields. operationName is only
// required if multiple operations are present in the query.'
//
//
// GET
// ---
//
// http://myapi/graphql?query={me{name}}
// "Query variables can be sent as a JSON-encoded string in an additional query parameter
// called variables. If the query contains several named operations, an operationName query
// parameter can be used to control which one should be executed."
//
// acceptGzip sends "Accept-Encoding: gzip" header to the server, which would return the
// response after gzip.
// gzipEncoding would compress the request to the server and add "Content-Encoding: gzip"
// header to the same.

type GraphQLParams struct {
	Query         string                    `json:"query"`
	OperationName string                    `json:"operationName"`
	Variables     map[string]interface{}    `json:"variables"`
	Extensions    *schema.RequestExtensions `json:"extensions,omitempty"`
	acceptGzip    bool
	gzipEncoding  bool
	Headers       http.Header
}

type requestExecutor func(t *testing.T, url string, params *GraphQLParams) *GraphQLResponse

// GraphQLResponse GraphQL response structure.
// see https://graphql.github.io/graphql-spec/June2018/#sec-Response
type GraphQLResponse struct {
	Data       json.RawMessage        `json:"data,omitempty"`
	Errors     x.GqlErrorList         `json:"errors,omitempty"`
	Extensions map[string]interface{} `json:"extensions,omitempty"`
}

type Tweets struct {
	Id        string `json:"id,omitempty"`
	Text      string `json:"text,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	User      *User  `json:"user,omitempty"`
}

type User struct {
	Username string `json:"username,omitempty"`
	Age      uint64 `json:"age,omitempty"`
	IsPublic bool   `json:"isPublic,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	Password string `json:"password,omitempty"`
}

type country struct {
	ID     string   `json:"id,omitempty"`
	Name   string   `json:"name,omitempty"`
	States []*state `json:"states,omitempty"`
}

type author struct {
	ID            string     `json:"id,omitempty"`
	Name          string     `json:"name,omitempty"`
	Qualification string     `json:"qualification,omitempty"`
	Dob           *time.Time `json:"dob,omitempty"`
	Reputation    float32    `json:"reputation,omitempty"`
	Country       *country   `json:"country,omitempty"`
	Posts         []*post    `json:"posts,omitempty"`
}

type user struct {
	Name     string `json:"name,omitempty"`
	Password string `json:"password,omitempty"`
}

type post struct {
	PostID      string    `json:"postID,omitempty"`
	Title       string    `json:"title,omitempty"`
	Text        string    `json:"text,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	Topic       string    `json:"topic,omitempty"`
	NumLikes    int       `json:"numLikes,omitempty"`
	NumViews    int64     `json:"numViews,omitempty"`
	IsPublished bool      `json:"isPublished,omitempty"`
	PostType    string    `json:"postType,omitempty"`
	Author      *author   `json:"author,omitempty"`
	Category    *category `json:"category,omitempty"`
}

type category struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Posts []post `json:"posts,omitempty"`
}

type state struct {
	ID      string   `json:"id,omitempty"`
	Name    string   `json:"name,omitempty"`
	Code    string   `json:"xcode,omitempty"`
	Capital string   `json:"capital,omitempty"`
	Country *country `json:"country,omitempty"`
	Region  *region  `json:"region,omitempty"`
}

type region struct {
	ID       string    `json:"id,omitempty"`
	Name     string    `json:"name,omitempty"`
	District *district `json:"district,omitempty"`
}

type district struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type movie struct {
	ID       string      `json:"id,omitempty"`
	Name     string      `json:"name,omitempty"`
	Director []*director `json:"moviedirector,omitempty"`
}

type director struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type teacher struct {
	ID      string     `json:"id,omitempty"`
	Xid     string     `json:"xid,omitempty"`
	Name    string     `json:"name,omitempty"`
	Subject string     `json:"subject,omitempty"`
	Teaches []*student `json:"teaches,omitempty"`
}

type student struct {
	ID       string     `json:"id,omitempty"`
	Xid      string     `json:"xid,omitempty"`
	Name     string     `json:"name,omitempty"`
	TaughtBy []*teacher `json:"taughtBy,omitempty"`
}

type UserSecret struct {
	Id      string `json:"id,omitempty"`
	ASecret string `json:"aSecret,omitempty"`
	OwnedBy string `json:"ownedBy,omitempty"`
}

type Todo struct {
	Id    string `json:"id,omitempty"`
	Text  string `json:"text,omitempty"`
	Owner string `json:"owner,omitempty"`
}

type ProbeGraphQLResp struct {
	Healthy             bool `json:"-"`
	Status              string
	SchemaUpdateCounter uint64
}

type GqlSchema struct {
	Id              string
	Schema          string
	GeneratedSchema string
}

func probeGraphQL(authority string, header http.Header) (*ProbeGraphQLResp, error) {
	request, err := http.NewRequest("GET", "http://"+authority+"/probe/graphql", nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{}
	request.Header = header
	resp, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err = resp.Body.Close(); err != nil {
			glog.Errorf("Error while closing response body: %v", err)
		}
	}()

	probeResp := ProbeGraphQLResp{}
	if resp.StatusCode == http.StatusOK {
		probeResp.Healthy = true
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, &probeResp); err != nil {
		return nil, err
	}
	return &probeResp, nil
}

func retryProbeGraphQL(authority string, header http.Header) *ProbeGraphQLResp {
	for range 10 {
		resp, err := probeGraphQL(authority, header)
		if err == nil && resp.Healthy {
			return resp
		}
		time.Sleep(time.Second)
	}
	return nil
}

func RetryProbeGraphQL(t *testing.T, authority string, header http.Header) *ProbeGraphQLResp {
	if resp := retryProbeGraphQL(authority, header); resp != nil {
		return resp
	}
	debug.PrintStack()
	t.Fatal("Unable to get healthy response from /probe/graphql after 10 retries")
	return nil
}

// AssertSchemaUpdateCounterIncrement asserts that the schemaUpdateCounter is greater than the
// oldCounter, indicating that the GraphQL schema has been updated.
// If it can't make the assertion with enough retries, it fails the test.
func AssertSchemaUpdateCounterIncrement(t *testing.T, authority string, oldCounter uint64, header http.Header) {
	var newCounter uint64
	for range 20 {
		if newCounter = RetryProbeGraphQL(t, authority,
			header).SchemaUpdateCounter; newCounter == oldCounter+1 {
			return
		}
		time.Sleep(time.Second)
	}

	// Even after atleast 10 seconds, the schema update hasn't reached GraphQL layer.
	// That indicates something fatal.
	debug.PrintStack()
	t.Fatalf(safelyUpdateGQLSchemaErr, newCounter, oldCounter)
}

func containsRetryableCreateNamespaceError(resp *GraphQLResponse) bool {
	if resp.Errors == nil {
		return false
	}
	errStr := resp.Errors.Error()
	for _, retryableErr := range retryableCreateNamespaceErrors {
		if strings.Contains(errStr, retryableErr) {
			return true
		}
	}
	return false
}

func CreateNamespaces(t *testing.T, headers http.Header, whichAlpha string, count int) []uint64 {
	var ns []uint64
	for i := 1; i <= count; i++ {
		ns = append(ns, CreateNamespace(t, headers, whichAlpha))
	}
	return ns
}

func CreateNamespace(t *testing.T, headers http.Header, whichAlpha string) uint64 {
	adminUrl := "http://" + testutil.ContainerAddr(whichAlpha, 8080) + "/admin"
	createNamespace := &GraphQLParams{
		Query: `mutation {
					addNamespace{
						namespaceId
					}
				}`,
		Headers: headers,
	}

	// keep retrying as long as we get a retryable error
	var gqlResponse *GraphQLResponse
	for {
		gqlResponse = createNamespace.ExecuteAsPost(t, adminUrl)
		if containsRetryableCreateNamespaceError(gqlResponse) {
			continue
		}
		RequireNoGQLErrors(t, gqlResponse)
		break
	}

	var resp struct {
		AddNamespace struct {
			NamespaceId uint64
		}
	}
	require.NoError(t, json.Unmarshal(gqlResponse.Data, &resp))
	require.Greater(t, resp.AddNamespace.NamespaceId, x.GalaxyNamespace)
	return resp.AddNamespace.NamespaceId
}

func ListNamespaces(t *testing.T, jwtToken string, headers http.Header, whichAlpha string) []uint64 {
	adminUrl := "http://" + testutil.ContainerAddr(whichAlpha, 8080) + "/admin"

	listNamespaces := &GraphQLParams{
		Query: `query{
			 state {
			 namespaces
			 }
		 }`,
		Headers: headers,
	}

	gqlResponse := listNamespaces.ExecuteAsPost(t, adminUrl)
	RequireNoGQLErrors(t, gqlResponse)

	var resp struct {
		State struct {
			Namespaces []uint64 `json:"namespaces"`
		} `json:"state"`
	}
	require.NoError(t, json.Unmarshal(gqlResponse.Data, &resp))
	return resp.State.Namespaces
}

func DeleteNamespace(t *testing.T, id uint64, header http.Header, whichAlpha string) {
	adminUrl := "http://" + testutil.ContainerAddr(whichAlpha, 8080) + "/admin"
	deleteNamespace := &GraphQLParams{
		Query: `mutation deleteNamespace($id:Int!){
					deleteNamespace(input:{namespaceId:$id}){
						namespaceId
					}
				}`,
		Variables: map[string]interface{}{"id": id},
		Headers:   header,
	}

	gqlResponse := deleteNamespace.ExecuteAsPost(t, adminUrl)
	RequireNoGQLErrors(t, gqlResponse)
}

func getGQLSchema(t *testing.T, authority string, header http.Header) *GraphQLResponse {
	getSchemaParams := &GraphQLParams{
		Query: `query {
			getGQLSchema {
				id
				schema
				generatedSchema
			}
		}`,
		Headers: header,
	}
	return getSchemaParams.ExecuteAsPost(t, "http://"+authority+"/admin")
}

// AssertGetGQLSchema queries the current GraphQL schema using getGQLSchema query and asserts that
// the query doesn't give any errors. It returns a *GqlSchema received in response to the query.
func AssertGetGQLSchema(t *testing.T, authority string, header http.Header) *GqlSchema {
	resp := getGQLSchema(t, authority, header)
	RequireNoGQLErrors(t, resp)

	var getResult struct {
		GetGQLSchema *GqlSchema
	}
	require.NoError(t, json.Unmarshal(resp.Data, &getResult))

	return getResult.GetGQLSchema
}

// In addition to AssertGetGQLSchema, it also asserts that the response returned from the
// getGQLSchema query isn't nil and the Id in the response is actually a uid.
func AssertGetGQLSchemaRequireId(t *testing.T, authority string, header http.Header) *GqlSchema {
	resp := AssertGetGQLSchema(t, authority, header)
	require.NotNil(t, resp)
	testutil.RequireUid(t, resp.Id)
	return resp
}

func updateGQLSchema(t *testing.T, authority, schema string, headers http.Header) *GraphQLResponse {
	updateSchemaParams := &GraphQLParams{
		Query: `mutation updateGQLSchema($sch: String!) {
			updateGQLSchema(input: { set: { schema: $sch }}) {
				gqlSchema {
					id
					schema
					generatedSchema
				}
			}
		}`,
		Variables: map[string]interface{}{"sch": schema},
		Headers:   headers,
	}
	return updateSchemaParams.ExecuteAsPost(t, "http://"+authority+"/admin")
}

func containsRetryableUpdateGQLSchemaError(str string) bool {
	for _, retryableErr := range retryableUpdateGQLSchemaErrors {
		if strings.Contains(str, retryableErr) {
			return true
		}
	}
	return false
}

// RetryUpdateGQLSchema tries to update the GraphQL schema and if it receives a retryable error, it
// keeps retrying until it either receives no error or a non-retryable error. Then it returns the
// GraphQLResponse it received as a result of calling updateGQLSchema.
func RetryUpdateGQLSchema(t *testing.T, authority, schema string, headers http.Header) *GraphQLResponse {
	for {
		resp := updateGQLSchema(t, authority, schema, headers)
		// return the response if we didn't get any error or get a non-retryable error
		if resp.Errors == nil || !containsRetryableUpdateGQLSchemaError(resp.Errors.Error()) {
			return resp
		}

		// otherwise, retry schema update
		t.Logf("Got error while updateGQLSchema: %s. Retrying...\n", resp.Errors.Error())
		time.Sleep(time.Second)
	}
}

// AssertUpdateGQLSchemaSuccess updates the GraphQL schema, asserts that the update succeeded and the
// returned response is correct. It returns a *GqlSchema it received in the response.
func AssertUpdateGQLSchemaSuccess(t *testing.T, authority, schema string,
	headers http.Header) *GqlSchema {
	// update the GraphQL schema
	updateResp := RetryUpdateGQLSchema(t, authority, schema, headers)
	// sanity: we shouldn't get any errors from update
	RequireNoGQLErrors(t, updateResp)

	// sanity: update response should reflect the new schema
	var updateResult struct {
		UpdateGQLSchema struct {
			GqlSchema *GqlSchema
		}
	}
	if err := json.Unmarshal(updateResp.Data, &updateResult); err != nil {
		debug.PrintStack()
		t.Fatalf("failed to unmarshal updateGQLSchema response: %s", err.Error())
	}
	require.NotNil(t, updateResult.UpdateGQLSchema.GqlSchema)
	testutil.RequireUid(t, updateResult.UpdateGQLSchema.GqlSchema.Id)
	require.Equalf(t, updateResult.UpdateGQLSchema.GqlSchema.Schema, schema,
		"updateGQLSchema response doesn't reflect the updated schema")

	return updateResult.UpdateGQLSchema.GqlSchema
}

// AssertUpdateGQLSchemaFailure tries to update the GraphQL schema and asserts that the update
// failed with all of the given errors.
func AssertUpdateGQLSchemaFailure(t *testing.T, authority, schema string, headers http.Header,
	expectedErrors []string) {
	resp := RetryUpdateGQLSchema(t, authority, schema, headers)
	require.Equal(t, `{"updateGQLSchema":null}`, string(resp.Data))
	errString := resp.Errors.Error()
	for _, err := range expectedErrors {
		require.Contains(t, errString, err)
	}
}

// SafelyUpdateGQLSchema can be safely used in tests to update the GraphQL schema. Once the control
// returns from it, one can be sure that the newly applied schema is the one being served by the
// GraphQL layer, and hence it is safe to make any queries as per the new schema. Note that if the
// schema being provided is same as the current schema in the GraphQL layer, then this function will
// fail the test with a fatal error.
func SafelyUpdateGQLSchema(t *testing.T, authority, schema string, headers http.Header) *GqlSchema {
	// first, make an initial probe to get the schema update counter
	oldCounter := RetryProbeGraphQL(t, authority, headers).SchemaUpdateCounter

	// update the GraphQL schema
	gqlSchema := AssertUpdateGQLSchemaSuccess(t, authority, schema, headers)

	// now, return only after the GraphQL layer has seen the schema update.
	// This makes sure that one can make queries as per the new schema.
	AssertSchemaUpdateCounterIncrement(t, authority, oldCounter, headers)
	return gqlSchema
}

// SafelyUpdateGQLSchemaOnAlpha1 is SafelyUpdateGQLSchema for alpha1 test container.
func SafelyUpdateGQLSchemaOnAlpha1(t *testing.T, schema string) *GqlSchema {
	return SafelyUpdateGQLSchema(t, Alpha1HTTP, schema, nil)
}

// SafelyDropAllWithGroot can be used in tests for doing DROP_ALL when ACL is enabled.
// This should be used after at least one schema update operation has succeeded.
// Once the control returns from it, one can be sure that the DROP_ALL has reached
// the GraphQL layer and the existing schema has been updated to an empty schema.
func SafelyDropAllWithGroot(t *testing.T) {
	safelyDropAll(t, true)
}

// SafelyDropAll can be used in tests for doing DROP_ALL when ACL is disabled.
// This should be used after at least one schema update operation has succeeded.
// Once the control returns from it, one can be sure that the DROP_ALL has reached
// the GraphQL layer and the existing schema has been updated to an empty schema.
func SafelyDropAll(t *testing.T) {
	safelyDropAll(t, false)
}

func safelyDropAll(t *testing.T, withGroot bool) {
	// first, make an initial probe to get the schema update counter
	oldCounter := RetryProbeGraphQL(t, Alpha1HTTP, nil).SchemaUpdateCounter

	// do DROP_ALL
	var dg *dgo.Dgraph
	var err error
	if withGroot {
		dg, err = testutil.DgraphClientWithGroot(Alpha1gRPC)
	} else {
		dg, err = testutil.DgraphClient(Alpha1gRPC)
	}
	require.NoError(t, err)
	testutil.DropAll(t, dg)

	// now, return only after the GraphQL layer has seen the schema update.
	// This makes sure that one can make queries as per the new schema.
	AssertSchemaUpdateCounterIncrement(t, Alpha1HTTP, oldCounter, nil)
}

func updateGQLSchemaUsingAdminSchemaEndpt(t *testing.T, authority, schema string) string {
	resp, err := http.Post("http://"+authority+"/admin/schema", "", strings.NewReader(schema))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return string(b)
}

func retryUpdateGQLSchemaUsingAdminSchemaEndpt(t *testing.T, authority, schema string) string {
	for {
		resp := updateGQLSchemaUsingAdminSchemaEndpt(t, authority, schema)
		// return the response in case of success or a non-retryable error.
		if !containsRetryableUpdateGQLSchemaError(resp) {
			return resp
		}

		// otherwise, retry schema update
		t.Logf("Got error while updateGQLSchemaUsingAdminSchemaEndpt: %s. Retrying...\n", resp)
		time.Sleep(time.Second)
	}
}

func assertUpdateGqlSchemaUsingAdminSchemaEndpt(t *testing.T, authority, schema string, headers http.Header) {
	// first, make an initial probe to get the schema update counter
	oldCounter := RetryProbeGraphQL(t, authority, headers).SchemaUpdateCounter

	// update the GraphQL schema and assert success
	require.JSONEq(t, `{"data":{"code":"Success","message":"Done"}}`,
		retryUpdateGQLSchemaUsingAdminSchemaEndpt(t, authority, schema))

	// now, return only after the GraphQL layer has seen the schema update.
	// This makes sure that one can make queries as per the new schema.
	AssertSchemaUpdateCounterIncrement(t, authority, oldCounter, headers)
}

// JSONEqGraphQL compares two JSON strings obtained from a /graphql response.
// To avoid issues, don't use space for indentation in expected input.
//
// The comparison requirements for JSON reported by /graphql are following:
//   - The key order matters in object comparison, i.e.
//     {"hello": "world", "foo": "bar"}
//     is not same as:
//     {"foo": "bar", "hello": "world"}
//   - A key missing in an object is not same as that key present with value null, i.e.
//     {"hello": "world"}
//     is not same as:
//     {"hello": "world", "foo": null}
//   - Integers that are out of the [-(2^53)+1, (2^53)-1] precision range supported by JSON RFC,
//     should still be encoded with full precision. i.e., the number 9007199254740993 ( = 2^53 + 1)
//     should not get encoded as 9007199254740992 ( = 2^53). This happens in Go's standard JSON
//     parser due to IEEE754 precision loss for floating point numbers.
//
// The above requirements are not satisfied by the standard require.JSONEq or testutil.CompareJSON
// methods.
// In order to satisfy all these requirements, this implementation just requires that the input
// strings be equal after removing `\r`, `\n`, `\t` whitespace characters from the inputs.
// TODO:
//
//	Find a better way to do this such that order isn't mandated in list comparison.
//	So that it is actually usable at places it is not used at present.
func JSONEqGraphQL(t *testing.T, expected, actual string) {
	expected = strings.ReplaceAll(expected, "\r", "")
	expected = strings.ReplaceAll(expected, "\n", "")
	expected = strings.ReplaceAll(expected, "\t", "")

	actual = strings.ReplaceAll(actual, "\r", "")
	actual = strings.ReplaceAll(actual, "\n", "")
	actual = strings.ReplaceAll(actual, "\t", "")

	require.Equal(t, expected, actual)
}

func (twt *Tweets) DeleteByID(t *testing.T, user string, metaInfo *testutil.AuthMeta) {
	getParams := &GraphQLParams{
		Headers: GetJWT(t, user, "", metaInfo),
		Query: `
			mutation delTweets ($filter : TweetsFilter!){
			  	deleteTweets (filter: $filter) {
					numUids
			  	}
			}
		`,
		Variables: map[string]interface{}{"filter": map[string]interface{}{
			"id": map[string]interface{}{"eq": twt.Id},
		}},
	}
	gqlResponse := getParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
}

func (us *UserSecret) Delete(t *testing.T, user, role string, metaInfo *testutil.AuthMeta) {
	getParams := &GraphQLParams{
		Headers: GetJWT(t, user, role, metaInfo),
		Query: `
			mutation deleteUserSecret($ids: [ID!]) {
				deleteUserSecret(filter:{id:$ids}) {
					msg
				}
			}
		`,
		Variables: map[string]interface{}{"ids": []string{us.Id}},
	}
	gqlResponse := getParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
}

func addSchemaAndData(schema, data []byte, client *dgo.Dgraph, headers http.Header) {
	// first, make an initial probe to get the schema update counter
	oldProbe := retryProbeGraphQL(Alpha1HTTP, headers)

	// then, add the GraphQL schema
	for {
		err := addSchema(GraphqlAdminURL, string(schema))
		if err == nil {
			break
		}

		if containsRetryableUpdateGQLSchemaError(err.Error()) {
			glog.Infof("Got error while addSchemaAndData: %v. Retrying...\n", err)
			time.Sleep(time.Second)
			continue
		}

		// panic, if got a non-retryable error
		x.Panic(err)
	}

	// now, move forward only after the GraphQL layer has seen the schema update.
	// This makes sure that one can make queries as per the new schema.
	i := 0
	var newProbe *ProbeGraphQLResp
	for ; i < 10; i++ {
		newProbe = retryProbeGraphQL(Alpha1HTTP, headers)
		if newProbe.SchemaUpdateCounter > oldProbe.SchemaUpdateCounter {
			break
		}
		time.Sleep(time.Second)
	}
	// Even after atleast 10 seconds, the schema update hasn't reached GraphQL layer.
	// That indicates something fatal.
	if i == 10 {
		x.Panic(errors.Errorf(safelyUpdateGQLSchemaErr, newProbe.SchemaUpdateCounter,
			oldProbe.SchemaUpdateCounter))
	}

	err := maybePopulateData(client, data)
	if err != nil {
		x.Panic(err)
	}
}

func BootstrapServer(schema, data []byte) {
	err := CheckGraphQLStarted(GraphqlAdminURL)
	if err != nil {
		x.Panic(errors.Errorf(
			"Waited for GraphQL test server to become available, but it never did.\n"+
				"Got last error %+v", err.Error()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := grpc.DialContext(ctx, Alpha1gRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		x.Panic(err)
	}
	client := dgo.NewDgraphClient(api.NewDgraphClient(d))

	addSchemaAndData(schema, data, client, nil)
	if err = d.Close(); err != nil {
		x.Panic(err)
	}
}

// RunAll runs all the test functions in this package as sub tests.
func RunAll(t *testing.T) {
	// admin tests
	t.Run("admin", admin)
	t.Run("health", health)
	t.Run("partial health", partialHealth)
	t.Run("alias should work in admin", adminAlias)
	t.Run("state", adminState)
	t.Run("propagate client remote ip", clientInfoLogin)

	// schema tests
	t.Run("graphql descriptions", graphQLDescriptions)
	// header tests
	t.Run("touched uids header", touchedUidsHeader)
	t.Run("cache-control header", cacheControlHeader)

	// encoding
	t.Run("gzip compression", gzipCompression)
	t.Run("gzip compression header", gzipCompressionHeader)
	t.Run("gzip compression no header", gzipCompressionNoHeader)

	// query tests
	t.Run("get request", getRequest)
	t.Run("get query empty variable", getQueryEmptyVariable)
	t.Run("post request with application/graphql", queryApplicationGraphQl)
	t.Run("query by type", queryByType)
	t.Run("uid alias", uidAlias)
	t.Run("order at root", orderAtRoot)
	t.Run("page at root", pageAtRoot)
	t.Run("regexp", regExp)
	t.Run("multiple search indexes", multipleSearchIndexes)
	t.Run("multiple search indexes wrong field", multipleSearchIndexesWrongField)
	t.Run("hash search", hashSearch)
	t.Run("in filter", inFilterOnString)
	t.Run("in filter on Int", inFilterOnInt)
	t.Run("in filter on Float", inFilterOnFloat)
	t.Run("in filter on DateTime", inFilterOnDateTime)
	t.Run("between filter", betweenFilter)
	t.Run("deep between filter", deepBetweenFilter)
	t.Run("deep filter", deepFilter)
	t.Run("deep has filter", deepHasFilter)
	t.Run("many queries", manyQueries)
	t.Run("query order at root", queryOrderAtRoot)
	t.Run("queries with error", queriesWithError)
	t.Run("date filters", dateFilters)
	t.Run("float filters", floatFilters)
	t.Run("has filters", hasFilters)
	t.Run("has filter on list of fields", hasFilterOnListOfFields)
	t.Run("Int filters", int32Filters)
	t.Run("Int64 filters", int64Filters)
	t.Run("boolean filters", booleanFilters)
	t.Run("term filters", termFilters)
	t.Run("full text filters", fullTextFilters)
	t.Run("string exact filters", stringExactFilters)
	t.Run("scalar list filters", scalarListFilters)
	t.Run("skip directive", skipDirective)
	t.Run("include directive", includeDirective)
	t.Run("include and skip directive", includeAndSkipDirective)
	t.Run("query by mutliple ids", queryByMultipleIds)
	t.Run("enum filter", enumFilter)
	t.Run("default enum filter", defaultEnumFilter)
	t.Run("query by multiple invalid ids", queryByMultipleInvalidIds)
	t.Run("query typename", queryTypename)
	t.Run("query nested typename", queryNestedTypename)
	t.Run("typename for interface", typenameForInterface)
	t.Run("query only typename", queryOnlyTypename)
	t.Run("query nested only typename", querynestedOnlyTypename)
	t.Run("test onlytypename for interface types", onlytypenameForInterface)
	t.Run("entities Query on extended type with key field of type String", entitiesQueryWithKeyFieldOfTypeString)
	t.Run("entities Query on extended type with key field of type Int", entitiesQueryWithKeyFieldOfTypeInt)

	t.Run("get state by xid", getStateByXid)
	t.Run("get state without args", getStateWithoutArgs)
	t.Run("get state by both xid and uid", getStateByBothXidAndUid)
	t.Run("query state by xid", queryStateByXid)
	t.Run("query state by xid regex", queryStateByXidRegex)
	t.Run("multiple operations", multipleOperations)
	t.Run("query post with author", queryPostWithAuthor)
	t.Run("queries have extensions", queriesHaveExtensions)
	t.Run("queries with debug flag have dql query in extensions", queriesWithDebugFlagHaveDQLQueryInExtensions)
	t.Run("queries have touched_uids even if there are GraphQL errors", erroredQueriesHaveTouchedUids)
	t.Run("alias works for queries", queryWithAlias)
	t.Run("multiple aliases for same field in query", queryWithMultipleAliasOfSameField)
	t.Run("cascade directive", queryWithCascade)
	t.Run("filter in queries with array for AND/OR", filterInQueriesWithArrayForAndOr)
	t.Run("query geo near filter", queryGeoNearFilter)
	t.Run("persisted query", persistedQuery)
	t.Run("query aggregate without filter", queryAggregateWithoutFilter)
	t.Run("query aggregate with filter", queryAggregateWithFilter)
	t.Run("query aggregate on empty data", queryAggregateOnEmptyData)
	t.Run("query aggregate on empty scalar data", queryAggregateOnEmptyData2)
	t.Run("query aggregate with alias", queryAggregateWithAlias)
	t.Run("query aggregate with repeated fields", queryAggregateWithRepeatedFields)
	t.Run("query aggregate at child level", queryAggregateAtChildLevel)
	t.Run("query aggregate at child level with filter", queryAggregateAtChildLevelWithFilter)
	t.Run("query aggregate at child level with empty data", queryAggregateAtChildLevelWithEmptyData)
	t.Run("query aggregate at child level on empty scalar data", queryAggregateOnEmptyData3)
	t.Run("query aggregate at child level with multiple alias", queryAggregateAtChildLevelWithMultipleAlias)
	t.Run("query aggregate at child level with repeated fields", queryAggregateAtChildLevelWithRepeatedFields)
	t.Run("query aggregate and other fields at child level", queryAggregateAndOtherFieldsAtChildLevel)
	t.Run("query at child level with multiple alias on scalar field", queryChildLevelWithMultipleAliasOnScalarField)
	t.Run("checkUserPassword query", passwordTest)
	t.Run("query id directive with int", idDirectiveWithInt)
	t.Run("query id directive with int64", idDirectiveWithInt64)
	t.Run("query filter ID values coercion to List", queryFilterWithIDInputCoercion)
	t.Run("query multiple language Fields", queryMultipleLangFields)
	t.Run("query @id field with interface arg on interface", queryWithIDFieldAndInterfaceArg)

	// mutation tests
	t.Run("add mutation", addMutation)
	t.Run("update mutation by ids", updateMutationByIds)
	t.Run("update mutation by name", updateMutationByName)
	t.Run("update mutation by name no match", updateMutationByNameNoMatch)
	t.Run("update delete", updateRemove)
	t.Run("filter in update", filterInUpdate)
	t.Run("selection in add object", testSelectionInAddObject)
	t.Run("delete mutation with multiple ids", deleteMutationWithMultipleIds)
	t.Run("delete mutation with single id", deleteMutationWithSingleID)
	t.Run("delete mutation by name", deleteMutationByName)
	t.Run("delete mutation removes references", deleteMutationReferences)
	t.Run("add mutation updates references", addMutationReferences)
	t.Run("update set mutation updates references", updateMutationReferences)
	t.Run("delete wrong id", deleteWrongID)
	t.Run("many mutations", manyMutations)
	t.Run("mutations with deep filter", mutationWithDeepFilter)
	t.Run("many mutations with query error", manyMutationsWithQueryError)
	t.Run("query interface after add mutation", queryInterfaceAfterAddMutation)
	t.Run("add mutation with xid", addMutationWithXID)
	t.Run("deep mutations", deepMutations)
	t.Run("add multiple mutations", testMultipleMutations)
	t.Run("deep XID mutations", deepXIDMutations)
	t.Run("three level xid", testThreeLevelXID)
	t.Run("nested add mutation with multiple linked lists and @hasInverse",
		nestedAddMutationWithMultipleLinkedListsAndHasInverse)
	t.Run("add mutation with @hasInverse overrides correctly", addMutationWithHasInverseOverridesCorrectly)
	t.Run("error in multiple mutations", addMultipleMutationWithOneError)
	t.Run("dgraph directive with reverse edge adds data correctly",
		addMutationWithReverseDgraphEdge)
	t.Run("numUids test", testNumUids)
	t.Run("empty delete", mutationEmptyDelete)
	t.Run("duplicate xid in single mutation", deepMutationDuplicateXIDsSameObjectTest)
	t.Run("query typename in mutation", queryTypenameInMutation)
	t.Run("ensure alias in mutation payload", ensureAliasInMutationPayload)
	t.Run("mutations have extensions", mutationsHaveExtensions)
	t.Run("alias works for mutations", mutationsWithAlias)
	t.Run("three level deep", threeLevelDeepMutation)
	t.Run("update mutation without set & remove", updateMutationTestsWithDifferentSetRemoveCases)
	t.Run("Input coercing for int64 type", int64BoundaryTesting)
	t.Run("List of integers", intWithList)
	t.Run("Check cascade with mutation without ID field", checkCascadeWithMutationWithoutIDField)
	t.Run("Geo - Point type", mutationPointType)
	t.Run("Geo - Polygon type", mutationPolygonType)
	t.Run("Geo - MultiPolygon type", mutationMultiPolygonType)
	t.Run("filter in mutations with array for AND/OR", filterInMutationsWithArrayForAndOr)
	t.Run("filter in update mutations with array for AND/OR", filterInUpdateMutationsWithFilterAndOr)
	t.Run("mutation id directive with int", idDirectiveWithIntMutation)
	t.Run("mutation id directive with int64", idDirectiveWithInt64Mutation)
	t.Run("add mutation on extended type with field of ID type as key field", addMutationOnExtendedTypeWithIDasKeyField)
	t.Run("add mutation with deep extended type objects", addMutationWithDeepExtendedTypeObjects)
	t.Run("three level double XID mutation", threeLevelDoubleXID)
	t.Run("two levels linked to one XID", twoLevelsLinkedToXID)
	t.Run("cyclically linked mutation", cyclicMutation)
	t.Run("parallel mutations", parallelMutations)
	t.Run("input coercion to list", inputCoerciontoList)
	t.Run("multiple external Id's tests", multipleXidsTests)
	t.Run("Upsert Mutation Tests", upsertMutationTests)
	t.Run("Update language tag fields", updateLangTagFields)
	t.Run("mutation with @id field and interface arg", addMutationWithIDFieldHavingInterfaceArg)
	t.Run("xid update and nullable tests", xidUpdateAndNullableTests)

	// error tests
	t.Run("graphql completion on", graphQLCompletionOn)
	t.Run("request validation errors", requestValidationErrors)
	t.Run("panic catcher", panicCatcher)
	t.Run("deep mutation errors", deepMutationErrors)
	t.Run("not generated query, mutation using generate directive", notGeneratedAPIErrors)

	// fragment tests
	t.Run("fragment in mutation", fragmentInMutation)
	t.Run("fragment in query", fragmentInQuery)
	t.Run("fragment in query on Interface", fragmentInQueryOnInterface)
	t.Run("fragment in query on union", fragmentInQueryOnUnion)
	t.Run("fragment in query on Object", fragmentInQueryOnObject)

	// lambda tests
	t.Run("lambda on type field", lambdaOnTypeField)
	t.Run("lambda on interface field", lambdaOnInterfaceField)
	t.Run("lambda on query using dql", lambdaOnQueryUsingDql)
	t.Run("lambda on mutation using graphql", lambdaOnMutationUsingGraphQL)
	t.Run("lambda on query with no unique parents", lambdaOnQueryWithNoUniqueParents)
	t.Run("query lambda field in a mutation with duplicate @id", lambdaInMutationWithDuplicateId)
	t.Run("lambda with apollo federation", lambdaWithApolloFederation)
	t.Run("lambdaOnMutate hooks", lambdaOnMutateHooks)
}

func gunzipData(data []byte) ([]byte, error) {
	b := bytes.NewBuffer(data)

	r, err := gzip.NewReader(b)
	if err != nil {
		return nil, err
	}

	var resB bytes.Buffer
	if _, err := resB.ReadFrom(r); err != nil {
		return nil, err
	}
	return resB.Bytes(), nil
}

func gzipData(data []byte) ([]byte, error) {
	var b bytes.Buffer
	gz, err := gzip.NewWriterLevel(&b, gzip.BestSpeed)
	x.Check(err)

	if _, err := gz.Write(data); err != nil {
		return nil, err
	}

	if err := gz.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// This tests that if a request has gzip header but the body is
// not compressed, then it should return an error
func gzipCompressionHeader(t *testing.T) {
	queryCountry := &GraphQLParams{
		Query: `query {
			queryCountry {
				name
			}
		}`,
	}

	req, err := queryCountry.CreateGQLPost(GraphqlURL)
	require.NoError(t, err)

	req.Header.Set("Content-Encoding", "gzip")

	resData, err := RunGQLRequest(req)
	require.NoError(t, err)

	var result *GraphQLResponse
	err = json.Unmarshal(resData, &result)
	require.NoError(t, err)
	require.NotNil(t, result.Errors)
	require.Contains(t, result.Errors[0].Message, "Unable to parse gzip")
}

// This tests that if a req's body is compressed but the
// header is not present, then it should return an error
func gzipCompressionNoHeader(t *testing.T) {
	queryCountry := &GraphQLParams{
		Query: `query {
			queryCountry {
				name
			}
		}`,
		gzipEncoding: true,
	}

	req, err := queryCountry.CreateGQLPost(GraphqlURL)
	require.NoError(t, err)

	req.Header.Del("Content-Encoding")
	resData, err := RunGQLRequest(req)
	require.NoError(t, err)

	var result *GraphQLResponse
	err = json.Unmarshal(resData, &result)
	require.NoError(t, err)
	require.NotNil(t, result.Errors)
	require.Contains(t, result.Errors[0].Message, "Not a valid GraphQL request body")
}

func getRequest(t *testing.T) {
	add(t, getExecutor)
}

func getQueryEmptyVariable(t *testing.T) {
	queryCountry := &GraphQLParams{
		Query: `query {
			queryCountry {
				name
			}
		}`,
	}
	req, err := queryCountry.createGQLGet(GraphqlURL)
	require.NoError(t, err)

	q := req.URL.Query()
	q.Del("variables")
	req.URL.RawQuery = q.Encode()

	res := queryCountry.Execute(t, req)
	RequireNoGQLErrors(t, res)
}

// Execute takes a HTTP request from either ExecuteAsPost or ExecuteAsGet
// and executes the request
func (params *GraphQLParams) Execute(t require.TestingT, req *http.Request) *GraphQLResponse {
	for h := range params.Headers {
		req.Header.Set(h, params.Headers.Get(h))
	}
	res, err := RunGQLRequest(req)
	require.NoError(t, err)

	var result *GraphQLResponse
	if params.acceptGzip {
		res, err = gunzipData(res)
		require.NoError(t, err)
		require.Contains(t, req.Header.Get("Accept-Encoding"), "gzip")
	}
	err = json.Unmarshal(res, &result)
	require.NoError(t, err)

	return result
}

// ExecuteAsPost builds a HTTP POST request from the GraphQL input structure
// and executes the request to url.
func (params *GraphQLParams) ExecuteAsPost(t require.TestingT, url string) *GraphQLResponse {
	req, err := params.CreateGQLPost(url)
	require.NoError(t, err)

	return params.Execute(t, req)
}

// ExecuteAsPostApplicationGraphql builds an HTTP Post with type application/graphql
// Note, variables are not allowed
func (params *GraphQLParams) ExecuteAsPostApplicationGraphql(t *testing.T, url string) *GraphQLResponse {
	require.Empty(t, params.Variables)

	req, err := params.createApplicationGQLPost(url)
	require.NoError(t, err)

	return params.Execute(t, req)
}

// ExecuteAsGet builds a HTTP GET request from the GraphQL input structure
// and executes the request to url.
func (params *GraphQLParams) ExecuteAsGet(t *testing.T, url string) *GraphQLResponse {
	req, err := params.createGQLGet(url)
	require.NoError(t, err)

	return params.Execute(t, req)
}

func getExecutor(t *testing.T, url string, params *GraphQLParams) *GraphQLResponse {
	return params.ExecuteAsGet(t, url)
}

func postExecutor(t *testing.T, url string, params *GraphQLParams) *GraphQLResponse {
	return params.ExecuteAsPost(t, url)
}

func (params *GraphQLParams) createGQLGet(url string) (*http.Request, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	q := req.URL.Query()
	q.Add("query", params.Query)
	q.Add("operationName", params.OperationName)

	variableString, err := json.Marshal(params.Variables)
	if err != nil {
		return nil, err
	}
	q.Add("variables", string(variableString))

	req.URL.RawQuery = q.Encode()
	if params.acceptGzip {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	return req, nil
}

func (params *GraphQLParams) buildPostRequest(url string, body []byte, contentType string) (*http.Request, error) {
	var err error
	if params.gzipEncoding {
		if body, err = gzipData(body); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	if params.gzipEncoding {
		req.Header.Set("Content-Encoding", "gzip")
	}

	if params.acceptGzip {
		req.Header.Set("Accept-Encoding", "gzip")
	}

	return req, nil
}

func (params *GraphQLParams) CreateGQLPost(url string) (*http.Request, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	return params.buildPostRequest(url, body, "application/json")
}

func (params *GraphQLParams) createApplicationGQLPost(url string) (*http.Request, error) {
	return params.buildPostRequest(url, []byte(params.Query), "application/graphql")
}

// RunGQLRequest runs a HTTP GraphQL request and returns the data or any errors.
func RunGQLRequest(req *http.Request) ([]byte, error) {
	client := &http.Client{Timeout: 200 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	// GraphQL server should always return OK, even when there are errors
	if status := resp.StatusCode; status != http.StatusOK {
		return nil, errors.Errorf("unexpected status code: %v", status)
	}

	if strings.ToLower(resp.Header.Get("Content-Type")) != "application/json" {
		return nil, errors.Errorf("unexpected content type: %v", resp.Header.Get("Content-Type"))
	}

	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		return nil, errors.Errorf("cors headers weren't set in response")
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			glog.Warningf("error closing body: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Errorf("unable to read response body: %v", err)
	}

	return body, nil
}

func requireUID(t *testing.T, uid string) {
	_, err := strconv.ParseUint(uid, 0, 64)
	require.NoError(t, err)
}

func RequireNoGQLErrors(t *testing.T, resp *GraphQLResponse) {
	require.NotNil(t, resp)
	if resp.Errors != nil {
		t.Logf("required no GraphQL errors, but received: %s\n", resp.Errors.Error())
		debug.PrintStack()
		t.FailNow()
	}
}

func (gqlRes *GraphQLResponse) RequireNoGQLErrors(t *testing.T) {
	RequireNoGQLErrors(t, gqlRes)
}

func PopulateGraphQLData(client *dgo.Dgraph, data []byte) error {
	mu := &api.Mutation{
		CommitNow: true,
		SetJson:   data,
	}
	_, err := client.NewTxn().Mutate(context.Background(), mu)
	if err != nil {
		return errors.Wrap(err, "Unable to add GraphQL test data")
	}
	return nil
}

func maybePopulateData(client *dgo.Dgraph, data []byte) error {
	if data == nil {
		return nil
	}
	// Helps in local dev to not re-add data multiple times.
	countries, err := allCountriesAdded()
	if err != nil {
		return errors.Wrap(err, "couldn't determine if GraphQL data had already been added")
	}
	if len(countries) > 0 {
		return nil
	}
	return PopulateGraphQLData(client, data)
}

func allCountriesAdded() ([]*country, error) {
	body, err := json.Marshal(&GraphQLParams{Query: `query { queryCountry { name } }`})
	if err != nil {
		return nil, errors.Wrap(err, "unable to build GraphQL query")
	}

	req, err := http.NewRequest("POST", GraphqlURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, errors.Wrap(err, "unable to build GraphQL request")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := RunGQLRequest(req)
	if err != nil {
		return nil, errors.Wrap(err, "error running GraphQL query")
	}

	var result struct {
		Data struct {
			QueryCountry []*country
		}
	}
	err = json.Unmarshal(resp, &result)
	if err != nil {
		return nil, errors.Wrap(err, "error trying to unmarshal GraphQL query result")
	}

	return result.Data.QueryCountry, nil
}

func CheckGraphQLStarted(url string) error {
	var err error
	// Because of how GraphQL starts (it needs to read the schema from Dgraph),
	// there's no guarantee that GraphQL is available by now.  So we
	// need to try and connect and potentially retry a few times.
	for range 60 {
		_, err = hasCurrentGraphQLSchema(url)
		if err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return err
}

func hasCurrentGraphQLSchema(url string) (bool, error) {

	schemaQry := &GraphQLParams{
		Query: `query { getGQLSchema { schema } }`,
	}
	req, err := schemaQry.CreateGQLPost(url)
	if err != nil {
		return false, errors.Wrap(err, "while creating gql post")
	}

	res, err := RunGQLRequest(req)
	if err != nil {
		return false, errors.Wrap(err, "error running GraphQL query")
	}

	var result *GraphQLResponse
	err = json.Unmarshal(res, &result)
	if err != nil {
		return false, errors.Wrap(err, "error unmarshalling result")
	}

	if len(result.Errors) > 0 {
		return false, result.Errors
	}

	var sch struct {
		GetGQLSchema struct {
			Schema string
		}
	}

	err = json.Unmarshal(result.Data, &sch)
	if err != nil {
		return false, errors.Wrap(err, "error trying to unmarshal GraphQL query result")
	}

	if sch.GetGQLSchema.Schema == "" {
		return false, nil
	}

	return true, nil
}

func addSchema(url, schema string) error {
	add := &GraphQLParams{
		Query: `mutation updateGQLSchema($sch: String!) {
			updateGQLSchema(input: { set: { schema: $sch }}) {
				gqlSchema {
					schema
				}
			}
		}`,
		Variables: map[string]interface{}{"sch": schema},
	}
	req, err := add.CreateGQLPost(url)
	if err != nil {
		return errors.Wrap(err, "error creating GraphQL query")
	}

	resp, err := RunGQLRequest(req)
	if err != nil {
		return errors.Wrap(err, "error running GraphQL query")
	}

	var addResult struct {
		Data struct {
			UpdateGQLSchema struct {
				GQLSchema struct {
					Schema string
				}
			}
		}
		Errors []interface{}
	}

	err = json.Unmarshal(resp, &addResult)
	if err != nil {
		return errors.Wrap(err, "error trying to unmarshal GraphQL mutation result")
	}

	if len(addResult.Errors) > 0 {
		return errors.Errorf("%v", addResult.Errors)
	}

	if addResult.Data.UpdateGQLSchema.GQLSchema.Schema != schema {
		return errors.New("GraphQL schema mutation failed")
	}

	return nil
}

func GetJWT(t *testing.T, user, role interface{}, metaInfo *testutil.AuthMeta) http.Header {
	metaInfo.AuthVars = map[string]interface{}{}
	if user != nil {
		metaInfo.AuthVars["USER"] = user
	}

	if role != nil {
		metaInfo.AuthVars["ROLE"] = role
	}

	require.NotNil(t, metaInfo.PrivateKeyPath)
	jwtToken, err := metaInfo.GetSignedToken(metaInfo.PrivateKeyPath, 300*time.Second)
	require.NoError(t, err)

	h := make(http.Header)
	h.Add(metaInfo.Header, jwtToken)
	return h
}

func GetJWTWithNullUser(t *testing.T, role interface{}, metaInfo *testutil.AuthMeta) http.Header {
	metaInfo.AuthVars = map[string]interface{}{}
	metaInfo.AuthVars["USER"] = nil
	metaInfo.AuthVars["ROLE"] = role
	require.NotNil(t, metaInfo.PrivateKeyPath)
	jwtToken, err := metaInfo.GetSignedToken(metaInfo.PrivateKeyPath, 300*time.Second)
	require.NoError(t, err)
	h := make(http.Header)
	h.Add(metaInfo.Header, jwtToken)
	return h
}

func GetJWTForInterfaceAuth(t *testing.T, user, role string, ans bool, metaInfo *testutil.AuthMeta) http.Header {
	metaInfo.AuthVars = map[string]interface{}{}
	if user != "" {
		metaInfo.AuthVars["USER"] = user
	}

	if role != "" {
		metaInfo.AuthVars["ROLE"] = role
	}

	metaInfo.AuthVars["ANS"] = ans

	require.NotNil(t, metaInfo.PrivateKeyPath)
	jwtToken, err := metaInfo.GetSignedToken(metaInfo.PrivateKeyPath, 300*time.Second)
	require.NoError(t, err)
	h := make(http.Header)
	h.Add(metaInfo.Header, jwtToken)
	return h
}

func BootstrapAuthData() ([]byte, []byte) {
	schemaFile := "../auth/schema.graphql"
	schema, err := os.ReadFile(schemaFile)
	x.Panic(err)

	jsonFile := "../auth/test_data.json"
	data, err := os.ReadFile(jsonFile)
	if err != nil {
		panic(errors.Wrapf(err, "Unable to read file %s.", jsonFile))
	}
	return schema, data
}
