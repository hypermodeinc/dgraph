//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package audit_encrypted

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/testutil/testaudit"
)

func TestMain(m *testing.M) {
	if runtime.GOOS != "linux" && os.Getenv("DGRAPH_BINARY") == "" {
		fmt.Println("Skipping audit_encrypted tests on non-Linux platforms due to dgraph binary dependency")
		fmt.Println("You can set the DGRAPH_BINARY environment variable to path of a native dgraph binary to run these tests")
		os.Exit(0)
	}
	m.Run()
}

func TestZeroAuditEncrypted(t *testing.T) {
	state, err := testutil.GetState()
	require.NoError(t, err)
	nId := state.Zeros["1"].Id
	defer os.RemoveAll(fmt.Sprintf("audit_dir/za/zero_audit_0_%s.log", nId))
	defer os.RemoveAll(fmt.Sprintf("audit_dir/za/zero_audit_0_%s.log.enc", nId))
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

	var args []string
	args = append(args, "audit", "decrypt",
		"--encryption_key_file=../../enc/test-fixtures/enc-key",
		"--in", fmt.Sprintf("audit_dir/za/zero_audit_0_%s.log.enc", nId),
		"--out", fmt.Sprintf("audit_dir/za/zero_audit_0_%s.log", nId))

	decrypt := exec.Command(testutil.DgraphBinaryPath(), args...)

	t.Log("Running: ", decrypt.Args)

	out, err := decrypt.CombinedOutput()
	if err != nil {
		fmt.Printf("Error %v\n", err)
		fmt.Printf("Output %v\n", string(out))
		t.Fatal(err)
	}

	testaudit.VerifyLogs(t, fmt.Sprintf("audit_dir/za/zero_audit_0_%s.log", nId), msgs)
}

func TestAlphaAuditEncrypted(t *testing.T) {
	state, err := testutil.GetState()
	require.NoError(t, err)
	var nId string
	for key := range state.Groups["1"].Members {
		nId = key
	}
	defer os.Remove(fmt.Sprintf("audit_dir/aa/alpha_audit_1_%s.log", nId))
	defer os.Remove(fmt.Sprintf("audit_dir/aa/alpha_audit_1_%s.log.enc", nId))
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
		"/query": {"-H", "'Content-Type: application/dql'", "--ipv4", "-X", "POST", fmt.Sprintf("%s/query", testutil.GetSockAddrHttp()),
			"-d", `$'
			{
			 balances(func: anyofterms(name, "Alice Bob")) {
			   uid
			   name
			   balance
			 }
			}'`},
		"/mutate": {"-H", "'Content-Type: application/rdf'", "--ipv4", "-X",
			"POST", fmt.Sprintf("%s/mutate?startTs=4", testutil.GetSockAddrHttp()), "-d", `$'
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

	var args []string
	args = append(args, "audit", "decrypt",
		"--encryption_key_file=../../enc/test-fixtures/enc-key",
		"--in", fmt.Sprintf("audit_dir/aa/alpha_audit_1_%s.log.enc", nId),
		"--out", fmt.Sprintf("audit_dir/aa/alpha_audit_1_%s.log", nId))

	decrypt := exec.Command(testutil.DgraphBinaryPath(), args...)

	t.Log("Running: ", decrypt.Args)

	out, err := decrypt.CombinedOutput()
	if err != nil {
		fmt.Printf("Error %v\n", err)
		fmt.Printf("Output %v\n", string(out))
		t.Fatal(err)
	}

	testaudit.VerifyLogs(t, fmt.Sprintf("audit_dir/aa/alpha_audit_1_%s.log", nId), msgs)
}

// deprecated logs generated with dgraph v22.0.2
func TestZeroAuditDecryptDeprecated(t *testing.T) {

	zeroCmd := map[string]string{
		"/removeNode": "",
		"/assign":     "",
		"/moveTablet": ""}

	// generate expected audit log contents
	msgs := make([]string, 0)
	for i := 0; i < 500; i++ {
		for req := range zeroCmd {
			msgs = append(msgs, req)
		}
	}

	var args []string
	args = append(args, "audit", "decrypt",
		"--encryption_key_file=../../enc/test-fixtures/enc-key",
		"--in", "audit_dir_deprecated/za/zero_audit_0_1.log.enc",
		"--out", "audit_dir_deprecated/za/zero_audit_0_1.log")

	decrypt := exec.Command(testutil.DgraphBinaryPath(), args...)

	t.Log("Running: ", decrypt.Args)

	defer os.Remove("audit_dir_deprecated/za/zero_audit_0_1.log")
	out, err := decrypt.CombinedOutput()
	if err != nil {
		fmt.Printf("Error %v\n", err)
		fmt.Printf("Output %v\n", string(out))
		t.Fatal(err)
	}

	testaudit.VerifyLogs(t, "audit_dir_deprecated/za/zero_audit_0_1.log", msgs)

}

// deprecated logs generated with dgraph v22.0.2
func TestAlphaAuditDecryptDeprecated(t *testing.T) {
	testCommand := map[string]string{
		"/admin":   "",
		"/graphql": "",
		"/query":   "",
		"/mutate":  "",
	}

	msgs := make([]string, 0)
	// generate expected alpha audit log contents
	for i := 0; i < 200; i++ {
		for req := range testCommand {
			msgs = append(msgs, req)
		}
	}

	var args []string
	args = append(args, "audit", "decrypt",
		"--encryption_key_file=../../enc/test-fixtures/enc-key",
		"--in", "audit_dir_deprecated/aa/alpha_audit_1_1.log.enc",
		"--out", "audit_dir_deprecated/aa/alpha_audit_1_1.log")

	decrypt := exec.Command(testutil.DgraphBinaryPath(), args...)

	t.Log("Running: ", decrypt.Args)

	defer os.Remove("audit_dir_deprecated/aa/alpha_audit_1_1.log")
	out, err := decrypt.CombinedOutput()
	if err != nil {
		fmt.Printf("Error %v\n", err)
		fmt.Printf("Output %v\n", string(out))
		t.Fatal(err)
	}

	testaudit.VerifyLogs(t, "audit_dir_deprecated/aa/alpha_audit_1_1.log", msgs)

}
