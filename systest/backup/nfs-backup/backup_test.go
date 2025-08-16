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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/systest/backup/common"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/worker"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var (
	copyBackupDir  = "./data/copied_backups"
	testDir        = "./data"
	backupDstHA    = "/ha_backup"
	backupDstNonHA = "/non_ha_backup"
)

func TestBackupHAClust(t *testing.T) {

	backupRestoreTest(t, testutil.GetSockAddr(), testutil.GetSockAddrAlpha4Http(),
		testutil.GetSockAddrZeroHttp(), backupDstHA, testutil.GetSockAddrHttp())
}

func TestBackupNonHAClust(t *testing.T) {

	backupRestoreTest(t, testutil.GetSockAddrAlpha7(), testutil.GetSockAddrAlpha8Http(),
		testutil.GetSockAddrZero7Http(), backupDstNonHA, testutil.GetSockAddrAlpha7Http())
}

func backupRestoreTest(t *testing.T, backupAlphaSocketAddr string, restoreAlphaAddr string,
	backupZeroAddr string, backupDst string, backupAlphaSocketAddrHttp string) {

	conn, err := grpc.NewClient(backupAlphaSocketAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	dg := dgo.NewDgraphClient(api.NewDgraphClient(conn))
	ctx := context.Background()
	require.NoError(t, dg.Alter(ctx, &api.Operation{DropAll: true}))
	// Add schema and types.
	require.NoError(t, dg.Alter(ctx, &api.Operation{Schema: `movie: string .
		 type Node {
			 movie
		 }`}))

	// Add initial data.
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
	totalObjectCount := 5
	t.Logf("--- Original uid mapping: %+v\n", original.Uids)
	// Move tablet to group 1 to avoid messes later.
	_, err = http.Get("http://" + backupZeroAddr + "/moveTablet?tablet=movie&group=1")
	require.NoError(t, err)
	// After the move, we need to pause a bit to give zero a chance to quorum.
	t.Log("Pausing to let zero move tablet...")
	moveOk := false
	for !moveOk {
		state, err := testutil.GetState()
		require.NoError(t, err)
		if _, ok := state.Groups["1"].Tablets[x.NamespaceAttr(x.RootNamespace, "movie")]; ok {
			moveOk = true
			break
		}
	}
	require.True(t, moveOk)
	// Setup test directories.
	dirSetup(t)
	// //mostly because of a race condition adding sleep
	// time.Sleep(time.Second * 10)
	_ = runBackup(t, 1, 1, backupAlphaSocketAddrHttp, backupDst)
	restoreStatusCode := restore(t, "", restoreAlphaAddr, backupDst, "non-incremantal", 1)
	require.Equal(t, "Success", restoreStatusCode)
	testutil.WaitForRestore(t, dg, restoreAlphaAddr)
	// We check expected Objects vs Received Objects from restoreed db
	checkObjectCount(t, totalObjectCount, restoreAlphaAddr)
	// Add more data for the incremental backup.
	incr1, err := dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			 <_:x6> <movie> "Harry Potter Part 1" .
			 <_:x7> <movie> "Harry Potter Part 2" .
		 `),
	})
	t.Logf("%+v", incr1)
	require.NoError(t, err)
	// Perform first incremental backup.
	totalObjectCount = totalObjectCount + 2
	_ = runBackup(t, 2, 2, backupAlphaSocketAddrHttp, backupDst)
	// Add more data for a second incremental backup.
	_, err = dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			 <_:x8> <movie> "The Shape of Water" .
			 <_:x9> <movie> "The Black Panther" .
		 `),
	})
	t.Logf("%+v", incr1)
	require.NoError(t, err)
	_ = runBackup(t, 3, 3, backupAlphaSocketAddrHttp, backupDst)
	restoreStatusCode = restore(t, "", restoreAlphaAddr, backupDst, "incremental", 2)
	require.Equal(t, "Success", restoreStatusCode)
	testutil.WaitForRestore(t, dg, restoreAlphaAddr)
	checkObjectCount(t, totalObjectCount, restoreAlphaAddr)
	restoreStatusCode = restore(t, "", restoreAlphaAddr, backupDst, "incremental", 3)
	require.Equal(t, "Success", restoreStatusCode)
	testutil.WaitForRestore(t, dg, restoreAlphaAddr)
	totalObjectCount = totalObjectCount + 2
	checkObjectCount(t, totalObjectCount, restoreAlphaAddr)
	// Add more data for a second full backup.
	_, err = dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			 <_:x10> <movie> "El laberinto del fauno" .
			 <_:x11> <movie> "Black Panther 2" .
		 `),
	})
	t.Logf("%+v", incr1)
	require.NoError(t, err)
	totalObjectCount = totalObjectCount + 2
	// Perform second full backup.
	_ = runBackupInternal(t, true, 4, 4, backupAlphaSocketAddrHttp, backupDst)
	restoreStatusCode = restore(t, "", restoreAlphaAddr, backupDst, "non-incremental", 4)
	require.Equal(t, "Success", restoreStatusCode)
	testutil.WaitForRestore(t, dg, restoreAlphaAddr)
	checkObjectCount(t, totalObjectCount, restoreAlphaAddr)
	// Do a DROP_DATA
	require.NoError(t, dg.Alter(ctx, &api.Operation{DropOp: api.Operation_DATA}))
	// add some data
	_, err = dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
				 <_:x12> <movie> "El laberinto del fauno" .
				 <_:x13> <movie> "Black Panther 2" .
			 `),
	})
	require.NoError(t, err)
	totalObjectCount = 2
	// perform an incremental backup and then restore
	_ = runBackup(t, 5, 5, backupAlphaSocketAddrHttp, backupDst)
	restoreStatusCode = restore(t, "", restoreAlphaAddr, backupDst, "non-incremental", 5)
	require.Equal(t, "Success", restoreStatusCode)
	testutil.WaitForRestore(t, dg, restoreAlphaAddr)
	checkObjectCount(t, totalObjectCount, restoreAlphaAddr)
	// Clean up test directories.
	dirCleanup(t)
}

