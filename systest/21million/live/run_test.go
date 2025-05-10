//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package bulk

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hypermodeinc/dgraph/v25/systest/21million/common"
	"github.com/hypermodeinc/dgraph/v25/testutil"
)

func TestQueries(t *testing.T) {
	t.Run("Run queries", common.TestQueriesFor21Million)
}

func TestMain(m *testing.M) {
	schemaFile := filepath.Join(testutil.TestDataDirectory, "21million.schema")
	rdfFile := filepath.Join(testutil.TestDataDirectory, "21million.rdf.gz")
	if err := testutil.LiveLoad(testutil.LiveOpts{
		Alpha:      testutil.ContainerAddr("alpha1", 9080),
		RdfFile:    rdfFile,
		SchemaFile: schemaFile,
	}); err != nil {
		cleanupAndExit(1)
	}

	exitCode := m.Run()
	cleanupAndExit(exitCode)
}

func cleanupAndExit(exitCode int) {
	_ = os.RemoveAll("./t")
	os.Exit(exitCode)
}
