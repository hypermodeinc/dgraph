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
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/dgraph-io/badger/v4/options"
	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/systest/backup/common"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/worker"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var (
	copyBackupDir  = "./data/backups_copy"
	restoreDir     = "./data/restore"
	alphaBackupDir = "/data/backups"
	oldBackupDir1  = "/data/to_restore/1"
	oldBackupDir2  = "/data/to_restore/2"
	oldBackupDir3  = "/data/to_restore/3"
)

func sendRestoreRequest(t *testing.T, location string) {
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
	resp := testutil.MakeGQLRequestWithTLS(t, &params, testutil.GetAlphaClientConfig(t))
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

// This test takes a backup and then restores an old backup in a cluster incrementally.
// Next, cleans up the cluster and tries restoring the backups above.
// Regression test for DGRAPH-2775
func TestBackupOfOldRestore(t *testing.T) {
	common.DirSetup(t)
	common.CopyOldBackupDir(t)

	conn, err := grpc.NewClient(testutil.GetSockAddr(), grpc.WithTransportCredentials(credentials.NewTLS(testutil.GetAlphaClientConfig(t))))
	require.NoError(t, err)
	dg := dgo.NewDgraphClient(api.NewDgraphClient(conn))
	require.NoError(t, err)

	testutil.DropAll(t, dg)
	time.Sleep(2 * time.Second)

	_ = runBackup(t, 3, 1)

	sendRestoreRequest(t, oldBackupDir1)
	testutil.WaitForRestore(t, dg, testutil.GetSockAddrHttp())

	q := `{ authors(func: has(Author.name)) { count(uid) } }`
	resp, err := dg.NewTxn().Query(context.Background(), q)
	require.NoError(t, err)
	require.JSONEq(t, "{\"authors\":[{\"count\":1}]}", string(resp.Json))

	_ = runBackup(t, 6, 2)

	// Clean the cluster and try restoring the backups created above.
	testutil.DropAll(t, dg)
	time.Sleep(2 * time.Second)
	sendRestoreRequest(t, alphaBackupDir)
	testutil.WaitForRestore(t, dg, testutil.GetSockAddrHttp())

	resp, err = dg.NewTxn().Query(context.Background(), q)
	require.NoError(t, err)
	require.JSONEq(t, "{\"authors\":[{\"count\":1}]}", string(resp.Json))
}

// This test restores the old backups.
// The backup dir contains:
// - Full backup with pred "p1", "p2", "p3". (insert k1, k2, k3).
// - Incremental backup after drop data was called and "p2", "p3", "p4" inserted. --> (insert k4,k5)
// - Incremental backup after "p3" was dropped.
func TestRestoreOfOldBackup(t *testing.T) {
	test := func(dir string) {
		common.DirSetup(t)
		common.CopyOldBackupDir(t)

		conn, err := grpc.NewClient(testutil.GetSockAddr(),
			grpc.WithTransportCredentials(credentials.NewTLS(testutil.GetAlphaClientConfig(t))))
		require.NoError(t, err)
		dg := dgo.NewDgraphClient(api.NewDgraphClient(conn))
		require.NoError(t, err)

		testutil.DropAll(t, dg)
		time.Sleep(2 * time.Second)

		sendRestoreRequest(t, dir)
		testutil.WaitForRestore(t, dg, testutil.GetSockAddrHttp())

		queryAndCheck := func(pred string, cnt int) {
			q := fmt.Sprintf(`{ me(func: has(%s)) { count(uid) } }`, pred)
			r := fmt.Sprintf("{\"me\":[{\"count\":%d}]}", cnt)
			resp, err := dg.NewTxn().Query(context.Background(), q)
			require.NoError(t, err)
			require.JSONEq(t, r, string(resp.Json))
		}

		queryAndCheck("p1", 0)
		queryAndCheck("p2", 2)
		queryAndCheck("p3", 0)
		queryAndCheck("p4", 2)
	}
	t.Run("backup of 20.11", func(t *testing.T) { test(oldBackupDir2) })
	t.Run("backup of 21.03", func(t *testing.T) { test(oldBackupDir3) })
}

func TestBackupFilesystem(t *testing.T) {
	conn, err := grpc.NewClient(testutil.GetSockAddr(),
		grpc.WithTransportCredentials(credentials.NewTLS(testutil.GetAlphaClientConfig(t))))
	require.NoError(t, err)
	dg := dgo.NewDgraphClient(api.NewDgraphClient(conn))

	ctx := context.Background()
	require.NoError(t, dg.Alter(ctx, &api.Operation{DropAll: true}))

	// Add schema and types.
	require.NoError(t, dg.Alter(ctx, &api.Operation{Schema: `movie: string .
	 name: string @index(hash) .
     type Node {
         movie
     }`}))

	var buf bytes.Buffer
	for i := 0; i < 10000; i++ {
		buf.Write([]byte(fmt.Sprintf(`<_:x%d> <name> "ibrahim" .
		`, i)))
	}
	// Add initial data.
	_, err = dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: buf.Bytes(),
	})

	require.NoError(t, err)
	original, err := dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			<_:x1> <movie> "BIRDS MAN OR (THE UNEXPECTED VIRTUE OF IGNORANCE)" .
			<_:x2> <movie> "Spotlight" .
			<_:x3> <movie> "Moonlight" .
			<_:x4> <movie> "THE SHAPE OF WATERLOO" .
			<_:x5> <movie> "BLACK PUNTER" .
		`),
	})
	require.NoError(t, err)
	t.Logf("--- Original uid mapping: %+v\n", original.Uids)

	// Move tablet to group 1 to avoid messes later.
	client := testutil.GetHttpsClient(t)
	_, err = client.Get("https://" + testutil.GetSockAddrZeroHttp() + "/moveTablet?tablet=movie&group=1")
	require.NoError(t, err)

	// After the move, we need to pause a bit to give zero a chance to quorum.
	t.Log("Pausing to let zero move tablet...")
	moveOk := false
	for retry := 5; retry > 0; retry-- {
		state, err := testutil.GetStateHttps(testutil.GetAlphaClientConfig(t))
		require.NoError(t, err)
		if _, ok := state.Groups["1"].Tablets[x.NamespaceAttr(x.RootNamespace, "movie")]; ok {
			moveOk = true
			break
		}
		time.Sleep(1 * time.Second)
	}

	require.True(t, moveOk)
	// Setup test directories.
	common.DirSetup(t)

	// Send backup request.
	_ = runBackup(t, 3, 1)
	restored := runRestore(t, copyBackupDir, "", math.MaxUint64)

	// Check the predicates and types in the schema are as expected.
	// TODO: refactor tests so that minio and filesystem tests share most of their logic.
	preds := []string{"dgraph.graphql.schema", "name", "dgraph.graphql.xid", "dgraph.type",
		"movie", "dgraph.graphql.p_query", "dgraph.drop.op", "dgraph.namespace.name", "dgraph.namespace.id"}
	types := []string{"Node", "dgraph.graphql", "dgraph.namespace", "dgraph.graphql.persisted_query"}
	testutil.CheckSchema(t, preds, types)

	verifyUids := func(count int) {
		query := `
		{
			me(func: eq(name, "ibrahim")) {
				count(uid)
			}
		}`
		res, err := dg.NewTxn().Query(context.Background(), query)
		require.NoError(t, err)
		require.JSONEq(t, string(res.GetJson()), fmt.Sprintf(`{"me":[{"count":%d}]}`, count))
	}
	verifyUids(10000)

	checks := []struct {
		blank, expected string
	}{
		{blank: "x1", expected: "BIRDS MAN OR (THE UNEXPECTED VIRTUE OF IGNORANCE)"},
		{blank: "x2", expected: "Spotlight"},
		{blank: "x3", expected: "Moonlight"},
		{blank: "x4", expected: "THE SHAPE OF WATERLOO"},
		{blank: "x5", expected: "BLACK PUNTER"},
	}
	for _, check := range checks {
		require.EqualValues(t, check.expected, restored[original.Uids[check.blank]])
	}

	// Add more data for the incremental backup.
	incr1, err := dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(fmt.Sprintf(`
			<%s> <movie> "Birdman or (The Unexpected Virtue of Ignorance)" .
			<%s> <movie> "The Shape of Waterloo" .
		`, original.Uids["x1"], original.Uids["x4"])),
	})
	t.Logf("%+v", incr1)
	require.NoError(t, err)

	// Update schema and types to make sure updates to the schema are backed up.
	require.NoError(t, dg.Alter(ctx, &api.Operation{Schema: `
		movie: string .
		actor: string .
		name: string @index(hash) .
		type Node {
			movie
		}
		type NewNode {
			actor
		}`}))

	// Perform first incremental backup.
	_ = runBackup(t, 6, 2)
	restored = runRestore(t, copyBackupDir, "", incr1.Txn.CommitTs)

	// Check the predicates and types in the schema are as expected.
	preds = append(preds, "actor")
	types = append(types, "NewNode")
	testutil.CheckSchema(t, preds, types)

	// Perform some checks on the restored values.
	checks = []struct {
		blank, expected string
	}{
		{blank: "x1", expected: "Birdman or (The Unexpected Virtue of Ignorance)"},
		{blank: "x4", expected: "The Shape of Waterloo"},
	}
	for _, check := range checks {
		require.EqualValues(t, check.expected, restored[original.Uids[check.blank]])
	}

	// Add more data for a second incremental backup.
	incr2, err := dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(fmt.Sprintf(`
				<%s> <movie> "The Shape of Water" .
				<%s> <movie> "The Black Panther" .
			`, original.Uids["x4"], original.Uids["x5"])),
	})
	require.NoError(t, err)

	// Perform second incremental backup.
	_ = runBackup(t, 9, 3)
	restored = runRestore(t, copyBackupDir, "", incr2.Txn.CommitTs)
	testutil.CheckSchema(t, preds, types)

	checks = []struct {
		blank, expected string
	}{
		{blank: "x4", expected: "The Shape of Water"},
		{blank: "x5", expected: "The Black Panther"},
	}
	for _, check := range checks {
		require.EqualValues(t, check.expected, restored[original.Uids[check.blank]])
	}

	// Add more data for a second full backup.
	incr3, err := dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(fmt.Sprintf(`
				<%s> <movie> "El laberinto del fauno" .
				<%s> <movie> "Black Panther 2" .
			`, original.Uids["x4"], original.Uids["x5"])),
	})
	require.NoError(t, err)

	// Perform second full backup.
	_ = runBackupInternal(t, true, 12, 4)
	restored = runRestore(t, copyBackupDir, "", incr3.Txn.CommitTs)
	testutil.CheckSchema(t, preds, types)

	// Check all the values were restored to their most recent value.
	checks = []struct {
		blank, expected string
	}{
		{blank: "x1", expected: "Birdman or (The Unexpected Virtue of Ignorance)"},
		{blank: "x2", expected: "Spotlight"},
		{blank: "x3", expected: "Moonlight"},
		{blank: "x4", expected: "El laberinto del fauno"},
		{blank: "x5", expected: "Black Panther 2"},
	}
	for _, check := range checks {
		require.EqualValues(t, check.expected, restored[original.Uids[check.blank]])
	}

	verifyUids(10000)

	// Do a DROP_DATA
	require.NoError(t, dg.Alter(ctx, &api.Operation{DropOp: api.Operation_DATA}))

	// add some data
	incr4, err := dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
				<_:x1> <movie> "El laberinto del fauno" .
				<_:x2> <movie> "Black Panther 2" .
			`),
	})
	require.NoError(t, err)

	// perform an incremental backup and then restore
	dirs := runBackup(t, 15, 5)
	restored = runRestore(t, copyBackupDir, "", incr4.Txn.CommitTs)
	testutil.CheckSchema(t, preds, types)

	// Check that the newly added data is the only data for the movie predicate
	require.Len(t, restored, 2)
	checks = []struct {
		blank, expected string
	}{
		{blank: "x1", expected: "El laberinto del fauno"},
		{blank: "x2", expected: "Black Panther 2"},
	}
	for _, check := range checks {
		require.EqualValues(t, check.expected, restored[incr4.Uids[check.blank]])
	}

	// Verify that there is no data for predicate `name`
	verifyUids(0)

	// Remove the full backup testDirs and verify restore catches the error.
	require.NoError(t, os.RemoveAll(dirs[0]))
	require.NoError(t, os.RemoveAll(dirs[3]))
	common.RunFailingRestore(t, copyBackupDir, "", incr4.Txn.CommitTs)

	// Clean up test directories.
	common.DirCleanup(t)
}

