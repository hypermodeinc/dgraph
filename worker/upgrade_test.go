//go:build upgrade

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/dgraphapi"
	"github.com/hypermodeinc/dgraph/v25/dgraphtest"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var client *dgraphapi.GrpcClient
var (
	schemaIndexed    = `friend: [uid] @count @reverse .`
	schemaNonIndexed = `friend: [uid] .`
)

func setClusterEdge(t *testing.T, rdf string) error {
	mu := &api.Mutation{SetNquads: []byte(rdf), CommitNow: true}
	return testutil.RetryMutation(client.Dgraph, mu)
}

func delClusterEdge(t *testing.T, rdf string) error {
	mu := &api.Mutation{DelNquads: []byte(rdf), CommitNow: true}
	return testutil.RetryMutation(client.Dgraph, mu)
}

func setSchema(schema string) {
	var err error
	for retry := 0; retry < 60; retry++ {
		err = client.Alter(context.Background(), &api.Operation{Schema: schema})
		if err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	panic(fmt.Sprintf("Could not alter schema. Got error %v", err.Error()))
}

func populateCluster(t *testing.T, dc dgraphapi.Cluster) {
	x.Panic(client.Alter(context.Background(), &api.Operation{DropAll: true}))

	isHigher, err := dgraphtest.IsHigherVersion(dc.GetVersion(), "e648e774befe03ca2e602b192b4c888cddba6b89")
	x.Panic(err)
	if isHigher {
		_, _, err := client.AllocateUIDs(context.Background(), 65536)
		x.Panic(err)
	} else {
		x.Panic(dc.AssignUids(client.Dgraph, 65536))
	}

	setSchema(schemaIndexed)
	require.NoError(t, delClusterEdge(t, fmt.Sprintf("<%#x> <friend> <%#x> .", 1, 2)))
}

func TestCountReverseIndex(t *testing.T) {
	uc := &dgraphtest.UpgradeCombo{
		Before:   "v24.0.4",
		After:    "local",
		Strategy: dgraphtest.InPlace,
	}

	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).
		WithReplicas(1).WithACL(time.Hour).WithVersion(uc.Before)
	c, err := dgraphtest.NewLocalCluster(conf)
	x.Panic(err)
	fmt.Println(c)

	var code = 2

	defer func() { c.Cleanup(code != 0) }()
	x.Panic(c.Start())

	dg, cleanup, err := c.Client()
	client = dg
	x.Panic(err)
	defer cleanup()
	x.Panic(dg.LoginIntoNamespace(context.Background(), dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.RootNamespace))

	populateCluster(t, c)

	err = delClusterEdge(t, fmt.Sprintf("<%#x> <friend> <%#x> .", 1, 2))
	require.Contains(t, err.Error(), "Transaction is too old")

	x.Panic(c.Upgrade(uc.After, uc.Strategy))

	dg, cleanup, err = c.Client()
	client = dg
	x.Panic(err)
	defer cleanup()
	x.Panic(dg.LoginIntoNamespace(context.Background(), dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.RootNamespace))

	setSchema(schemaNonIndexed)
	setSchema(schemaIndexed)

	require.NoError(t, delClusterEdge(t, fmt.Sprintf("<%#x> <friend> <%#x> .", 1, 2)))
	require.NoError(t, delClusterEdge(t, fmt.Sprintf("<%#x> <friend> <%#x> .", 1, 2)))
	require.NoError(t, delClusterEdge(t, fmt.Sprintf("<%#x> <friend> <%#x> .", 1, 2)))
	require.NoError(t, delClusterEdge(t, fmt.Sprintf("<%#x> <friend> <%#x> .", 1, 2)))

	code = 0
}
