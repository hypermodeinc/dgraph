/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package version

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/testutil"
)

// Test `dgraph version` with an empty config file.
func TestMain(m *testing.M) {
	if runtime.GOOS != "linux" && os.Getenv("DGRAPH_BINARY") == "" {
		fmt.Println("Skipping version tests on non-Linux platforms due to dgraph binary dependency")
		fmt.Println("You can set the DGRAPH_BINARY environment variable to path of a native dgraph binary to run these tests")
		os.Exit(0)
	}
	m.Run()
}

func TestDgraphVersion(t *testing.T) {
	tmpPath := t.TempDir()
	configPath := filepath.Join(tmpPath, "config.yml")
	configFile, err := os.Create(configPath)
	require.NoError(t, err)
	defer configFile.Close()
	require.NoError(t, testutil.Exec(testutil.DgraphBinaryPath(), "version", "--config", configPath))
}
