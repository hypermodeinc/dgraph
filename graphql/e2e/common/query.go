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

//nolint:lll
package common

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/spf13/cast"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/graphql/schema"
	"github.com/hypermodeinc/dgraph/v24/testutil"
	"github.com/hypermodeinc/dgraph/v24/x"
)

func queryCountryByRegExp(t *testing.T, regexp string, expectedCountries []*country) {
	getCountryParams := &GraphQLParams{
		Query: `query queryCountry($regexp: String!) {
			queryCountry(filter: { name: { regexp: $regexp } }) {
				name
			}
		}`,
		Variables: map[string]interface{}{"regexp": regexp},
	}

	gqlResponse := getCountryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var expected, result struct {
		QueryCountry []*country
	}
	expected.QueryCountry = expectedCountries
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)

	countrySort := func(i, j int) bool {
		return result.QueryCountry[i].Name < result.QueryCountry[j].Name
	}
	sort.Slice(result.QueryCountry, countrySort)

	if diff := cmp.Diff(expected, result); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
}

func touchedUidsHeader(t *testing.T) {
	query := &GraphQLParams{
		Query: `query {
			queryCountry {
				name
			}
		}`,
	}
	req, err := query.CreateGQLPost(GraphqlURL)
	require.NoError(t, err)

	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	// confirm that the header value is a non-negative integer
	touchedUidsInHeader, err := strconv.ParseUint(resp.Header.Get("Graphql-TouchedUids"), 10, 64)
	require.NoError(t, err)
	require.Greater(t, touchedUidsInHeader, uint64(0))

	// confirm that the value in header is same as the value in body
	var gqlResp GraphQLResponse
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &gqlResp))
	require.Equal(t, touchedUidsInHeader, uint64(gqlResp.Extensions["touched_uids"].(float64)))
}

func cacheControlHeader(t *testing.T) {
	query := &GraphQLParams{
		Query: `query @cacheControl(maxAge: 5) {
			queryCountry {
				name
			}
		}`,
	}
	req, err := query.CreateGQLPost(GraphqlURL)
	require.NoError(t, err)

	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	// confirm that the header value is a non-negative integer
	require.Equal(t, "public,max-age=5", resp.Header.Get("Cache-Control"))
	require.Equal(t, "Accept-Encoding", resp.Header.Get("Vary"))
}

// This test checks that all the different combinations of
// request sending compressed / uncompressed query and receiving
// compressed / uncompressed result.
func gzipCompression(t *testing.T) {
	r := []bool{false, true}
	for _, acceptGzip := range r {
		for _, gzipEncoding := range r {
			t.Run(fmt.Sprintf("TestQueryByType acceptGzip=%t gzipEncoding=%t",
				acceptGzip, gzipEncoding), func(t *testing.T) {

				queryByTypeWithEncoding(t, acceptGzip, gzipEncoding)
			})
		}
	}
}

func queryByType(t *testing.T) {
	queryByTypeWithEncoding(t, true, true)
}

func queryByTypeWithEncoding(t *testing.T, acceptGzip, gzipEncoding bool) {
	queryCountry := &GraphQLParams{
		Query: `query {
			queryCountry {
				name
			}
		}`,
		acceptGzip:   acceptGzip,
		gzipEncoding: gzipEncoding,
	}

	gqlResponse := queryCountry.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var expected, result struct {
		QueryCountry []*country
	}
	expected.QueryCountry = []*country{
		{Name: "Angola"},
		{Name: "Bangladesh"},
		{Name: "India"},
		{Name: "Mozambique"},
	}
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)

	sort.Slice(result.QueryCountry, func(i, j int) bool {
		return result.QueryCountry[i].Name < result.QueryCountry[j].Name
	})

	if diff := cmp.Diff(expected, result); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
}

func uidAlias(t *testing.T) {
	queryCountry := &GraphQLParams{
		Query: `query {
			queryCountry(order: { asc: name }) {
				uid: name
			}
		}`,
	}
	type countryUID struct {
		UID string
	}

	gqlResponse := queryCountry.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var expected, result struct {
		QueryCountry []*countryUID
	}
	expected.QueryCountry = []*countryUID{
		{UID: "Angola"},
		{UID: "Bangladesh"},
		{UID: "India"},
		{UID: "Mozambique"},
	}
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)

	if diff := cmp.Diff(expected, result); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
}

func orderAtRoot(t *testing.T) {
	queryCountry := &GraphQLParams{
		Query: `query {
			queryCountry(order: { asc: name }) {
				name
			}
		}`,
	}

	gqlResponse := queryCountry.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var expected, result struct {
		QueryCountry []*country
	}
	expected.QueryCountry = []*country{
		{Name: "Angola"},
		{Name: "Bangladesh"},
		{Name: "India"},
		{Name: "Mozambique"},
	}
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)

	if diff := cmp.Diff(expected, result); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
}

func pageAtRoot(t *testing.T) {
	queryCountry := &GraphQLParams{
		Query: `query {
			queryCountry(order: { desc: name }, first: 2, offset: 1) {
				name
			}
		}`,
	}

	gqlResponse := queryCountry.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var expected, result struct {
		QueryCountry []*country
	}
	expected.QueryCountry = []*country{
		{Name: "India"},
		{Name: "Bangladesh"},
	}
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)

	if diff := cmp.Diff(expected, result); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
}

func regExp(t *testing.T) {
	queryCountryByRegExp(t, "/[Aa]ng/",
		[]*country{
			{Name: "Angola"},
			{Name: "Bangladesh"},
		})
}

func multipleSearchIndexes(t *testing.T) {
	query := `query queryPost($filter: PostFilter){
		  queryPost (filter: $filter) {
		      title
		  }
	}`

	testCases := []interface{}{
		map[string]interface{}{"title": map[string]interface{}{"anyofterms": "Introducing"}},
		map[string]interface{}{"title": map[string]interface {
		}{"alloftext": "Introducing GraphQL in Dgraph"}},
	}
	for _, filter := range testCases {
		getCountryParams := &GraphQLParams{
			Query:     query,
			Variables: map[string]interface{}{"filter": filter},
		}

		gqlResponse := getCountryParams.ExecuteAsPost(t, GraphqlURL)
		RequireNoGQLErrors(t, gqlResponse)

		var expected, result struct {
			QueryPost []*post
		}

		expected.QueryPost = []*post{
			{Title: "Introducing GraphQL in Dgraph"},
		}
		err := json.Unmarshal([]byte(gqlResponse.Data), &result)
		require.NoError(t, err)

		if diff := cmp.Diff(expected, result); diff != "" {
			t.Errorf("result mismatch (-want +got):\n%s", diff)
		}
	}
}

func multipleSearchIndexesWrongField(t *testing.T) {
	queryPostParams := &GraphQLParams{
		Query: `query {
			queryPost (filter: {title : { regexp : "/Introducing.*$/" }} ) {
			    title
			}
		}`,
	}

	gqlResponse := queryPostParams.ExecuteAsPost(t, GraphqlURL)
	require.NotNil(t, gqlResponse.Errors)

	expected := `Field "regexp" is not defined by type StringFullTextFilter_StringTermFilter`
	require.Contains(t, gqlResponse.Errors[0].Error(), expected)
}

func hashSearch(t *testing.T) {
	queryAuthorParams := &GraphQLParams{
		Query: `query {
			queryAuthor(filter: { name: { eq: "Ann Author" } }) {
				name
				dob
			}
		}`,
	}

	gqlResponse := queryAuthorParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var expected, result struct {
		QueryAuthor []*author
	}
	dob := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	expected.QueryAuthor = []*author{{Name: "Ann Author", Dob: &dob}}
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)

	if diff := cmp.Diff(expected, result); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
}

func allPosts(t *testing.T) []*post {
	queryPostParams := &GraphQLParams{
		Query: `query {
			queryPost {
				postID
				title
				text
				tags
				numLikes
				isPublished
				postType
			}
		}`,
	}
	gqlResponse := queryPostParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result struct {
		QueryPost []*post
	}
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)

	require.Equal(t, 4, len(result.QueryPost))

	return result.QueryPost
}

func entitiesQueryWithKeyFieldOfTypeString(t *testing.T) {
	addSpaceShipParams := &GraphQLParams{
		Query: `mutation addSpaceShip($id1: String!, $id2: String!, $id3: String!, $id4: String! ) {
			addSpaceShip(input: [{id: $id1, missions: [{id: "Mission1", designation: "Apollo1"}]},{id: $id2, missions: [{id: "Mission2", designation: "Apollo2"}]},{id: $id3, missions: [{id: "Mission3", designation: "Apollo3"}]}, {id: $id4, missions: [{id: "Mission4", designation: "Apollo4"}]}]){
				spaceShip {
					id
					missions {
						id
						designation
					}
				}
			}
		}`,
		Variables: map[string]interface{}{
			"id1": "SpaceShip1",
			"id2": "SpaceShip2",
			"id3": "SpaceShip3",
			"id4": "SpaceShip4",
		},
	}

	gqlResponse := addSpaceShipParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	entitiesQueryParams := &GraphQLParams{
		Query: `query _entities($typeName: String!, $id1: String!, $id2: String!, $id3: String!, $id4: String!){
			_entities(representations: [{__typename: $typeName, id: $id4},{__typename: $typeName, id: $id2},{__typename: $typeName, id: $id1},{__typename: $typeName, id: $id3},{__typename: $typeName, id: $id1}]) {
				... on SpaceShip {
					missions(order: {asc: id}){
						id
						designation
					}
				}
			}
		}`,
		Variables: map[string]interface{}{
			"typeName": "SpaceShip",
			"id1":      "SpaceShip1",
			"id2":      "SpaceShip2",
			"id3":      "SpaceShip3",
			"id4":      "SpaceShip4",
		},
	}

	entitiesResp := entitiesQueryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, entitiesResp)

	expectedJSON := `{"_entities":[{"missions":[{"designation":"Apollo4","id":"Mission4"}]},{"missions":[{"designation":"Apollo2","id":"Mission2"}]},{"missions":[{"designation":"Apollo1","id":"Mission1"}]},{"missions":[{"designation":"Apollo3","id":"Mission3"}]},{"missions":[{"designation":"Apollo1","id":"Mission1"}]}]}`

	JSONEqGraphQL(t, expectedJSON, string(entitiesResp.Data))

	spaceShipDeleteFilter := map[string]interface{}{
		"id": map[string]interface{}{"in": []string{"SpaceShip1", "SpaceShip2", "SpaceShip3", "SpaceShip4"}},
	}
	DeleteGqlType(t, "SpaceShip", spaceShipDeleteFilter, 4, nil)

	missionDeleteFilter := map[string]interface{}{
		"id": map[string]interface{}{"in": []string{"Mission1", "Mission2", "Mission3", "Mission4"}},
	}
	DeleteGqlType(t, "Mission", missionDeleteFilter, 4, nil)

}

func entitiesQueryWithKeyFieldOfTypeInt(t *testing.T) {
	addPlanetParams := &GraphQLParams{
		Query: `mutation {
			addPlanet(input: [{id: 1, missions: [{id: "Mission1", designation: "Apollo1"}]},{id: 2, missions: [{id: "Mission2", designation: "Apollo2"}]},{id: 3, missions: [{id: "Mission3", designation: "Apollo3"}]}, {id: 4, missions: [{id: "Mission4", designation: "Apollo4"}]}]){
				planet {
					id
					missions {
						id
						designation
					}
				}
			}
		}`,
	}

	gqlResponse := addPlanetParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	entitiesQueryParams := &GraphQLParams{
		Query: `query _entities($typeName: String!, $id1: Int!, $id2: Int!, $id3: Int!, $id4: Int!){
			_entities(representations: [{__typename: $typeName, id: $id4},{__typename: $typeName, id: $id2},{__typename: $typeName, id: $id1},{__typename: $typeName, id: $id3},{__typename: $typeName, id: $id1}]) {
				... on Planet {
					missions(order: {asc: id}){
						id
						designation
					}
				}
			}
		}`,
		Variables: map[string]interface{}{
			"typeName": "Planet",
			"id1":      1,
			"id2":      2,
			"id3":      3,
			"id4":      4,
		},
	}

	entitiesResp := entitiesQueryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, entitiesResp)

	expectedJSON := `{"_entities":[{"missions":[{"designation":"Apollo4","id":"Mission4"}]},{"missions":[{"designation":"Apollo2","id":"Mission2"}]},{"missions":[{"designation":"Apollo1","id":"Mission1"}]},{"missions":[{"designation":"Apollo3","id":"Mission3"}]},{"missions":[{"designation":"Apollo1","id":"Mission1"}]}]}`

	JSONEqGraphQL(t, expectedJSON, string(entitiesResp.Data))

	planetDeleteFilter := map[string]interface{}{"id": map[string]interface{}{"in": []int{1, 2, 3, 4}}}
	DeleteGqlType(t, "Planet", planetDeleteFilter, 4, nil)

	missionDeleteFilter := map[string]interface{}{
		"id": map[string]interface{}{"in": []string{"Mission1", "Mission2", "Mission3", "Mission4"}},
	}
	DeleteGqlType(t, "Mission", missionDeleteFilter, 4, nil)

}

