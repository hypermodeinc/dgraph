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
package resolve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"gopkg.in/yaml.v3"

	dgoapi "github.com/dgraph-io/dgo/v240/protos/api"
	_ "github.com/dgraph-io/gqlparser/v2/validator/rules" // make gql validator init() all rules
	"github.com/hypermodeinc/dgraph/v24/dql"
	"github.com/hypermodeinc/dgraph/v24/graphql/authorization"
	"github.com/hypermodeinc/dgraph/v24/graphql/dgraph"
	"github.com/hypermodeinc/dgraph/v24/graphql/schema"
	"github.com/hypermodeinc/dgraph/v24/graphql/test"
	"github.com/hypermodeinc/dgraph/v24/testutil"
	"github.com/hypermodeinc/dgraph/v24/x"
)

type AuthQueryRewritingCase struct {
	Name string

	// JWT variables
	JWTVar map[string]interface{}

	// GQL query and variables
	GQLQuery  string
	Variables string

	// Dgraph upsert query and mutations built from the GQL
	DGQuery        string
	DGQuerySec     string
	DGMutations    []*dgraphMutation
	DGMutationsSec []*dgraphMutation

	Length string

	// UIDS and json from the Dgraph result
	Uids        string
	Json        string
	QueryJSON   string
	DeleteQuery string

	// Post-mutation auth query and result Dgraph returns from that query
	AuthQuery string
	AuthJson  string

	// Indicates if we should skip auth query verification when using authExecutor.
	// Example: Top level RBAC rules is true.
	SkipAuth bool

	Error *x.GqlError
}

type authExecutor struct {
	t     *testing.T
	state int

	// existence query and its result in JSON
	dgQuery         string
	queryResultJSON string

	// initial mutation
	dgQuerySec string
	// json is the response of the query following the mutation
	json string
	uids string

	// auth
	authQuery string
	authJson  string

	skipAuth bool
}

func (ex *authExecutor) Execute(ctx context.Context, req *dgoapi.Request,
	field schema.Field) (*dgoapi.Response, error) {
	ex.state++
	// Existence Query is not executed if it is empty. Increment the state value.
	if ex.dgQuery == "" && ex.state == 1 {
		ex.state++
	}
	switch ex.state {
	case 1:
		// existence query.
		require.Equal(ex.t, ex.dgQuery, req.Query)

		// Return mocked result of existence query.
		return &dgoapi.Response{
			Json: []byte(ex.queryResultJSON),
		}, nil

	case 2:
		// mutation to create new nodes
		var assigned map[string]string
		if ex.uids != "" {
			require.NoError(ex.t, json.Unmarshal([]byte(ex.uids), &assigned))
		}

		// Check query generated along with mutation.
		require.Equal(ex.t, ex.dgQuerySec, req.Query)

		if len(assigned) == 0 {
			// skip state 3, there's no new nodes to apply auth to
			ex.state++
		}

		// For rules that don't require auth, it should directly go to step 4.
		if ex.skipAuth {
			ex.state++
		}

		return &dgoapi.Response{
			Json:    []byte(ex.json),
			Uids:    assigned,
			Metrics: &dgoapi.Metrics{NumUids: map[string]uint64{touchedUidsKey: 0}},
		}, nil

	case 3:
		// auth

		// check that we got the expected auth query
		require.Equal(ex.t, ex.authQuery, req.Query)

		// respond to query
		return &dgoapi.Response{
			Json:    []byte(ex.authJson),
			Metrics: &dgoapi.Metrics{NumUids: map[string]uint64{touchedUidsKey: 0}},
		}, nil

	case 4:
		// final result

		return &dgoapi.Response{
			Json:    []byte(`{"done": "and done"}`),
			Metrics: &dgoapi.Metrics{NumUids: map[string]uint64{touchedUidsKey: 0}},
		}, nil
	}

	panic("test failed")
}

func (ex *authExecutor) CommitOrAbort(ctx context.Context,
	tc *dgoapi.TxnContext) (*dgoapi.TxnContext, error) {
	return &dgoapi.TxnContext{}, nil
}

func TestStringCustomClaim(t *testing.T) {
	sch, err := os.ReadFile("../e2e/auth/schema.graphql")
	require.NoError(t, err, "Unable to read schema file")

	authSchema, err := testutil.AppendAuthInfo(sch, jwt.SigningMethodHS256.Name, "", false)
	require.NoError(t, err)

	schema := test.LoadSchemaFromString(t, string(authSchema))
	require.NotNil(t, schema.Meta().AuthMeta())

	// Token with custom claim:
	// "https://xyz.io/jwt/claims": {
	// 	"USERNAME": "Random User",
	// 	"email": "random@dgraph.io"
	//   }
	//
	// It also contains standard claim :  "email": "test@dgraph.io", but the
	// value of "email" gets overwritten by the value present inside custom claim.
	token := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyLCJleHAiOjM1MTYyMzkwMjIsImVtYWlsIjoidGVzdEBkZ3JhcGguaW8iLCJodHRwczovL3h5ei5pby9qd3QvY2xhaW1zIjp7IlVTRVJOQU1FIjoiUmFuZG9tIFVzZXIiLCJlbWFpbCI6InJhbmRvbUBkZ3JhcGguaW8ifX0.6XvP9wlvHx8ZBBMH9iyy49cRiIk7H6NNoZf69USkg2c"
	md := metadata.New(map[string]string{"authorizationJwt": token})
	ctx := metadata.NewIncomingContext(context.Background(), md)

	customClaims, err := schema.Meta().AuthMeta().ExtractCustomClaims(ctx)
	require.NoError(t, err)
	authVar := customClaims.AuthVariables
	result := map[string]interface{}{
		"sub":      "1234567890",
		"name":     "John Doe",
		"USERNAME": "Random User",
		"email":    "random@dgraph.io",
	}
	delete(authVar, "exp")
	delete(authVar, "iat")
	require.Equal(t, authVar, result)
}

