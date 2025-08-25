//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/systest/backup/common"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var (
	copyBackupDir  = "./data/backups_copy"
	alphaBackupDir = "/data/backups"
)

func TestBackupMultiTenancy(t *testing.T) {
	ctx := context.Background()

	dg := testutil.DgClientWithLogin(t, "groot", "password", x.RootNamespace)
	testutil.DropAll(t, dg)

	galaxyCreds := &testutil.LoginParams{UserID: "groot", Passwd: "password", Namespace: x.RootNamespace}
	galaxyToken, err := testutil.Login(t, galaxyCreds)
	require.NoError(t, err, "login failed")

	// Create a new namespace
	ns1, err := testutil.CreateNamespaceWithRetry(t, galaxyToken)
	require.NoError(t, err)
	ns2, err := testutil.CreateNamespaceWithRetry(t, galaxyToken)
	require.NoError(t, err)
	dg1 := testutil.DgClientWithLogin(t, "groot", "password", ns1)
	dg2 := testutil.DgClientWithLogin(t, "groot", "password", ns2)

	addSchema := func(dg *dgo.Dgraph) {
		// Add schema and types.
		require.NoError(t, dg.Alter(ctx, &api.Operation{Schema: `movie: string .
	 name: string @index(hash) .
     type Node {
         movie
     }`}))
	}

	addSchema(dg)
	addSchema(dg1)
	addSchema(dg2)

	addData := func(dg *dgo.Dgraph, name string) *api.Response {
		var buf bytes.Buffer
		// Add initial data.
		_, err = dg.NewTxn().Mutate(ctx, &api.Mutation{
			CommitNow: true,
			SetNquads: buf.Bytes(),
		})

		require.NoError(t, err)
		original, err := dg.NewTxn().Mutate(ctx, &api.Mutation{
			CommitNow: true,
			SetNquads: []byte(`
			<_:x1> <movie> "a" .
			<_:x2> <movie> "b" .
			<_:x3> <movie> "c" .
			<_:x4> <movie> "d" .
			<_:x5> <movie> "e" .
		`),
		})
		require.NoError(t, err)
		t.Logf("--- Original uid mapping: %+v\n", original.Uids)
		return original
	}

	original := make(map[uint64]*api.Response)
	original[x.RootNamespace] = addData(dg, "galaxy")
	original[ns1] = addData(dg1, "ns1")
	original[ns2] = addData(dg2, "ns2")

	// Setup test directories.
	common.DirSetup(t)

	// Send backup request.
	_ = runBackup(t, galaxyToken, 3, 1)
	testutil.DropAll(t, dg)
	sendRestoreRequest(t, alphaBackupDir, galaxyToken.AccessJwt)
	testutil.WaitForRestore(t, dg, testutil.GetSockAddrHttp())

	query := `{ q(func: has(movie)) { count(uid) } }`
	expectedResponse := `{ "q": [{ "count": 5 }]}`
	testutil.VerifyQueryResponse(t, dg, query, expectedResponse)
	testutil.VerifyQueryResponse(t, dg1, query, expectedResponse)
	testutil.VerifyQueryResponse(t, dg2, query, expectedResponse)

	// Call drop data from namespace ns2.
	require.NoError(t, dg2.Alter(ctx, &api.Operation{DropOp: api.Operation_DATA}))
	// Send backup request.
	_ = runBackup(t, galaxyToken, 6, 2)
	testutil.DropAll(t, dg)
	sendRestoreRequest(t, alphaBackupDir, galaxyToken.AccessJwt)
	testutil.WaitForRestore(t, dg, testutil.GetSockAddrHttp())
	testutil.VerifyQueryResponse(t, dg, query, expectedResponse)
	testutil.VerifyQueryResponse(t, dg1, query, expectedResponse)
	testutil.VerifyQueryResponse(t, dg2, query, `{ "q": [{ "count": 0 }]}`)

	// After deleting a namespace in incremental backup, we should not be able to get the data from
	// banned namespace.
	require.NoError(t, testutil.DeleteNamespace(t, galaxyToken, ns1))
	_ = runBackup(t, galaxyToken, 9, 3)
	testutil.DropAll(t, dg)
	sendRestoreRequest(t, alphaBackupDir, galaxyToken.AccessJwt)
	testutil.WaitForRestore(t, dg, testutil.GetSockAddrHttp())
	query = `{ q(func: has(movie)) { count(uid) } }`
	expectedResponse = `{ "q": [{ "count": 5 }]}`
	testutil.VerifyQueryResponse(t, dg, query, expectedResponse)
	expectedResponse = `{ "q": [{ "count": 0 }]}`
	testutil.VerifyQueryResponse(t, dg1, query, expectedResponse)

	common.DirCleanup(t)
}

func runBackup(t *testing.T, token *testutil.HttpToken, numExpectedFiles, numExpectedDirs int) []string {
	return runBackupInternal(t, token, false, numExpectedFiles, numExpectedDirs)
}

func runBackupInternal(t *testing.T, token *testutil.HttpToken, forceFull bool, numExpectedFiles,
	numExpectedDirs int) []string {
	backupRequest := `mutation backup($dst: String!, $ff: Boolean!) {
			backup(input: {destination: $dst, forceFull: $ff}) {
				response {
					code
				}
				taskId
			}
		}`

	params := testutil.GraphQLParams{
		Query: backupRequest,
		Variables: map[string]interface{}{
			"dst": alphaBackupDir,
			"ff":  forceFull,
		},
	}

	resp := testutil.MakeRequest(t, token, params)
	var data interface{}
	require.NoError(t, json.Unmarshal(resp.Data, &data))
	require.Equal(t, "Success", testutil.JsonGet(data, "backup", "response", "code").(string))
	taskId := testutil.JsonGet(data, "backup", "taskId").(string)
	testutil.WaitForTask(t, taskId, false, testutil.GetSockAddrHttp())

	// Verify that the right amount of files and directories were created.
	common.CopyToLocalFs(t)

	files := x.WalkPathFunc(copyBackupDir, func(path string, isdir bool) bool {
		return !isdir && strings.HasSuffix(path, ".backup") && strings.HasPrefix(path, "data/backups_copy/dgraph.")
	})
	require.Equal(t, numExpectedFiles, len(files))

	dirs := x.WalkPathFunc(copyBackupDir, func(path string, isdir bool) bool {
		return isdir && strings.HasPrefix(path, "data/backups_copy/dgraph.")
	})
	require.Equal(t, numExpectedDirs, len(dirs))

	return dirs
}

func sendRestoreRequest(t *testing.T, location string, token string) {
	if location == "" {
		location = "/data/backup"
	}
	params := testutil.GraphQLParams{
		Query: `mutation restore($location: String!) {
			restore(input: {location: $location}) {
				code
				message
			}
		}`,
		Variables: map[string]interface{}{
			"location": location,
		},
	}
	resp := testutil.MakeGQLRequestWithAccessJwt(t, &params, token)
	resp.RequireNoGraphQLErrors(t)

	var restoreResp struct {
		Restore struct {
			Code    string
			Message string
		}
	}

	require.NoError(t, json.Unmarshal(resp.Data, &restoreResp))
	require.Equal(t, restoreResp.Restore.Code, "Success")
}
