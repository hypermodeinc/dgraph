//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package multi_group

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func runTests(t *testing.T, client *dgo.Dgraph) {
	type testCase struct {
		query      string
		wantResult string
	}
	suite := func(initialSchema string, setJSON string, cases []testCase) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		require.NoError(t, client.Alter(ctx, &api.Operation{
			DropAll: true,
		}))
		require.NoError(t, client.Alter(ctx, &api.Operation{
			Schema: initialSchema,
		}))

		txn := client.NewTxn()
		_, err := txn.Mutate(ctx, &api.Mutation{SetJson: []byte(setJSON)})
		require.NoError(t, err)
		require.NoError(t, txn.Commit(ctx))

		for _, test := range cases {
			txn := client.NewTxn()
			reply, err := txn.Query(ctx, test.query)
			require.NoError(t, err)
			testutil.CompareJSON(t, test.wantResult, string(reply.GetJson()))
		}
	}

	suite(
		"name: string @index(term) .",
		`[
			{ "name": "Michael" },
			{ "name": "Amit" },
			{ "name": "Luke" },
			{ "name": "Darth" },
			{ "name": "Sarah" },
			{ "name": "Ricky" },
			{ "name": "Hugo" }
		]`,
		[]testCase{
			{`
				{
					q(func: eq(name, "Hugo")) {
						name
					}
				}`, `
				{
				"q": [
				  {
					"name": "Hugo"
				  }
				]
			  }`,
			},
		},
	)
}

func TestClusterSetupWithMultiGroup(t *testing.T) {
	c := &x.TLSHelperConfig{
		CertRequired:     true,
		Cert:             "../tls/alpha1/client.alpha1.crt",
		Key:              "../tls/alpha1/client.alpha1.key",
		ServerName:       "alpha1",
		RootCACert:       "../tls/alpha1/ca.crt",
		UseSystemCACerts: true,
	}
	tlsConf, err := x.GenerateClientTLSConfig(c)
	require.NoError(t, err)
	dgConn, err := grpc.NewClient(testutil.GetSockAddr(), grpc.WithTransportCredentials(credentials.NewTLS(tlsConf)))
	require.NoError(t, err)
	client := dgo.NewDgraphClient(api.NewDgraphClient(dgConn))
	runTests(t, client)
}