func TestAudienceClaim(t *testing.T) {
	sch, err := os.ReadFile("../e2e/auth/schema.graphql")
	require.NoError(t, err, "Unable to read schema file")

	authSchema, err := testutil.AppendAuthInfo(sch, jwt.SigningMethodHS256.Name, "", false)
	require.NoError(t, err)

	schema := test.LoadSchemaFromString(t, string(authSchema))
	require.NotNil(t, schema.Meta().AuthMeta())

	// Verify that authorization information is set correctly.
	metainfo := schema.Meta().AuthMeta()
	require.Equal(t, metainfo.Algo, jwt.SigningMethodHS256.Name)
	require.Equal(t, metainfo.Header, "X-Test-Auth")
	require.Equal(t, metainfo.Namespace, "https://xyz.io/jwt/claims")
	require.Equal(t, metainfo.VerificationKey, "secretkey")
	require.Equal(t, metainfo.Audience, []string{"aud1", "63do0q16n6ebjgkumu05kkeian", "aud5"})

	testCases := []struct {
		name  string
		token string
		err   error
	}{
		{
			name:  `Token with valid audience: { "aud": "63do0q16n6ebjgkumu05kkeian" }`,
			token: "eyJraWQiOiIyRWplN2tIRklLZS92MFRVT3JRYlVJWWJxSWNNUHZ2TFBjM3RSQ25EclBBPSIsImFsZyI6IkhTMjU2In0.eyJzdWIiOiI1MDk1MGI0MC0yNjJmLTRiMjYtODhhNy1jYmJiNzgwYjIxNzYiLCJjb2duaXRvOmdyb3VwcyI6WyJBRE1JTiJdLCJlbWFpbF92ZXJpZmllZCI6dHJ1ZSwiaXNzIjoiaHR0cHM6Ly9jb2duaXRvLWlkcC5hcC1zb3V0aGVhc3QtMi5hbWF6b25hd3MuY29tL2FwLXNvdXRoZWFzdC0yX0dmbWVIZEZ6NCIsImNvZ25pdG86dXNlcm5hbWUiOiI1MDk1MGI0MC0yNjJmLTRiMjYtODhhNy1jYmJiNzgwYjIxNzYiLCJodHRwczovL3h5ei5pby9qd3QvY2xhaW1zIjoie1wiVVNFUlwiOiBcIjUwOTUwYjQwLTI2MmYtNGIyNi04OGE3LWNiYmI3ODBiMjE3NlwiLCBcIlJPTEVcIjogXCJBRE1JTlwifSIsImF1ZCI6IjYzZG8wcTE2bjZlYmpna3VtdTA1a2tlaWFuIiwiZXZlbnRfaWQiOiIzMWM5ZDY4NC0xZDQ1LTQ2ZjctOGMyYi1jYzI3YjFmNmYwMWIiLCJ0b2tlbl91c2UiOiJpZCIsImF1dGhfdGltZSI6MTU5MDMzMzM1NiwibmFtZSI6IkRhdmlkIFBlZWsiLCJleHAiOjQ1OTAzNzYwMzIsImlhdCI6MTU5MDM3MjQzMiwiZW1haWwiOiJkYXZpZEB0eXBlam9pbi5jb20ifQ.g6rAkPdNIJ6wvXOo6F4XmoVqqbGs_CdUHx_k7NrvLY8",
		},
		{
			name:  `Token with invalid audience: { "aud": "invalidAudience" }`,
			token: "eyJraWQiOiIyRWplN2tIRklLZS92MFRVT3JRYlVJWWJxSWNNUHZ2TFBjM3RSQ25EclBBPSIsImFsZyI6IkhTMjU2In0.eyJzdWIiOiI1MDk1MGI0MC0yNjJmLTRiMjYtODhhNy1jYmJiNzgwYjIxNzYiLCJjb2duaXRvOmdyb3VwcyI6WyJBRE1JTiJdLCJlbWFpbF92ZXJpZmllZCI6dHJ1ZSwiaXNzIjoiaHR0cHM6Ly9jb2duaXRvLWlkcC5hcC1zb3V0aGVhc3QtMi5hbWF6b25hd3MuY29tL2FwLXNvdXRoZWFzdC0yX0dmbWVIZEZ6NCIsImNvZ25pdG86dXNlcm5hbWUiOiI1MDk1MGI0MC0yNjJmLTRiMjYtODhhNy1jYmJiNzgwYjIxNzYiLCJodHRwczovL3h5ei5pby9qd3QvY2xhaW1zIjoie1wiVVNFUlwiOiBcIjUwOTUwYjQwLTI2MmYtNGIyNi04OGE3LWNiYmI3ODBiMjE3NlwiLCBcIlJPTEVcIjogXCJBRE1JTlwifSIsImF1ZCI6ImludmFsaWRBdWRpZW5jZSIsImV2ZW50X2lkIjoiMzFjOWQ2ODQtMWQ0NS00NmY3LThjMmItY2MyN2IxZjZmMDFiIiwidG9rZW5fdXNlIjoiaWQiLCJhdXRoX3RpbWUiOjE1OTAzMzMzNTYsIm5hbWUiOiJEYXZpZCBQZWVrIiwiZXhwIjo0NTkwMzc2MDMyLCJpYXQiOjE1OTAzNzI0MzIsImVtYWlsIjoiZGF2aWRAdHlwZWpvaW4uY29tIn0.-8UxKvv6_0_hCbV3f6KEoP223BrCrP0eWWdoG-Gf3FQ",
			err:   fmt.Errorf("JWT `aud` value doesn't match with the audience"),
		},
		{
			name:  "Token without audience field",
			token: "eyJraWQiOiIyRWplN2tIRklLZS92MFRVT3JRYlVJWWJxSWNNUHZ2TFBjM3RSQ25EclBBPSIsImFsZyI6IkhTMjU2In0.eyJzdWIiOiI1MDk1MGI0MC0yNjJmLTRiMjYtODhhNy1jYmJiNzgwYjIxNzYiLCJjb2duaXRvOmdyb3VwcyI6WyJBRE1JTiJdLCJlbWFpbF92ZXJpZmllZCI6dHJ1ZSwiaXNzIjoiaHR0cHM6Ly9jb2duaXRvLWlkcC5hcC1zb3V0aGVhc3QtMi5hbWF6b25hd3MuY29tL2FwLXNvdXRoZWFzdC0yX0dmbWVIZEZ6NCIsImNvZ25pdG86dXNlcm5hbWUiOiI1MDk1MGI0MC0yNjJmLTRiMjYtODhhNy1jYmJiNzgwYjIxNzYiLCJodHRwczovL3h5ei5pby9qd3QvY2xhaW1zIjoie1wiVVNFUlwiOiBcIjUwOTUwYjQwLTI2MmYtNGIyNi04OGE3LWNiYmI3ODBiMjE3NlwiLCBcIlJPTEVcIjogXCJBRE1JTlwifSIsImV2ZW50X2lkIjoiMzFjOWQ2ODQtMWQ0NS00NmY3LThjMmItY2MyN2IxZjZmMDFiIiwidG9rZW5fdXNlIjoiaWQiLCJhdXRoX3RpbWUiOjE1OTAzMzMzNTYsIm5hbWUiOiJEYXZpZCBQZWVrIiwiZXhwIjo0NTkwMzc2MDMyLCJpYXQiOjE1OTAzNzI0MzIsImVtYWlsIjoiZGF2aWRAdHlwZWpvaW4uY29tIn0.Fjxh-sZM9eDRBRHKyLJ8MxAsSSZ-IX2f0z-Saq37t7U",
		},
		{
			name:  `Token with multiple audience: {"aud": ["aud1", "aud2", "aud3"]}`,
			token: "eyJraWQiOiIyRWplN2tIRklLZS92MFRVT3JRYlVJWWJxSWNNUHZ2TFBjM3RSQ25EclBBPSIsImFsZyI6IkhTMjU2In0.eyJzdWIiOiI1MDk1MGI0MC0yNjJmLTRiMjYtODhhNy1jYmJiNzgwYjIxNzYiLCJjb2duaXRvOmdyb3VwcyI6WyJBRE1JTiJdLCJlbWFpbF92ZXJpZmllZCI6dHJ1ZSwiaXNzIjoiaHR0cHM6Ly9jb2duaXRvLWlkcC5hcC1zb3V0aGVhc3QtMi5hbWF6b25hd3MuY29tL2FwLXNvdXRoZWFzdC0yX0dmbWVIZEZ6NCIsImNvZ25pdG86dXNlcm5hbWUiOiI1MDk1MGI0MC0yNjJmLTRiMjYtODhhNy1jYmJiNzgwYjIxNzYiLCJodHRwczovL3h5ei5pby9qd3QvY2xhaW1zIjoie1wiVVNFUlwiOiBcIjUwOTUwYjQwLTI2MmYtNGIyNi04OGE3LWNiYmI3ODBiMjE3NlwiLCBcIlJPTEVcIjogXCJBRE1JTlwifSIsImF1ZCI6WyJhdWQxIiwiYXVkMiIsImF1ZDMiXSwiZXZlbnRfaWQiOiIzMWM5ZDY4NC0xZDQ1LTQ2ZjctOGMyYi1jYzI3YjFmNmYwMWIiLCJ0b2tlbl91c2UiOiJpZCIsImF1dGhfdGltZSI6MTU5MDMzMzM1NiwibmFtZSI6IkRhdmlkIFBlZWsiLCJleHAiOjQ1OTAzNzYwMzIsImlhdCI6MTU5MDM3MjQzMiwiZW1haWwiOiJkYXZpZEB0eXBlam9pbi5jb20ifQ.LK31qlAVQHzu5mvEsPPRoNb59u8X9ITL_1re6wYGEtA",
		},
	}

	for _, tcase := range testCases {
		t.Run(tcase.name, func(t *testing.T) {
			md := metadata.New(map[string]string{"authorizationJwt": tcase.token})
			ctx := metadata.NewIncomingContext(context.Background(), md)

			_, err := metainfo.ExtractCustomClaims(ctx)
			require.Equal(t, tcase.err, err)
		})
	}
}

