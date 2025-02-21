//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 */

package certverifyifgiven

import (
	"context"
	"fmt"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v24/testutil"
)

func TestAccessWithoutClientCert(t *testing.T) {
	conf := viper.New()
	conf.Set("tls", fmt.Sprintf("ca-cert=%s; server-name=%s;",
		// ca-cert
		"../tls/ca.crt",
		// server-name
		"node"))

	dg, err := testutil.DgraphClientWithCerts(testutil.SockAddrLocalhost, conf)
	require.NoError(t, err, "Unable to get dgraph client: %v", err)
	require.NoError(t, dg.DropAll(context.Background()))
}

func TestAccessWithClientCert(t *testing.T) {
	conf := viper.New()
	conf.Set("tls", fmt.Sprintf("ca-cert=%s; server-name=%s; client-cert=%s; client-key=%s;",
		// ca-cert
		"../tls/ca.crt",
		// server-name
		"node",
		// client-cert
		"../tls/client.acl.crt",
		// client-key
		"../tls/client.acl.key"))

	dg, err := testutil.DgraphClientWithCerts(testutil.SockAddrLocalhost, conf)
	require.NoError(t, err, "Unable to get dgraph client: %v", err)
	require.NoError(t, dg.DropAll(context.Background()))
}

func TestCurlAccessWithoutClientCert(t *testing.T) {
	curlArgs := []string{
		"--cacert", "../tls/ca.crt", "https://" + testutil.SockAddrHttpLocalhost + "/alter",
		"-d", "name: string @index(exact) .",
	}
	testutil.VerifyCurlCmd(t, curlArgs, &testutil.CurlFailureConfig{
		ShouldFail: false,
	})
}

func TestCurlAccessWithClientCert(t *testing.T) {
	curlArgs := []string{
		"--cacert", "../tls/ca.crt",
		"--cert", "../tls/client.acl.crt",
		"--key", "../tls/client.acl.key",
		"https://" + testutil.SockAddrHttpLocalhost + "/alter",
		"-d", "name: string @index(exact) .",
	}
	testutil.VerifyCurlCmd(t, curlArgs, &testutil.CurlFailureConfig{
		ShouldFail: false,
	})
}