func inFilterOnString(t *testing.T) {
	addStateParams := &GraphQLParams{
		Query: `mutation addState($name1: String!, $code1: String!, $name2: String!, $code2: String! ) {
			addState(input: [{name: $name1, xcode: $code1},{name: $name2, xcode: $code2}]) {
				state {
					xcode
					name
				}
			}
		}`,

		Variables: map[string]interface{}{
			"name1": "A State",
			"code1": "abc",
			"name2": "B State",
			"code2": "def",
		},
	}

	gqlResponse := addStateParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	updateStateParams := &GraphQLParams{
		Query: `mutation{
			updateState(input: {
				filter: {
					xcode: { in: ["abc", "def"]}},
				set: {
					capital: "Common Capital"} }){
				state{
					xcode
					name
					capital
				}
			}
		  }`,
	}
	gqlResponse = updateStateParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	getStateParams := &GraphQLParams{
		Query: `query{
			queryState(filter: {xcode: {in: ["abc", "def"]}}, order: { asc: name }){
				xcode
				name
				capital
			}
		}`,
	}

	gqlResponse = getStateParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result struct {
		QueryState []*state
	}
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)
	require.Equal(t, 2, len(result.QueryState))
	queriedResult := map[string]*state{}
	queriedResult[result.QueryState[0].Name] = result.QueryState[0]
	queriedResult[result.QueryState[1].Name] = result.QueryState[1]

	state1 := &state{
		Name:    "A State",
		Code:    "abc",
		Capital: "Common Capital",
	}
	state2 := &state{
		Name:    "B State",
		Code:    "def",
		Capital: "Common Capital",
	}

	if diff := cmp.Diff(state1, queriedResult[state1.Name]); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff(state2, queriedResult[state2.Name]); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}

	deleteFilter := map[string]interface{}{"xcode": map[string]interface{}{"in": []string{"abc", "def"}}}
	DeleteGqlType(t, "State", deleteFilter, 2, nil)
}

func inFilterOnInt(t *testing.T) {
	queryPostParams := &GraphQLParams{
		Query: `query {
			queryPost(filter: {numLikes: {in: [1, 77, 100, 150, 200]}}) {
				title
				numLikes
			}
		}`,
	}

	gqlResponse := queryPostParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result struct {
		QueryPost []*post
	}

	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)
	require.Equal(t, 3, len(result.QueryPost))
}

func inFilterOnFloat(t *testing.T) {
	queryAuthorParams := &GraphQLParams{
		Query: `query {
			queryAuthor(filter: {reputation: {in: [6.6, 8.9, 9.5]}}) {
				name
			}
		}`,
	}

	gqlResponse := queryAuthorParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result struct {
		QueryAuthor []*author
	}

	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)
	require.Equal(t, 2, len(result.QueryAuthor))
}

func inFilterOnDateTime(t *testing.T) {
	queryAuthorParams := &GraphQLParams{
		Query: `query {
			queryAuthor(filter: {dob: {in: ["2001-01-01","2002-02-01", "2005-01-01"]}}) {
				name
			}
		}`,
	}

	gqlResponse := queryAuthorParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result struct {
		QueryAuthor []*author
	}

	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)
	require.Equal(t, 1, len(result.QueryAuthor))
}

func betweenFilter(t *testing.T) {
	queryPostParams := &GraphQLParams{
		Query: `query {
			queryPost(filter: {numLikes: {between: {min:90, max:100}}}) {
				title
				numLikes
			}
		}`,
	}

	gqlResponse := queryPostParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result struct {
		QueryPost []*post
	}

	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)
	require.Equal(t, 1, len(result.QueryPost))

	expected := &post{
		Title:    "Introducing GraphQL in Dgraph",
		NumLikes: 100,
	}

	if diff := cmp.Diff(expected, result.QueryPost[0]); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
}

func deepBetweenFilter(t *testing.T) {
	queryPostParams := &GraphQLParams{
		Query: `query{
			queryAuthor(filter: {reputation: {between: {min:6.0, max: 7.2}}}){
			  name
			  reputation
			  posts(filter: {topic: {between: {min: "GraphQL", max: "GraphQL+-"}}}){
				title
				topic
			  }
			}
		  }`,
	}

	gqlResponse := queryPostParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result struct {
		QueryAuthor []*author
	}

	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)
	require.Equal(t, 1, len(result.QueryAuthor))

	expected := &author{
		Name:       "Ann Author",
		Reputation: 6.6,
		Posts:      []*post{{Title: "Introducing GraphQL in Dgraph", Topic: "GraphQL"}},
	}

	if diff := cmp.Diff(expected, result.QueryAuthor[0]); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}

}

func deepFilter(t *testing.T) {
	getAuthorParams := &GraphQLParams{
		Query: `query {
			queryAuthor(filter: { name: { eq: "Ann Other Author" } }) {
				name
				posts(filter: { title: { anyofterms: "GraphQL" } }) {
					title
				}
			}
		}`,
	}

	gqlResponse := getAuthorParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result struct {
		QueryAuthor []*author
	}
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)
	require.Equal(t, 1, len(result.QueryAuthor))

	expected := &author{
		Name:  "Ann Other Author",
		Posts: []*post{{Title: "Learning GraphQL in Dgraph"}},
	}

	if diff := cmp.Diff(expected, result.QueryAuthor[0]); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
}

func deepHasFilter(t *testing.T) {
	newCountry := addCountry(t, postExecutor)
	newAuthor := addAuthor(t, newCountry.ID, postExecutor)
	newPost1 := addPostWithNullText(t, newAuthor.ID, newCountry.ID, postExecutor)
	newPost2 := addPost(t, newAuthor.ID, newCountry.ID, postExecutor)
	getAuthorParams := &GraphQLParams{
		Query: `query {
			queryAuthor(filter: { name: { eq: "Test Author" } }) {
				name
				posts(filter: {not :{ has : text } }) {
					title
				}
			}
		}`,
	}

	gqlResponse := getAuthorParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result struct {
		QueryAuthor []*author
	}
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)
	require.Equal(t, 1, len(result.QueryAuthor))

	expected := &author{
		Name:  "Test Author",
		Posts: []*post{{Title: "No text"}},
	}

	if diff := cmp.Diff(expected, result.QueryAuthor[0]); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
	cleanUp(t, []*country{newCountry}, []*author{newAuthor}, []*post{newPost1, newPost2})
}

// manyQueries runs multiple queries in the one block.  Internally, the GraphQL
// server should run those concurrently, but the results should be returned in the
// requested order.  This makes sure those many test runs are reassembled correctly.
func manyQueries(t *testing.T) {
	posts := allPosts(t)

	getPattern := `getPost(postID: "%s") {
		postID
		title
		text
		tags
		isPublished
		postType
		numLikes
	}
	`

	var bld strings.Builder
	x.Check2(bld.WriteString("query {\n"))
	for idx, p := range posts {
		x.Check2(bld.WriteString(fmt.Sprintf("  query%v : ", idx)))
		x.Check2(bld.WriteString(fmt.Sprintf(getPattern, p.PostID)))
	}
	x.Check2(bld.WriteString("}"))

	queryParams := &GraphQLParams{
		Query: bld.String(),
	}

	gqlResponse := queryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result map[string]*post
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)

	for idx, expectedPost := range posts {
		resultPost := result[fmt.Sprintf("query%v", idx)]
		if diff := cmp.Diff(expectedPost, resultPost); diff != "" {
			t.Errorf("result mismatch (-want +got):\n%s", diff)
		}
	}
}

func queryOrderAtRoot(t *testing.T) {
	posts := allPosts(t)

	answers := make([]*post, 2)
	for _, p := range posts {
		if p.NumLikes == 77 {
			answers[0] = p
		} else if p.NumLikes == 100 {
			answers[1] = p
		}
	}

	filter := map[string]interface{}{
		"postID": []string{answers[0].PostID, answers[1].PostID},
	}

	orderLikesDesc := map[string]interface{}{
		"desc": "numLikes",
	}

	orderLikesAsc := map[string]interface{}{
		"asc": "numLikes",
	}

	var result, expected struct {
		QueryPost []*post
	}

	cases := map[string]struct {
		Order    map[string]interface{}
		First    int
		Offset   int
		Expected []*post
	}{
		"orderAsc": {
			Order:    orderLikesAsc,
			First:    2,
			Offset:   0,
			Expected: []*post{answers[0], answers[1]},
		},
		"orderDesc": {
			Order:    orderLikesDesc,
			First:    2,
			Offset:   0,
			Expected: []*post{answers[1], answers[0]},
		},
		"first": {
			Order:    orderLikesDesc,
			First:    1,
			Offset:   0,
			Expected: []*post{answers[1]},
		},
		"offset": {
			Order:    orderLikesDesc,
			First:    2,
			Offset:   1,
			Expected: []*post{answers[0]},
		},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			getParams := &GraphQLParams{
				Query: `query queryPost($filter: PostFilter, $order: PostOrder,
		$first: Int, $offset: Int) {
			queryPost(
			  filter: $filter,
			  order: $order,
			  first: $first,
			  offset: $offset) {
				postID
				title
				text
				tags
				isPublished
				postType
				numLikes
			}
		}
		`,
				Variables: map[string]interface{}{
					"filter": filter,
					"order":  test.Order,
					"first":  test.First,
					"offset": test.Offset,
				},
			}

			gqlResponse := getParams.ExecuteAsPost(t, GraphqlURL)
			RequireNoGQLErrors(t, gqlResponse)

			expected.QueryPost = test.Expected
			err := json.Unmarshal([]byte(gqlResponse.Data), &result)
			require.NoError(t, err)

			require.Equal(t, len(result.QueryPost), len(expected.QueryPost))
			if diff := cmp.Diff(expected, result); diff != "" {
				t.Errorf("result mismatch (-want +got):\n%s", diff)
			}
		})
	}

}

// queriesWithError runs multiple queries in the one block with
// an error.  Internally, the GraphQL server should run those concurrently, and
// an error in one query should not affect the results of any others.
func queriesWithError(t *testing.T) {
	posts := allPosts(t)

	getPattern := `getPost(postID: "%s") {
		postID
		title
		text
		tags
		isPublished
		postType
		numLikes
	}
	`

	// make one random query fail
	shouldFail := rand.New(rand.NewSource(time.Now().UnixNano())).Intn(len(posts))

	var bld strings.Builder
	x.Check2(bld.WriteString("query {\n"))
	for idx, p := range posts {
		x.Check2(bld.WriteString(fmt.Sprintf("  query%v : ", idx)))
		if idx == shouldFail {
			x.Check2(bld.WriteString(fmt.Sprintf(getPattern, "Not_An_ID")))
		} else {
			x.Check2(bld.WriteString(fmt.Sprintf(getPattern, p.PostID)))
		}
	}
	x.Check2(bld.WriteString("}"))

	queryParams := &GraphQLParams{
		Query: bld.String(),
	}

	gqlResponse := queryParams.ExecuteAsPost(t, GraphqlURL)
	require.Len(t, gqlResponse.Errors, 1, "expected 1 error from malformed query")

	var result map[string]*post
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)

	for idx, expectedPost := range posts {
		resultPost := result[fmt.Sprintf("query%v", idx)]
		if idx == shouldFail {
			require.Nil(t, resultPost, "expected this query to fail and return nil")
		} else {
			if diff := cmp.Diff(expectedPost, resultPost); diff != "" {
				t.Errorf("result mismatch (-want +got):\n%s", diff)
			}
		}
	}
}

func dateFilters(t *testing.T) {
	cases := map[string]struct {
		Filter   interface{}
		Expected []*author
	}{
		"less than": {
			Filter:   map[string]interface{}{"dob": map[string]interface{}{"lt": "2000-01-01"}},
			Expected: []*author{{Name: "Ann Other Author"}}},
		"less or equal": {
			Filter:   map[string]interface{}{"dob": map[string]interface{}{"le": "2000-01-01"}},
			Expected: []*author{{Name: "Ann Author"}, {Name: "Ann Other Author"}}},
		"equal": {
			Filter:   map[string]interface{}{"dob": map[string]interface{}{"eq": "2000-01-01"}},
			Expected: []*author{{Name: "Ann Author"}}},
		"greater or equal": {
			Filter:   map[string]interface{}{"dob": map[string]interface{}{"ge": "2000-01-01"}},
			Expected: []*author{{Name: "Ann Author"}, {Name: "Three Author"}}},
		"greater than": {
			Filter:   map[string]interface{}{"dob": map[string]interface{}{"gt": "2000-01-01"}},
			Expected: []*author{{Name: "Three Author"}}},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			authorTest(t, test.Filter, test.Expected)
		})
	}
}