func TestInvalidAuthInfo(t *testing.T) {
	sch, err := os.ReadFile("../e2e/auth/schema.graphql")
	require.NoError(t, err, "Unable to read schema file")
	authSchema, err := testutil.AppendJWKAndVerificationKey(sch)
	require.NoError(t, err)
	_, err = schema.NewHandler(string(authSchema), false)
	require.Error(t, err, fmt.Errorf("Expecting either JWKUrl/JWKUrls or (VerificationKey, Algo), both were given"))
}

func TestMissingAudienceWithJWKUrl(t *testing.T) {
	sch, err := os.ReadFile("../e2e/auth/schema.graphql")
	require.NoError(t, err, "Unable to read schema file")
	authSchema, err := testutil.AppendAuthInfoWithJWKUrlAndWithoutAudience(sch)
	require.NoError(t, err)
	_, err = schema.NewHandler(string(authSchema), false)
	require.Error(t, err, fmt.Errorf("required field missing in Dgraph.Authorization: `Audience`"))
}

func TestVerificationWithJWKUrl(t *testing.T) {
	sch, err := os.ReadFile("../e2e/auth/schema.graphql")
	require.NoError(t, err, "Unable to read schema file")

	authSchema, err := testutil.AppendAuthInfoWithJWKUrl(sch)
	require.NoError(t, err)

	schema := test.LoadSchemaFromString(t, string(authSchema))
	require.NotNil(t, schema.Meta().AuthMeta())

	// Verify that authorization information is set correctly.
	metainfo := schema.Meta().AuthMeta()
	require.Equal(t, metainfo.Algo, "")
	require.Equal(t, metainfo.Header, "X-Test-Auth")
	require.Equal(t, metainfo.Namespace, "https://xyz.io/jwt/claims")
	require.Equal(t, metainfo.VerificationKey, "")
	require.Equal(t, metainfo.JWKUrl, "")
	require.Equal(t, metainfo.JWKUrls, []string{"https://dev-hr2kugfp.us.auth0.com/.well-known/jwks.json"})

	testCase := struct {
		name  string
		token string
	}{
		name:  `Valid Token`,
		token: "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCIsImtpZCI6IjJKdVZuRkc0Q2JBX0E1VVNkenlDMyJ9.eyJnaXZlbl9uYW1lIjoibWluaGFqIiwiZmFtaWx5X25hbWUiOiJzaGFrZWVsIiwibmlja25hbWUiOiJtc3JpaXRkIiwibmFtZSI6Im1pbmhhaiBzaGFrZWVsIiwicGljdHVyZSI6Imh0dHBzOi8vbGgzLmdvb2dsZXVzZXJjb250ZW50LmNvbS9hLS9BT2gxNEdnYzVEZ2cyQThWZFNzWUNnc2RlR3lFMHM1d01Gdmd2X1htZDA4Q3B3PXM5Ni1jIiwibG9jYWxlIjoiZW4iLCJ1cGRhdGVkX2F0IjoiMjAyMS0wMy0wOVQxMDowOTozNi4yMDNaIiwiZW1haWwiOiJtc3JpaXRkQGdtYWlsLmNvbSIsImVtYWlsX3ZlcmlmaWVkIjp0cnVlLCJpc3MiOiJodHRwczovL2Rldi1ocjJrdWdmcC51cy5hdXRoMC5jb20vIiwic3ViIjoiZ29vZ2xlLW9hdXRoMnwxMDM2NTgyNjIxNzU2NDczNzEwNjQiLCJhdWQiOiJIaGFYa1FWUkJuNWUwSzNEbU1wMnpiakk4aTF3Y3YyZSIsImlhdCI6MTYxNTI4NDU3NywiZXhwIjo1MjE1Mjg0NTc3LCJub25jZSI6IlVtUk9NbTV0WWtoR2NGVjVOWGRhVGtKV1UyWm5ZM0pKUzNSR1ZsWk1jRzFLZUVkMGQzWkdkVTFuYXc9PSJ9.rlVl0tGOCypIts0C52g1qyiNaFV3UnDafJETXTGbt-toWvtCyZsa-JySgwG0DD1rMYm-gdwyJcjJlgwVPQD3ZlkJqbFFNvY4cX5injiOljpVFOHKXdi7tehY9We_vv1KYYpvhGMsE4u7o8tz2wEctdLTXT7omEq7gSdHuDgpM-h-K2RLApU8oyu8YOIqQlrqGgJ7Q8jy-jxMlU7BoZVz38FokjmkSapAAVORsbdEqPgQjeDnjaDQ5bRhxZUMSeKvvpvtVlPaeM1NI4S0R3g0qUGvX6L6qsLZqIilSQUiUaOEo8bLNBFHOxhBbocF-R-x40nSYjdjrEz60A99mz5XAA",
	}

	md := metadata.New(map[string]string{"authorizationJwt": testCase.token})
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err = metainfo.ExtractCustomClaims(ctx)
	require.NoError(t, err)
}

