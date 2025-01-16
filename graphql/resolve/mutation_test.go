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
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	dgoapi "github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/graphql/dgraph"
	"github.com/hypermodeinc/dgraph/v24/graphql/schema"
	"github.com/hypermodeinc/dgraph/v24/graphql/test"
	"github.com/hypermodeinc/dgraph/v24/testutil"
	"github.com/hypermodeinc/dgraph/v24/x"
)

// Tests showing that GraphQL mutations -> Dgraph mutations
// is working as expected.
//
// Note: this doesn't include GQL validation errors!  The rewriting code assumes
// it's rewriting a mutation that's valid (with valid variables) for the schema.
// So can't test GQL errors here - that's integration testing on the pipeline to
// ensure that those errors get caught before they reach rewriting.

type testCase struct {
	Name            string
	GQLMutation     string
	GQLVariables    string
	Explanation     string
	DGMutations     []*dgraphMutation
	DGMutationsSec  []*dgraphMutation
	DGQuery         string
	DGQuerySec      string
	Error           *x.GqlError
	Error2          *x.GqlError
	ValidationError *x.GqlError
	QNameToUID      string
}

type dgraphMutation struct {
	SetJSON    string
	DeleteJSON string
	Cond       string
}

func TestMutationRewriting(t *testing.T) {
	t.Run("Validate Mutations", func(t *testing.T) {
		mutationValidation(t, "validate_mutation_test.yaml", NewAddRewriter)
	})
	t.Run("Add Mutation Rewriting", func(t *testing.T) {
		mutationRewriting(t, "add_mutation_test.yaml", NewAddRewriter)
	})
	t.Run("Update Mutation Rewriting", func(t *testing.T) {
		mutationRewriting(t, "update_mutation_test.yaml", NewUpdateRewriter)
	})
	t.Run("Delete Mutation Rewriting", func(t *testing.T) {
		deleteMutationRewriting(t, "delete_mutation_test.yaml", NewDeleteRewriter)
	})
}

func mutationValidation(t *testing.T, file string, rewriterFactory func() MutationRewriter) {
	b, err := os.ReadFile(file)
	require.NoError(t, err, "Unable to read test file")

	var tests []testCase
	err = yaml.Unmarshal(b, &tests)
	require.NoError(t, err, "Unable to unmarshal tests to yaml.")

	gqlSchema := test.LoadSchemaFromFile(t, "schema.graphql")

	for _, tcase := range tests {
		t.Run(tcase.Name, func(t *testing.T) {
			// -- Arrange --
			var vars map[string]interface{}
			if tcase.GQLVariables != "" {
				require.NoError(t, json.Unmarshal([]byte(tcase.GQLVariables), &vars))
			}

			_, err := gqlSchema.Operation(
				&schema.Request{
					Query:     tcase.GQLMutation,
					Variables: vars,
				})

			require.NotNil(t, err)
			require.Equal(t, err.Error(), tcase.ValidationError.Error())
		})
	}
}

func benchmark3LevelDeep(num int, b *testing.B) {
	t := &testing.T{}
	gqlSchema := test.LoadSchemaFromFile(t, "schema.graphql")

	innerTeachers := make([]interface{}, 0)
	for i := 1; i <= num; i++ {
		innerTeachers = append(innerTeachers, map[string]interface{}{
			"xid":  fmt.Sprintf("S%d", i),
			"name": fmt.Sprintf("Name%d", i),
		})
	}

	vars := map[string]interface{}{
		"input": []interface{}{map[string]interface{}{
			"xid":  "S0",
			"name": "Name0",
			"taughtBy": []interface{}{map[string]interface{}{
				"xid":     "T0",
				"name":    "Teacher0",
				"teaches": innerTeachers,
			}},
		}},
	}

	op, _ := gqlSchema.Operation(
		&schema.Request{
			Query: `
			mutation addStudent($input: [AddStudentInput!]!) {
				addStudent(input: $input) {
					student {
						xid
					}
				}
			}`,
			Variables: vars,
		})
	mut := test.GetMutation(t, op)

	addRewriter := NewAddRewriter()
	idExistence := make(map[string]string)
	for n := 0; n < b.N; n++ {
		_, _, _ = addRewriter.RewriteQueries(context.Background(), mut)
		_, _ = addRewriter.Rewrite(context.Background(), mut, idExistence)
	}
}

func Benchmark3LevelDeep5(b *testing.B)     { benchmark3LevelDeep(5, b) }
func Benchmark3LevelDeep19(b *testing.B)    { benchmark3LevelDeep(19, b) }
func Benchmark3LevelDeep100(b *testing.B)   { benchmark3LevelDeep(100, b) }
func Benchmark3LevelDeep1000(b *testing.B)  { benchmark3LevelDeep(1000, b) }
func Benchmark3LevelDeep10000(b *testing.B) { benchmark3LevelDeep(10000, b) }

