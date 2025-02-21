//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 */

package certrequest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v24/testutil"
)

func TestAccessOverPlaintext(t *testing.T) {
	dg, err := testutil.DgraphClient(testutil.SockAddr)
	require.NoError(t, err)
	require.Error(t, dg.DropAll(context.Background()))
}

func TestAccessWithCaCert(t *testing.T) {
	conf := viper.New()
	conf.Set("tls", fmt.Sprintf("ca-cert=%s; server-name=%s;",
		// ca-cert
		"../tls/ca.crt",
		// server-name
		"node"))

	dg, err := testutil.DgraphClientWithCerts(testutil.SockAddr, conf)
	require.NoError(t, err, "Unable to get dgraph client: %v", err)
	for i := 0; i < 20; i++ {
		err := dg.DropAll(context.Background())
		if err == nil {
			break
		}
		if strings.Contains(err.Error(), "first record does not look like a TLS handshake") {
			// this is a transient error that happens when the server is still starting up
			time.Sleep(time.Second)
			continue
		}
	}
}

func TestCurlAccessWithCaCert(t *testing.T) {
	// curl over plaintext should fail
	curlPlainTextArgs := []string{
		"--ipv4",
		"https://" + testutil.SockAddrHttpLocalhost + "/alter",
		"-d", "name: string @index(exact) .",
	}
	testutil.VerifyCurlCmd(t, curlPlainTextArgs, &testutil.CurlFailureConfig{
		ShouldFail: true,
		CurlErrMsg: "SSL",
	})

	curlArgs := []string{
		"--cacert", "../tls/ca.crt", "--ipv4",
		"https://" + testutil.SockAddrHttpLocalhost + "/alter",
		"-d", "name: string @index(exact) .",
	}
	testutil.VerifyCurlCmd(t, curlArgs, &testutil.CurlFailureConfig{
		ShouldFail: false,
	})
}