func TestVerificationWithMultipleJWKUrls(t *testing.T) {
	sch, err := os.ReadFile("../e2e/auth/schema.graphql")
	require.NoError(t, err, "Unable to read schema file")

	authSchema, err := testutil.AppendAuthInfoWithMultipleJWKUrls(sch)
	require.NoError(t, err)

	schema := test.LoadSchemaFromString(t, string(authSchema))
	require.NotNil(t, schema.Meta().AuthMeta())

	// Verify that authorization information is set correctly.
	metainfo := schema.Meta().AuthMeta()
	require.Equal(t, metainfo.Algo, "")
	require.Equal(t, metainfo.Header, "X-Test-Auth")
	require.Equal(t, metainfo.Namespace, "https://xyz.io/jwt/claims")
	require.Equal(t, metainfo.VerificationKey, "")
	require.Equal(t, metainfo.JWKUrl, "")
	require.Equal(t, metainfo.JWKUrls,
		[]string{"https://www.googleapis.com/service_accounts/v1/jwk/securetoken@system.gserviceaccount.com",
			"https://dev-hr2kugfp.us.auth0.com/.well-known/jwks.json"},
	)

	testCases := []struct {
		name    string
		token   string
		invalid bool
	}{
		{
			name:    `Expired Token`,
			token:   "eyJhbGciOiJSUzI1NiIsImtpZCI6IjE2NzUwM2UwYWVjNTJkZGZiODk2NTIxYjkxN2ZiOGUyMGMxZjMzMDAiLCJ0eXAiOiJKV1QifQ.eyJpc3MiOiJodHRwczovL3NlY3VyZXRva2VuLmdvb2dsZS5jb20vZmlyLXByb2plY3QxLTI1OWU3IiwiYXVkIjoiZmlyLXByb2plY3QxLTI1OWU3IiwiYXV0aF90aW1lIjoxNjAxNDQ0NjM0LCJ1c2VyX2lkIjoiMTdHb3h2dU5CWlc5YTlKU3Z3WXhROFc0bjE2MyIsInN1YiI6IjE3R294dnVOQlpXOWE5SlN2d1l4UThXNG4xNjMiLCJpYXQiOjE2MDE0NDQ2MzQsImV4cCI6MTYwMTQ0ODIzNCwiZW1haWwiOiJtaW5oYWpAZGdyYXBoLmlvIiwiZW1haWxfdmVyaWZpZWQiOmZhbHNlLCJmaXJlYmFzZSI6eyJpZGVudGl0aWVzIjp7ImVtYWlsIjpbIm1pbmhhakBkZ3JhcGguaW8iXX0sInNpZ25faW5fcHJvdmlkZXIiOiJwYXNzd29yZCJ9fQ.q5YmOzOUkZHNjlz53hgLNSVg-brIU9tLJ4jLC0_Xurl5wEbyZ6D_KQ9-UFqbl2HR6R1V5kpaf6eDFR3c83i1PpCbJ4LTjHAf_njQvL75ByERld23lZtKZyEeE6ujdFXL8ne4fI2qenD1Xeqx9AnXbLf7U_CvZpbX3l1wj7p0Lpn7qixi0AztuLSJMLkMfFpaiwyFZQivi4cqtnI25VIsK6a4KIpl1Sk0AHT-lv9PRadd_JDjWAIzD0SfhpZOskaeA9PljVMp-Y3Xscwg_Qc6u1MIBPg1jKO-ngjhWkgEWBoz5F836P7phT60LVBHhYuk-jRN6HSSNWQ3ineuN-jBkg",
			invalid: true,
		},
		{
			name:    `Valid Token`,
			token:   "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCIsImtpZCI6IjJKdVZuRkc0Q2JBX0E1VVNkenlDMyJ9.eyJnaXZlbl9uYW1lIjoibWluaGFqIiwiZmFtaWx5X25hbWUiOiJzaGFrZWVsIiwibmlja25hbWUiOiJtc3JpaXRkIiwibmFtZSI6Im1pbmhhaiBzaGFrZWVsIiwicGljdHVyZSI6Imh0dHBzOi8vbGgzLmdvb2dsZXVzZXJjb250ZW50LmNvbS9hLS9BT2gxNEdnYzVEZ2cyQThWZFNzWUNnc2RlR3lFMHM1d01Gdmd2X1htZDA4Q3B3PXM5Ni1jIiwibG9jYWxlIjoiZW4iLCJ1cGRhdGVkX2F0IjoiMjAyMS0wMy0wOVQxMDowOTozNi4yMDNaIiwiZW1haWwiOiJtc3JpaXRkQGdtYWlsLmNvbSIsImVtYWlsX3ZlcmlmaWVkIjp0cnVlLCJpc3MiOiJodHRwczovL2Rldi1ocjJrdWdmcC51cy5hdXRoMC5jb20vIiwic3ViIjoiZ29vZ2xlLW9hdXRoMnwxMDM2NTgyNjIxNzU2NDczNzEwNjQiLCJhdWQiOiJIaGFYa1FWUkJuNWUwSzNEbU1wMnpiakk4aTF3Y3YyZSIsImlhdCI6MTYxNTI4NDU3NywiZXhwIjo1MjE1Mjg0NTc3LCJub25jZSI6IlVtUk9NbTV0WWtoR2NGVjVOWGRhVGtKV1UyWm5ZM0pKUzNSR1ZsWk1jRzFLZUVkMGQzWkdkVTFuYXc9PSJ9.rlVl0tGOCypIts0C52g1qyiNaFV3UnDafJETXTGbt-toWvtCyZsa-JySgwG0DD1rMYm-gdwyJcjJlgwVPQD3ZlkJqbFFNvY4cX5injiOljpVFOHKXdi7tehY9We_vv1KYYpvhGMsE4u7o8tz2wEctdLTXT7omEq7gSdHuDgpM-h-K2RLApU8oyu8YOIqQlrqGgJ7Q8jy-jxMlU7BoZVz38FokjmkSapAAVORsbdEqPgQjeDnjaDQ5bRhxZUMSeKvvpvtVlPaeM1NI4S0R3g0qUGvX6L6qsLZqIilSQUiUaOEo8bLNBFHOxhBbocF-R-x40nSYjdjrEz60A99mz5XAA",
			invalid: false,
		},
	}

	for _, tcase := range testCases {
		t.Run(tcase.name, func(t *testing.T) {
			md := metadata.New(map[string]string{"authorizationJwt": tcase.token})
			ctx := metadata.NewIncomingContext(context.Background(), md)

			_, err := metainfo.ExtractCustomClaims(ctx)
			if tcase.invalid {
				require.True(t, strings.Contains(err.Error(),
					"unable to parse jwt token:token is unverifiable: error while executing keyfunc: Invalid kid"))
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TODO(arijit): Generate the JWT token instead of using pre generated token.
func TestJWTExpiry(t *testing.T) {
	sch, err := os.ReadFile("../e2e/auth/schema.graphql")
	require.NoError(t, err, "Unable to read schema file")

	authSchema, err := testutil.AppendAuthInfo(sch, jwt.SigningMethodHS256.Name, "", false)
	require.NoError(t, err)

	schema := test.LoadSchemaFromString(t, string(authSchema))
	require.NotNil(t, schema.Meta().AuthMeta())

	// Verify that authorization information is set correctly.
	metainfo := schema.Meta().AuthMeta()
	require.Equal(t, metainfo.Algo, jwt.SigningMethodHS256.Name)
	require.Equal(t, metainfo.Header, "X-Test-Auth")
	require.Equal(t, metainfo.Namespace, "https://xyz.io/jwt/claims")
	require.Equal(t, metainfo.VerificationKey, "secretkey")

	testCases := []struct {
		name    string
		token   string
		invalid bool
	}{
		{
			name:  `Token without expiry value`,
			token: "eyJraWQiOiIyRWplN2tIRklLZS92MFRVT3JRYlVJWWJxSWNNUHZ2TFBjM3RSQ25EclBBPSIsImFsZyI6IkhTMjU2In0.eyJzdWIiOiI1MDk1MGI0MC0yNjJmLTRiMjYtODhhNy1jYmJiNzgwYjIxNzYiLCJjb2duaXRvOmdyb3VwcyI6WyJBRE1JTiJdLCJlbWFpbF92ZXJpZmllZCI6dHJ1ZSwiaXNzIjoiaHR0cHM6Ly9jb2duaXRvLWlkcC5hcC1zb3V0aGVhc3QtMi5hbWF6b25hd3MuY29tL2FwLXNvdXRoZWFzdC0yX0dmbWVIZEZ6NCIsImNvZ25pdG86dXNlcm5hbWUiOiI1MDk1MGI0MC0yNjJmLTRiMjYtODhhNy1jYmJiNzgwYjIxNzYiLCJodHRwczovL3h5ei5pby9qd3QvY2xhaW1zIjoie1wiVVNFUlwiOiBcIjUwOTUwYjQwLTI2MmYtNGIyNi04OGE3LWNiYmI3ODBiMjE3NlwiLCBcIlJPTEVcIjogXCJBRE1JTlwifSIsImV2ZW50X2lkIjoiMzFjOWQ2ODQtMWQ0NS00NmY3LThjMmItY2MyN2IxZjZmMDFiIiwidG9rZW5fdXNlIjoiaWQiLCJhdXRoX3RpbWUiOjE1OTAzMzMzNTYsIm5hbWUiOiJEYXZpZCBQZWVrIiwiaWF0IjoxNTkwMzcyNDMyLCJlbWFpbCI6ImRhdmlkQHR5cGVqb2luLmNvbSJ9.f79YmZgz_YDBzf0dQ_dY_VQOjpGt4Z_MJ3LsvXrIQeQ",
		},
		{
			name:    `Expired token`,
			token:   "eyJraWQiOiIyRWplN2tIRklLZS92MFRVT3JRYlVJWWJxSWNNUHZ2TFBjM3RSQ25EclBBPSIsImFsZyI6IkhTMjU2In0.eyJzdWIiOiI1MDk1MGI0MC0yNjJmLTRiMjYtODhhNy1jYmJiNzgwYjIxNzYiLCJjb2duaXRvOmdyb3VwcyI6WyJBRE1JTiJdLCJlbWFpbF92ZXJpZmllZCI6dHJ1ZSwiaXNzIjoiaHR0cHM6Ly9jb2duaXRvLWlkcC5hcC1zb3V0aGVhc3QtMi5hbWF6b25hd3MuY29tL2FwLXNvdXRoZWFzdC0yX0dmbWVIZEZ6NCIsImNvZ25pdG86dXNlcm5hbWUiOiI1MDk1MGI0MC0yNjJmLTRiMjYtODhhNy1jYmJiNzgwYjIxNzYiLCJodHRwczovL3h5ei5pby9qd3QvY2xhaW1zIjoie1wiVVNFUlwiOiBcIjUwOTUwYjQwLTI2MmYtNGIyNi04OGE3LWNiYmI3ODBiMjE3NlwiLCBcIlJPTEVcIjogXCJBRE1JTlwifSIsImV2ZW50X2lkIjoiMzFjOWQ2ODQtMWQ0NS00NmY3LThjMmItY2MyN2IxZjZmMDFiIiwidG9rZW5fdXNlIjoiaWQiLCJhdXRoX3RpbWUiOjE1OTAzMzMzNTYsIm5hbWUiOiJEYXZpZCBQZWVrIiwiZXhwIjo1OTAzNzYwMzIsImlhdCI6MTU5MDM3MjQzMiwiZW1haWwiOiJkYXZpZEB0eXBlam9pbi5jb20ifQ.cxTip2mZLf6hYBHYAyJ7pqohhpMdrVOaySFAtp3PfKg",
			invalid: true,
		},
	}

	for _, tcase := range testCases {
		t.Run(tcase.name, func(t *testing.T) {
			md := metadata.New(map[string]string{"authorizationJwt": tcase.token})
			ctx := metadata.NewIncomingContext(context.Background(), md)

			_, err := metainfo.ExtractCustomClaims(ctx)
			if tcase.invalid {
				require.True(t, strings.Contains(err.Error(), "token is expired"))
			}
		})
	}
}

// Tests showing that the query rewriter produces the expected Dgraph queries
// when it also needs to write in auth.
func queryRewriting(t *testing.T, sch string, authMeta *testutil.AuthMeta, b []byte) {
	var tests []AuthQueryRewritingCase
	err := yaml.Unmarshal(b, &tests)
	require.NoError(t, err, "Unable to unmarshal tests to yaml.")

	testRewriter := NewQueryRewriter()
	gqlSchema := test.LoadSchemaFromString(t, sch)

	for _, tcase := range tests {
		t.Run(tcase.Name, func(t *testing.T) {
			op, err := gqlSchema.Operation(
				&schema.Request{
					Query: tcase.GQLQuery,
					// Variables: tcase.Variables,
				})
			require.NoError(t, err)
			gqlQuery := test.GetQuery(t, op)

			// Clear the map and initialize it.
			authMeta.AuthVars = make(map[string]interface{})
			for k, v := range tcase.JWTVar {
				authMeta.AuthVars[k] = v
			}

			ctx := context.Background()
			if !strings.HasPrefix(tcase.Name, "Query with missing jwt token") {
				ctx, err = authMeta.AddClaimsToContext(ctx)
				require.NoError(t, err)
			}

			dgQuery, err := testRewriter.Rewrite(ctx, gqlQuery)

			if tcase.Error != nil {
				require.NotNil(t, err)
				require.Equal(t, err.Error(), tcase.Error.Error())
				require.Nil(t, dgQuery)
			} else {
				require.NoError(t, err)
				require.Equal(t, tcase.DGQuery, dgraph.AsString(dgQuery))
			}
			// Check for unused variables.
			_, err = dql.Parse(dql.Request{Str: dgraph.AsString(dgQuery)})
			require.NoError(t, err)
		})
	}
}

// Tests that the queries that run after a mutation get auth correctly added in.
func mutationQueryRewriting(t *testing.T, sch string, authMeta *testutil.AuthMeta) {
	tests := map[string]struct {
		gqlMut      string
		rewriter    func() MutationRewriter
		assigned    map[string]string
		idExistence map[string]string
		result      map[string]interface{}
		dgQuery     string
	}{
		"Add Ticket": {
			gqlMut: `mutation {
				addTicket(input: [{title: "A ticket", onColumn: {colID: "0x1"}}]) {
				  ticket {
					id
					title
					onColumn {
						colID
						name
					}
				  }
				}
			  }`,
			rewriter:    NewAddRewriter,
			assigned:    map[string]string{"Ticket_2": "0x4"},
			idExistence: map[string]string{"Column_1": "0x1"},
			dgQuery: `query {
  AddTicketPayload.ticket(func: uid(TicketRoot)) {
    Ticket.id : uid
    Ticket.title : Ticket.title
    Ticket.onColumn : Ticket.onColumn @filter(uid(Column_1)) {
      Column.colID : uid
      Column.name : Column.name
    }
  }
  TicketRoot as var(func: uid(Ticket_4)) @filter(uid(Ticket_Auth5))
  Ticket_4 as var(func: uid(0x4))
  Ticket_Auth5 as var(func: uid(Ticket_4)) @cascade {
    Ticket.onColumn : Ticket.onColumn {
      Column.inProject : Column.inProject {
        Project.roles : Project.roles @filter(eq(Role.permission, "VIEW")) {
          Role.assignedTo : Role.assignedTo @filter(eq(User.username, "user1"))
        }
      }
    }
  }
  var(func: uid(TicketRoot)) {
    Column_2 as Ticket.onColumn
  }
  Column_1 as var(func: uid(Column_2)) @filter(uid(Column_Auth3))
  Column_Auth3 as var(func: uid(Column_2)) @cascade {
    Column.inProject : Column.inProject {
      Project.roles : Project.roles @filter(eq(Role.permission, "VIEW")) {
        Role.assignedTo : Role.assignedTo @filter(eq(User.username, "user1"))
      }
    }
  }
}`,
		},
		"Update Ticket": {
			gqlMut: `mutation {
				updateTicket(input: {filter: {id: ["0x4"]}, set: {title: "Updated title"} }) {
					ticket {
						id
						title
						onColumn {
							colID
							name
						}
					  }
				}
			  }`,
			rewriter:    NewUpdateRewriter,
			idExistence: map[string]string{},
			result: map[string]interface{}{
				"updateTicket": []interface{}{map[string]interface{}{"uid": "0x4"}}},
			dgQuery: `query {
  UpdateTicketPayload.ticket(func: uid(TicketRoot)) {
    Ticket.id : uid
    Ticket.title : Ticket.title
    Ticket.onColumn : Ticket.onColumn @filter(uid(Column_1)) {
      Column.colID : uid
      Column.name : Column.name
    }
  }
  TicketRoot as var(func: uid(Ticket_4)) @filter(uid(Ticket_Auth5))
  Ticket_4 as var(func: uid(0x4))
  Ticket_Auth5 as var(func: uid(Ticket_4)) @cascade {
    Ticket.onColumn : Ticket.onColumn {
      Column.inProject : Column.inProject {
        Project.roles : Project.roles @filter(eq(Role.permission, "VIEW")) {
          Role.assignedTo : Role.assignedTo @filter(eq(User.username, "user1"))
        }
      }
    }
  }
  var(func: uid(TicketRoot)) {
    Column_2 as Ticket.onColumn
  }
  Column_1 as var(func: uid(Column_2)) @filter(uid(Column_Auth3))
  Column_Auth3 as var(func: uid(Column_2)) @cascade {
    Column.inProject : Column.inProject {
      Project.roles : Project.roles @filter(eq(Role.permission, "VIEW")) {
        Role.assignedTo : Role.assignedTo @filter(eq(User.username, "user1"))
      }
    }
  }
}`,
		},
	}

	gqlSchema := test.LoadSchemaFromString(t, sch)

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// -- Arrange --
			rewriter := tt.rewriter()
			op, err := gqlSchema.Operation(&schema.Request{Query: tt.gqlMut})
			require.NoError(t, err)
			gqlMutation := test.GetMutation(t, op)

			authMeta.AuthVars = map[string]interface{}{
				"USER": "user1",
			}
			ctx, err := authMeta.AddClaimsToContext(context.Background())
			require.NoError(t, err)

			_, _, _ = rewriter.RewriteQueries(context.Background(), gqlMutation)
			_, err = rewriter.Rewrite(ctx, gqlMutation, tt.idExistence)
			require.NoError(t, err)

			// -- Act --
			dgQuery, err := rewriter.FromMutationResult(
				ctx, gqlMutation, tt.assigned, tt.result)

			// -- Assert --
			require.NoError(t, err)
			require.Equal(t, tt.dgQuery, dgraph.AsString(dgQuery))

			// Check for unused variables.
			_, err = dql.Parse(dql.Request{Str: dgraph.AsString(dgQuery)})
			require.NoError(t, err)
		})

	}
}

// Tests showing that the query rewriter produces the expected Dgraph queries
// for delete when it also needs to write in auth - this doesn't extend to other nodes
// it only ever applies at the top level because delete only deletes the nodes
// referenced by the filter, not anything deeper.
func deleteQueryRewriting(t *testing.T, sch string, authMeta *testutil.AuthMeta, b []byte) {
	var tests []AuthQueryRewritingCase
	err := yaml.Unmarshal(b, &tests)
	require.NoError(t, err, "Unable to unmarshal tests to yaml.")

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

	gqlSchema := test.LoadSchemaFromString(t, sch)

	for _, tcase := range tests {
		t.Run(tcase.Name, func(t *testing.T) {
			// -- Arrange --
			var vars map[string]interface{}
			if tcase.Variables != "" {
				require.NoError(t, json.Unmarshal([]byte(tcase.Variables), &vars))
			}

			op, err := gqlSchema.Operation(
				&schema.Request{
					Query:     tcase.GQLQuery,
					Variables: vars,
				})
			require.NoError(t, err)
			mut := test.GetMutation(t, op)
			rewriterToTest := NewDeleteRewriter()

			// Clear the map and initialize it.
			authMeta.AuthVars = make(map[string]interface{})
			for k, v := range tcase.JWTVar {
				authMeta.AuthVars[k] = v
			}

			ctx := context.Background()
			if !authMeta.ClosedByDefault {
				ctx, err = authMeta.AddClaimsToContext(ctx)
				require.NoError(t, err)
			}

			// -- Act --
			_, _, _ = rewriterToTest.RewriteQueries(context.Background(), mut)
			idExistence := make(map[string]string)
			upsert, err := rewriterToTest.Rewrite(ctx, mut, idExistence)

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

// In an add mutation
//
//	mutation {
//		addAnswer(input: [
//		  {
//			text: "...",
//			datePublished: "2020-03-26",
//			author: { username: "u1" },
//			inAnswerTo: { id: "0x7e" }
//		  }
//		]) {
//		  answer { ... }
//
// There's no initial auth verification.  We add the nodes and then check the auth rules.
// So the only auth to check is through authorizeNewNodes() function.
//
// We don't need to test the json mutations that are created, because those are the same
// as in add_mutation_test.yaml.  What we need to test is the processing around if
// new nodes are checked properly - the query generated to check them, and the post-processing.
func mutationAdd(t *testing.T, sch string, authMeta *testutil.AuthMeta, b []byte) {
	var tests []AuthQueryRewritingCase
	err := yaml.Unmarshal(b, &tests)
	require.NoError(t, err, "Unable to unmarshal tests to yaml.")

	gqlSchema := test.LoadSchemaFromString(t, sch)

	for _, tcase := range tests {
		t.Run(tcase.Name, func(t *testing.T) {
			checkAddUpdateCase(t, gqlSchema, tcase, NewAddRewriter, authMeta)
		})
	}
}

// In an update mutation we first need to check that the generated query only finds the
// authorised nodes - it takes the users filter and applies auth.  Then we need to check
// that any nodes added by the mutation were also allowed.
//
// We don't need to test the json mutations that are created, because those are the same
// as in update_mutation_test.yaml.  What we need to test is the processing around if
// new nodes are checked properly - the query generated to check them, and the post-processing.
func mutationUpdate(t *testing.T, sch string, authMeta *testutil.AuthMeta, b []byte) {
	var tests []AuthQueryRewritingCase
	err := yaml.Unmarshal(b, &tests)
	require.NoError(t, err, "Unable to unmarshal tests to yaml.")

	gqlSchema := test.LoadSchemaFromString(t, sch)
	for _, tcase := range tests {
		t.Run(tcase.Name, func(t *testing.T) {
			checkAddUpdateCase(t, gqlSchema, tcase, NewUpdateRewriter, authMeta)
		})
	}
}

func checkAddUpdateCase(
	t *testing.T,
	gqlSchema schema.Schema,
	tcase AuthQueryRewritingCase,
	rewriter func() MutationRewriter,
	authMeta *testutil.AuthMeta) {
	// -- Arrange --
	var vars map[string]interface{}
	if tcase.Variables != "" {
		require.NoError(t, json.Unmarshal([]byte(tcase.Variables), &vars))
	}

	op, err := gqlSchema.Operation(
		&schema.Request{
			Query:     tcase.GQLQuery,
			Variables: vars,
		})
	require.NoError(t, err)
	mut := test.GetMutation(t, op)

	// Clear the map and initialize it.
	authMeta.AuthVars = make(map[string]interface{})
	for k, v := range tcase.JWTVar {
		authMeta.AuthVars[k] = v
	}

	ctx := context.Background()
	if !authMeta.ClosedByDefault {
		ctx, err = authMeta.AddClaimsToContext(ctx)
		require.NoError(t, err)
	}

	ex := &authExecutor{
		t:               t,
		json:            tcase.Json,
		queryResultJSON: tcase.QueryJSON,
		dgQuerySec:      tcase.DGQuerySec,
		uids:            tcase.Uids,
		dgQuery:         tcase.DGQuery,
		authQuery:       tcase.AuthQuery,
		authJson:        tcase.AuthJson,
		skipAuth:        tcase.SkipAuth,
	}
	resolver := NewDgraphResolver(rewriter(), ex)

	// -- Act --
	resolved, success := resolver.Resolve(ctx, mut)

	// -- Assert --
	// most cases are built into the authExecutor
	if tcase.Error != nil {
		require.False(t, success, "Mutation should have failed as it throws an error")
		require.NotNil(t, resolved.Err)
		require.Equal(t, tcase.Error.Error(), resolved.Err.Error())
	} else {
		require.True(t, success, "Mutation should have not failed as it did not throw an error")
	}
}

func TestAuthQueryRewriting(t *testing.T) {
	sch, err := os.ReadFile("../e2e/auth/schema.graphql")
	require.NoError(t, err, "Unable to read schema file")

	jwtAlgo := []string{jwt.SigningMethodHS256.Name, jwt.SigningMethodRS256.Name}

	for _, algo := range jwtAlgo {
		result, err := testutil.AppendAuthInfo(sch, algo, "../e2e/auth/sample_public_key.pem", false)
		require.NoError(t, err)
		strSchema := string(result)

		authMeta, err := authorization.Parse(strSchema)
		require.NoError(t, err)

		metaInfo := &testutil.AuthMeta{
			PublicKey:       authMeta.VerificationKey,
			Namespace:       authMeta.Namespace,
			Algo:            authMeta.Algo,
			ClosedByDefault: authMeta.ClosedByDefault,
		}

		b := read(t, "auth_query_test.yaml")
		t.Run("Query Rewriting "+algo, func(t *testing.T) {
			queryRewriting(t, strSchema, metaInfo, b)
		})

		t.Run("Mutation Query Rewriting "+algo, func(t *testing.T) {
			mutationQueryRewriting(t, strSchema, metaInfo)
		})

		b = read(t, "auth_add_test.yaml")
		t.Run("Add Mutation "+algo, func(t *testing.T) {
			mutationAdd(t, strSchema, metaInfo, b)
		})

		b = read(t, "auth_update_test.yaml")
		t.Run("Update Mutation "+algo, func(t *testing.T) {
			mutationUpdate(t, strSchema, metaInfo, b)
		})

		b = read(t, "auth_delete_test.yaml")
		t.Run("Delete Query Rewriting "+algo, func(t *testing.T) {
			deleteQueryRewriting(t, strSchema, metaInfo, b)
		})
	}
}

func TestAuthQueryRewritingWithDefaultClosedByFlag(t *testing.T) {
	sch, err := os.ReadFile("../e2e/auth/schema.graphql")
	require.NoError(t, err, "Unable to read schema file")
	algo := jwt.SigningMethodHS256.Name
	result, err := testutil.AppendAuthInfo(sch, algo, "../e2e/auth/sample_public_key.pem", true)
	require.NoError(t, err)
	strSchema := string(result)

	authMeta, err := authorization.Parse(strSchema)
	require.NoError(t, err)

	metaInfo := &testutil.AuthMeta{
		PublicKey:       authMeta.VerificationKey,
		Namespace:       authMeta.Namespace,
		Algo:            authMeta.Algo,
		ClosedByDefault: authMeta.ClosedByDefault,
	}

	b := read(t, "auth_closed_by_default_query_test.yaml")
	t.Run("Query Rewriting "+algo, func(t *testing.T) {
		queryRewriting(t, strSchema, metaInfo, b)
	})

	b = read(t, "auth_closed_by_default_add_test.yaml")
	t.Run("Add Mutation "+algo, func(t *testing.T) {
		mutationAdd(t, strSchema, metaInfo, b)
	})

	b = read(t, "auth_closed_by_default_update_test.yaml")
	t.Run("Update Mutation "+algo, func(t *testing.T) {
		mutationUpdate(t, strSchema, metaInfo, b)
	})

	b = read(t, "auth_closed_by_default_delete_test.yaml")
	t.Run("Delete Query Rewriting "+algo, func(t *testing.T) {
		deleteQueryRewriting(t, strSchema, metaInfo, b)
	})
}

func read(t *testing.T, file string) []byte {
	b, err := os.ReadFile(file)
	require.NoError(t, err, "Unable to read test file")
	return b
}
