//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package acl

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func TestLoginOverTLS(t *testing.T) {
	conf := viper.New()
	conf.Set("tls", fmt.Sprintf("ca-cert=%s; server-name=%s;",
		// ca-cert
		"../mtls_internal/tls/live/ca.crt",
		// server-name
		"alpha1"))

	dg, err := testutil.DgraphClientWithCerts(testutil.GetSockAddr(), conf)
	require.NoError(t, err)
	for i := 0; i < 30; i++ {
		err = dg.LoginIntoNamespace(context.Background(), "groot", "password", x.RootNamespace)
		if err == nil {
			return
		}
		fmt.Printf("Login failed: %v. Retrying...\n", err)
		time.Sleep(time.Second)
	}

	t.Fatalf("Unable to login to %s\n", err)
}
