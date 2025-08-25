//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package certrequireandverify

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/testutil"
)

func TestAccessWithoutClientCert(t *testing.T) {
	conf := viper.New()
	conf.Set("tls", fmt.Sprintf("ca-cert=%s; server-name=%s;",
		// ca-cert
		"../tls/ca.crt",
		// server-name
		"node"))

	dg, err := testutil.DgraphClientWithCerts(testutil.GetSockAddrLocalhost(), conf)
	require.NoError(t, err, "Unable to get dgraph client: %v", err)
	require.Error(t, dg.Alter(context.Background(), &api.Operation{DropAll: true}))
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

	dg, err := testutil.DgraphClientWithCerts(testutil.GetSockAddr(), conf)
	require.NoError(t, err, "Unable to get dgraph client: %v", err)
	require.NoError(t, dg.Alter(context.Background(), &api.Operation{DropAll: true}))
}

func TestCurlAccessWithoutClientCert(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Skipping curl test on non-linux OS")
	}
	curlArgs := []string{
		"--cacert", "../tls/ca.crt", "https://" + testutil.GetSockAddrHttpLocalhost() + "/alter",
		"-d", "name: string @index(exact) .",
	}
	testutil.VerifyCurlCmd(t, curlArgs, &testutil.CurlFailureConfig{
		ShouldFail: true,
		CurlErrMsg: "alert",
	})
}

func TestCurlAccessWithClientCert(t *testing.T) {
	curlArgs := []string{
		"--cacert", "../tls/ca.crt",
		"--cert", "../tls/client.acl.crt",
		"--key", "../tls/client.acl.key",
		"https://" + testutil.GetSockAddrHttpLocalhost() + "/alter",
		"-d", "name: string @index(exact) .",
	}
	testutil.VerifyCurlCmd(t, curlArgs, &testutil.CurlFailureConfig{
		ShouldFail: false,
	})
}

func TestGQLAdminHealthWithClientCert(t *testing.T) {
	// Read the root cert file.
	caCert, err := os.ReadFile("../tls/ca.crt")
	require.NoError(t, err, "Unable to read root cert file : %v", err)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caCert)

	// Load the client cert file.
	clientCert, err := tls.LoadX509KeyPair("../tls/client.acl.crt", "../tls/client.acl.key")
	require.NoError(t, err, "Unable to read client cert file : %v", err)
	tlsConfig := tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{clientCert},
	}
	transport := http.Transport{
		TLSClientConfig: &tlsConfig,
	}
	client := http.Client{
		Timeout:   time.Second * 10,
		Transport: &transport,
	}

	healthCheckQuery := []byte(`{"query":"query {\n health {\n status\n }\n}"}`)
	gqlAdminEndpoint := "https://" + testutil.GetSockAddrHttpLocalhost() + "/admin"
	req, err := http.NewRequest("POST", gqlAdminEndpoint, bytes.NewBuffer(healthCheckQuery))
	require.NoError(t, err, "Failed to create request : %v", err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	require.NoError(t, err, "Https request failed: %v", err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "Error while reading http response: %v", err)
	require.Contains(t, string(body), `"status":"healthy"`)
}

func TestGQLAdminHealthWithoutClientCert(t *testing.T) {
	// Read the root cert file.
	caCert, err := os.ReadFile("../tls/ca.crt")
	require.NoError(t, err, "Unable to read root cert file : %v", err)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caCert)

	tlsConfig := tls.Config{
		RootCAs: pool,
	}
	transport := http.Transport{
		TLSClientConfig: &tlsConfig,
	}
	client := http.Client{
		Timeout:   time.Second * 10,
		Transport: &transport,
	}

	healthCheckQuery := []byte(`{"query":"query {\n health {\n message\n status\n }\n}"}`)
	gqlAdminEndpoint := "https://" + testutil.GetSockAddrHttpLocalhost() + "/admin"
	req, err := http.NewRequest("POST", gqlAdminEndpoint, bytes.NewBuffer(healthCheckQuery))
	require.NoError(t, err, "Failed to create request : %v", err)
	req.Header.Set("Content-Type", "application/json")

	_, err = client.Do(req)
	require.Contains(t, err.Error(), "remote error: tls")
}
