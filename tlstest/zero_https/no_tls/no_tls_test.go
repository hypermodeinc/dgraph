//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package no_tls

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/testutil"
)

type testCase struct {
	url        string
	statusCode int
	response   string
}

var testCasesHttp = []testCase{
	{
		url:        "/health",
		response:   "OK",
		statusCode: 200,
	},
	{
		url:        "/state",
		response:   "\"id\":\"1\",\"groupId\":0,\"addr\":\"zero1:5080\",\"leader\":true,\"amDead\":false",
		statusCode: 200,
	},
}

func TestZeroWithNoTLS(t *testing.T) {
	client := http.Client{
		Timeout: time.Second * 10,
	}
	defer client.CloseIdleConnections()
	for _, test := range testCasesHttp {
		request, err := http.NewRequest("GET", "http://"+testutil.GetSockAddrZeroHttp()+test.url, nil)
		require.NoError(t, err)
		do, err := client.Do(request)
		require.NoError(t, err)
		if do != nil && do.StatusCode != test.statusCode {
			t.Fatalf("status code is not same. Got: %d Expected: %d", do.StatusCode, test.statusCode)
		}

		body := readResponseBody(t, do)
		if !strings.Contains(strings.ReplaceAll(string(body), " ", ""), test.response) {
			t.Fatalf("response is not same. Got: %s Expected: %s", string(body), test.response)
		}
	}
}

func readResponseBody(t *testing.T, do *http.Response) []byte {
	defer func() { _ = do.Body.Close() }()
	body, err := io.ReadAll(do.Body)
	require.NoError(t, err)
	return body
}