func floatFilters(t *testing.T) {
	cases := map[string]struct {
		Filter   interface{}
		Expected []*author
	}{
		"less than": {
			Filter:   map[string]interface{}{"reputation": map[string]interface{}{"lt": 8.9}},
			Expected: []*author{{Name: "Ann Author"}}},
		"less or equal": {
			Filter:   map[string]interface{}{"reputation": map[string]interface{}{"le": 8.9}},
			Expected: []*author{{Name: "Ann Author"}, {Name: "Ann Other Author"}}},
		"equal": {
			Filter:   map[string]interface{}{"reputation": map[string]interface{}{"eq": 8.9}},
			Expected: []*author{{Name: "Ann Other Author"}}},
		"greater or equal": {
			Filter:   map[string]interface{}{"reputation": map[string]interface{}{"ge": 8.9}},
			Expected: []*author{{Name: "Ann Other Author"}, {Name: "Three Author"}}},
		"greater than": {
			Filter:   map[string]interface{}{"reputation": map[string]interface{}{"gt": 8.9}},
			Expected: []*author{{Name: "Three Author"}}},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			authorTest(t, test.Filter, test.Expected)
		})
	}
}

func authorTest(t *testing.T, filter interface{}, expected []*author) {
	queryParams := &GraphQLParams{
		Query: `query filterVariable($filter: AuthorFilter) {
			queryAuthor(filter: $filter, order: { asc: name }) {
				name
			}
		}`,
		Variables: map[string]interface{}{"filter": filter},
	}

	gqlResponse := queryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result struct {
		QueryAuthor []*author
	}
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)

	if diff := cmp.Diff(expected, result.QueryAuthor); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
}

func int32Filters(t *testing.T) {
	cases := map[string]struct {
		Filter   interface{}
		Expected []*post
	}{
		"less than": {
			Filter: map[string]interface{}{"numLikes": map[string]interface{}{"lt": 87}},
			Expected: []*post{
				{Title: "GraphQL doco"},
				{Title: "Random post"}}},
		"less or equal": {
			Filter: map[string]interface{}{"numLikes": map[string]interface{}{"le": 87}},
			Expected: []*post{
				{Title: "GraphQL doco"},
				{Title: "Learning GraphQL in Dgraph"},
				{Title: "Random post"}}},
		"equal": {
			Filter:   map[string]interface{}{"numLikes": map[string]interface{}{"eq": 87}},
			Expected: []*post{{Title: "Learning GraphQL in Dgraph"}}},
		"greater or equal": {
			Filter: map[string]interface{}{"numLikes": map[string]interface{}{"ge": 87}},
			Expected: []*post{
				{Title: "Introducing GraphQL in Dgraph"},
				{Title: "Learning GraphQL in Dgraph"}}},
		"greater than": {
			Filter:   map[string]interface{}{"numLikes": map[string]interface{}{"gt": 87}},
			Expected: []*post{{Title: "Introducing GraphQL in Dgraph"}}},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			postTest(t, test.Filter, test.Expected)
		})
	}
}

func hasFilters(t *testing.T) {
	newCountry := addCountry(t, postExecutor)
	newAuthor := addAuthor(t, newCountry.ID, postExecutor)
	newPost := addPostWithNullText(t, newAuthor.ID, newCountry.ID, postExecutor)

	Filter := map[string]interface{}{"has": "text"}
	Expected := []*post{
		{Title: "GraphQL doco"},
		{Title: "Introducing GraphQL in Dgraph"},
		{Title: "Learning GraphQL in Dgraph"},
		{Title: "Random post"}}

	postTest(t, Filter, Expected)
	cleanUp(t, []*country{newCountry}, []*author{newAuthor}, []*post{newPost})
}

func hasFilterOnListOfFields(t *testing.T) {
	newCountry := addCountry(t, postExecutor)
	newAuthor := addAuthor(t, newCountry.ID, postExecutor)
	newPost := addPostWithNullText(t, newAuthor.ID, newCountry.ID, postExecutor)
	Filter := map[string]interface{}{"not": map[string]interface{}{"has": []interface{}{"text", "numViews"}}}
	Expected := []*post{
		{Title: "No text"},
	}
	postTest(t, Filter, Expected)
	cleanUp(t, []*country{newCountry}, []*author{newAuthor}, []*post{newPost})
}

func int64Filters(t *testing.T) {
	cases := map[string]struct {
		Filter   interface{}
		Expected []*post
	}{
		"less than": {
			Filter: map[string]interface{}{"numViews": map[string]interface{}{"lt": 274877906944}},
			Expected: []*post{
				{Title: "GraphQL doco"},
				{Title: "Random post"}}},
		"less or equal": {
			Filter: map[string]interface{}{"numViews": map[string]interface{}{"le": 274877906944}},
			Expected: []*post{
				{Title: "GraphQL doco"},
				{Title: "Learning GraphQL in Dgraph"},
				{Title: "Random post"}}},
		"equal": {
			Filter:   map[string]interface{}{"numViews": map[string]interface{}{"eq": 274877906944}},
			Expected: []*post{{Title: "Learning GraphQL in Dgraph"}}},
		"greater or equal": {
			Filter: map[string]interface{}{"numViews": map[string]interface{}{"ge": 274877906944}},
			Expected: []*post{
				{Title: "Introducing GraphQL in Dgraph"},
				{Title: "Learning GraphQL in Dgraph"}}},
		"greater than": {
			Filter:   map[string]interface{}{"numViews": map[string]interface{}{"gt": 274877906944}},
			Expected: []*post{{Title: "Introducing GraphQL in Dgraph"}}},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			postTest(t, test.Filter, test.Expected)
		})
	}
}

func booleanFilters(t *testing.T) {
	cases := map[string]struct {
		Filter   interface{}
		Expected []*post
	}{
		"true": {
			Filter: map[string]interface{}{"isPublished": true},
			Expected: []*post{
				{Title: "GraphQL doco"},
				{Title: "Introducing GraphQL in Dgraph"},
				{Title: "Learning GraphQL in Dgraph"}}},
		"false": {
			Filter:   map[string]interface{}{"isPublished": false},
			Expected: []*post{{Title: "Random post"}}},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			postTest(t, test.Filter, test.Expected)
		})
	}
}

func termFilters(t *testing.T) {
	cases := map[string]struct {
		Filter   interface{}
		Expected []*post
	}{
		"all of terms": {
			Filter: map[string]interface{}{
				"title": map[string]interface{}{"allofterms": "GraphQL Dgraph"}},
			Expected: []*post{
				{Title: "Introducing GraphQL in Dgraph"},
				{Title: "Learning GraphQL in Dgraph"}}},
		"any of terms": {
			Filter: map[string]interface{}{
				"title": map[string]interface{}{"anyofterms": "GraphQL Dgraph"}},
			Expected: []*post{
				{Title: "GraphQL doco"},
				{Title: "Introducing GraphQL in Dgraph"},
				{Title: "Learning GraphQL in Dgraph"}}},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			postTest(t, test.Filter, test.Expected)
		})
	}
}

func fullTextFilters(t *testing.T) {
	cases := map[string]struct {
		Filter   interface{}
		Expected []*post
	}{
		"all of text": {
			Filter: map[string]interface{}{
				"text": map[string]interface{}{"alloftext": "learn GraphQL"}},
			Expected: []*post{
				{Title: "GraphQL doco"},
				{Title: "Learning GraphQL in Dgraph"}}},
		"any of text": {
			Filter: map[string]interface{}{
				"text": map[string]interface{}{"anyoftext": "learn GraphQL"}},
			Expected: []*post{
				{Title: "GraphQL doco"},
				{Title: "Introducing GraphQL in Dgraph"},
				{Title: "Learning GraphQL in Dgraph"}}},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			postTest(t, test.Filter, test.Expected)
		})
	}
}

func stringExactFilters(t *testing.T) {
	cases := map[string]struct {
		Filter   interface{}
		Expected []*post
	}{
		"less than": {
			Filter:   map[string]interface{}{"topic": map[string]interface{}{"lt": "GraphQL"}},
			Expected: []*post{{Title: "GraphQL doco"}}},
		"less or equal": {
			Filter: map[string]interface{}{"topic": map[string]interface{}{"le": "GraphQL"}},
			Expected: []*post{
				{Title: "GraphQL doco"},
				{Title: "Introducing GraphQL in Dgraph"}}},
		"equal": {
			Filter:   map[string]interface{}{"topic": map[string]interface{}{"eq": "GraphQL"}},
			Expected: []*post{{Title: "Introducing GraphQL in Dgraph"}}},
		"greater or equal": {
			Filter: map[string]interface{}{"topic": map[string]interface{}{"ge": "GraphQL"}},
			Expected: []*post{
				{Title: "Introducing GraphQL in Dgraph"},
				{Title: "Learning GraphQL in Dgraph"},
				{Title: "Random post"}}},
		"greater than": {
			Filter: map[string]interface{}{"topic": map[string]interface{}{"gt": "GraphQL"}},
			Expected: []*post{
				{Title: "Learning GraphQL in Dgraph"},
				{Title: "Random post"}}},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			postTest(t, test.Filter, test.Expected)
		})
	}
}

func scalarListFilters(t *testing.T) {

	// tags is a list of strings with @search(by: "exact").  So all the filters
	// lt, le, ... mean "is there something in the list that's lt 'Dgraph'", etc.

	cases := map[string]struct {
		Filter   interface{}
		Expected []*post
	}{
		"less than": {
			Filter:   map[string]interface{}{"tags": map[string]interface{}{"lt": "Dgraph"}},
			Expected: []*post{{Title: "Introducing GraphQL in Dgraph"}}},
		"less or equal": {
			Filter: map[string]interface{}{"tags": map[string]interface{}{"le": "Dgraph"}},
			Expected: []*post{
				{Title: "GraphQL doco"},
				{Title: "Introducing GraphQL in Dgraph"},
				{Title: "Learning GraphQL in Dgraph"}}},
		"equal": {
			Filter:   map[string]interface{}{"tags": map[string]interface{}{"eq": "Database"}},
			Expected: []*post{{Title: "Introducing GraphQL in Dgraph"}}},
		"greater or equal": {
			Filter: map[string]interface{}{"tags": map[string]interface{}{"ge": "Dgraph"}},
			Expected: []*post{
				{Title: "GraphQL doco"},
				{Title: "Introducing GraphQL in Dgraph"},
				{Title: "Learning GraphQL in Dgraph"},
				{Title: "Random post"}}},
		"greater than": {
			Filter:   map[string]interface{}{"tags": map[string]interface{}{"gt": "GraphQL"}},
			Expected: []*post{{Title: "Random post"}}},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			postTest(t, test.Filter, test.Expected)
		})
	}
}

func postTest(t *testing.T, filter interface{}, expected []*post) {
	queryParams := &GraphQLParams{
		Query: `query filterVariable($filter: PostFilter) {
			queryPost(filter: $filter, order: { asc: title }) {
				title
			}
		}`,
		Variables: map[string]interface{}{"filter": filter},
	}

	gqlResponse := queryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result struct {
		QueryPost []*post
	}
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)

	if diff := cmp.Diff(expected, result.QueryPost); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
}

func skipDirective(t *testing.T) {
	getAuthorParams := &GraphQLParams{
		Query: `query ($skipPost: Boolean!, $skipName: Boolean!) {
			queryAuthor(filter: { name: { eq: "Ann Other Author" } }) {
				name @skip(if: $skipName)
				dob
				reputation
				posts @skip(if: $skipPost) {
					title
				}
			}
		}`,
		Variables: map[string]interface{}{
			"skipPost": true,
			"skipName": false,
		},
	}

	gqlResponse := getAuthorParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	expected := `{"queryAuthor":[{"name":"Ann Other Author",
		"dob":"1988-01-01T00:00:00Z","reputation":8.9}]}`
	require.JSONEq(t, expected, string(gqlResponse.Data))
}

func includeDirective(t *testing.T) {
	getAuthorParams := &GraphQLParams{
		Query: `query ($includeName: Boolean!, $includePost: Boolean!) {
			queryAuthor(filter: { name: { eq: "Ann Other Author" } }) {
			  name @include(if: $includeName)
			  dob
			  posts @include(if: $includePost) {
				title
			  }
			}
		  }`,
		Variables: map[string]interface{}{
			"includeName": true,
			"includePost": false,
		},
	}

	gqlResponse := getAuthorParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	expected := `{"queryAuthor":[{"name":"Ann Other Author","dob":"1988-01-01T00:00:00Z"}]}`
	require.JSONEq(t, expected, string(gqlResponse.Data))
}

