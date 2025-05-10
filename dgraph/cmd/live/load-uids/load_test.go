//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var (
	testDataDir string
	dg          *dgo.Dgraph
)

var (
	alphaService    string
	alphaName       string
	alphaExportPath string
	localExportPath = "./export_copy"
)

func checkDifferentUid(t *testing.T, wantMap, gotMap map[string]interface{}) {
	require.NotEqual(t, gotMap["q"].([]interface{})[0].(map[string]interface{})["uid"],
		wantMap["q"].([]interface{})[0].(map[string]interface{})["uid"],
		"new uid was assigned")

	gotMap["q"].([]interface{})[0].(map[string]interface{})["uid"] = -1
	wantMap["q"].([]interface{})[0].(map[string]interface{})["uid"] = -1
	testutil.CompareJSONMaps(t, wantMap, gotMap)
}

func checkUpsertLoadedData(t *testing.T) {
	resp, err := dg.NewTxn().Query(context.Background(), `
		{
			q(func: eq(xid, "m.1234")) {
				xid
				name
				value
			}
		}
	`)
	require.NoError(t, err)

	gotMap := testutil.UnmarshalJSON(t, string(resp.GetJson()))
	wantMap := testutil.UnmarshalJSON(t, `
		{
		    "q": [
			    {
					"xid": "m.1234",
					"name": "name 1234",
					"value": "value 1234"
			    }
			]
		}
	`)

	testutil.CompareJSONMaps(t, wantMap, gotMap)
}

func TestLiveLoadUpsertAtOnce(t *testing.T) {
	testutil.DropAll(t, dg)

	file := testDataDir + "/xid_a.rdf, " + testDataDir + "/xid_b.rdf"

	pipeline := [][]string{
		{testutil.DgraphBinaryPath(), "live",
			"--schema", testDataDir + "/xid.schema", "--files", file, "--alpha",
			alphaService, "--creds", "user=groot;password=password;", "-U", "xid"},
	}
	_, err := testutil.Pipeline(pipeline)
	require.NoError(t, err, "live loading JSON file exited with error")

	checkUpsertLoadedData(t)
}

func TestLiveLoadUpsert(t *testing.T) {
	testutil.DropAll(t, dg)

	pipeline := [][]string{
		{testutil.DgraphBinaryPath(), "live",
			"--schema", testDataDir + "/xid.schema", "--files", testDataDir + "/xid_a.rdf",
			"--alpha", alphaService, "--creds", "user=groot;password=password;", "-U", "xid"},
	}
	_, err := testutil.Pipeline(pipeline)
	require.NoError(t, err, "live loading JSON file exited with error")

	pipeline = [][]string{
		{testutil.DgraphBinaryPath(), "live",
			"--schema", testDataDir + "/xid.schema", "--files", testDataDir + "/xid_b.rdf",
			"--alpha", alphaService, "--creds", "user=groot;password=password;", "-U", "xid"},
	}
	_, err = testutil.Pipeline(pipeline)
	require.NoError(t, err, "live loading JSON file exited with error")

	checkUpsertLoadedData(t)
}

func checkLoadedData(t *testing.T, newUids bool) {
	resp, err := dg.NewTxn().Query(context.Background(), `
		{
			q(func: anyofterms(name, "Homer")) {
				uid
				name
				age
				role
			}
		}
	`)
	require.NoError(t, err)

	gotMap := testutil.UnmarshalJSON(t, string(resp.GetJson()))
	wantMap := testutil.UnmarshalJSON(t, `
		{
		    "q": [
					{
					"uid": "0x2001",
					"name": "Homer",
					"age": 38,
					"role": "father"
			    }
			]
		}
	`)
	if newUids {
		checkDifferentUid(t, wantMap, gotMap)
	} else {
		testutil.CompareJSONMaps(t, wantMap, gotMap)
	}

	resp, err = dg.NewTxn().Query(context.Background(), `
		{
			q(func: anyofterms(name, "Maggie")) {
				uid
				name
				role
				carries
			}
		}
	`)
	require.NoError(t, err)

	gotMap = testutil.UnmarshalJSON(t, string(resp.GetJson()))
	wantMap = testutil.UnmarshalJSON(t, `
		{
		    "q": [
				{
					"uid": "0x3003",
					"name": "Maggie",
					"role": "daughter",
					"carries": "pacifier"
			    }
			]
		}
	`)
	if newUids {
		checkDifferentUid(t, wantMap, gotMap)
	} else {
		testutil.CompareJSONMaps(t, wantMap, gotMap)
	}
}

