//go:build integration2

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hypermodeinc/dgraph/v25/dgraphapi"
	"github.com/hypermodeinc/dgraph/v25/dgraphtest"
	"github.com/hypermodeinc/dgraph/v25/x"

	"github.com/stretchr/testify/require"
)

const (
	gqlSchema = `[
		{
		  "namespace": 0,
		  "schema": "type Message {id: ID!\ncontent: String!\nauthor: String\nuniqueId: Int64! @id\n}"
		},
		{
		  "namespace": 1,
		  "schema": "type Template {id: ID!\ncontent: String!\nuniqueId: Int64! @id\n}"
		}
	  ]`
	jsonData = `
	[
		{"Message.content": "XBNTBGBHGQ", "Message.author": "PXYNHBWGGD",
			"Message.uniqueId": 7, "dgraph.type": "Message", "namespace": 0},
		{"Message.content": "ILEOMLXRYX", "Message.author": "BBBZKURCJH",
			"Message.uniqueId": 5, "dgraph.type": "Message", "namespace": 0},
		{"Message.content": "RFAXPWUCUN", "Message.author": "CMZEOCORNL",
			"Message.uniqueId": 0, "dgraph.type": "Message", "namespace": 0},
		{"Message.content": "ZKCRMYNBLT", "Message.author": "TYLORHNKJA",
			"Message.uniqueId": 9, "dgraph.type": "Message", "namespace": 0},
		{"Message.content": "HMODLPKCHE", "Message.author": "ZNTIZEYBMV",
			"Message.uniqueId": 4, "dgraph.type": "Message", "namespace": 0},
		{"Message.content": "FBIOEOJBZF", "Message.author": "EQXLNWFYBN",
			"Message.uniqueId": 6, "dgraph.type": "Message", "namespace": 0},
		{"Message.content": "DVTCTXCVYI", "Message.author": "USYMVFJYXA",
			"Message.uniqueId": 3, "dgraph.type": "Message", "namespace": 0},
		{"Message.content": "SOWTAXHTCT", "Message.author": "SAILDEMEJV",
			"Message.uniqueId": 8, "dgraph.type": "Message", "namespace": 0},
		{"Message.content": "MLMQWMJQQW", "Message.author": "ANBSOCYLXB",
			"Message.uniqueId": 1, "dgraph.type": "Message", "namespace": 0},
		{"Message.content": "CVFSBBIDCL", "Message.author": "JONAEYCCTQ",
			"Message.uniqueId": 2, "dgraph.type": "Message", "namespace": 0},
		{"Template.content": "t1", "Template.uniqueId": 1, "dgraph.type": "Template", "namespace": 1},
		{"Template.content": "t2", "Template.uniqueId": 2, "dgraph.type": "Template", "namespace": 1},
		{"Template.content": "t3", "Template.uniqueId": 3, "dgraph.type": "Template", "namespace": 1},
		{"dgraph.xid": "groot", "dgraph.password": "password", "dgraph.type": "dgraph.type.User",
			"dgraph.user.group": {"dgraph.xid": "guardians", "dgraph.type": "dgraph.type.Group",
			"namespace": 1}, "namespace": 1}
	]`
)

func TestBulkLoaderNoDqlSchema(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Skipping test on non-Linux platforms due to dgraph binary dependency")
	}
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(2).WithNumZeros(1).
		WithACL(time.Hour).WithReplicas(1).WithBulkLoadOutDir(t.TempDir())
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()

	// start zero
	require.NoError(t, c.StartZero(0))
	require.NoError(t, c.HealthCheck(true))

	baseDir := t.TempDir()
	gqlSchemaFile := filepath.Join(baseDir, "gql.schema")
	require.NoError(t, os.WriteFile(gqlSchemaFile, []byte(gqlSchema), os.ModePerm))
	dataFile := filepath.Join(baseDir, "data.json")
	require.NoError(t, os.WriteFile(dataFile, []byte(jsonData), os.ModePerm))

	opts := dgraphtest.BulkOpts{
		DataFiles:      []string{dataFile},
		GQLSchemaFiles: []string{gqlSchemaFile},
	}
	require.NoError(t, c.BulkLoad(opts))

	// start Alphas
	require.NoError(t, c.Start())

	// run some queries and ensure everything looks good
	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.RootNamespace))

	params := dgraphapi.GraphQLParams{
		Query: `query {
			getMessage(uniqueId: 3) {
				content
				author
			}
		}`,
	}
	data, err := hc.RunGraphqlQuery(params, false)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{
		"getMessage": {
		  "content": "DVTCTXCVYI",
		  "author": "USYMVFJYXA"
		}
	  }`, string(data))

	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, 1))
	params = dgraphapi.GraphQLParams{
		Query: `query {
			getTemplate(uniqueId: 2) {
				content
			}
		}`,
	}
	data, err = hc.RunGraphqlQuery(params, false)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{
		"getTemplate": {
		  "content": "t2"
		}
	  }`, string(data))
}