func includeAndSkipDirective(t *testing.T) {
	getAuthorParams := &GraphQLParams{
		Query: `query ($includeFalse: Boolean!, $skipTrue: Boolean!, $includeTrue: Boolean!,
			$skipFalse: Boolean!) {
			queryAuthor (filter: { name: { eq: "Ann Other Author" } }) {
			  dob @include(if: $includeFalse) @skip(if: $skipFalse)
			  reputation @include(if: $includeFalse) @skip(if: $skipTrue)
			  name @include(if: $includeTrue) @skip(if: $skipFalse)
			  posts(filter: { title: { anyofterms: "GraphQL" } }, first: 10)
			    @include(if: $includeTrue) @skip(if: $skipTrue) {
				title
				tags
			  }
			  postsAggregate {
				__typename @include(if: $includeFalse) @skip(if: $skipFalse)
				count @include(if: $includeFalse) @skip(if: $skipTrue)
				titleMin @include(if: $includeTrue) @skip(if: $skipFalse)
				numLikesMax @include(if: $includeTrue) @skip(if: $skipTrue)
			  }
			}
			aggregatePost {
			  __typename @include(if: $includeFalse) @skip(if: $skipFalse)
			  count @include(if: $includeFalse) @skip(if: $skipTrue)
			  titleMin @include(if: $includeTrue) @skip(if: $skipFalse)
			  numLikesMax @include(if: $includeTrue) @skip(if: $skipTrue)
			}
		  }`,
		Variables: map[string]interface{}{
			"includeFalse": false,
			"includeTrue":  true,
			"skipFalse":    false,
			"skipTrue":     true,
		},
	}

	gqlResponse := getAuthorParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	expected := `{
	  "queryAuthor": [
		{
		  "name": "Ann Other Author",
		  "postsAggregate": {
			"titleMin": "Learning GraphQL in Dgraph"
		  }
		}
	  ],
	  "aggregatePost": {
		"titleMin": "GraphQL doco"
	  }
	}`
	require.JSONEq(t, expected, string(gqlResponse.Data))
}

func queryByMultipleIds(t *testing.T) {
	posts := allPosts(t)
	ids := make([]string, 0, len(posts))
	for _, post := range posts {
		ids = append(ids, post.PostID)
	}

	queryParams := &GraphQLParams{
		Query: `query queryPost($filter: PostFilter) {
			queryPost(filter: $filter) {
				postID
				title
				text
				tags
				numLikes
				isPublished
				postType
			}
		}`,
		Variables: map[string]interface{}{"filter": map[string]interface{}{
			"postID": ids,
		}},
	}

	gqlResponse := queryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result struct {
		QueryPost []*post
	}
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)
	if diff := cmp.Diff(posts, result.QueryPost); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
}

func enumFilter(t *testing.T) {
	posts := allPosts(t)

	queryParams := &GraphQLParams{
		Query: `query queryPost($filter: PostFilter) {
			queryPost(filter: $filter) {
				postID
				title
				text
				tags
				numLikes
				isPublished
				postType
			}
		}`,
	}

	facts := make([]*post, 0, len(posts))
	questions := make([]*post, 0, len(posts))
	for _, post := range posts {
		if post.PostType == "Fact" {
			facts = append(facts, post)
		}
		if post.PostType == "Question" {
			questions = append(questions, post)
		}
	}

	cases := map[string]struct {
		Filter   interface{}
		Expected []*post
	}{
		"Hash Filter test": {
			Filter: map[string]interface{}{
				"postType": map[string]interface{}{
					"eq": "Fact",
				},
			},
			Expected: facts,
		},

		"Regexp Filter test": {
			Filter: map[string]interface{}{
				"postType": map[string]interface{}{
					"regexp": "/(Fact)|(Question)/",
				},
			},
			Expected: append(questions, facts...),
		},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			queryParams.Variables = map[string]interface{}{"filter": test.Filter}

			gqlResponse := queryParams.ExecuteAsPost(t, GraphqlURL)
			RequireNoGQLErrors(t, gqlResponse)

			var result struct {
				QueryPost []*post
			}

			postSort := func(i, j int) bool {
				return result.QueryPost[i].Title < result.QueryPost[j].Title
			}
			testSort := func(i, j int) bool {
				return test.Expected[i].Title < test.Expected[j].Title
			}

			err := json.Unmarshal([]byte(gqlResponse.Data), &result)
			sort.Slice(result.QueryPost, postSort)
			sort.Slice(test.Expected, testSort)

			require.NoError(t, err)
			if diff := cmp.Diff(test.Expected, result.QueryPost); diff != "" {
				t.Errorf("result mismatch (-want +got):\n%s", diff)
			}
		})

	}
}

func queryApplicationGraphQl(t *testing.T) {
	getCountryParams := &GraphQLParams{
		Query: `query queryCountry {
			queryCountry {
				name
			}
		}`,
	}

	gqlResponse := getCountryParams.ExecuteAsPostApplicationGraphql(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	expected := `{
	"queryCountry": [
          { "name": "Angola"},
          { "name": "Bangladesh"},
		  { "name": "India"},
          { "name": "Mozambique"}
        ]
}`
	testutil.CompareJSON(t, expected, string(gqlResponse.Data))

}

func queryTypename(t *testing.T) {
	getCountryParams := &GraphQLParams{
		Query: `query queryCountry {
			queryCountry {
				name
				__typename
			}
		}`,
	}

	gqlResponse := getCountryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	expected := `{
	"queryCountry": [
          {
                "name": "Angola",
                "__typename": "Country"
          },
          {
                "name": "Bangladesh",
                "__typename": "Country"
          },
		  {
                "name": "India",
                "__typename": "Country"
          },
          {
                "name": "Mozambique",
                "__typename": "Country"
          }
        ]
}`
	testutil.CompareJSON(t, expected, string(gqlResponse.Data))

}

func queryNestedTypename(t *testing.T) {
	getCountryParams := &GraphQLParams{
		Query: `query {
			queryAuthor(filter: { name: { eq: "Ann Author" } }) {
				name
				dob
				posts {
					title
					__typename
				}
			}
		}`,
	}

	gqlResponse := getCountryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	expected := `{
	"queryAuthor": [
	  {
		"name": "Ann Author",
		"dob": "2000-01-01T00:00:00Z",
		"posts": [
		  {
			"title": "Introducing GraphQL in Dgraph",
			"__typename": "Post"
		  },
		  {
			"title": "GraphQL doco",
			"__typename": "Post"
		  }
		]
	  }
	]
}`
	testutil.CompareJSON(t, expected, string(gqlResponse.Data))
}

func typenameForInterface(t *testing.T) {
	newStarship := addStarship(t)
	humanID := addHuman(t, newStarship.ID)
	droidID := addDroid(t)
	updateCharacter(t, humanID)

	t.Run("test __typename for interface types", func(t *testing.T) {
		queryCharacterParams := &GraphQLParams{
			Query: `query {
				queryCharacter (filter: {
					appearsIn: {
						in: [EMPIRE]
					}
				}) {
					name
					__typename
					... on Human {
						totalCredits
			                }
					... on Droid {
						primaryFunction
			                }
				}
			}`,
		}

		expected := `{
		"queryCharacter": [
		  {
			"name":"Han Solo",
			"__typename": "Human",
			"totalCredits": 10
		  },
		  {
			"name": "R2-D2",
			"__typename": "Droid",
			"primaryFunction": "Robot"
		  }
		]
	  }`

		gqlResponse := queryCharacterParams.ExecuteAsPost(t, GraphqlURL)
		RequireNoGQLErrors(t, gqlResponse)
		testutil.CompareJSON(t, expected, string(gqlResponse.Data))
	})

	cleanupStarwars(t, newStarship.ID, humanID, droidID)
}

func queryOnlyTypename(t *testing.T) {

	newCountry1 := addCountry(t, postExecutor)
	newCountry2 := addCountry(t, postExecutor)
	newCountry3 := addCountry(t, postExecutor)

	getCountryParams := &GraphQLParams{
		Query: `query {
			queryCountry(filter: { name: {eq: "Testland"}}) {
				__typename
			}
		}`,
	}

	gqlResponse := getCountryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	expected := `{
	"queryCountry": [
         {
               "__typename": "Country"
         },
         {
               "__typename": "Country"
         },
         {
               "__typename": "Country"
         }

       ]
}`

	require.JSONEq(t, expected, string(gqlResponse.Data))
	cleanUp(t, []*country{newCountry1, newCountry2, newCountry3}, []*author{}, []*post{})
}

func querynestedOnlyTypename(t *testing.T) {

	newCountry := addCountry(t, postExecutor)
	newAuthor := addAuthor(t, newCountry.ID, postExecutor)
	newPost1 := addPost(t, newAuthor.ID, newCountry.ID, postExecutor)
	newPost2 := addPost(t, newAuthor.ID, newCountry.ID, postExecutor)
	newPost3 := addPost(t, newAuthor.ID, newCountry.ID, postExecutor)

	getCountryParams := &GraphQLParams{
		Query: `query {
			queryAuthor(filter: { name: { eq: "Test Author" } }) {
				posts {
					__typename
				}
			}
		}`,
	}

	gqlResponse := getCountryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	expected := `{
	"queryAuthor": [
	  {
		"posts": [
		  {
			"__typename": "Post"
		  },
                  {
			"__typename": "Post"
		  },
		  {

			"__typename": "Post"
		  }
		]
	  }
	]
}`
	require.JSONEq(t, expected, string(gqlResponse.Data))
	cleanUp(t, []*country{newCountry}, []*author{newAuthor}, []*post{newPost1, newPost2, newPost3})
}

func onlytypenameForInterface(t *testing.T) {
	newStarship := addStarship(t)
	humanID := addHuman(t, newStarship.ID)
	droidID := addDroid(t)
	updateCharacter(t, humanID)

	t.Run("test __typename for interface types", func(t *testing.T) {
		queryCharacterParams := &GraphQLParams{
			Query: `query {
				queryCharacter (filter: {
					appearsIn: {
						in: [EMPIRE]
					}
				}) {


					... on Human {
						__typename
			                }
					... on Droid {
						__typename
			                }
				}
			}`,
		}

		expected := `{
		"queryCharacter": [
		  {
                      "__typename": "Human"
		  },
		  {
	             "__typename": "Droid"
		  }
		]
	  }`

		gqlResponse := queryCharacterParams.ExecuteAsPost(t, GraphqlURL)
		RequireNoGQLErrors(t, gqlResponse)
		testutil.CompareJSON(t, expected, string(gqlResponse.Data))
	})

	cleanupStarwars(t, newStarship.ID, humanID, droidID)
}

func defaultEnumFilter(t *testing.T) {
	newStarship := addStarship(t)
	humanID := addHuman(t, newStarship.ID)
	droidID := addDroid(t)
	updateCharacter(t, humanID)

	t.Run("test query enum default index on appearsIn", func(t *testing.T) {
		queryCharacterParams := &GraphQLParams{
			Query: `query {
				queryCharacter (filter: {
					appearsIn: {
						in: [EMPIRE]
					}
				}) {
					name
					appearsIn
				}
			}`,
		}

		gqlResponse := queryCharacterParams.ExecuteAsPost(t, GraphqlURL)
		RequireNoGQLErrors(t, gqlResponse)

		expected := `{
		"queryCharacter": [
		  {
			"name":"Han Solo",
			"appearsIn": ["EMPIRE"]
		  },
		  {
			"name": "R2-D2",
			"appearsIn": ["EMPIRE"]
		  }
		]
	  }`
		testutil.CompareJSON(t, expected, string(gqlResponse.Data))
	})

	cleanupStarwars(t, newStarship.ID, humanID, droidID)
}

func queryByMultipleInvalidIds(t *testing.T) {
	queryParams := &GraphQLParams{
		Query: `query queryPost($filter: PostFilter) {
			queryPost(filter: $filter) {
				postID
				title
				text
				tags
				numLikes
				isPublished
				postType
			}
		}`,
		Variables: map[string]interface{}{"filter": map[string]interface{}{
			"postID": []string{"foo", "bar"},
		}},
	}
	// Since the ids are invalid and can't be converted to uint64, the query sent to Dgraph should
	// have func: uid() at root and should return 0 results.

	gqlResponse := queryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	require.Equal(t, `{"queryPost":[]}`, string(gqlResponse.Data))
	var result struct {
		QueryPost []*post
	}
	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)
	require.Equal(t, 0, len(result.QueryPost))
}

func getStateByXid(t *testing.T) {
	getStateParams := &GraphQLParams{
		Query: `{
			getState(xcode: "nsw") {
				name
			}
		}`,
	}

	gqlResponse := getStateParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	require.Equal(t, `{"getState":{"name":"NSW"}}`, string(gqlResponse.Data))
}

func getStateWithoutArgs(t *testing.T) {
	getStateParams := &GraphQLParams{
		Query: `{
			getState {
				name
			}
		}`,
	}

	gqlResponse := getStateParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	require.JSONEq(t, `{"getState":null}`, string(gqlResponse.Data))
}

func getStateByBothXidAndUid(t *testing.T) {
	getStateParams := &GraphQLParams{
		Query: `{
			getState(xcode: "nsw", id: "0x1") {
				name
			}
		}`,
	}

	gqlResponse := getStateParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	require.JSONEq(t, `{"getState":null}`, string(gqlResponse.Data))
}

func queryStateByXid(t *testing.T) {
	getStateParams := &GraphQLParams{
		Query: `{
			queryState(filter: { xcode: { eq: "nsw"}}) {
				name
			}
		}`,
	}

	gqlResponse := getStateParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	require.Equal(t, `{"queryState":[{"name":"NSW"}]}`, string(gqlResponse.Data))
}

