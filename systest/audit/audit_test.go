//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package audit

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/testutil/testaudit"
)

func TestZeroAudit(t *testing.T) {
	state, err := testutil.GetState()
	require.NoError(t, err)
	nId := state.Zeros["1"].Id
	defer os.RemoveAll(fmt.Sprintf("audit_dir/za/zero_audit_0_%s.log", nId))
	zeroCmd := map[string][]string{
		"/removeNode": {`--location`, "--request", "GET", "--ipv4",
			fmt.Sprintf("%s/removeNode?id=3&group=1", testutil.GetSockAddrZeroHttp())},
		"/assign": {"--location", "--request", "GET", "--ipv4",
			fmt.Sprintf("%s/assign?what=uids&num=100", testutil.GetSockAddrZeroHttp())},
		"/moveTablet": {"--location", "--request", "GET", "--ipv4",
			fmt.Sprintf("%s/moveTablet?tablet=name&group=2", testutil.GetSockAddrZeroHttp())}}

	msgs := make([]string, 0)
	// logger is buffered. make calls in bunch so that dont want to wait for flush
	for i := 0; i < 500; i++ {
		for req, c := range zeroCmd {
			msgs = append(msgs, req)
			cmd := exec.Command("curl", c...)
			if out, err := cmd.CombinedOutput(); err != nil {
				fmt.Println(string(out))
				t.Fatal(err)
			}
		}
	}

	testaudit.VerifyLogs(t, fmt.Sprintf("audit_dir/za/zero_audit_0_%s.log", nId), msgs)
}

func TestAlphaAudit(t *testing.T) {
	state, err := testutil.GetState()
	require.NoError(t, err)
	var nId string
	for key := range state.Groups["1"].Members {
		nId = key
	}
	defer os.Remove(fmt.Sprintf("audit_dir/aa/alpha_audit_1_%s.log", nId))
	testCommand := map[string][]string{
		"/admin": {"--location", "--request", "POST", "--ipv4",
			fmt.Sprintf("%s/admin", testutil.GetSockAddrHttp()),
			"--header", "Content-Type: application/json",
			"--data-raw", `'{"query":"mutation {\n  backup(
input: {destination: \"/Users/sankalanparajuli/work/backup\"}) {\n    response {\n      message\n      code\n    }\n  }\n}\n","variables":{}}'`}, //nolint:lll

		"/graphql": {"--location", "--request", "POST", "--ipv4", fmt.Sprintf("%s/graphql", testutil.GetSockAddrHttp()),
			"--header", "Content-Type: application/json",
			"--data-raw", `'{"query":"query {\n  __schema {\n    __typename\n  }\n}","variables":{}}'`},

		"/alter": {"-X", "POST", "--ipv4", fmt.Sprintf("%s/alter", testutil.GetSockAddrHttp()), "-d",
			`name: string @index(term) .
			type Person {
			  name
			}`},
		"/query": {"-H", "'Content-Type: application/dql'", "-X", "POST", "--ipv4", fmt.Sprintf("%s/query", testutil.GetSockAddrHttp()),
			"-d", `$'
			{
			 balances(func: anyofterms(name, "Alice Bob")) {
			   uid
			   name
			   balance
			 }
			}'`},
		"/mutate": {"-H", "'Content-Type: application/rdf'", "-X",
			"POST", "--ipv4", fmt.Sprintf("%s/mutate?startTs=4", testutil.GetSockAddrHttp()), "-d", `$'
			{
			 set {
			   <0x1> <balance> "110" .
			   <0x1> <dgraph.type> "Balance" .
			   <0x2> <balance> "60" .
			   <0x2> <dgraph.type> "Balance" .
			 }
			}
			'`},
	}

	msgs := make([]string, 0)
	// logger is buffered. make calls in bunch so that dont want to wait for flush
	for i := 0; i < 200; i++ {
		for req, c := range testCommand {
			msgs = append(msgs, req)
			cmd := exec.Command("curl", c...)
			if out, err := cmd.CombinedOutput(); err != nil {
				fmt.Println(string(out))
				t.Fatal(err)
			}
		}
	}
	testaudit.VerifyLogs(t, fmt.Sprintf("audit_dir/aa/alpha_audit_1_%s.log", nId), msgs)
}