func TestLiveLoadJsonUidKeep(t *testing.T) {
	testutil.DropAll(t, dg)

	pipeline := [][]string{
		{testutil.DgraphBinaryPath(), "live",
			"--schema", testDataDir + "/family.schema", "--files", testDataDir + "/family.json",
			"--alpha", alphaService, "--creds", "user=groot;password=password;"},
	}
	_, err := testutil.Pipeline(pipeline)
	require.NoError(t, err, "live loading JSON file exited with error")

	checkLoadedData(t, false)
}

func TestLiveLoadJsonUidDiscard(t *testing.T) {
	testutil.DropAll(t, dg)

	pipeline := [][]string{
		{testutil.DgraphBinaryPath(), "live", "--new_uids",
			"--schema", testDataDir + "/family.schema", "--files", testDataDir + "/family.json",
			"--alpha", alphaService, "--creds", "user=groot;password=password;"},
	}
	_, err := testutil.Pipeline(pipeline)
	require.NoError(t, err, "live loading JSON file exited with error")

	checkLoadedData(t, true)
}

func TestLiveLoadRdfUidKeep(t *testing.T) {
	testutil.DropAll(t, dg)

	pipeline := [][]string{
		{testutil.DgraphBinaryPath(), "live",
			"--schema", testDataDir + "/family.schema", "--files", testDataDir + "/family.rdf",
			"--alpha", alphaService, "--creds", "user=groot;password=password;"},
	}
	_, err := testutil.Pipeline(pipeline)
	require.NoError(t, err, "live loading JSON file exited with error")

	checkLoadedData(t, false)
}

func TestLiveLoadRdfUidDiscard(t *testing.T) {
	testutil.DropAll(t, dg)

	pipeline := [][]string{
		{testutil.DgraphBinaryPath(), "live", "--new_uids",
			"--schema", testDataDir + "/family.schema", "--files", testDataDir + "/family.rdf",
			"--alpha", alphaService, "--creds", "user=groot;password=password;"},
	}
	_, err := testutil.Pipeline(pipeline)
	require.NoError(t, err, "live loading JSON file exited with error")

	checkLoadedData(t, true)
}

func TestLiveLoadExportedSchema(t *testing.T) {
	testutil.DropAll(t, dg)

	// initiate export
	params := &testutil.GraphQLParams{
		Query: `
			mutation {
			  export(input: {format: "rdf"}) {
				response {
				  code
				  message
				}
			  }
			}`,
	}
	token := testutil.GrootHttpLogin("http://" + testutil.SockAddrHttp + "/admin")
	resp := testutil.MakeGQLRequestWithAccessJwt(t, params, token.AccessJwt)
	require.Nilf(t, resp.Errors, resp.Errors.Error())

	// wait a bit to be sure export is complete
	time.Sleep(8 * time.Second)

	// copy the export files from docker
	exportId, groupId := copyExportToLocalFs(t)

	// then loading the exported files should work
	pipeline := [][]string{
		{testutil.DgraphBinaryPath(), "live",
			"--schema", localExportPath + "/" + exportId + "/" + groupId + ".schema.gz",
			"--files", localExportPath + "/" + exportId + "/" + groupId + ".rdf.gz",
			"--encryption",
			x.BuildEncFlag(testDataDir + "/../../../../enc/test-fixtures/enc-key"),
			"--alpha", alphaService, "--creds", "user=groot;password=password;"},
	}
	_, err := testutil.Pipeline(pipeline)
	require.NoError(t, err, "live loading exported schema exited with error")

	// cleanup copied export files
	require.NoError(t, os.RemoveAll(localExportPath), "Error removing export copy directory")
}