func queryStateByXidRegex(t *testing.T) {
	getStateParams := &GraphQLParams{
		Query: `{
			queryState(filter: { xcode: { regexp: "/n/"}}) {
				name
			}
		}`,
	}

	gqlResponse := getStateParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t, `{"queryState":[{"name":"Nusa"},{"name": "NSW"}]}`,
		string(gqlResponse.Data))
}

func multipleOperations(t *testing.T) {
	params := &GraphQLParams{
		Query: `query sortCountryByNameDesc {
			queryCountry(order: { desc: name }, first: 1) {
				name
			}
		}

		query sortCountryByNameAsc {
			queryCountry(order: { asc: name }, first: 1) {
				name
			}
		}
		`,
		OperationName: "sortCountryByNameAsc",
	}

	cases := []struct {
		name          string
		operationName string
		expectedError string
		expected      []*country
	}{
		{
			"second query name as operation name",
			"sortCountryByNameAsc",
			"",
			[]*country{{Name: "Angola"}},
		},
		{
			"first query name as operation name",
			"sortCountryByNameDesc",
			"",
			[]*country{{Name: "Mozambique"}},
		},
		{
			"operation name doesn't exist",
			"sortCountryByName",
			"Supplied operation name sortCountryByName isn't present in the request.",
			nil,
		},
		{
			"operation name is empty",
			"",
			"Operation name must by supplied when query has more than 1 operation.",
			nil,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			params.OperationName = test.operationName
			gqlResponse := params.ExecuteAsPost(t, GraphqlURL)
			if test.expectedError != "" {
				require.NotNil(t, gqlResponse.Errors)
				require.Equal(t, test.expectedError, gqlResponse.Errors[0].Error())
				return
			}
			RequireNoGQLErrors(t, gqlResponse)

			var expected, result struct {
				QueryCountry []*country
			}
			expected.QueryCountry = test.expected
			err := json.Unmarshal([]byte(gqlResponse.Data), &result)
			require.NoError(t, err)

			if diff := cmp.Diff(expected, result); diff != "" {
				t.Errorf("result mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func queryPostWithAuthor(t *testing.T) {
	queryPostParams := &GraphQLParams{
		Query: `query {
			queryPost (filter: {title : { anyofterms : "Introducing" }} ) {
				title
				author {
					name
				}
			}
		}`,
	}

	gqlResponse := queryPostParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`{"queryPost":[{"title":"Introducing GraphQL in Dgraph","author":{"name":"Ann Author"}}]}`,
		string(gqlResponse.Data))
}

func queriesHaveExtensions(t *testing.T) {
	query := &GraphQLParams{
		Query: `query {
			queryPost {
				title
			}
		}`,
	}

	touchedUidskey := "touched_uids"
	gqlResponse := query.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	require.Contains(t, gqlResponse.Extensions, touchedUidskey)
	require.Greater(t, int(gqlResponse.Extensions[touchedUidskey].(float64)), 0)
}

func queriesWithDebugFlagHaveDQLQueryInExtensions(t *testing.T) {
	query := &GraphQLParams{
		Query: `query {
			queryPost {
				title
			}
		}`,
	}

	gqlResponse := query.ExecuteAsPost(t, GraphqlURL+"?debug=true")
	RequireNoGQLErrors(t, gqlResponse)

	// Needed for directives tests (Post remapped to myPost)
	pattern := regexp.MustCompile(`query \{\n\s*query(?:Post|myPost)\(func: type\((?:Post|myPost)\)\) \{\n\s*Post\.title : (?:Post|myPost)\.title\n\s*dgraph\.uid : uid\n\s*\}\n\}`)

	require.Contains(t, gqlResponse.Extensions, "dql_query")
	require.NotEmpty(t, gqlResponse.Extensions["dql_query"])
	require.True(t, pattern.MatchString(gqlResponse.Extensions["dql_query"].(string)))
}

func erroredQueriesHaveTouchedUids(t *testing.T) {
	country1 := addCountry(t, postExecutor)
	country2 := addCountry(t, postExecutor)

	// delete the first country's name.
	// The schema states type Country `{ ... name: String! ... }`
	// so a query error will be raised if we ask for the country's name in a
	// query.  Don't think a GraphQL update can do this ATM, so do through Dgraph.
	d, err := grpc.Dial(Alpha1gRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	client := dgo.NewDgraphClient(api.NewDgraphClient(d))
	mu := &api.Mutation{
		CommitNow: true,
		DelNquads: []byte(fmt.Sprintf("<%s> <Country.name> * .", country1.ID)),
	}
	_, err = client.NewTxn().Mutate(context.Background(), mu)
	require.NoError(t, err)

	// query country's name with some other things, that should give us error for missing name.
	query := &GraphQLParams{
		Query: `query ($ids: [ID!]) {
			queryCountry(filter: {id: $ids}) {
				id
				name
			}
		}`,
		Variables: map[string]interface{}{"ids": []interface{}{country1.ID, country2.ID}},
	}
	gqlResponse := query.ExecuteAsPost(t, GraphqlURL)

	// the data should have first country as null
	expectedResponse := fmt.Sprintf(`{
		"queryCountry": [
			null,
			{"id": "%s", "name": "Testland"}
		]
	}`, country2.ID)
	testutil.CompareJSON(t, expectedResponse, string(gqlResponse.Data))

	// we should also get error for the missing name field
	require.Equal(t, x.GqlErrorList{{
		Message: "Non-nullable field 'name' (type String!) was not present " +
			"in result from Dgraph.  GraphQL error propagation triggered.",
		Locations: []x.Location{{Line: 4, Column: 5}},
		Path:      []interface{}{"queryCountry", float64(0), "name"},
	}}, gqlResponse.Errors)

	// response should have extensions
	require.NotNil(t, gqlResponse.Extensions)
	// it should have touched_uids filled in from Dgraph response's metrics
	touchedUidskey := "touched_uids"
	require.Contains(t, gqlResponse.Extensions, touchedUidskey)
	require.Greater(t, int(gqlResponse.Extensions[touchedUidskey].(float64)), 0)

	// cleanup
	deleteCountry(t, map[string]interface{}{"id": []interface{}{country1.ID, country2.ID}}, 2, nil)
}

func queryWithAlias(t *testing.T) {
	queryPostParams := &GraphQLParams{
		Query: `query {
			post : queryPost (filter: {title : { anyofterms : "Introducing" }} ) {
				type : __typename
				title
				postTitle : title
				postAuthor : author {
					theName : name
				}
			}
		}`,
	}

	gqlResponse := queryPostParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`{
			"post": [ {
				"type": "Post",
				"title": "Introducing GraphQL in Dgraph",
				"postTitle": "Introducing GraphQL in Dgraph",
				"postAuthor": { "theName": "Ann Author" }}]}`,
		string(gqlResponse.Data))
}

func queryWithMultipleAliasOfSameField(t *testing.T) {
	queryAuthorParams := &GraphQLParams{
		Query: `query {
			queryAuthor (filter: {name: {eq: "Ann Other Author"}}){
			  name
			  p1: posts(filter: {numLikes: {ge: 80}}){
				title
				numLikes
			  }
			  p2: posts(filter: {numLikes: {le: 5}}){
				title
				numLikes
			  }
			}
		  }`,
	}

	gqlResponse := queryAuthorParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`{
		"queryAuthor": [
			{
				"name": "Ann Other Author",
				"p1": [
					{
						"title": "Learning GraphQL in Dgraph",
						"numLikes": 87
					}
				],
				"p2": [
					{
						"title": "Random post",
						"numLikes": 1
					}
				]
			}
		]
	}`,
		string(gqlResponse.Data))
}

func DgraphDirectiveWithSpecialCharacters(t *testing.T) {
	mutation := &GraphQLParams{
		Query: `
		mutation {
			addMessage(input : [{content : "content1", author: "author1"}]) {
				message {
					content
					author
				}
			}
		}`,
	}
	result := `{"addMessage":{"message":[{"content":"content1","author":"author1"}]}}`
	gqlResponse := mutation.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	require.JSONEq(t, result, string(gqlResponse.Data))

	queryParams := &GraphQLParams{
		Query: `
		query {
			queryMessage {
				content
				author
			}
		}`,
	}
	result = `{"queryMessage":[{"content":"content1","author":"author1"}]}`
	gqlResponse = queryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	require.JSONEq(t, result, string(gqlResponse.Data))
}

func queryWithCascade(t *testing.T) {
	// for testing normal and parameterized @cascade with get by ID and filter queries on multiple levels
	authors := addMultipleAuthorFromRef(t, []*author{
		{
			Name:       "George",
			Reputation: 4.5,
			Posts:      []*post{{Title: "A show about nothing", Text: "Got ya!", Tags: []string{}}},
		}, {
			Name:       "Jerry",
			Reputation: 4.6,
			Country:    &country{Name: "outer Galaxy2"},
			Posts:      []*post{{Title: "Outside", Tags: []string{}}},
		}, {
			Name:    "Kramer",
			Country: &country{Name: "outer space2"},
			Posts:   []*post{{Title: "Ha! Cosmo Kramer", Text: "Giddy up!", Tags: []string{}}},
		},
	}, postExecutor)
	newStarship := addStarship(t)
	humanID := addHuman(t, newStarship.ID)
	authorIds := []string{authors[0].ID, authors[1].ID, authors[2].ID}
	postIds := []string{authors[0].Posts[0].PostID, authors[1].Posts[0].PostID,
		authors[2].Posts[0].PostID}
	countryIds := []string{authors[1].Country.ID, authors[2].Country.ID}
	getAuthorByIdQuery := `query ($id: ID!) {
							  getAuthor(id: $id) @cascade {
								reputation
								posts {
								  text
								}
							  }
							}`

	// for testing @cascade with get by XID queries
	states := []*state{
		{Name: "California", Code: "CA", Capital: "Sacramento"},
		{Name: "Texas", Code: "TX"},
	}
	addStateParams := GraphQLParams{
		Query: `mutation ($input: [AddStateInput!]!) {
					addState(input: $input) {
						numUids
					}
				}`,
		Variables: map[string]interface{}{"input": states},
	}
	resp := addStateParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, resp)
	testutil.CompareJSON(t, `{"addState":{"numUids":2}}`, string(resp.Data))
	getStateByXidQuery := `query ($xid: String!) {
							  getState(xcode: $xid) @cascade {
								xcode
								capital
							  }
							}`

	tcases := []struct {
		name      string
		query     string
		variables map[string]interface{}
		respData  string
	}{
		{
			name:      "@cascade on get by ID query returns null",
			query:     getAuthorByIdQuery,
			variables: map[string]interface{}{"id": authors[1].ID},
			respData:  `{"getAuthor": null}`,
		}, {
			name:      "@cascade on get by ID query returns author",
			query:     getAuthorByIdQuery,
			variables: map[string]interface{}{"id": authors[0].ID},
			respData: `{
							"getAuthor": {
								"reputation": 4.5,
								"posts": [{
									"text": "Got ya!"
								}]
							}
						}`,
		}, {
			name:      "@cascade on get by XID query returns null",
			query:     getStateByXidQuery,
			variables: map[string]interface{}{"xid": states[1].Code},
			respData:  `{"getState": null}`,
		}, {
			name:      "@cascade on get by XID query returns state",
			query:     getStateByXidQuery,
			variables: map[string]interface{}{"xid": states[0].Code},
			respData: `{
							"getState": {
								"xcode": "CA",
								"capital": "Sacramento"
							}
						}`,
		}, {
			name: "@cascade on filter query",
			query: `query ($ids: [ID!]) {
					  queryAuthor(filter: {id: $ids}) @cascade {
						reputation
						posts {
						  text
						}
					  }
					}`,
			variables: map[string]interface{}{"ids": authorIds},
			respData: `{
							"queryAuthor": [{
								"reputation": 4.5,
								"posts": [{
									"text": "Got ya!"
								}]
							}]
						}`,
		}, {
			name: "@cascade on query field",
			query: `query ($ids: [ID!]) {
					  queryAuthor(filter: {id: $ids}) {
						reputation
						posts @cascade {
						  title
						  text
						}
					  }
					}`,
			variables: map[string]interface{}{"ids": authorIds},
			respData: `{
							"queryAuthor": [{
								"reputation": 4.5,
								"posts": [{
									"title": "A show about nothing",
									"text": "Got ya!"
								}]
							},{
								"reputation": 4.6,
								"posts": []
							},{
								"reputation": null,
								"posts": [{
									"title": "Ha! Cosmo Kramer",
									"text": "Giddy up!"
								}]
							}]
						}`,
		},
		{
			name: "parameterized cascade with argument at outer level only",
			query: `query ($ids: [ID!]) {
						queryAuthor(filter: {id: $ids})  @cascade(fields:["name"]) {
							reputation
							name
							country {
								name
							}
						}
					}`,
			variables: map[string]interface{}{"ids": authorIds},
			respData: `{
						  "queryAuthor": [
							{
							  "reputation": 4.6,
							  "name": "Jerry",
							  "country": {
								"name": "outer Galaxy2"
							  }
							},
							{
							  "name": "Kramer",
							  "reputation": null,
							  "country": {
								"name": "outer space2"
							  }
							},
							{
							  "reputation": 4.5,
							  "name": "George",
							  "country": null
							}
						  ]
						}`,
		},
		{
			name: "parameterized cascade only at inner level ",
			query: `query ($ids: [ID!]) {
						queryAuthor(filter: {id: $ids})  {
							reputation
							name
							posts @cascade(fields:["text"]) {
								title
								text
							}
						}
					}`,
			variables: map[string]interface{}{"ids": authorIds},
			respData: `{
						  "queryAuthor": [
							{
							  "reputation": 4.5,
							  "name": "George",
							  "posts": [
								{
								  "title": "A show about nothing",
								  "text": "Got ya!"
								}
							  ]
							},
							{
							  "name": "Kramer",
							  "reputation": null,
							  "posts": [
								{
								  "title": "Ha! Cosmo Kramer",
								  "text": "Giddy up!"
								}
							  ]
							},
							{
							  "name": "Jerry",
							  "reputation": 4.6,
							  "posts": []
							}
						  ]
						}`,
		},
		{
			name: "parameterized cascade at all levels ",
			query: `query ($ids: [ID!]) {
						queryAuthor(filter: {id: $ids}) @cascade(fields:["reputation","name"]) {
							reputation
							name
							dob
							posts @cascade(fields:["text"]) {
								title
								text
							}
						}
					}`,
			variables: map[string]interface{}{"ids": authorIds},
			respData: `{
						  "queryAuthor": [
							{
							  "reputation": 4.5,
							  "name": "George",
							  "dob": null,
							  "posts": [
								{
								  "title": "A show about nothing",
								  "text": "Got ya!"
								}
							  ]
							},
							{
							  "dob": null,
							  "name": "Jerry",
							  "posts": [],
							  "reputation": 4.6
							}
						  ]
						}`,
		},
		{
			name: "parameterized cascade at all levels using variables",
			query: `query ($ids: [ID!],$fieldsRoot: [String], $fieldsDeep: [String]) {
						queryAuthor(filter: {id: $ids}) @cascade(fields: $fieldsRoot) {
							reputation
							name
							dob
							posts @cascade(fields: $fieldsDeep) {
								title
								text
							}
						}
					}`,
			variables: map[string]interface{}{
				"ids":        authorIds,
				"fieldsRoot": []string{"reputation", "name"},
				"fieldsDeep": []string{"text"},
			},
			respData: `{
						  "queryAuthor": [
							{
							  "reputation": 4.5,
							  "name": "George",
							  "dob": null,
							  "posts": [
								{
								  "title": "A show about nothing",
								  "text": "Got ya!"
								}
							  ]
							},
							{
							  "dob": null,
							  "name": "Jerry",
							  "posts": [],
							  "reputation": 4.6
							}
						  ]
						}`,
		},
		{
			name: "parameterized cascade on ID type ",
			query: `query ($ids: [ID!]) {
						queryAuthor(filter: {id: $ids}) @cascade(fields:["reputation","id"]) {
							reputation
							name
							dob
						}
					}`,
			variables: map[string]interface{}{"ids": authorIds},
			respData: `{
						  "queryAuthor": [
							{
							  "reputation": 4.5,
							  "name": "George",
							  "dob": null
							},
							{
							  "dob": null,
							  "name": "Jerry",
							  "reputation": 4.6
							}
						  ]
						}`,
		},
		{
			name: "parameterized cascade on field of interface ",
			query: `query  {
						queryHuman() @cascade(fields:["name"]) {
							name
							totalCredits
						}
					}`,
			respData: `{
						  "queryHuman": [
							{
							  "name": "Han",
							  "totalCredits": 10
							}
						  ]
						}`,
		},
		{
			name: "parameterized cascade on interface ",
			query: `query {
						queryCharacter (filter: { appearsIn: { in: [EMPIRE] } }) @cascade(fields:["appearsIn"]){
							name
							appearsIn
						}
					}`,
			respData: `{
						  "queryCharacter": [
							{
							  "name": "Han",
							  "appearsIn": [
								"EMPIRE"
							  ]
							}
						  ]
						}`,
		},
	}

	for _, tcase := range tcases {
		t.Run(tcase.name, func(t *testing.T) {
			params := &GraphQLParams{
				Query:     tcase.query,
				Variables: tcase.variables,
			}
			resp := params.ExecuteAsPost(t, GraphqlURL)
			RequireNoGQLErrors(t, resp)
			testutil.CompareJSON(t, tcase.respData, string(resp.Data))
		})
	}

	// cleanup
	deleteAuthors(t, authorIds, nil)
	deleteCountry(t, map[string]interface{}{"id": countryIds}, len(countryIds), nil)
	DeleteGqlType(t, "Post", map[string]interface{}{"postID": postIds}, len(postIds), nil)
	deleteState(t, GetXidFilter("xcode", []interface{}{states[0].Code, states[1].Code}), len(states),
		nil)
	cleanupStarwars(t, newStarship.ID, humanID, "")
}

func filterInQueriesWithArrayForAndOr(t *testing.T) {
	// for testing filter with AND,OR connectives
	authors := addMultipleAuthorFromRef(t, []*author{
		{
			Name:          "George",
			Reputation:    4.5,
			Qualification: "Phd in CSE",
			Posts:         []*post{{Title: "A show about nothing", Text: "Got ya!", Tags: []string{}}},
		}, {
			Name:          "Jerry",
			Reputation:    4.6,
			Qualification: "Phd in ECE",
			Country:       &country{Name: "outer Galaxy2"},
			Posts:         []*post{{Title: "Outside", Tags: []string{}}},
		}, {
			Name:          "Kramer",
			Reputation:    4.2,
			Qualification: "PostDoc in CSE",
			Country:       &country{Name: "outer space2"},
			Posts:         []*post{{Title: "Ha! Cosmo Kramer", Text: "Giddy up!", Tags: []string{}}},
		},
	}, postExecutor)
	newStarship := addStarship(t)
	humanID := addHuman(t, newStarship.ID)
	authorIds := []string{authors[0].ID, authors[1].ID, authors[2].ID}
	postIds := []string{authors[0].Posts[0].PostID, authors[1].Posts[0].PostID,
		authors[2].Posts[0].PostID}
	countryIds := []string{authors[1].Country.ID, authors[2].Country.ID}

	states := []*state{
		{Name: "California", Code: "CA", Capital: "Sacramento"},
		{Name: "Texas", Code: "TX"},
	}
	addStateParams := GraphQLParams{
		Query: `mutation ($input: [AddStateInput!]!) {
					addState(input: $input) {
						numUids
					}
				}`,
		Variables: map[string]interface{}{"input": states},
	}
	resp := addStateParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, resp)
	testutil.CompareJSON(t, `{"addState":{"numUids":2}}`, string(resp.Data))

	tcases := []struct {
		name      string
		query     string
		variables string
		respData  string
	}{
		{
			name: "Filter with only AND key at top level",
			query: `query{
                      queryAuthor(filter:{and:{name:{eq:"George"}}}){
                        name
						reputation
                        posts {
                          text
                        }
                      }
				    }`,
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "posts": [
								{
								  "text": "Got ya!"
								}
							  ]
							}
						  ]
						}`,
		},
		{
			name: "Filter with only AND key at top level using variables",
			query: `query($filter:AuthorFilter) {
                      queryAuthor(filter:$filter){
                        name
                        reputation
                        posts {
                          text
                        }
                      }
			    	}`,
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "posts": [
								{
								  "text": "Got ya!"
								}
							  ]
							}
						  ]
						}`,

			variables: `{"filter":{"and":{"name":{"eq":"George"}}}}`,
		},
		{
			name: "Filter with only OR key at top level",
			query: `query {
                      queryAuthor(filter:{or:{name:{eq:"George"}}}){
                        name
                        reputation
                        posts {
						  text
                        }
                      }
				    }`,
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "posts": [
								{
								  "text": "Got ya!"
								}
							  ]
							}
						  ]
						}`,
		},
		{
			name: "Filter with only OR key at top level using variables",
			query: `query($filter:AuthorFilter) {
                      queryAuthor(filter:$filter){
						name
                        reputation
                        posts {
                          text
                        }
                      }
			    	}`,
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "posts": [
								{
								  "text": "Got ya!"
								}
							  ]
							}
						  ]
						}`,
			variables: `{"filter":{"or":{"name":{"eq":"George"}}}}`,
		}, {
			name: "Filter with Nested AND using variables",
			query: `query($filter:AuthorFilter) {
                      queryAuthor(filter:$filter){
                        name
                        reputation
                        posts {
                         text
                        }
                      }
				    }`,
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "posts": [
								{
								  "text": "Got ya!"
								}
							  ]
							}
						  ]
						}`,
			variables: `{"filter":{"and":[{"name":{"eq":"George"}},{"and":{"reputation":{"eq":4.5}}}]}}`,
		},
		{
			name: "Filter with Nested AND",
			query: `query{
                      queryAuthor(filter:{and:[{name:{eq:"George"}},{and:{reputation:{eq:4.5}}}]}){
                        name
                        reputation
                        posts {
                          text
                        }
                       }
				     }`,
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "posts": [
								{
								  "text": "Got ya!"
								}
							  ]
							}
						  ]
						}`,
		},
		{
			name: "Filter with Nested OR",
			query: `query{
                      queryAuthor(filter:{or:[{name:{eq:"George"}},{or:{reputation:{eq:4.2}}}]}){
                        name
                        reputation
						posts {
                          text
                        }
                      }
				    }`,
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "posts": [
								{
								  "text": "Got ya!"
								}
							  ]
							},
							{
							  "name": "Kramer",
							  "reputation": 4.2,
							  "posts": [
								{
								  "text": "Giddy up!"
								}
							  ]
							}
						  ]
						}`,
		},
		{
			name: "Filter with Nested OR using variables",
			query: `query($filter:AuthorFilter) {
                      queryAuthor(filter:$filter){
                        name
						reputation
                        posts {
                          text
                        }
                      }
			      	}`,
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "posts": [
								{
								  "text": "Got ya!"
								}
							  ]
							},
							{
							  "name": "Kramer",
							  "reputation": 4.2,
							  "posts": [
								{
								  "text": "Giddy up!"
								}
							  ]
							}
						  ]
						}`,
			variables: `{"filter":{"or":[{"name":{"eq":"George"}},{"or":{"reputation":{"eq":4.2}}}]}}`,
		},
		{
			name: "(A OR B) AND (C OR D) using variables",
			query: `query($filter:AuthorFilter) {
                       queryAuthor(filter:$filter){
                         name
                         reputation
                         posts {
                           text
                        }
                       }
				     }`,
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "posts": [
								{
								  "text": "Got ya!"
								}
							  ]
							}
						  ]
						}`,
			variables: `{"filter":{"and": [{"name":{"eq": "George"},"or":{"name":{"eq": "Alice"}}},
						{"reputation":{"eq": 3}, "or":{"reputation":{"eq": 4.5}}}]}}`,
		},
		{
			name: "(A AND B AND C) using variables",
			query: `query($filter:AuthorFilter) {
                       queryAuthor(filter:$filter){
                         name
                         reputation
                         qualification
                          posts {
                            text
                          }
                       }
			      	}`,
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "qualification": "Phd in CSE",
							  "posts": [
								{
								  "text": "Got ya!"
								}
							  ]
							}
						  ]
						}`,
			variables: `{"filter":{"and": [{"name":{"eq": "George"}},{"reputation":{"eq": 4.5}},{"qualification": {"eq": "Phd in CSE"}}]}}`,
		},
		{
			name: "(A OR B OR C) using variables",
			query: `query($filter:AuthorFilter) {
                       queryAuthor(filter:$filter){
                         name
                         reputation
                         qualification
                       }
				    }`,
			respData: `{
						  "queryAuthor": [
							{
							  "name": "Kramer",
							  "qualification": "PostDoc in CSE",
							  "reputation": 4.2
							},
							{
							  "name": "George",
							  "qualification": "Phd in CSE",
							  "reputation": 4.5
							},
							{
							  "name": "Jerry",
							  "qualification": "Phd in ECE",
							  "reputation": 4.6
							}
						  ]
						}`,
			variables: `{"filter":{"or": [{"name": {"eq": "George"}}, {"reputation": {"eq": 4.6}}, {"qualification": {"eq": "PostDoc in CSE"}}]}}`,
		},
	}

	for _, tcase := range tcases {
		t.Run(tcase.name, func(t *testing.T) {
			var vars map[string]interface{}
			if tcase.variables != "" {
				err := json.Unmarshal([]byte(tcase.variables), &vars)
				require.NoError(t, err)
			}
			params := &GraphQLParams{
				Query:     tcase.query,
				Variables: vars,
			}
			resp := params.ExecuteAsPost(t, GraphqlURL)
			RequireNoGQLErrors(t, resp)
			testutil.CompareJSON(t, tcase.respData, string(resp.Data))
		})
	}

	// cleanup
	deleteAuthors(t, authorIds, nil)
	deleteCountry(t, map[string]interface{}{"id": countryIds}, len(countryIds), nil)
	DeleteGqlType(t, "Post", map[string]interface{}{"postID": postIds}, len(postIds), nil)
	deleteState(t, GetXidFilter("xcode", []interface{}{states[0].Code, states[1].Code}), len(states),
		nil)
	cleanupStarwars(t, newStarship.ID, humanID, "")
}

