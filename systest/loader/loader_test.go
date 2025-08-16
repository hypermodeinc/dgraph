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

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func TestMain(m *testing.M) {
	if runtime.GOOS != "linux" {
		fmt.Println("Skipping loader tests on non-Linux platforms due to dgraph binary dependency")
		os.Exit(0)
	}
	m.Run()
}

// TestLoaderXidmap checks that live loader re-uses xidmap on loading data from two different files
func TestLoaderXidmap(t *testing.T) {

	// test that the cert exists and is valid
	certPath := "../../tlstest/mtls_internal/tls/live/ca.crt"
	_, err := os.Stat(certPath)
	require.NoError(t, err, "CA certificate file not found")

	conf := viper.GetViper()
	conf.Set("tls", fmt.Sprintf("ca-cert=%s; server-name=%s; internal-port=%v;",
		// ca-cert
		"../../tlstest/mtls_internal/tls/live/ca.crt",
		// server-name
		"alpha1",
		// internal-port
		true))

	dg, err := testutil.DgraphClientWithCerts(testutil.GetSockAddrLocalhost(), conf)
	require.NoError(t, err)
	ctx := context.Background()
	testutil.DropAll(t, dg)
	tmpDir := t.TempDir()

	data, err := filepath.Abs("testdata/first.rdf.gz")
	require.NoError(t, err)

	tlsDir, err := filepath.Abs("../../tlstest/mtls_internal/tls/live")
	require.NoError(t, err)

	tlsFlag := fmt.Sprintf(
		`ca-cert=%s; internal-port=%v; client-cert=%s; client-key=%s; server-name=%s;`,
		// ca-cert
		tlsDir+"/ca.crt",
		// internal-port
		true,
		// client-cert
		tlsDir+"/client.liveclient.crt",
		// client-key
		tlsDir+"/client.liveclient.key",
		// server-name
		"alpha1")

	err = testutil.ExecWithOpts([]string{testutil.DgraphBinaryPath(), "live",
		"--tls", tlsFlag,
		"--files", data,
		"--alpha", testutil.GetSockAddrLocalhost(),
		"--zero", testutil.GetSockAddrZeroLocalhost(),
		"-x", "x"}, testutil.CmdOpts{Dir: tmpDir})
	require.NoError(t, err)

	// Load another file, live should reuse the xidmap.
	data, err = filepath.Abs("testdata/second.rdf.gz")
	require.NoError(t, err)
	err = testutil.ExecWithOpts([]string{testutil.DgraphBinaryPath(), "live",
		"--tls", tlsFlag,
		"--files", data,
		"--alpha", testutil.GetSockAddrLocalhost(),
		"--zero", testutil.GetSockAddrZeroLocalhost(),
		"-x", "x"}, testutil.CmdOpts{Dir: tmpDir})
	require.NoError(t, err)

	op := api.Operation{Schema: "name: string @index(exact) ."}
	x.Check(dg.Alter(ctx, &op))

	query := `
	{
		q(func: eq(name, "Alice")) {
			age
			location
			friend{
				name
			}
		}
	}`
	expected := `{"q":[{"age":"13","location":"Wonderland","friend":[{"name":"Bob"}]}]}`

	resp, err := dg.NewReadOnlyTxn().Query(ctx, query)
	require.NoError(t, err)
	testutil.CompareJSON(t, expected, string(resp.GetJson()))

}
