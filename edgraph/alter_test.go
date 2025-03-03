//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package edgraph

import (
	"context"
	"testing"

	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/dgraphtest"

	"github.com/stretchr/testify/require"
)

func TestDropAllInNs(t *testing.T) {
	dc := dgraphtest.NewComposeCluster()
	client, cleanup, err := dc.Client()
	require.NoError(t, err)
	defer cleanup()

	// Drop all data
	require.NoError(t, client.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.DropAllNs(context.Background()))

	// Create two namespaces
	ctx := context.Background()
	require.NoError(t, client.CreateNamespace(ctx, "ns1"))
	require.NoError(t, client.CreateNamespace(ctx, "ns2"))

	nsMaps, err := client.ListNamespaces(ctx)
	require.NoError(t, err)
	ns1ID := nsMaps["ns1"].Id
	require.NotZero(t, ns1ID)
	ns2ID := nsMaps["ns2"].Id
	require.NotZero(t, ns2ID)

	// namespace 1
	require.NoError(t, client.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, ns1ID))
	require.NoError(t, client.SetupSchema(`name: string @index(exact) .`))
	_, err = client.Mutate(&api.Mutation{
		SetNquads: []byte(`_:a <name> "Alice" .`),
		CommitNow: true,
	})
	require.NoError(t, err)
	resp, err := client.Query(`{ q(func: has(name)) { name } }`)
	require.NoError(t, err)
	require.JSONEq(t, `{"q":[{"name":"Alice"}]}`, string(resp.GetJson()))

	// namespace 2
	require.NoError(t, client.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, ns2ID))
	require.NoError(t, client.SetupSchema(`name: string @index(exact) .`))
	_, err = client.Mutate(&api.Mutation{
		SetNquads: []byte(`_:a <name> "Bob" .`),
		CommitNow: true,
	})
	require.NoError(t, err)
	resp, err = client.Query(`{ q(func: has(name)) { name } }`)
	require.NoError(t, err)
	require.JSONEq(t, `{"q":[{"name":"Bob"}]}`, string(resp.GetJson()))

	// Drop everything in namespace 1
	require.NoError(t, client.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.Dgraph.DropAll(context.Background(), "ns1"))

	require.NoError(t, client.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, ns2ID))
	resp, err = client.Query(`{ q(func: has(name)) { name } }`)
	require.NoError(t, err)
	require.JSONEq(t, `{"q":[{"name":"Bob"}]}`, string(resp.GetJson()))

	require.NoError(t, client.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, ns1ID))
	resp, err = client.Query(`{ q(func: has(name)) { name } }`)
	require.NoError(t, err)
	require.JSONEq(t, `{"q":[]}`, string(resp.GetJson()))
}