func queryGeoNearFilter(t *testing.T) {
	addHotelParams := &GraphQLParams{
		Query: `
		mutation addHotel($hotels: [AddHotelInput!]!) {
		  addHotel(input: $hotels) {
			hotel {
			  name
			  location {
				latitude
				longitude
			  }
			}
		  }
		}`,
		Variables: map[string]interface{}{"hotels": []interface{}{
			map[string]interface{}{
				"name": "Taj Hotel 1",
				"location": map[string]interface{}{
					"latitude":  11.11,
					"longitude": 22.22,
				},
			},
			map[string]interface{}{
				"name": "Taj Hotel 2",
				"location": map[string]interface{}{
					"latitude":  33.33,
					"longitude": 22.22,
				},
			},
			map[string]interface{}{
				"name": "Taj Hotel 3",
				"location": map[string]interface{}{
					"latitude":  11.11,
					"longitude": 33.33,
				},
			},
		},
		},
	}
	gqlResponse := addHotelParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	queryHotel := &GraphQLParams{
		Query: `
			query {
			  queryHotel(filter: { location: { near: { distance: 100, coordinate: { latitude: 11.11, longitude: 22.22} } } }) {
				name
				location {
				  latitude
				  longitude
				}
			  }
			}`,
	}
	gqlResponse = queryHotel.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	queryHotelExpected := `
	{
		"queryHotel":[{
			"name" : "Taj Hotel 1",
			"location" : {
				"latitude" : 11.11,
				"longitude" : 22.22
			}
		}]
	}`
	testutil.CompareJSON(t, queryHotelExpected, string(gqlResponse.Data))
	// Cleanup
	DeleteGqlType(t, "Hotel", map[string]interface{}{}, 3, nil)
}