// function to check object count
func checkObjectCount(t *testing.T, expectedCount int, restoreAlphaAddr string) {
	checkCountRequest := `query {
		 movieCount(func: has(movie)) {
		   count(uid)
		 }
	   }`

	//Check object count from newly created restore alpha
	adminUrl := "http://" + restoreAlphaAddr + "/query"
	params := testutil.GraphQLParams{
		Query: checkCountRequest,
	}
	b, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := http.Post(adminUrl, "application/json", bytes.NewBuffer(b))
	require.NoError(t, err)
	defer resp.Body.Close()
	var data interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&data))
	receivedMap := testutil.JsonGet(data, "data", "movieCount").([]interface{})
	movieCount := testutil.JsonGet(receivedMap[0], "count").(float64)
	require.Equal(t, expectedCount, int(movieCount))
}

func runBackup(t *testing.T, numExpectedFiles, numExpectedDirs int,
	backupAlphaSocketAddrHttp string, backupDst string) []string {

	return runBackupInternal(t, false, numExpectedFiles, numExpectedDirs, backupAlphaSocketAddrHttp, backupDst)
}

func runBackupInternal(t *testing.T, forceFull bool, numExpectedFiles,
	numExpectedDirs int, backupAlphaSocketAddrHttp string, backupDst string) []string {
	backupRequest := `mutation backup($dst: String!, $ff: Boolean!) {
		backup(input: {destination: $dst, forceFull: $ff}) {
			response {
				code
			}
			taskId
		}
	}`
	adminUrl := "http://" + backupAlphaSocketAddrHttp + "/admin"
	params := testutil.GraphQLParams{
		Query: backupRequest,
		Variables: map[string]interface{}{
			"dst": "/mnt" + backupDst,
			"ff":  forceFull,
		},
	}
	b, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := http.Post(adminUrl, "application/json", bytes.NewBuffer(b))
	require.NoError(t, err)
	defer resp.Body.Close()
	var data interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&data))
	require.Equal(t, "Success", testutil.JsonGet(data, "data", "backup", "response", "code").(string))
	taskId := testutil.JsonGet(data, "data", "backup", "taskId").(string)
	testutil.WaitForTask(t, taskId, false, backupAlphaSocketAddrHttp)
	// Verify that the right amount of files and directories were created.
	// We are not using local folder to back up.
	common.CopyToLocalFsFromNFS(t, backupDst, copyBackupDir)
	// List all the folders in the NFS mounted directory.
	files := x.WalkPathFunc(copyBackupDir, func(path string, isdir bool) bool {
		return !isdir && strings.HasSuffix(path, ".backup") && strings.HasPrefix(path, "data/copied_backups/dgraph.")
	})
	require.Equal(t, numExpectedFiles, len(files))
	dirs := x.WalkPathFunc(copyBackupDir, func(path string, isdir bool) bool {
		return isdir && strings.HasPrefix(path, "data/copied_backups/dgraph.")
	})
	require.Equal(t, numExpectedDirs, len(dirs))
	b, err = os.ReadFile(filepath.Join(copyBackupDir, "manifest.json"))
	require.NoError(t, err)
	var manifest worker.MasterManifest
	require.NoError(t, json.Unmarshal(b, &manifest))
	require.Equal(t, numExpectedDirs, len(manifest.Manifests))
	return dirs
}

func restore(t *testing.T, lastDir string, restoreAlphaAddr string,
	backupDst string, backupType string, backupNum int) string {

	var restoreRequest string
	if backupType != "incremental" {
		restoreRequest = `mutation restore($loc: String!) {
			restore(input: {location: $loc}) {
					code
					message
				}
		}`

	} else {
		restoreRequest = `mutation restore($loc: String!, $bn: Int) {
			restore(input: {location: $loc, backupNum: $bn}) {
					code
					message
				}
		}`

	}
	// For restore we have to always use newly added restore cluster
	adminUrl := "http://" + restoreAlphaAddr + "/admin"
	params := testutil.GraphQLParams{
		Query: restoreRequest,
		Variables: map[string]interface{}{
			"loc": "/mnt" + backupDst,
			"bn":  backupNum,
		},
	}
	b, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := http.Post(adminUrl, "application/json", bytes.NewBuffer(b))
	require.NoError(t, err)
	defer resp.Body.Close()
	var data interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&data))
	restoreStatusCode := testutil.JsonGet(data, "data", "restore", "code").(string)
	return restoreStatusCode
}

func dirSetup(t *testing.T) {
	// Clean up data from previous runs.
	dirCleanup(t)
	if err := os.Mkdir(testDir, os.ModePerm); err != nil {
		t.Fatalf("Error while creating directory: %s", err.Error())
	}
}

func dirCleanup(t *testing.T) {
	if err := os.RemoveAll("./data"); err != nil {
		t.Fatalf("Error removing direcotory: %s", err.Error())
	}
	nfsContainerName := testutil.DockerPrefix + "_nfs_1"
	cmdStr := "docker exec -d " + nfsContainerName + " rm -rf" + " /data" + backupDstHA + " /data" + backupDstNonHA
	cmd := exec.Command("/bin/sh", "-c", cmdStr)
	_, err := cmd.Output()
	if err != nil {
		// if there was any error, print it here
		fmt.Println("could not run command: ", err)
	}
}