func copyExportToLocalFs(t *testing.T) (string, string) {
	require.NoError(t, os.RemoveAll(localExportPath), "Error removing directory")
	require.NoError(t, testutil.DockerCp(alphaExportPath, localExportPath),
		"Error copying files from docker container")

	childDirs, err := os.ReadDir(localExportPath)
	require.NoError(t, err, "Couldn't read local export copy directory")
	require.True(t, len(childDirs) > 0, "Local export copy directory is empty!!!")

	exportFiles, err := os.ReadDir(localExportPath + "/" + childDirs[0].Name())
	require.NoError(t, err, "Couldn't read child of local export copy directory")
	require.True(t, len(exportFiles) > 0, "no exported files found!!!")

	groupId := strings.Split(exportFiles[0].Name(), ".")[0]

	return childDirs[0].Name(), groupId
}

func extractErrLine(output string) string {
	m := regexp.MustCompile(`Error while processing(.)*(rdf|json):`)
	errLine := m.FindString(output)
	return errLine
}

func TestLiveLoadFileName(t *testing.T) {
	testutil.DropAll(t, dg)

	pipeline := [][]string{
		{testutil.DgraphBinaryPath(), "live",
			"--files", testDataDir + "/correct1.rdf," + testDataDir + "/errored1.rdf",
			"--alpha", alphaService, "--creds", "user=groot;password=password;"},
	}

	out, err := testutil.Pipeline(pipeline)
	require.Error(t, err, "error expected: live loader exited with no error")
	errLine := extractErrLine(out)
	errLineExp := fmt.Sprintf(`Error while processing data file %s/errored1.rdf:`, testDataDir)
	require.Equal(t, errLineExp, errLine, "incorrect name for errored file")
}

func TestLiveLoadFileNameMultipleErrored(t *testing.T) {
	testutil.DropAll(t, dg)

	pipeline := [][]string{
		{testutil.DgraphBinaryPath(), "live",
			"--files", testDataDir + "/correct1.rdf," + testDataDir + "/errored1.rdf," +
				testDataDir + "/errored2.rdf", "--alpha", alphaService, "--creds", "user=groot;password=password;"},
	}

	out, err := testutil.Pipeline(pipeline)
	require.Error(t, err, "error expected: live loader exited with no error")
	errLine := extractErrLine(out)
	errLineExp1 := fmt.Sprintf(`Error while processing data file %s/errored1.rdf:`, testDataDir)
	errLineExp2 := fmt.Sprintf(`Error while processing data file %s/errored2.rdf:`, testDataDir)
	assert.Contains(t, []string{errLineExp1, errLineExp2}, errLine, "incorrect name for errored file")
}

func TestLiveLoadFileNameMultipleCorrect(t *testing.T) {
	testutil.DropAll(t, dg)

	pipeline := [][]string{
		{testutil.DgraphBinaryPath(), "live",
			"--files", testDataDir + "/correct1.rdf," + testDataDir + "/correct2.rdf," +
				testDataDir + "/errored1.rdf", "--alpha", alphaService, "--creds", "user=groot;password=password;"},
	}

	out, err := testutil.Pipeline(pipeline)
	require.Error(t, err, "error expected: live loader exited with no error")
	errLine := extractErrLine(out)
	errLineExp := fmt.Sprintf(`Error while processing data file %s/errored1.rdf:`, testDataDir)
	require.Equal(t, errLineExp, errLine, "incorrect name for errored file")
}

func TestMain(m *testing.M) {
	alphaName = testutil.Instance
	alphaService = testutil.SockAddr

	x.AssertTrue(strings.Count(alphaName, "_") == 2)
	left := strings.Index(alphaName, "_")
	right := strings.LastIndex(alphaName, "_")
	alphaExportPath = alphaName + ":/data/" + alphaName[left+1:right] + "/export"
	fmt.Printf("alphaExportPath: %s\n", alphaExportPath)

	_, thisFile, _, _ := runtime.Caller(0)
	testDataDir = filepath.Dir(thisFile)
	fmt.Printf("Using test data dir: %s\n", testDataDir)

	var err error
	dg, err = testutil.DgraphClientWithGroot(testutil.SockAddr)
	if err != nil {
		log.Fatalf("Error while getting a dgraph client: %v", err)
	}
	x.Panic(dg.Alter(
		context.Background(), &api.Operation{DropAll: true}))

	// Try to create any files in a dedicated temp directory that gets cleaned up
	// instead of all over /tmp or the working directory.
	tmpDir, err := os.MkdirTemp("", "test.tmp-")
	x.Panic(err)
	x.Panic(os.Chdir(tmpDir))
	defer os.RemoveAll(tmpDir)

	m.Run()
}