func deleteMutationRewriting(t *testing.T, file string, rewriterFactory func() MutationRewriter) {
	b, err := os.ReadFile(file)
	require.NoError(t, err, "Unable to read test file")

	var tests []testCase
	err = yaml.Unmarshal(b, &tests)
	require.NoError(t, err, "Unable to unmarshal tests to yaml.")

	gqlSchema := test.LoadSchemaFromFile(t, "schema.graphql")

	compareMutations := func(t *testing.T, test []*dgraphMutation, generated []*dgoapi.Mutation) {
		require.Len(t, generated, len(test))
		for i, expected := range test {
			require.Equal(t, expected.Cond, generated[i].Cond)
			if len(generated[i].SetJson) > 0 || expected.SetJSON != "" {
				require.JSONEq(t, expected.SetJSON, string(generated[i].SetJson))
			}
			if len(generated[i].DeleteJson) > 0 || expected.DeleteJSON != "" {
				require.JSONEq(t, expected.DeleteJSON, string(generated[i].DeleteJson))
			}
		}
	}

	for _, tcase := range tests {
		t.Run(tcase.Name, func(t *testing.T) {
			// -- Arrange --
			var vars map[string]interface{}
			if tcase.GQLVariables != "" {
				require.NoError(t, json.Unmarshal([]byte(tcase.GQLVariables), &vars))
			}

			op, err := gqlSchema.Operation(
				&schema.Request{
					Query:     tcase.GQLMutation,
					Variables: vars,
				})
			if tcase.ValidationError != nil {
				require.NotNil(t, err)
				require.Equal(t, tcase.ValidationError.Error(), err.Error())
				return
			} else {
				require.NoError(t, err)
			}
			mut := test.GetMutation(t, op)
			rewriterToTest := rewriterFactory()

			// -- Act --
			_, _, _ = rewriterToTest.RewriteQueries(context.Background(), mut)
			idExistence := make(map[string]string)
			upsert, err := rewriterToTest.Rewrite(context.Background(), mut, idExistence)
			// -- Assert --
			if tcase.Error != nil || err != nil {
				require.NotNil(t, err)
				require.NotNil(t, tcase.Error)
				require.Equal(t, tcase.Error.Error(), err.Error())
				return
			}

			require.Equal(t, tcase.DGQuery, dgraph.AsString(upsert[0].Query))
			compareMutations(t, tcase.DGMutations, upsert[0].Mutations)

			if len(upsert) > 1 {
				require.Equal(t, tcase.DGQuerySec, dgraph.AsString(upsert[1].Query))
				compareMutations(t, tcase.DGMutationsSec, upsert[1].Mutations)
			}
		})
	}
}

func mutationRewriting(t *testing.T, file string, rewriterFactory func() MutationRewriter) {
	b, err := os.ReadFile(file)
	require.NoError(t, err, "Unable to read test file")

	var tests []testCase
	err = yaml.Unmarshal(b, &tests)
	require.NoError(t, err, "Unable to unmarshal tests to yaml.")

	gqlSchema := test.LoadSchemaFromFile(t, "schema.graphql")

	compareMutations := func(t *testing.T, test []*dgraphMutation, generated []*dgoapi.Mutation) {
		require.Len(t, generated, len(test))
		for i, expected := range test {
			require.Equal(t, expected.Cond, generated[i].Cond)
			if len(generated[i].SetJson) > 0 || expected.SetJSON != "" {
				require.JSONEq(t, expected.SetJSON, string(generated[i].SetJson))
			}

			if len(generated[i].DeleteJson) > 0 || expected.DeleteJSON != "" {
				require.JSONEq(t, expected.DeleteJSON, string(generated[i].DeleteJson))
			}
		}
	}

	for _, tcase := range tests {
		t.Run(tcase.Name, func(t *testing.T) {
			// -- Arrange --
			var vars map[string]interface{}
			if tcase.GQLVariables != "" {
				require.NoError(t, json.Unmarshal([]byte(tcase.GQLVariables), &vars))
			}

			op, err := gqlSchema.Operation(
				&schema.Request{
					Query:     tcase.GQLMutation,
					Variables: vars,
				})
			if tcase.ValidationError != nil {
				require.NotNil(t, err)
				require.Equal(t, tcase.ValidationError.Error(), err.Error())
				return
			} else {
				require.NoError(t, err)
			}
			mut := test.GetMutation(t, op)

			rewriterToTest := rewriterFactory()

			// -- Query --
			queries, _, err := rewriterToTest.RewriteQueries(context.Background(), mut)
			// -- Assert --
			if tcase.Error != nil || err != nil {
				require.NotNil(t, err)
				require.NotNil(t, tcase.Error)
				require.Equal(t, tcase.Error.Error(), err.Error())
				return
			}
			require.Equal(t, tcase.DGQuery, dgraph.AsString(queries))

			// -- Parse qNameToUID map
			qNameToUID := make(map[string]string)
			if tcase.QNameToUID != "" {
				err = json.Unmarshal([]byte(tcase.QNameToUID), &qNameToUID)
				require.NoError(t, err)
			}

			// Mutate
			upsert, err := rewriterToTest.Rewrite(context.Background(), mut, qNameToUID)
			if tcase.Error2 != nil || err != nil {
				require.NotNil(t, err)
				require.NotNil(t, tcase.Error2)
				require.Equal(t, tcase.Error2.Error(), err.Error())
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, len(upsert))
			compareMutations(t, tcase.DGMutations, upsert[0].Mutations)

			// Compare the query generated along with mutations.
			dgQuerySec := dgraph.AsString(upsert[0].Query)
			require.Equal(t, tcase.DGQuerySec, dgQuerySec)
		})
	}
}