func runBackup(t *testing.T, numExpectedFiles, numExpectedDirs int) []string {
	return runBackupInternal(t, false, numExpectedFiles, numExpectedDirs)
}

func runBackupInternal(t *testing.T, forceFull bool, numExpectedFiles,
	numExpectedDirs int) []string {
	backupRequest := `mutation backup($dst: String!, $ff: Boolean!) {
			backup(input: {destination: $dst, forceFull: $ff}) {
				response {
					code
				}
				taskId
			}
		}`

	adminUrl := "https://" + testutil.GetSockAddrHttp() + "/admin"
	params := testutil.GraphQLParams{
		Query: backupRequest,
		Variables: map[string]interface{}{
			"dst": alphaBackupDir,
			"ff":  forceFull,
		},
	}
	b, err := json.Marshal(params)
	require.NoError(t, err)

	client := testutil.GetHttpsClient(t)
	resp, err := client.Post(adminUrl, "application/json", bytes.NewBuffer(b))
	require.NoError(t, err)
	defer resp.Body.Close()

	var data interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&data))
	require.Equal(t, "Success",
		testutil.JsonGet(data, "data", "backup", "response", "code").(string))
	taskId := testutil.JsonGet(data, "data", "backup", "taskId").(string)
	testutil.WaitForTask(t, taskId, true, testutil.GetSockAddrHttp())

	// Verify that the right amount of files and directories were created.
	common.CopyToLocalFs(t)

	files := x.WalkPathFunc(copyBackupDir, func(path string, isdir bool) bool {
		return !isdir && strings.HasSuffix(path, ".backup") &&
			strings.HasPrefix(path, "data/backups_copy/dgraph.")
	})
	require.Equal(t, numExpectedFiles, len(files))

	dirs := x.WalkPathFunc(copyBackupDir, func(path string, isdir bool) bool {
		return isdir && strings.HasPrefix(path, "data/backups_copy/dgraph.")
	})
	require.Equal(t, numExpectedDirs, len(dirs))

	b, err = os.ReadFile(filepath.Join(copyBackupDir, "manifest.json"))
	require.NoError(t, err)
	var manifest worker.MasterManifest
	require.NoError(t, json.Unmarshal(b, &manifest))
	require.Equal(t, numExpectedDirs, len(manifest.Manifests))

	return dirs
}

func runRestore(t *testing.T, backupLocation, lastDir string, commitTs uint64) map[string]string {
	// Recreate the restore directory to make sure there's no previous data when
	// calling restore.
	require.NoError(t, os.RemoveAll(restoreDir))

	t.Logf("--- Restoring from: %q", backupLocation)
	result := worker.RunOfflineRestore(restoreDir, backupLocation,
		lastDir, "", nil, options.Snappy, 0)
	require.NoError(t, result.Err)

	for i, pdir := range []string{"p1", "p2", "p3"} {
		pdir = filepath.Join("./data/restore", pdir)
		groupId, err := x.ReadGroupIdFile(pdir)
		require.NoError(t, err)
		require.Equal(t, uint32(i+1), groupId)
	}

	pdir := "./data/restore/p1"
	restored, err := testutil.GetPredicateValues(pdir, x.AttrInRootNamespace("movie"), commitTs)
	require.NoError(t, err)
	t.Logf("--- Restored values: %+v\n", restored)
	return restored
}
