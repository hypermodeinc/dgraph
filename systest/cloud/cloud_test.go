//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	minio "github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/graphql/e2e/common"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func setup(t *testing.T) {
	dc := testutil.DgClientWithLogin(t, "groot", "password", x.RootNamespace)
	require.NoError(t, dc.Alter(context.Background(), &api.Operation{DropAll: true}))
}

func readFile(t *testing.T, path string) []byte {
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func getHttpToken(t *testing.T, user, password string, ns uint64) *testutil.HttpToken {
	jwt := testutil.GetAccessJwt(t, testutil.JwtParams{
		User:   user,
		Groups: []string{"guardians"},
		Ns:     ns,
		Exp:    time.Hour,
		Secret: readFile(t, "../../acl/hmac-secret"),
	})

	return &testutil.HttpToken{
		UserId:    user,
		Password:  password,
		AccessJwt: jwt,
	}
}

func graphqlHelper(t *testing.T, query string, headers http.Header,
	expectedResult string) {
	params := &common.GraphQLParams{
		Query:   query,
		Headers: headers,
	}
	queryResult := params.ExecuteAsPost(t, common.GraphqlURL)
	common.RequireNoGQLErrors(t, queryResult)
	testutil.CompareJSON(t, expectedResult, string(queryResult.Data))
}

func TestDisallowNonGalaxy(t *testing.T) {
	setup(t)

	galaxyToken := getHttpToken(t, "groot", "password", x.RootNamespace)
	// Create a new namespace
	ns, err := testutil.CreateNamespaceWithRetry(t, galaxyToken)
	require.NoError(t, err)
	require.Greater(t, int(ns), 0)

	nsToken := getHttpToken(t, "groot", "password", ns)
	header := http.Header{}
	header.Set("X-Dgraph-AccessToken", nsToken.AccessJwt)

	// User from namespace ns should be able to query/mutate.
	schema := `
	type Author {
		id: ID!
		name: String
	}`
	common.SafelyUpdateGQLSchema(t, common.Alpha1HTTP, schema, header)

	graphqlHelper(t, `
	mutation {
		addAuthor(input:{name: "Alice"}) {
			author{
				name
			}
		}
	}`, header,
		`{
			"addAuthor": {
				"author":[{
					"name":"Alice"
				}]
			}
		}`)

	query := `
	query {
		queryAuthor {
			name
		}
	}`
	graphqlHelper(t, query, header,
		`{
			"queryAuthor": [
				{
					"name":"Alice"
				}
			]
		}`)

	// Login to namespace 1 via groot and create new user alice. Non-galaxy namespace user should
	// not be able to do so in cloud mode.
	_, err = testutil.HttpLogin(&testutil.LoginParams{
		Endpoint:  testutil.AdminUrl(),
		UserID:    "groot",
		Passwd:    "password",
		Namespace: ns,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "operation is not allowed in shared cloud mode")

	// Ns superadmin should not be able to create user.
	resp := testutil.CreateUser(t, nsToken, "alice", "newpassword")
	require.Greater(t, len(resp.Errors), 0)
	require.Contains(t, resp.Errors.Error(), "unauthorized to mutate acl predicates")

	// root superadmin should be able to create user.
	resp = testutil.CreateUser(t, galaxyToken, "alice", "newpassword")
	require.Equal(t, 0, len(resp.Errors))
}

func TestEnvironmentAccess(t *testing.T) {
	setup(t)

	galaxyToken := getHttpToken(t, "groot", "password", x.RootNamespace)
	// Create a new namespace
	ns, err := testutil.CreateNamespaceWithRetry(t, galaxyToken)
	require.NoError(t, err)
	require.Greater(t, int(ns), 0)

	nsToken := getHttpToken(t, "groot", "password", ns)
	header := http.Header{}
	header.Set("X-Dgraph-AccessToken", nsToken.AccessJwt)

	// Create a minio bucket.
	bucketname := "dgraph-export"
	mc, err := testutil.NewMinioClient()
	require.NoError(t, err)
	require.NoError(t, mc.MakeBucket(context.Background(), bucketname, minio.MakeBucketOptions{}))
	minioDest := "minio://minio:9001/dgraph-export?secure=false"

	// Export without the minio creds should fail for non-galaxy.
	resp := testutil.Export(t, nsToken, minioDest, "", "")
	require.Greater(t, len(resp.Errors), 0)
	require.Contains(t, resp.Errors.Error(), "task failed")

	// Export with the minio creds should work for non-galaxy.
	resp = testutil.Export(t, nsToken, minioDest, "accesskey", "secretkey")
	require.Zero(t, len(resp.Errors))

	// Galaxy guardian should provide the credentials as well.
	resp = testutil.Export(t, galaxyToken, minioDest, "accesskey", "secretkey")
	require.Zero(t, len(resp.Errors))
}
