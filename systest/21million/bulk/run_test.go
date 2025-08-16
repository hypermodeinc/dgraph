//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package bulk

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hypermodeinc/dgraph/v25/chunker"
	"github.com/hypermodeinc/dgraph/v25/systest/21million/common"
	"github.com/hypermodeinc/dgraph/v25/testutil"
)

func TestQueries(t *testing.T) {
	t.Run("Run queries", common.TestQueriesFor21Million)
}

func BenchmarkQueries(b *testing.B) {
	_, thisFile, _, _ := runtime.Caller(0)
	queryDir := filepath.Join(filepath.Dir(thisFile), "../queries")

	// For this test we DON'T want to start with an empty database.
	dg, err := testutil.DgraphClient(testutil.ContainerAddr("alpha1", 9080))
	if err != nil {
		panic(fmt.Sprintf("Error while getting a dgraph client: %v", err))
	}

	files, err := os.ReadDir(queryDir)
	if err != nil {
		panic(fmt.Sprintf("Error reading directory: %s", err.Error()))
	}

	for _, file := range files {
		if !strings.HasPrefix(file.Name(), "query-") {
			continue
		}
		b.Run(file.Name(), func(b *testing.B) {
			filename := filepath.Join(queryDir, file.Name())
			reader, cleanup := chunker.FileReader(filename, nil)
			bytes, _ := io.ReadAll(reader)
			contents := string(bytes[:])
			cleanup()

			// The test query and expected result are separated by a delimiter.
			bodies := strings.SplitN(contents, "\n---\n", 2)
			// Dgraph can get into unhealthy state sometime. So, add retry for every query.
			// If a query takes too long to run, it probably means dgraph is stuck and there's
			// no point in waiting longer or trying more tests.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			_, err := dg.NewTxn().Query(ctx, bodies[0])
			if err != nil {
				panic(err)
			}
			cancel()
		})
	}
}

func TestMain(m *testing.M) {
	if runtime.GOOS != "linux" {
		fmt.Println("Skipping 21million bulk tests on non-Linux platforms due to dgraph binary dependency")
		os.Exit(0)
	}
	schemaFile := filepath.Join(testutil.TestDataDirectory, "21million.schema")
	rdfFile := filepath.Join(testutil.TestDataDirectory, "21million.rdf.gz")
	if err := testutil.MakeDirEmpty([]string{"out/0", "out/1", "out/2"}); err != nil {
		os.Exit(1)
	}

	if err := testutil.BulkLoad(testutil.BulkOpts{
		Zero:       testutil.GetSockAddrZero(),
		Shards:     1,
		RdfFile:    rdfFile,
		SchemaFile: schemaFile,
	}); err != nil {
		cleanupAndExit(1)
	}

	if err := testutil.StartAlphas("./alpha.yml"); err != nil {
		cleanupAndExit(1)
	}
	exitCode := m.Run()
	cleanupAndExit(exitCode)
}

func cleanupAndExit(exitCode int) {
	if testutil.StopAlphasAndDetectRace([]string{"alpha1"}) {
		// if there is race fail the test
		exitCode = 1
	}
	_ = os.RemoveAll("out")
	os.Exit(exitCode)
}
