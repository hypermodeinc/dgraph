//go:build integration

/*
 * Copyright 2017-2023 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package edgraph

import (
	"context"
	"testing"

	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/dgraphtest"
)

func TestNamespaces(t *testing.T) {
	dc := dgraphtest.NewComposeCluster()
	client, cleanup, err := dc.Client()
	require.NoError(t, err)
	defer cleanup()

	// Drop all data
	require.NoError(t, client.Login(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.DropAll())

	// Create two namespaces
	require.NoError(t, client.CreateNamespace("ns1", dgraphapi.DefaultPassword))
	require.NoError(t, client.CreateNamespace("ns2", dgraphapi.DefaultPassword))

	// namespace 1
	require.NoError(t, client.LoginUsingNamespaceName(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, "ns1"))
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
	require.NoError(t, client.LoginUsingNamespaceName(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, "ns2"))
	require.NoError(t, client.SetupSchema(`name: string @index(exact) .`))
	_, err = client.Mutate(&api.Mutation{
		SetNquads: []byte(`_:a <name> "Bob" .`),
		CommitNow: true,
	})
	require.NoError(t, err)
	resp, err = client.Query(`{ q(func: has(name)) { name } }`)
	require.NoError(t, err)
	require.JSONEq(t, `{"q":[{"name":"Bob"}]}`, string(resp.GetJson()))

	// rename ns2 namespace
	require.NoError(t, client.Login(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.RenameNamespace("ns2", "ns2-new"))

	// check if the data is still there
	require.NoError(t, client.LoginUsingNamespaceName(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, "ns2-new"))
	resp, err = client.Query(`{ q(func: has(name)) { name } }`)
	require.NoError(t, err)
	require.JSONEq(t, `{"q":[{"name":"Bob"}]}`, string(resp.GetJson()))

	// List Namespaces
	require.NoError(t, client.Login(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	nsMaps, err := client.ListNamespace()
	require.NoError(t, err)
	require.Len(t, nsMaps, 2)

	// drop ns2-new namespace
	require.NoError(t, client.DropNamespace("ns2-new"))
	require.ErrorContains(t, client.LoginUsingNamespaceName(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, "ns2-new"),
		"invalid username or password")
	nsMaps, err = client.ListNamespace()
	require.NoError(t, err)
	require.Len(t, nsMaps, 1)

	// Get the ID of ns1
	ns1, ok := nsMaps["ns1"]
	require.True(t, ok)

	// drop ns1 namespace
	require.NoError(t, client.Login(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.DropNamespaceWithID(ns1.Id))
	require.ErrorContains(t, client.LoginUsingNamespaceName(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, "ns1"),
		"invalid username or password")
}

func TestCreateNamespaceErr(t *testing.T) {
	dc := dgraphtest.NewComposeCluster()
	client, cleanup, err := dc.Client()
	require.NoError(t, err)
	defer cleanup()

	// Drop all data
	require.NoError(t, client.Login(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.DropAll())

	// create ns1
	require.NoError(t, client.CreateNamespace("ns1", dgraphapi.DefaultPassword))

	// create namespace with wrong name
	require.ErrorContains(t, client.CreateNamespace("--", dgraphapi.DefaultPassword),
		"namespace name [--] cannot start with _ or -")
	require.ErrorContains(t, client.CreateNamespace("dgraph", dgraphapi.DefaultPassword),
		"namespace name [dgraph] cannot start with dgraph")
	require.ErrorContains(t, client.CreateNamespace("", dgraphapi.DefaultPassword),
		"namespace name cannot be empty")
	require.ErrorContains(t, client.CreateNamespace("ns🏆", dgraphapi.DefaultPassword),
		"namespace name [ns🏆] has invalid characters")
	require.ErrorContains(t, client.CreateNamespace("ns1", dgraphapi.DefaultPassword),
		`namespace "ns1" already exists`)

	// create namespace with wrong auth
	require.NoError(t, client.LoginUsingNamespaceName(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, "ns1"))
	require.ErrorContains(t, client.CreateNamespace("ns2", dgraphapi.DefaultPassword),
		"Non guardian of galaxy user cannot create namespace")
}

func TestDropNamespaceErr(t *testing.T) {
	dc := dgraphtest.NewComposeCluster()
	client, cleanup, err := dc.Client()
	require.NoError(t, err)
	defer cleanup()

	// Drop all data
	require.NoError(t, client.Login(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, client.DropAll())

	// create ns1
	require.NoError(t, client.CreateNamespace("ns1", dgraphapi.DefaultPassword))

	// Dropping a non-existent namespace should not be an error
	require.NoError(t, client.DropNamespace("ns2"))
	require.NoError(t, client.DropNamespaceWithID(10000000))

	// drop namespace with wrong name
	require.ErrorContains(t, client.DropNamespaceWithID(0),
		`namespace with id "0" does not exist`)

	// wrong auth
	require.ErrorContains(t, client.DropNamespace("ns1"),
		`namespace "ns1" cannot be dropped by non-admin user`)
}

// A test where we are creating a lots of namespaces constantly and in parallel

// ensure that nonadmin cannot modify the new predicates

// Operations without ACL

// What about upgrades?

// what if a namespace is created through graphql

// DQL Injection
