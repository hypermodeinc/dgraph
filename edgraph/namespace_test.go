//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package edgraph

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/dgraphtest"
	"github.com/hypermodeinc/dgraph/v24/x"
)

func TestNamespaces(t *testing.T) {
	dc := dgraphtest.NewComposeCluster()
	client, cleanup, err := dc.Client()
	require.NoError(t, err)
	defer cleanup()

	// Drop all data
	require.NoError(t, client.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.DropAll(context.Background()))

	// Create two namespaces
	ctx := context.Background()
	require.NoError(t, client.CreateNamespace(ctx, "ns1"))
	require.NoError(t, client.CreateNamespace(ctx, "ns2"))

	// namespace 1
	// require.NoError(t, client.LoginToNamespace(context.Background(),
	// 	"ns1", dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	// require.NoError(t, client.SetupSchema(`name: string @index(exact) .`))
	// _, err = client.Mutate(&api.Mutation{
	// 	SetNquads: []byte(`_:a <name> "Alice" .`),
	// 	CommitNow: true,
	// })
	// require.NoError(t, err)
	// resp, err := client.Query(`{ q(func: has(name)) { name } }`)
	// require.NoError(t, err)
	// require.JSONEq(t, `{"q":[{"name":"Alice"}]}`, string(resp.GetJson()))

	// namespace 2
	// require.NoError(t, client.LoginToNamespace(context.Background(),
	// 	"ns2", dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	// require.NoError(t, client.SetupSchema(`name: string @index(exact) .`))
	// _, err = client.Mutate(&api.Mutation{
	// 	SetNquads: []byte(`_:a <name> "Bob" .`),
	// 	CommitNow: true,
	// })
	// require.NoError(t, err)
	// resp, err = client.Query(`{ q(func: has(name)) { name } }`)
	// require.NoError(t, err)
	// require.JSONEq(t, `{"q":[{"name":"Bob"}]}`, string(resp.GetJson()))

	// rename ns2 namespace
	require.NoError(t, client.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.RenameNamespace(ctx, "ns2", "ns2-new"))

	// check if the data is still there
	// require.NoError(t, client.LoginToNamespace(context.Background(),
	// 	"ns2-new", dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	// resp, err = client.Query(`{ q(func: has(name)) { name } }`)
	// require.NoError(t, err)
	// require.JSONEq(t, `{"q":[{"name":"Bob"}]}`, string(resp.GetJson()))

	// List Namespaces
	require.NoError(t, client.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	nsMaps, err := client.ListNamespaces(ctx)
	require.NoError(t, err)
	require.Len(t, nsMaps, 3)

	// drop ns2-new namespace
	require.NoError(t, client.DropNamespace(ctx, "ns2-new"))
	// require.ErrorContains(t, client.LoginToNamespace(context.Background(),
	// 	"ns2-new", dgraphapi.DefaultUser, dgraphapi.DefaultPassword),
	// 	"invalid username or password")
	nsMaps, err = client.ListNamespaces(ctx)
	require.NoError(t, err)
	require.Len(t, nsMaps, 2)

	// drop ns1 namespace
	require.NoError(t, client.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.DropNamespace(ctx, "ns1"))
	// require.ErrorContains(t, client.LoginToNamespace(context.Background(),
	// 	"ns1", dgraphapi.DefaultUser, dgraphapi.DefaultPassword),
	// 	"invalid username or password")
}

func TestNamespacesPreV25(t *testing.T) {
	dc := dgraphtest.NewComposeCluster()
	client, cleanup, err := dc.Client()
	require.NoError(t, err)
	defer cleanup()

	hc, err := dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	// Drop all data
	require.NoError(t, client.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.DropAll(context.Background()))

	// Add two pre v25 namespaces
	ns1, err := hc.AddNamespace()
	require.NoError(t, err)
	ns2, err := hc.AddNamespace()
	require.NoError(t, err)

	// Add one v25 namespace
	ctx := context.Background()
	require.NoError(t, client.CreateNamespace(ctx, "ns3"))

	// Ensure that all the namespaces are correctly added
	nsMaps, err := client.ListNamespaces(ctx)
	require.NoError(t, err)
	require.Len(t, nsMaps, 4)
	require.Contains(t, nsMaps, fmt.Sprintf("dgraph-%v", ns1))
	require.Contains(t, nsMaps, fmt.Sprintf("dgraph-%v", ns2))
	require.Contains(t, nsMaps, "dgraph-0")
	require.Contains(t, nsMaps, "ns3")

	// Rename a graphql namespace
	require.NoError(t, client.RenameNamespace(ctx, fmt.Sprintf("dgraph-%v", ns1), "ns1"))

	// Ensure that all the namespaces are correctly added
	nsMaps, err = client.ListNamespaces(ctx)
	require.NoError(t, err)
	require.Len(t, nsMaps, 4)
	require.Contains(t, nsMaps, "ns1")
	require.Contains(t, nsMaps, fmt.Sprintf("dgraph-%v", ns2))
	require.Contains(t, nsMaps, "dgraph-0")
	require.Contains(t, nsMaps, "ns3")

	// Drop a pre v25 namespace
	require.NoError(t, client.DropNamespace(ctx, fmt.Sprintf("dgraph-%v", ns2)))

	// Drop a pre v25 namespace after renaming
	require.NoError(t, client.DropNamespace(ctx, "ns1"))

	// Ensure that all the namespaces are correctly added
	nsMaps, err = client.ListNamespaces(ctx)
	require.NoError(t, err)
	require.Len(t, nsMaps, 2)
	require.Contains(t, nsMaps, "dgraph-0")
	require.Contains(t, nsMaps, "ns3")

	// Add two pre v25 namespaces and ensure login works
	// ns4, err := hc.AddNamespace()
	// require.NoError(t, err)
	// require.NoError(t, client.LoginToNamespace(context.Background(),
	// 	fmt.Sprintf("dgraph-%v", ns4), dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	// require.NoError(t, client.SetupSchema(`name: string @index(exact) .`))
	// _, err = client.Mutate(&api.Mutation{
	// 	SetNquads: []byte(`_:a <name> "Alice" .`),
	// 	CommitNow: true,
	// })
	// require.NoError(t, err)
	// resp, err := client.Query(`{ q(func: has(name)) { name } }`)
	// require.NoError(t, err)
	// require.JSONEq(t, `{"q":[{"name":"Alice"}]}`, string(resp.GetJson()))
}

func TestCreateNamespaceErr(t *testing.T) {
	dc := dgraphtest.NewComposeCluster()
	client, cleanup, err := dc.Client()
	require.NoError(t, err)
	defer cleanup()

	// Drop all data
	require.NoError(t, client.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.DropAll(context.Background()))

	// create ns1
	ctx := context.Background()
	require.NoError(t, client.CreateNamespace(ctx, "ns1"))

	// create namespace with wrong name
	require.ErrorContains(t, client.CreateNamespace(ctx, ""),
		"namespace name cannot be empty")
	require.ErrorContains(t, client.CreateNamespace(ctx, "ns🏆"),
		"namespace name [ns🏆] has invalid characters")
	require.ErrorContains(t, client.CreateNamespace(ctx, "--"),
		"namespace name [--] cannot start with _ or -")
	require.ErrorContains(t, client.CreateNamespace(ctx, "dgraph"),
		"namespace name [dgraph] cannot start with dgraph")
	require.ErrorContains(t, client.CreateNamespace(ctx, "root"),
		"namespace name [root] is reserved")
	require.ErrorContains(t, client.CreateNamespace(ctx, "galaxy"),
		"namespace name [galaxy] is reserved")
	require.ErrorContains(t, client.CreateNamespace(ctx, "123"),
		"namespace name [123] cannot be a number")
	require.ErrorContains(t, client.CreateNamespace(ctx, "00123"),
		"namespace name [00123] cannot be a number")
	require.ErrorContains(t, client.CreateNamespace(ctx, "ns1"),
		`namespace "ns1" already exists`)

	// create namespace with wrong auth
	// require.NoError(t, client.LoginToNamespace(ctx,
	// 	"ns1", dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	// require.ErrorContains(t, client.CreateNamespace(ctx, "ns2"),
	// 	"Non guardian of galaxy user cannot create namespace")
}

func TestDropNamespaceErr(t *testing.T) {
	dc := dgraphtest.NewComposeCluster()
	client, cleanup, err := dc.Client()
	require.NoError(t, err)
	defer cleanup()

	// Drop all data
	require.NoError(t, client.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.DropAll(context.Background()))

	// create ns1
	ctx := context.Background()
	require.NoError(t, client.CreateNamespace(ctx, "ns1"))

	// Dropping a non-existent namespace should not be an error
	require.NoError(t, client.DropNamespace(ctx, "ns2"))
	require.NoError(t, client.DropNamespace(ctx, fmt.Sprintf("dgraph-%v", 10000000)))

	// bad request
	require.ErrorContains(t, client.DropNamespace(ctx, ""),
		`namespace name cannot be empty`)
	require.ErrorContains(t, client.DropNamespace(ctx, "root"),
		`namespace [root] cannot be renamed/dropped`)
	require.ErrorContains(t, client.DropNamespace(ctx, "galaxy"),
		`namespace [galaxy] cannot be renamed/dropped`)
	require.ErrorContains(t, client.DropNamespace(ctx, "dgraph-0"),
		`namespace [dgraph-0] cannot be renamed/dropped`)

	// pre v25 namespace
	require.ErrorContains(t, client.DropNamespace(ctx, "dgraph-"),
		`namespace "dgraph-" is not a legacy namespace`)
	require.ErrorContains(t, client.DropNamespace(ctx, "dgraph-ns1"),
		`invalid namespace name`)

	nsMaps, err := client.ListNamespaces(ctx)
	require.NoError(t, err)
	require.Len(t, nsMaps, 2)
	ns1 := nsMaps["ns1"]

	// this namespace has a real name, should not be dropped using dgraph- prefix name
	require.ErrorContains(t, client.DropNamespace(ctx, fmt.Sprintf("dgraph-%v", ns1.Id)), `not found`)

	// wrong auth
	// require.NoError(t, client.LoginToNamespace(ctx,
	// 	"ns1", dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	// require.ErrorContains(t, client.DropNamespace(ctx, "ns1"),
	// 	`Only guardian of galaxy is allowed to do this operation`)
}

func TestRenameNamespaceErr(t *testing.T) {
	dc := dgraphtest.NewComposeCluster()
	client, cleanup, err := dc.Client()
	require.NoError(t, err)
	defer cleanup()

	// Drop all data
	require.NoError(t, client.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.DropAll(context.Background()))

	// create ns1
	ctx := context.Background()
	require.NoError(t, client.CreateNamespace(ctx, "ns1"))
	nsMaps, err := client.ListNamespaces(ctx)
	require.NoError(t, err)
	require.Len(t, nsMaps, 2)
	ns1 := nsMaps["ns1"].Id

	// bad request
	require.ErrorContains(t, client.RenameNamespace(ctx, "", "ns2"),
		`namespace name cannot be empty`)
	require.ErrorContains(t, client.RenameNamespace(ctx, "root", "ns2"),
		`namespace [root] cannot be renamed/dropped`)
	require.ErrorContains(t, client.RenameNamespace(ctx, "galaxy", "ns2"),
		`namespace [galaxy] cannot be renamed/dropped`)
	require.ErrorContains(t, client.RenameNamespace(ctx, "dgraph-0", "ns2"),
		`namespace [dgraph-0] cannot be renamed/dropped`)
	require.ErrorContains(t, client.RenameNamespace(ctx, "ns1", ""),
		`namespace name cannot be empty`)
	require.ErrorContains(t, client.RenameNamespace(ctx, "ns1", "ns🌍s"),
		`namespace name [ns🌍s] has invalid characters`)
	require.ErrorContains(t, client.RenameNamespace(ctx, "ns1", "dgraph-23"),
		`namespace name [dgraph-23] cannot start with dgraph`)
	require.ErrorContains(t, client.RenameNamespace(ctx, "ns1", "root"),
		`namespace name [root] is reserved`)
	require.ErrorContains(t, client.RenameNamespace(ctx, "ns1", "galaxy"),
		`namespace name [galaxy] is reserved`)
	require.ErrorContains(t, client.RenameNamespace(ctx, "dgraph-", "ns3"),
		`namespace "dgraph-" is not a legacy namespace`)
	require.ErrorContains(t, client.RenameNamespace(ctx, fmt.Sprintf("dgraph-%v", ns1), "ns3"),
		`not found`)
}

func TestListNamespacesErr(t *testing.T) {
	dc := dgraphtest.NewComposeCluster()
	client, cleanup, err := dc.Client()
	require.NoError(t, err)
	defer cleanup()

	// Drop all data
	require.NoError(t, client.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.DropAll(context.Background()))

	// create ns1
	ctx := context.Background()
	require.NoError(t, client.CreateNamespace(ctx, "ns1"))

	// wrong auth
	// require.NoError(t, client.LoginToNamespace(ctx,
	// 	"ns1", dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	// _, err = client.ListNamespaces(ctx)
	// require.ErrorContains(t, err, "Only guardian of galaxy is allowed to do this operation")
}
