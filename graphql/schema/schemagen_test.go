/*
 * Copyright 2017-2025 Hypermode Inc. and Contributors
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

package schema

import (
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/dgraph-io/gqlparser/v2/gqlerror"
	_ "github.com/dgraph-io/gqlparser/v2/validator/rules"
	"github.com/dgraph-io/ristretto/v2/z"
	dschema "github.com/hypermodeinc/dgraph/v24/schema"
	"github.com/hypermodeinc/dgraph/v24/x"
)

type Tests map[string][]TestCase

type TestCase struct {
	Name    string
	Input   string
	Errlist gqlerror.List
	Output  string
}

func TestDGSchemaGen(t *testing.T) {
	fileName := "dgraph_schemagen_test.yml"
	byts, err := os.ReadFile(fileName)
	require.NoError(t, err, "Unable to read file %s", fileName)

	var tests Tests
	err = yaml.Unmarshal(byts, &tests)
	require.NoError(t, err, "Unable to unmarshal to yaml!")

	for _, schemas := range tests {
		for _, sch := range schemas {
			t.Run(sch.Name, func(t *testing.T) {

				schHandler, errs := NewHandler(sch.Input, false)
				require.NoError(t, errs)

				dgSchema := schHandler.DGSchema()
				if diff := cmp.Diff(strings.Split(sch.Output, "\n"),
					strings.Split(dgSchema, "\n")); diff != "" {
					t.Errorf("schema mismatch (-want +got):\n%s", diff)
				}
				_, err := dschema.Parse(dgSchema)
				require.NoError(t, err)
			})
		}
	}
}

func TestSchemaString(t *testing.T) {
	inputDir := "testdata/schemagen/input/"
	outputDir := "testdata/schemagen/output/"

	files, err := os.ReadDir(inputDir)
	require.NoError(t, err)

	for _, testFile := range files {
		t.Run(testFile.Name(), func(t *testing.T) {

			inputFileName := inputDir + testFile.Name()
			str1, err := os.ReadFile(inputFileName)
			require.NoError(t, err)

			schHandler, errs := NewHandler(string(str1), false)
			require.NoError(t, errs)

			newSchemaStr := schHandler.GQLSchema()

			_, err = FromString(newSchemaStr, x.GalaxyNamespace)
			require.NoError(t, err)
			outputFileName := outputDir + testFile.Name()
			str2, err := os.ReadFile(outputFileName)
			require.NoError(t, err)
			if diff := cmp.Diff(string(str2), newSchemaStr); diff != "" {
				// fmt.Printf("Generated Schema (%s):\n%s\n", testFile.Name(), newSchemaStr)
				t.Errorf("schema mismatch - diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestApolloServiceQueryResult(t *testing.T) {
	inputDir := "testdata/apolloservice/input/"
	outputDir := "testdata/apolloservice/output/"

	files, err := os.ReadDir(inputDir)
	require.NoError(t, err)

	for _, testFile := range files {
		t.Run(testFile.Name(), func(t *testing.T) {
			inputFileName := inputDir + testFile.Name()
			str1, err := os.ReadFile(inputFileName)
			require.NoError(t, err)

			schHandler, errs := NewHandler(string(str1), true)
			require.NoError(t, errs)

			apolloServiceResult := schHandler.GQLSchemaWithoutApolloExtras()

			_, err = FromString(schHandler.GQLSchema(), x.GalaxyNamespace)
			require.NoError(t, err)
			outputFileName := outputDir + testFile.Name()
			str2, err := os.ReadFile(outputFileName)
			require.NoError(t, err)
			if diff := cmp.Diff(string(str2), apolloServiceResult); diff != "" {
				t.Errorf("result mismatch - diff (- want +got):\n%s", diff)
			}
		})
	}
}

func TestSchemas(t *testing.T) {
	fileName := "gqlschema_test.yml"
	byts, err := os.ReadFile(fileName)
	require.NoError(t, err, "Unable to read file %s", fileName)

	var tests Tests
	err = yaml.Unmarshal(byts, &tests)
	require.NoError(t, err, "Error Unmarshalling to yaml!")

	t.Run("Valid Schemas", func(t *testing.T) {
		for _, sch := range tests["valid_schemas"] {
			t.Run(sch.Name, func(t *testing.T) {
				schHandler, errlist := NewHandler(sch.Input, false)
				require.NoError(t, errlist, sch.Name)

				newSchemaStr := schHandler.GQLSchema()

				_, err = FromString(newSchemaStr, x.GalaxyNamespace)
				require.NoError(t, err)
			})
		}
	})

	t.Run("Invalid Schemas", func(t *testing.T) {
		for _, sch := range tests["invalid_schemas"] {
			t.Run(sch.Name, func(t *testing.T) {
				schHandler, errlist := NewHandler(sch.Input, false)
				if errlist == nil {
					_, errlist = FromString(schHandler.GQLSchema(), x.GalaxyNamespace)
				}
				if diff := cmp.Diff(sch.Errlist, errlist, cmpopts.IgnoreUnexported(gqlerror.Error{})); diff != "" {
					t.Errorf("error mismatch (-want +got):\n%s", diff)
				}
			})
		}
	})
}

func TestAuthSchemas(t *testing.T) {
	fileName := "auth_schemas_test.yaml"
	byts, err := os.ReadFile(fileName)
	require.NoError(t, err, "Unable to read file %s", fileName)

	var tests map[string][]struct {
		Name    string
		Input   string
		Errlist x.GqlErrorList
		Output  string
	}
	err = yaml.Unmarshal(byts, &tests)
	require.NoError(t, err, "Error Unmarshalling to yaml!")

	t.Run("Valid Schemas", func(t *testing.T) {
		for _, sch := range tests["valid_schemas"] {
			t.Run(sch.Name, func(t *testing.T) {
				schHandler, errlist := NewHandler(sch.Input, false)
				require.NoError(t, errlist, sch.Name)

				_, authError := FromString(schHandler.GQLSchema(), x.GalaxyNamespace)
				require.NoError(t, authError, sch.Name)
			})
		}
	})

	t.Run("Invalid Schemas", func(t *testing.T) {
		for _, sch := range tests["invalid_schemas"] {
			t.Run(sch.Name, func(t *testing.T) {
				schHandler, errlist := NewHandler(sch.Input, false)
				require.NoError(t, errlist, sch.Name)

				_, authError := FromString(schHandler.GQLSchema(), x.GalaxyNamespace)

				if diff := cmp.Diff(authError, sch.Errlist); diff != "" {
					t.Errorf("error mismatch (-want +got):\n%s", diff)
				}
			})
		}
	})
}

// The other tests verify that @search works where it is expected to work,
// and show what the error messages look like.  This test shows all the cases
// that shouldn't work - i.e. we'll never accept a search where we don't
// expect one.  It's too annoying to have all the errors for this, so It just
// makes sure that there are as many errors as cases.
func TestOnlyCorrectSearchArgsWork(t *testing.T) {
	tests := map[string]struct {
		schema         string
		expectedErrors int
	}{
		"String searches don't apply to Int": {schema: `
			type X {
				str1: Int @search(by: [hash])
				str2: Int @search(by: [exact])
				str3: Int @search(by: [term])
				str4: Int @search(by: [fulltext])
				str5: Int @search(by: [trigram])
				str6: Int @search(by: [regexp])
			}`,
			expectedErrors: 6},
		"String searches don't apply to Float": {schema: `
			type X {
				str1: Float @search(by: [hash])
				str2: Float @search(by: [exact])
				str3: Float @search(by: [term])
				str4: Float @search(by: [fulltext])
				str5: Float @search(by: [trigram])
				str6: Float @search(by: [regexp])
			}`,
			expectedErrors: 6},
		"String searches don't apply to Boolean": {schema: `
			type X {
				str1: Boolean @search(by: [hash])
				str2: Boolean @search(by: [exact])
				str3: Boolean @search(by: [term])
				str4: Boolean @search(by: [fulltext])
				str5: Boolean @search(by: [trigram])
				str6: Boolean @search(by: [regexp])
			}`,
			expectedErrors: 6},
		"String searches don't apply to DateTime": {schema: `
			type X {
				str1: DateTime @search(by: [hash])
				str2: DateTime @search(by: [exact])
				str3: DateTime @search(by: [term])
				str4: DateTime @search(by: [fulltext])
				str5: DateTime @search(by: [trigram])
				str6: DateTime @search(by: [regexp])
			}`,
			expectedErrors: 6},
		"DateTime searches don't apply to Int": {schema: `
			type X {
				dt1: Int @search(by: [year])
				dt2: Int @search(by: [month])
				dt3: Int @search(by: [day])
				dt4: Int @search(by: [hour])
			}`,
			expectedErrors: 4},
		"DateTime searches don't apply to Float": {schema: `
			type X {
				dt1: Float @search(by: [year])
				dt2: Float @search(by: [month])
				dt3: Float @search(by: [day])
				dt4: Float @search(by: [hour])
			}`,
			expectedErrors: 4},
		"DateTime searches don't apply to Boolean": {schema: `
			type X {
				dt1: Boolean @search(by: [year])
				dt2: Boolean @search(by: [month])
				dt3: Boolean @search(by: [day])
				dt4: Boolean @search(by: [hour])
			}`,
			expectedErrors: 4},
		"DateTime searches don't apply to String": {schema: `
			type X {
				dt1: String @search(by: [year])
				dt2: String @search(by: [month])
				dt3: String @search(by: [day])
				dt4: String @search(by: [hour])
			}`,
			expectedErrors: 4},
		"Int searches only appy to Int": {schema: `
			type X {
				i1: Float @search(by: [int])
				i2: Boolean @search(by: [int])
				i3: String @search(by: [int])
				i4: DateTime @search(by: [int])
			}`,
			expectedErrors: 4},
		"Float searches only appy to Float": {schema: `
			type X {
				f1: Int @search(by: [float])
				f2: Boolean @search(by: [float])
				f3: String @search(by: [float])
				f4: DateTime @search(by: [float])
			}`,
			expectedErrors: 4},
		"Boolean searches only appy to Boolean": {schema: `
			type X {
				b1: Int @search(by: [bool])
				b2: Float @search(by: [bool])
				b3: String @search(by: [bool])
				b4: DateTime @search(by: [bool])
			}`,
			expectedErrors: 4},
		"Enums can only have hash, exact, regexp and trigram": {schema: `
			type X {
				e1: E @search(by: [int])
				e2: E @search(by: [float])
				e3: E @search(by: [bool])
				e4: E @search(by: [year])
				e5: E @search(by: [month])
				e6: E @search(by: [day])
				e7: E @search(by: [hour])
				e9: E @search(by: [term])
				e10: E @search(by: [fulltext])
			}
			enum E {
				A
			}`,
			expectedErrors: 9},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, errlist := NewHandler(test.schema, false)
			require.Len(t, errlist, test.expectedErrors,
				"every field in this test applies @search wrongly and should raise an error")
		})
	}
}

func TestMain(m *testing.M) {
	// set up the lambda url for unit tests
	x.Config.GraphQL = z.NewSuperFlag("lambda-url=http://localhost:8086/graphql-worker;").
		MergeAndCheckDefault("lambda-url=;")
	// now run the tests
	m.Run()
}
