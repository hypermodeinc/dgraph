//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package cdc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/testutil"
)

func TestCDC(t *testing.T) {
	if runtime.GOOS != "linux" && os.Getenv("DGRAPH_BINARY") == "" {
		fmt.Println("You can set the DGRAPH_BINARY environment variable to path of a native dgraph binary to run these tests")
		t.Skip("Skipping test on non-Linux platforms due to dgraph binary dependency")
	}
	defer os.RemoveAll("./cdc_logs/sink.log")
	path := testutil.DgraphBinaryPath()
	cmd := exec.Command(path, "increment", "--num", "10",
		"--alpha", testutil.GetSockAddr())
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Println(string(out))
		t.Fatal(err)
	}
	time.Sleep(time.Second * 15)
	verifyCDC(t, "./cdc_logs/sink.log")
}

type CDCEvent struct {
	Value struct {
		Event struct {
			Value int `json:"value"`
		} `json:"event"`
	} `json:"value"`
}

func verifyCDC(t *testing.T, path string) {
	abs, err := filepath.Abs(path)
	require.NoError(t, err)
	f, err := os.Open(abs)
	require.NoError(t, err)
	fileScanner := bufio.NewScanner(f)
	iter := 1
	for fileScanner.Scan() {
		bytes := fileScanner.Bytes()
		l := new(CDCEvent)
		require.NoError(t, json.Unmarshal(bytes, l))
		require.Equal(t, iter, l.Value.Event.Value)
		iter = iter + 1
	}
	require.Equal(t, iter, 11)
}
