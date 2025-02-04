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
	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/dgraphtest"
	"github.com/stretchr/testify/require"
)

func TestCreateNamespace(t *testing.T) {
	dc := dgraphtest.NewComposeCluster()
	client, cleanup, err := dc.Client()
	require.NoError(t, err)
	defer cleanup()

	// Drop all data
	require.NoError(t, client.DropAll())

	// Create two namespaces
	require.NoError(t, client.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, 0))
	t.Logf("Creating namespace ns1")
	require.NoError(t, client.CreateNamespace("ns1", dgraphapi.DefaultPassword))
	t.Logf("Creating namespace ns2")
	require.NoError(t, client.CreateNamespace("ns2", dgraphapi.DefaultPassword))

	// namespace 1
	t.Logf("Logging into namespace ns1")
	require.NoError(t, client.LoginUsingNamespaceName(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, "ns1"))

	t.Logf("Setting up schema in namespace ns1")
	require.NoError(t, client.SetupSchema(`name: string @index(exact) .`))

	t.Logf("Mutating data in namespace ns1")
	_, err = client.Mutate(&api.Mutation{
		SetNquads: []byte(`_:a <name> "Alice" .`),
		CommitNow: true,
	})
	require.NoError(t, err)

	t.Logf("Querying data in namespace ns1")
	resp, err := client.Query(`{ q(func: has(name)) { name } }`)
	require.NoError(t, err)
	require.JSONEq(t, `{"q":[{"name":"Alice"}]}`, string(resp.GetJson()))

	// setup schema in namespace 2
	t.Logf("Logging into namespace ns2")
	require.NoError(t, client.LoginUsingNamespaceName(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, "ns2"))

	t.Logf("Setting up schema in namespace ns2")
	require.NoError(t, client.SetupSchema(`name: string @index(exact) .`))

	t.Logf("Mutating data in namespace ns2")
	require.NoError(t, client.LoginUsingNamespaceName(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, "ns2"))
	_, err = client.Mutate(&api.Mutation{
		SetNquads: []byte(`_:a <name> "Bob" .`),
		CommitNow: true,
	})
	require.NoError(t, err)

	t.Logf("Querying data in namespace ns2")
	require.NoError(t, client.LoginUsingNamespaceName(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, "ns2"))
	resp, err = client.Query(`{ q(func: has(name)) { name } }`)
	require.NoError(t, err)
	require.JSONEq(t, `{"q":[{"name":"Bob"}]}`, string(resp.GetJson()))
}

// A test where we are creating a lots of namespaces constantly and in parallel

// What if I create the same namespace again?

// wrong auth

func TestCreateSameNamespace(t *testing.T) {
	dc := dgraphtest.NewComposeCluster()
	client, cleanup, err := dc.Client()
	require.NoError(t, err)
	defer cleanup()

	// Drop all data
	require.NoError(t, client.DropAll())

	// Create two namespaces
	require.NoError(t, client.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, 0))
	t.Logf("Creating namespace ns4")
	require.NoError(t, client.CreateNamespace("ns4", dgraphapi.DefaultPassword))
	t.Logf("Creating namespace ns4 again")
	require.ErrorContains(t, client.CreateNamespace("ns4", dgraphapi.DefaultPassword), `namespace "ns4" already exists`)
}