func TestMutationQueryRewriting(t *testing.T) {
	testTypes := map[string]struct {
		mut         string
		payloadType string
		rewriter    func() MutationRewriter
		idExistence map[string]string
		assigned    map[string]string
		result      map[string]interface{}
	}{
		"Add Post ": {
			mut:         `addPost(input: [{title: "A Post", author: {id: "0x1"}}])`,
			payloadType: "AddPostPayload",
			rewriter:    NewAddRewriter,
			idExistence: map[string]string{"Author_1": "0x1"},
			assigned:    map[string]string{"Post_2": "0x4"},
		},
		"Update Post ": {
			mut: `updatePost(input: {filter: {postID
				:  ["0x4"]}, set: {text: "Updated text"} }) `,
			payloadType: "UpdatePostPayload",
			rewriter:    NewUpdateRewriter,
			result: map[string]interface{}{
				"updatePost": []interface{}{map[string]interface{}{"uid": "0x4"}}},
		},
	}

	allowedTestTypes := map[string][]string{
		"UPDATE_MUTATION":     {"Update Post "},
		"ADD_UPDATE_MUTATION": {"Add Post ", "Update Post "},
	}

	b, err := os.ReadFile("mutation_query_test.yaml")
	require.NoError(t, err, "Unable to read test file")

	var tests map[string][]QueryRewritingCase
	err = yaml.Unmarshal(b, &tests)
	require.NoError(t, err, "Unable to unmarshal tests to yaml.")

	gqlSchema := test.LoadSchemaFromFile(t, "schema.graphql")

	for testType := range tests {
		for _, name := range allowedTestTypes[testType] {
			tt := testTypes[name]
			for _, tcase := range tests[testType] {
				t.Run(name+testType+tcase.Name, func(t *testing.T) {
					rewriter := tt.rewriter()
					// -- Arrange --
					gqlMutationStr := strings.Replace(tcase.GQLQuery, testType, tt.mut, 1)
					tcase.DGQuery = strings.Replace(tcase.DGQuery, "PAYLOAD_TYPE",
						tt.payloadType, 1)
					var vars map[string]interface{}
					if tcase.GQLVariables != "" {
						require.NoError(t, json.Unmarshal([]byte(tcase.GQLVariables), &vars))
					}
					op, err := gqlSchema.Operation(
						&schema.Request{
							Query:     gqlMutationStr,
							Variables: vars,
						})
					require.NoError(t, err)
					gqlMutation := test.GetMutation(t, op)

					_, _, _ = rewriter.RewriteQueries(context.Background(), gqlMutation)
					_, err = rewriter.Rewrite(context.Background(), gqlMutation, tt.idExistence)
					require.NoError(t, err)

					// -- Act --
					dgQuery, err := rewriter.FromMutationResult(
						context.Background(), gqlMutation, tt.assigned, tt.result)

					// -- Assert --
					require.NoError(t, err)
					require.Equal(t, tcase.DGQuery, dgraph.AsString(dgQuery))
				})
			}
		}
	}
}

func TestCustomHTTPMutation(t *testing.T) {
	b, err := os.ReadFile("custom_mutation_test.yaml")
	require.NoError(t, err, "Unable to read test file")

	var tests []HTTPRewritingCase
	err = yaml.Unmarshal(b, &tests)
	require.NoError(t, err, "Unable to unmarshal tests to yaml.")

	gqlSchema := test.LoadSchemaFromFile(t, "schema.graphql")

	for _, tcase := range tests {
		t.Run(tcase.Name, func(t *testing.T) {
			var vars map[string]interface{}
			if tcase.Variables != "" {
				require.NoError(t, json.Unmarshal([]byte(tcase.Variables), &vars))
			}

			op, err := gqlSchema.Operation(
				&schema.Request{
					Query:     tcase.GQLQuery,
					Variables: vars,
					Header: map[string][]string{
						"bogus":       {"header"},
						"X-App-Token": {"val"},
						"Auth0-Token": {"tok"},
					},
				})
			require.NoError(t, err)
			gqlMutation := test.GetMutation(t, op)

			client := newClient(t, tcase)
			resolver := NewHTTPMutationResolver(client)
			resolved, isResolved := resolver.Resolve(context.Background(), gqlMutation)
			require.True(t, isResolved)

			testutil.CompareJSON(t, tcase.ResolvedResponse, string(resolved.Data))
		})
	}
}