func persistedQuery(t *testing.T) {
	queryCountryParams := &GraphQLParams{
		Extensions: &schema.RequestExtensions{PersistedQuery: schema.PersistedQuery{
			Sha256Hash: "shaWithoutAnyPersistedQuery",
		}},
	}
	gqlResponse := queryCountryParams.ExecuteAsPost(t, GraphqlURL)
	require.Len(t, gqlResponse.Errors, 1)
	require.Contains(t, gqlResponse.Errors[0].Message, "PersistedQueryNotFound")

	queryCountryParams = &GraphQLParams{
		Query: `query ($countryName: String){
			queryCountry(filter: {name: {eq: $countryName}}) {
				name
			}
		}`,
		Variables: map[string]interface{}{"countryName": "Bangladesh"},
		Extensions: &schema.RequestExtensions{PersistedQuery: schema.PersistedQuery{
			Sha256Hash: "incorrectSha",
		}},
	}
	gqlResponse = queryCountryParams.ExecuteAsPost(t, GraphqlURL)
	require.Len(t, gqlResponse.Errors, 1)
	require.Contains(t, gqlResponse.Errors[0].Message, "provided sha does not match query")

	queryCountryParams.Extensions.PersistedQuery.Sha256Hash = "bbc0af44f82ce5c38e775f7f14c71e5eba1936b12b3e66c452ee262ef147f1ed"
	gqlResponse = queryCountryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	queryCountryParams.Query = ""
	gqlResponse = queryCountryParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	// test get method as well
	queryCountryParams.Extensions = nil
	gqlResponse = queryCountryParams.ExecuteAsGet(
		t,
		GraphqlURL+`?extensions={"persistedQuery":{"sha256Hash":"bbc0af44f82ce5c38e775f7f14c71e5eba1936b12b3e66c452ee262ef147f1ed"}}`,
	)
	RequireNoGQLErrors(t, gqlResponse)
}

func queryAggregateWithFilter(t *testing.T) {
	queryPostParams := &GraphQLParams{
		Query: `query {
			aggregatePost (filter: {title : { anyofterms : "Introducing" }} ) {
				count
				numLikesMax
				titleMin
			}
		}`,
	}

	gqlResponse := queryPostParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`{
					"aggregatePost":
						{
							"count":1,
							"numLikesMax": 100,
							"titleMin": "Introducing GraphQL in Dgraph"
						}
				}`,
		string(gqlResponse.Data))
}

func queryAggregateOnEmptyData(t *testing.T) {
	queryPostParams := &GraphQLParams{
		Query: `query {
			aggregatePost (filter: {title : { anyofterms : "Nothing" }} ) {
				count
				numLikesMax
				type: __typename
				titleMin
			}
		}`,
	}

	gqlResponse := queryPostParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	require.JSONEq(t,
		`{
			"aggregatePost": {
				"count": 0,
				"numLikesMax": null,
				"type": "PostAggregateResult",
				"titleMin": null
			}
		}`,
		string(gqlResponse.Data))
}

func queryAggregateOnEmptyData2(t *testing.T) {
	queryPostParams := &GraphQLParams{
		Query: `query {
			aggregateState (filter: {xcode : { eq : "nsw" }} ) {
				count
				capitalMax
				capitalMin
				xcodeMin
				xcodeMax
			}
		}`,
	}

	gqlResponse := queryPostParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`{
					"aggregateState":
						{
							"capitalMax": null,
							"capitalMin": null,
							"xcodeMin": "nsw",
							"xcodeMax": "nsw",
							"count": 1
						}
				}`,
		string(gqlResponse.Data))
}

func queryAggregateOnEmptyData3(t *testing.T) {
	queryNumberOfStates := &GraphQLParams{
		Query: `query
		{
			queryCountry(filter: { name: { eq: "India" } }) {
				name
				ag : statesAggregate {
					count
					nameMin
					capitalMax
					capitalMin
				}
			}
		}`,
	}
	gqlResponse := queryNumberOfStates.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`
		{
			"queryCountry": [{
				"name": "India",
				"ag": {
					"count" : 3,
					"nameMin": "Gujarat",
					"capitalMax": null,
					"capitalMin": null
				}
			}]
		}`,
		string(gqlResponse.Data))
}

func queryAggregateWithoutFilter(t *testing.T) {
	queryPostParams := &GraphQLParams{
		Query: `query {
			aggregatePost {
				titleMax
				titleMin
				numLikesSum
				numLikesAvg
				numLikesMax
				numLikesMin
				count
			}
		}`,
	}

	gqlResponse := queryPostParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`{
					"aggregatePost":
						{
							"count":4,
							"titleMax": "Random post",
							"titleMin": "GraphQL doco",
							"numLikesAvg": 66.25,
							"numLikesMax": 100,
							"numLikesMin": 1,
							"numLikesSum": 265
						}
				}`,
		string(gqlResponse.Data))
}

func queryAggregateWithAlias(t *testing.T) {
	queryPostParams := &GraphQLParams{
		Query: `query {
			aggregatePost {
				cnt: count
				tmin : titleMin
				tmax: titleMax
				navg : numLikesAvg
			}
		}`,
	}

	gqlResponse := queryPostParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`{
					"aggregatePost":
							{
								"cnt":4,
								"tmax": "Random post",
								"tmin": "GraphQL doco",
								"navg": 66.25
							}
				}`,
		string(gqlResponse.Data))
}

func queryAggregateWithRepeatedFields(t *testing.T) {
	queryPostParams := &GraphQLParams{
		Query: `query {
			aggregatePost {
				count
				cnt2 : count
				tmin : titleMin
				tmin_again : titleMin
				tmax: titleMax
				tmax_again : titleMax
				navg : numLikesAvg
				navg2 : numLikesAvg
			}
		}`,
	}

	gqlResponse := queryPostParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`{
					"aggregatePost":
							{
								"count":4,
								"cnt2":4,
								"tmax": "Random post",
								"tmax_again": "Random post",
								"tmin": "GraphQL doco",
								"tmin_again": "GraphQL doco",
								"navg": 66.25,
								"navg2": 66.25
							}
				}`,
		string(gqlResponse.Data))
}

func queryAggregateAtChildLevel(t *testing.T) {
	queryNumberOfStates := &GraphQLParams{
		Query: `query
		{
			queryCountry(filter: { name: { eq: "India" } }) {
				name
				ag : statesAggregate {
					count
					__typename
					nameMin
				}
			}
		}`,
	}
	gqlResponse := queryNumberOfStates.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`
		{
			"queryCountry": [{
				"name": "India",
				"ag": {
					"count" : 3,
					"__typename": "StateAggregateResult",
					"nameMin": "Gujarat"
				}
			}]
		}`,
		string(gqlResponse.Data))
}

func queryAggregateAtChildLevelWithFilter(t *testing.T) {
	queryNumberOfIndianStates := &GraphQLParams{
		Query: `query
		{
			queryCountry(filter: { name: { eq: "India" } }) {
				name
				ag : statesAggregate(filter: {xcode: {in: ["ka", "mh"]}}) {
                	count
					nameMin
                }
			}
		}`,
	}
	gqlResponse := queryNumberOfIndianStates.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`
		{
			"queryCountry": [{
				"name": "India",
				"ag": {
					"count" : 2,
					"nameMin" : "Karnataka"
				}
			}]
		}`,
		string(gqlResponse.Data))
}

func queryAggregateAtChildLevelWithEmptyData(t *testing.T) {
	queryNumberOfIndianStates := &GraphQLParams{
		Query: `query
		{
			queryCountry(filter: { name: { eq: "India" } }) {
				name
				ag : statesAggregate(filter: {xcode: {in: ["nothing"]}}) {
                	count
					__typename
					nameMin
                }
				n: name
			}
		}`,
	}
	gqlResponse := queryNumberOfIndianStates.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`
		{
			"queryCountry": [{
				"name": "India",
				"ag": {
					"count": 0,
					"__typename": "StateAggregateResult",
					"nameMin": null
				},
				"n": "India"
			}]
		}`,
		string(gqlResponse.Data))
}

func queryAggregateAtChildLevelWithMultipleAlias(t *testing.T) {
	queryNumberOfIndianStates := &GraphQLParams{
		Query: `query
		{
			queryCountry(filter: { name: { eq: "India" } }) {
				name
				ag1: statesAggregate(filter: {xcode: {in: ["ka", "mh"]}}) {
					count
					nameMax
				}
				ag2: statesAggregate(filter: {xcode: {in: ["ka", "mh", "gj", "xyz"]}}) {
					count
					nameMax
				}
			}
		}`,
	}
	gqlResponse := queryNumberOfIndianStates.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`
		{
			"queryCountry": [{
				"name": "India",
				"ag1": {
					"count" : 2,
					"nameMax" : "Maharashtra"
				},
				"ag2": {
					"count" : 3,
					"nameMax" : "Maharashtra"
				}
			}]
		}`,
		string(gqlResponse.Data))
}

func queryAggregateAtChildLevelWithRepeatedFields(t *testing.T) {
	queryNumberOfIndianStates := &GraphQLParams{
		Query: `query
		{
			queryCountry(filter: { name: { eq: "India" } }) {
				name
				ag1: statesAggregate(filter: {xcode: {in: ["ka", "mh"]}}) {
					count
					cnt2 : count
					nameMax
					nm : nameMax
				}
			}
		}`,
	}
	gqlResponse := queryNumberOfIndianStates.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`
		{
			"queryCountry": [{
				"name": "India",
				"ag1": {
					"count" : 2,
					"cnt2" : 2,
					"nameMax" : "Maharashtra",
					"nm": "Maharashtra"
				}
			}]
		}`,
		string(gqlResponse.Data))
}

func queryAggregateAndOtherFieldsAtChildLevel(t *testing.T) {
	queryNumberOfIndianStates := &GraphQLParams{
		Query: `query
		{
			queryCountry(filter: { name: { eq: "India" } }) {
				name
				ag : statesAggregate {
                	count
					nameMin
                },
				states {
					name
				}
			}
		}`,
	}
	gqlResponse := queryNumberOfIndianStates.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`
		{
			"queryCountry": [{
				"name": "India",
				"ag": {
					"count" : 3,
					"nameMin" : "Gujarat"
				},
				"states": [
				{
					"name": "Maharashtra"
				},
				{
					"name": "Gujarat"
				},
				{
					"name": "Karnataka"
				}]
			}]
		}`,
		string(gqlResponse.Data))
}

