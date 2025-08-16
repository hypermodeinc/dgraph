/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package testaudit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/stretchr/testify/require"
)

func VerifyLogs(t *testing.T, path string, cmds []string) {
	// to make sure that the audit log is flushed
	time.Sleep(time.Second * 5)

	abs, err := filepath.Abs(path)
	require.NoError(t, err)
	f, err := os.Open(abs)
	require.NoError(t, err)

	type log struct {
		Msg string `json:"endpoint"`
	}
	logMap := make(map[string]bool)

	fileScanner := bufio.NewScanner(f)
	for fileScanner.Scan() {
		bytes := fileScanner.Bytes()
		l := new(log)
		require.NoError(t, json.Unmarshal(bytes, l))
		logMap[l.Msg] = true
	}
	for _, m := range cmds {
		if !logMap[m] {
			t.Fatalf("audit logs not present for command %s", m)
		}
	}
}

// run manually to generate the encrypted audit log used in TestDecrypt
func TestGenerateAuditForTestDecrypt(t *testing.T) {
	// to generate audit logs, uncomment and run ./t --test=TestGenerateAuditForTestDecrypt
	t.Skip()
	zeroCmd := map[string][]string{
		"/removeNode": {`--location`, "--request", "GET", "--ipv4",
			fmt.Sprintf("%s/removeNode?id=3&group=1", testutil.GetSockAddrZeroHttp())},
		"/assign": {"--location", "--request", "GET", "--ipv4",
			fmt.Sprintf("%s/assign?what=uids&num=100", testutil.GetSockAddrZeroHttp())},
		"/moveTablet": {"--location", "--request", "GET", "--ipv4",
			fmt.Sprintf("%s/moveTablet?tablet=name&group=2", testutil.GetSockAddrZeroHttp())}}

	for _, c := range zeroCmd {
		cmd := exec.Command("curl", c...)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Println(string(out))
			t.Fatal(err)
		}
	}
}
