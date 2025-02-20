//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package live

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v240"
	"github.com/hypermodeinc/dgraph/v24/testutil"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var alphaService = testutil.SockAddr
var zeroService = testutil.SockAddrZero

var (
	testDataDir string
	dg          *dgo.Dgraph
)

// Just check the first and last entries and assumes everything in between is okay.
func checkLoadedData(t *testing.T) {
	resp, err := dg.NewTxn().Query(context.Background(), `
		{
			q(func: anyofterms(name, "Homer")) {
				name
				age
				role @facets(gender,generation)
				role@es
			}
		}
	`)
	require.NoError(t, err)
	testutil.CompareJSON(t, `
		{
			"q": [
					{
					"name": "Homer",
					"age": 38,
					"role": "father",
					"role@es": "padre",
					"role|gender": "male"
				}
			]
		}
	`, string(resp.GetJson()))

	resp, err = dg.NewTxn().Query(context.Background(), `
		{
			q(func: anyofterms(name, "Maggie")) {
				name
				role @facets(gender,generation)
				role@es
				carries
			}
		}
	`)
	require.NoError(t, err)
	testutil.CompareJSON(t, `
		{
			"q": [
				{
					"name": "Maggie",
					"role": "daughter",
					"role@es": "hija",
					"carries": "pacifier",
					"role|gender": "female",
					"role|generation": 3
				}
			]
		}
	`, string(resp.GetJson()))
}

func TestLiveLoadJSONFileEmpty(t *testing.T) {
	require.NoError(t, dg.DropAll(context.Background()))

	pipeline := [][]string{
		{"echo", "[]"},
		{testutil.DgraphBinaryPath(), "live",
			"--schema", testDataDir + "/family.schema", "--files", "/dev/stdin",
			"--alpha", alphaService, "--zero", zeroService,
			"--creds", "user=groot;password=password;"},
	}
	_, err := testutil.Pipeline(pipeline)
	require.NoError(t, err, "live loading JSON file ran successfully")
}

func TestLiveLoadJSONFile(t *testing.T) {
	require.NoError(t, dg.DropAll(context.Background()))

	pipeline := [][]string{
		{testutil.DgraphBinaryPath(), "live",
			"--schema", testDataDir + "/family.schema", "--files", testDataDir + "/family.json",
			"--alpha", alphaService, "--zero", zeroService,
			"--creds", "user=groot;password=password;"},
	}
	_, err := testutil.Pipeline(pipeline)
	require.NoError(t, err, "live loading JSON file exited with error")

	checkLoadedData(t)
}

func TestLiveLoadCanUseAlphaForAssigningUids(t *testing.T) {
	require.NoError(t, dg.DropAll(context.Background()))

	pipeline := [][]string{
		{testutil.DgraphBinaryPath(), "live",
			"--schema", testDataDir + "/family.schema", "--files", testDataDir + "/family.json",
			"--alpha", alphaService, "--zero", alphaService,
			"--creds", "user=groot;password=password;"},
	}
	_, err := testutil.Pipeline(pipeline)
	require.NoError(t, err, "live loading JSON file exited with error")

	checkLoadedData(t)
}

func TestLiveLoadJSONCompressedStream(t *testing.T) {
	require.NoError(t, dg.DropAll(context.Background()))

	pipeline := [][]string{
		{"gzip", "-c", testDataDir + "/family.json"},
		{testutil.DgraphBinaryPath(), "live",
			"--schema", testDataDir + "/family.schema", "--files", "/dev/stdin",
			"--alpha", alphaService, "--zero", zeroService,
			"--creds", "user=groot;password=password;"},
	}
	_, err := testutil.Pipeline(pipeline)
	require.NoError(t, err, "live loading JSON stream exited with error")

	checkLoadedData(t)
}

func TestLiveLoadJSONMultipleFiles(t *testing.T) {
	require.NoError(t, dg.DropAll(context.Background()))

	files := []string{
		testDataDir + "/family1.json",
		testDataDir + "/family2.json",
		testDataDir + "/family3.json",
	}
	fileList := strings.Join(files, ",")

	pipeline := [][]string{
		{testutil.DgraphBinaryPath(), "live",
			"--schema", testDataDir + "/family.schema", "--files", fileList,
			"--alpha", alphaService, "--zero", zeroService,
			"--creds", "user=groot;password=password;"},
	}
	_, err := testutil.Pipeline(pipeline)
	require.NoError(t, err, "live loading multiple JSON files exited with error")

	checkLoadedData(t)
}

func TestMain(m *testing.M) {
	_, thisFile, _, _ := runtime.Caller(0)
	testDataDir = filepath.Dir(thisFile)

	var err error
	dg, err = testutil.DgraphClientWithGroot(testutil.SockAddr)
	x.Panic(err)

	// Try to create any files in a dedicated temp directory that gets cleaned up
	// instead of all over /tmp or the working directory.
	tmpDir, err := os.MkdirTemp("", "test.tmp-")
	x.Panic(err)
	x.Panic(os.Chdir(tmpDir))
	defer os.RemoveAll(tmpDir)

	m.Run()
}