func queryChildLevelWithMultipleAliasOnScalarField(t *testing.T) {
	queryNumberOfIndianStates := &GraphQLParams{
		Query: `query
		{
			queryPost(filter: {numLikes: {ge: 100}}) {
				t1: title
				t2: title
			}
		}`,
	}
	gqlResponse := queryNumberOfIndianStates.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	testutil.CompareJSON(t,
		`
		{
			"queryPost": [
				{
					"t1": "Introducing GraphQL in Dgraph",
					"t2": "Introducing GraphQL in Dgraph"
				}
			]
		}`,
		string(gqlResponse.Data))
}

func checkUser(t *testing.T, userObj, expectedObj *user) {
	checkUserParams := &GraphQLParams{
		Query: `query checkUserPassword($name: String!, $pwd: String!) {
			checkUserPassword(name: $name, password: $pwd) { name }
		}`,
		Variables: map[string]interface{}{
			"name": userObj.Name,
			"pwd":  userObj.Password,
		},
	}

	gqlResponse := checkUserParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result struct {
		CheckUserPasword *user `json:"checkUserPassword,omitempty"`
	}

	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)

	opt := cmpopts.IgnoreFields(user{}, "Password")
	if diff := cmp.Diff(expectedObj, result.CheckUserPasword, opt); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
}

func checkUserPasswordWithAlias(t *testing.T, userObj, expectedObj *user) {
	checkUserParams := &GraphQLParams{
		Query: `query checkUserPassword($name: String!, $pwd: String!) {
			verify : checkUserPassword(name: $name, password: $pwd) { name }
		}`,
		Variables: map[string]interface{}{
			"name": userObj.Name,
			"pwd":  userObj.Password,
		},
	}

	gqlResponse := checkUserParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	var result struct {
		CheckUserPasword *user `json:"verify,omitempty"`
	}

	err := json.Unmarshal([]byte(gqlResponse.Data), &result)
	require.NoError(t, err)

	opt := cmpopts.IgnoreFields(user{}, "Password")
	if diff := cmp.Diff(expectedObj, result.CheckUserPasword, opt); diff != "" {
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}
}

func passwordTest(t *testing.T) {
	newUser := &user{
		Name:     "Test User",
		Password: "password",
	}

	addUserParams := &GraphQLParams{
		Query: `mutation addUser($user: [AddUserInput!]!) {
			addUser(input: $user) {
				user {
					name
				}
			}
		}`,
		Variables: map[string]interface{}{"user": []*user{newUser}},
	}

	updateUserParams := &GraphQLParams{
		Query: `mutation addUser($user: UpdateUserInput!) {
			updateUser(input: $user) {
				user {
					name
				}
			}
		}`,
		Variables: map[string]interface{}{"user": map[string]interface{}{
			"filter": map[string]interface{}{
				"name": map[string]interface{}{
					"eq": newUser.Name,
				},
			},
			"set": map[string]interface{}{
				"password": "password_new",
			},
		}},
	}

	t.Run("Test add and update user", func(t *testing.T) {
		gqlResponse := postExecutor(t, GraphqlURL, addUserParams)
		RequireNoGQLErrors(t, gqlResponse)
		require.Equal(t, `{"addUser":{"user":[{"name":"Test User"}]}}`,
			string(gqlResponse.Data))

		checkUser(t, newUser, newUser)
		checkUserPasswordWithAlias(t, newUser, newUser)
		checkUser(t, &user{Name: "Test User", Password: "Wrong Pass"}, nil)

		gqlResponse = postExecutor(t, GraphqlURL, updateUserParams)
		RequireNoGQLErrors(t, gqlResponse)
		require.Equal(t, `{"updateUser":{"user":[{"name":"Test User"}]}}`,
			string(gqlResponse.Data))
		checkUser(t, newUser, nil)
		updatedUser := &user{Name: newUser.Name, Password: "password_new"}
		checkUser(t, updatedUser, updatedUser)
	})

	deleteUser(t, *newUser)
}

func queryFilterWithIDInputCoercion(t *testing.T) {
	authors := addMultipleAuthorFromRef(t, []*author{
		{
			Name:          "George",
			Reputation:    4.5,
			Qualification: "Phd in CSE",
			Posts:         []*post{{Title: "A show about nothing", Text: "Got ya!", Tags: []string{}}},
		}, {
			Name:       "Jerry",
			Reputation: 4.6,
			Country:    &country{Name: "outer Galaxy2"},
			Posts:      []*post{{Title: "Outside", Tags: []string{}}},
		},
	}, postExecutor)
	authorIds := []string{authors[0].ID, authors[1].ID}
	postIds := []string{authors[0].Posts[0].PostID, authors[1].Posts[0].PostID}
	countryIds := []string{authors[1].Country.ID}
	authorIdsDecimal := []string{cast.ToString(cast.ToInt(authorIds[0])), cast.ToString(cast.ToInt(authorIds[1]))}
	tcases := []struct {
		name      string
		query     string
		variables map[string]interface{}
		respData  string
	}{
		{

			name: "Query using single ID in a filter",
			query: `query($filter:AuthorFilter){
                      queryAuthor(filter:$filter){
                        name
                        reputation
                        posts {
                          text
                        }
                      }
				    }`,
			variables: map[string]interface{}{"filter": map[string]interface{}{"id": authors[0].ID}},
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "posts": [
								{
								  "text": "Got ya!"
								}
							  ]
							}
						  ]
						}`,
		},
		{

			name: "Query using single ID given in variable of type integer coerced to string ",
			query: `query($filter:AuthorFilter){
                      queryAuthor(filter:$filter){
                        name
                        reputation
                        posts {
                          text
                        }
                      }
				    }`,
			variables: map[string]interface{}{"filter": map[string]interface{}{"id": cast.ToInt(authors[0].ID)}},
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "posts": [
								{
								  "text": "Got ya!"
								}
							  ]
							}
						  ]
						}`,
		},
		{

			name: "Query using multiple ID given in variable of type integer coerced to string",
			query: `query($filter:AuthorFilter){
                      queryAuthor(filter:$filter){
                        name
                        reputation
                        posts {
                          title
                        }
                      }
				    }`,
			variables: map[string]interface{}{
				"filter": map[string]interface{}{"id": []int{cast.ToInt(authors[0].ID), cast.ToInt(authors[1].ID)}},
			},
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "posts": [
								{
								  "title": "A show about nothing"
								}
							  ]
							},
							{
							  "name": "Jerry",
							  "reputation": 4.6,
							  "posts": [
								{
								  "title": "Outside"
								}
							  ]
							}
						  ]
						}`,
		},
		{

			name: "Query using single ID in a filter of type integer coerced to string",
			query: `query{
			         queryAuthor(filter:{id:` + authorIdsDecimal[0] + `}){
			           name
					   reputation
			           posts {
			             title
			           }
			         }
				    }`,
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "posts": [
								{
								  "title": "A show about nothing"
								}
							  ]
							}
						  ]
						}`,
		},
		{

			name: "Query using multiple ID in a filter of type integer coerced to string",
			query: `query{
			         queryAuthor(filter:{id:[` + authorIdsDecimal[0] + `,` + authorIdsDecimal[1] + `]}){
			           name
                       reputation
			           posts {
			             title
			           }
			         }
				    }`,
			respData: `{
						  "queryAuthor": [
							{
							  "name": "George",
							  "reputation": 4.5,
							  "posts": [
								{
								  "title": "A show about nothing"
								}
							  ]
							},
							{
							  "name": "Jerry",
							  "reputation": 4.6,
							  "posts": [
								{
								  "title": "Outside"
								}
							  ]
							}
						  ]
						}`,
		},
	}

	for _, tcase := range tcases {
		t.Run(tcase.name, func(t *testing.T) {
			params := &GraphQLParams{
				Query:     tcase.query,
				Variables: tcase.variables,
			}
			resp := params.ExecuteAsPost(t, GraphqlURL)
			RequireNoGQLErrors(t, resp)
			testutil.CompareJSON(t, tcase.respData, string(resp.Data))
		})
	}

	// cleanup
	deleteAuthors(t, authorIds, nil)
	deleteCountry(t, map[string]interface{}{"id": countryIds}, len(countryIds), nil)
	DeleteGqlType(t, "Post", map[string]interface{}{"postID": postIds}, len(postIds), nil)
}

func idDirectiveWithInt64(t *testing.T) {
	query := &GraphQLParams{
		Query: `query {
		  getBook(bookId: 1234567890) {
			bookId
			name
			desc
		  }
		}`,
	}

	response := query.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, response)
	var expected = `{
			"getBook": {
				"bookId": 1234567890,
				"name": "Dgraph and Graphql",
				"desc": "All love between dgraph and graphql"
			  }
		 }`
	require.JSONEq(t, expected, string(response.Data))
}

func idDirectiveWithInt(t *testing.T) {
	query := &GraphQLParams{
		Query: `query {
		  getChapter(chapterId: 1) {
			chapterId
			name
		  }
		}`,
	}

	response := query.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, response)
	var expected = `{
	  	"getChapter": {
			"chapterId": 1,
			"name": "How Dgraph Works"
		  }
	}`
	require.JSONEq(t, expected, string(response.Data))
}

func queryMultipleLangFields(t *testing.T) {
	// add three Persons
	addPersonParams := &GraphQLParams{
		Query: `
		mutation addPerson($person: [AddPersonInput!]!) {
	       addPerson(input: $person) {
             numUids
	       }
        }`,
		Variables: map[string]interface{}{"person": []interface{}{
			map[string]interface{}{
				"name":         "Bob",
				"professionEn": "writer",
			},
			map[string]interface{}{
				"name":         "Alice",
				"nameHi":       "ऐलिस",
				"professionEn": "cricketer",
			},
			map[string]interface{}{
				"name":         "Juliet",
				"nameHi":       "जूलियट",
				"nameZh":       "朱丽叶",
				"professionEn": "singer",
			},
		}},
	}

	gqlResponse := addPersonParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	queryPerson := &GraphQLParams{
		Query: `
			query {
             queryPerson(
               filter: {
                 or: [
                   { name: { eq: "Bob" } }
                   { nameHi: { eq: "ऐलिस" } }
                   { nameZh: { eq: "朱丽叶" } }
                 ]
               }
               order: { desc: nameHi }
             ) {
               name
               nameZh
               nameHi
               nameHiZh
               nameZhHi
               nameHi_Zh_Untag
               name_Untag_AnyLang
               professionEn
             }
          }`,
	}
	gqlResponse = queryPerson.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	queryPersonExpected := `
		{
			"queryPerson":	[
				{
					"name":"Juliet",
					"nameZh":"朱丽叶",
					"nameHi":"जूलियट",
					"nameHiZh":"जूलियट",
					"nameZhHi":"朱丽叶",
					"nameHi_Zh_Untag":"जूलियट",
					"name_Untag_AnyLang":"Juliet",
					"professionEn":"singer"
				},
				{
					"name":"Alice",
					"nameZh":null,
					"nameHi":"ऐलिस",
					"nameHiZh":"ऐलिस",
					"nameZhHi":"ऐलिस",
					"nameHi_Zh_Untag":"ऐलिस",
					"name_Untag_AnyLang":"Alice",
					"professionEn":"cricketer"
				},
				{	"name":"Bob",
					"nameZh":null,
					"nameHi":null,
					"nameHiZh":null,
					"nameZhHi":null,
					"nameHi_Zh_Untag":"Bob",
					"name_Untag_AnyLang":"Bob",
					"professionEn":"writer"
				}
			]
		}`

	JSONEqGraphQL(t, queryPersonExpected, string(gqlResponse.Data))
	// Cleanup
	DeleteGqlType(t, "Person", map[string]interface{}{}, 3, nil)
}

func queryWithIDFieldAndInterfaceArg(t *testing.T) {
	// add library member
	addLibraryMemberParams := &GraphQLParams{
		Query: `mutation addLibraryMember($input: [AddLibraryMemberInput!]!) {
                         addLibraryMember(input: $input, upsert: false) {
                          libraryMember {
                           refID
                          }
                         }
                        }`,
		Variables: map[string]interface{}{"input": []interface{}{
			map[string]interface{}{
				"refID":       "101",
				"name":        "Alice",
				"itemsIssued": []string{"Intro to Go", "Parallel Programming"},
				"readHours":   "4d2hr",
			}},
		},
	}

	gqlResponse := addLibraryMemberParams.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)

	queryMember := &GraphQLParams{
		Query: `
			query {
				getMember(refID: "101") {
					refID
					name
					itemsIssued
					... on LibraryMember {
						readHours
					}
				}
			}`,
	}

	gqlResponse = queryMember.ExecuteAsPost(t, GraphqlURL)
	RequireNoGQLErrors(t, gqlResponse)
	queryPersonExpected := `
		  {
              "getMember": {
                  "refID": "101",
                  "name": "Alice",
                  "itemsIssued": [
                      "Parallel Programming",
                      "Intro to Go"
                  ],
                  "readHours": "4d2hr"
              }
          }`

	require.JSONEq(t, queryPersonExpected, string(gqlResponse.Data))
	// Cleanup
	DeleteGqlType(t, "LibraryMember", map[string]interface{}{}, 1, nil)
}
