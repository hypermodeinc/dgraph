//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/systest/1million/common"
	"github.com/hypermodeinc/dgraph/v25/testutil"
)

func Test1Million(t *testing.T) {
	dg, err := testutil.DgraphClient(testutil.ContainerAddr("alpha1", 9080))
	if err != nil {
		t.Fatalf("Error while getting a dgraph client: %v", err)
	}

	for _, tt := range common.OneMillionTCs {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		resp, err := dg.NewTxn().Query(ctx, tt.Query)
		cancel()

		if ctx.Err() == context.DeadlineExceeded {
			t.Fatal("aborting test due to query timeout")
		}
		require.NoError(t, err)

		testutil.CompareJSON(t, tt.Resp, string(resp.Json))
	}
}

func TestMain(m *testing.M) {
	if runtime.GOOS != "linux" {
		fmt.Println("Skipping 1million tests on non-Linux platforms due to dgraph binary dependency")
		os.Exit(0)
	}
	noschemaFile := filepath.Join(testutil.TestDataDirectory, "1million-noindex.schema")
	rdfFile := filepath.Join(testutil.TestDataDirectory, "1million.rdf.gz")
	if err := testutil.MakeDirEmpty([]string{"out/0", "out/1", "out/2"}); err != nil {
		os.Exit(1)
	}

	if err := testutil.BulkLoad(testutil.BulkOpts{
		Zero:       testutil.GetSockAddrZero(),
		Shards:     1,
		RdfFile:    rdfFile,
		SchemaFile: noschemaFile,
	}); err != nil {
		cleanupAndExit(1)
	}

	if err := testutil.StartAlphas("./alpha.yml"); err != nil {
		fmt.Printf("Error while bringin up alphas. Error: %v\n", err)
		cleanupAndExit(1)
	}
	schemaFile := filepath.Join(testutil.TestDataDirectory, "1million.schema")
	client, err := testutil.DgraphClient(testutil.ContainerAddr("alpha1", 9080))
	if err != nil {
		fmt.Printf("Error while creating client. Error: %v\n", err)
		cleanupAndExit(1)
	}

	file, err := os.ReadFile(schemaFile)
	if err != nil {
		fmt.Printf("Error while reading schema file. Error: %v\n", err)
		cleanupAndExit(1)
	}

	if err = client.Alter(context.Background(), &api.Operation{
		Schema: string(file),
	}); err != nil {
		fmt.Printf("Error while indexing. Error: %v\n", err)
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
