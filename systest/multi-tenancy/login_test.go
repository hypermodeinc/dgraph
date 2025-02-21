//go:build integration
// +build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/x"
)

func addData(t *testing.T, gc *dgraphapi.GrpcClient) {
	require.NoError(t, gc.SetSchema(context.Background(), dgo.RootNamespace, `name: string  .`))

	rdfs := `
	_:a <name> "alice" .
	_:b <name> "bob" .
	_:c <name> "sagar" .
	_:d <name> "ajay" .`
	_, err := gc.Mutate(&api.Mutation{SetNquads: []byte(rdfs), CommitNow: true})
	require.NoError(t, err)
}

func (msuite *MultitenancyTestSuite) TestLoggingIntoTheNSAfterDropDataFromTheNS() {
	t := msuite.T()
	gc, cleanup, err := msuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()

	hc, err := msuite.dc.HTTPClient()
	require.NoError(t, err)

	require.NoError(t, gc.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, gc.DropAll(context.Background()))

	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))
	for i := 1; i < 5; i++ {
		ns, err := hc.AddNamespace()
		require.NoError(t, err)
		require.NoError(t, gc.LoginIntoNamespace(context.Background(), dgraphapi.DefaultUser, dgraphapi.DefaultPassword, ns))

		addData(t, gc)

		// Drop data from the namespace
		require.NoError(t, gc.Alter(context.Background(), &api.Operation{DropOp: api.Operation_DATA}))

		// Login into the namespace
		require.NoError(t, gc.LoginIntoNamespace(context.Background(),
			dgraphapi.DefaultUser, dgraphapi.DefaultPassword, ns))
	}
}

func (msuite *MultitenancyTestSuite) TestLoggingIntoAllNamespacesAfterDropDataOperationFromDefaultNs() {
	t := msuite.T()
	gc, cleanup, err := msuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()

	hc, err := msuite.dc.HTTPClient()
	require.NoError(t, err)

	require.NoError(t, gc.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))

	require.NoError(t, gc.DropAll(context.Background()))
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	nss := []uint64{}
	for i := 1; i <= 2; i++ {
		ns, err := hc.AddNamespace()
		require.NoError(t, err)
		nss = append(nss, ns)
		require.NoError(t, gc.LoginIntoNamespace(context.Background(),
			dgraphapi.DefaultUser, dgraphapi.DefaultPassword, ns))

		addData(t, gc)
	}

	// Drop data from default namespace
	require.NoError(t, gc.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))

	require.NoError(t, gc.DropAll(context.Background()))

	// verify here that login into the namespace should not fail
	require.NoError(t, gc.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))

	for _, ns := range nss {
		require.NoError(t, gc.LoginIntoNamespace(context.Background(),
			dgraphapi.DefaultUser, dgraphapi.DefaultPassword, ns))
		query := `{
			q(func: has(name)) {
			count(uid)
			}
		}`
		resp, err := gc.Query(query)
		require.NoError(t, err)
		require.Contains(t, string(resp.Json), `"count":4`)
	}
}
