//go:build integration2

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package checkupgrade

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/dgraphapi"
	"github.com/hypermodeinc/dgraph/v25/dgraphtest"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func TestCheckUpgrade(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).
		WithACL(time.Hour).WithVersion("57aa5c4ac")
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace))

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.RootNamespace))

	rdfs := `
		_:a <dgraph.xid> "user1" .
	    _:a <dgraph.type> "dgraph.type.User"  .
		_:b <dgraph.xid> "user1" .
		_:b <dgraph.type> "dgraph.type.User"  .`

	mu := &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	var nss []uint64
	for i := 0; i < 5; i++ {
		ns, err := hc.AddNamespace()
		require.NoError(t, err)
		require.NoError(t, gc.LoginIntoNamespace(context.Background(), "groot", "password", ns))
		mu = &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
		_, err = gc.Mutate(mu)
		require.NoError(t, err)
		nss = append(nss, ns)
	}

	conf1 := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour).WithVersion("local")
	c1, err := dgraphtest.NewLocalCluster(conf1)
	require.NoError(t, err)
	defer func() { c1.Cleanup(t.Failed()) }()
	require.NoError(t, c1.Start())
	alphaHttp, err := c.GetAlphaHttpPublicPort(0)
	require.NoError(t, err)

	args := []string{
		"checkupgrade",
		"--http_port", "localhost:" + alphaHttp,
		"--dgUser", "groot",
		"--password", "password",
		"--namespace", "1",
	}

	cmd := exec.Command(filepath.Join(c1.GetTempDir(), "dgraph"), args...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err)
	actualOutput := string(out)
	fmt.Println("logs of checkupgrade tool\n", actualOutput)
	expectedOutputPattern := `Found duplicate users in namespace: #\d+\ndgraph\.xid user1 , Uids: \[\d+x\d+ \d+x\d+\]\n`
	match, err := regexp.MatchString(expectedOutputPattern, actualOutput)
	require.NoError(t, err)

	if !match {
		t.Errorf("Output does not match expected pattern.\nExpected pattern:\n%s\n\nGot:\n%s",
			expectedOutputPattern, actualOutput)
	}
}

func TestQueryDuplicateNodes(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).
		WithACL(time.Hour).WithVersion("57aa5c4ac").WithAclAlg(jwt.GetSigningMethod("HS256"))
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	// defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())
	gc, cleanup, err := c.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace))
	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.RootNamespace))
	rdfs := `
			<0x40> <dgraph.xid> "user1" .
			<0x40> <dgraph.type> "dgraph.type.User"  .
			<0x50> <dgraph.xid> "user1" .
			<0x50> <dgraph.type> "dgraph.type.User"  .
			<0x60> <dgraph.xid> "user1" .
			<0x60> <dgraph.type> "dgraph.type.User"  .
			<0x60> <dgraph.user.group> <0x1>  .
			<0x50> <dgraph.user.group> <0x1>  .
			<0x70> <dgraph.xid> "user1" .
			<0x70> <dgraph.type> "dgraph.type.User"  .
			<0x80> <dgraph.xid> "user3" .
		    <0x80> <dgraph.type> "dgraph.type.User"  .
		    <0x90> <dgraph.xid> "user3" .
		    <0x90> <dgraph.type> "dgraph.type.User"  .
			<0x100> <dgraph.xid> "Group4" .
			<0x100> <dgraph.type> "dgraph.type.Group"  .
			<0x110> <dgraph.xid> "Group4" .
			<0x110> <dgraph.type> "dgraph.type.Group"  .
			<0x120> <dgraph.xid> "Group4" .
			<0x120> <dgraph.type> "dgraph.type.Group"  .
			<0x130> <dgraph.xid> "Group4" .
			<0x130> <dgraph.type> "dgraph.type.Group"  .
			<0x140> <dgraph.xid> "Group4" .
			<0x140> <dgraph.type> "dgraph.type.Group"  .
			<0x150> <dgraph.xid> "usrgrp1" .
			<0x150> <dgraph.type> "dgraph.type.User"  .
			<0x160> <dgraph.xid> "usrgrp1" .
			<0x160> <dgraph.type> "dgraph.type.User"  .
			<0x170> <dgraph.xid> "usrgrp1" .
			<0x170> <dgraph.type> "dgraph.type.User"  .
			<0x180> <dgraph.xid> "usrgrp1" .
			<0x180> <dgraph.type> "dgraph.type.Group"  .
			<0x200> <dgraph.xid> "usrgrp2" .
			<0x200> <dgraph.type> "dgraph.type.Group"  .
			<0x210> <dgraph.xid> "usrgrp2" .
			<0x210> <dgraph.type> "dgraph.type.User"  .
		`
	mu := &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	duplicateNodes, err := queryDuplicateNodes(hc)
	require.NoError(t, err)

	du := map[string][]string{
		"user1":   {"0x40", "0x50", "0x60", "0x70"},
		"user3":   {"0x80", "0x90"},
		"usrgrp1": {"0x150", "0x160", "0x170"},
	}

	dg := map[string][]string{
		"Group4":  {"0x100", "0x110", "0x120", "0x130", "0x140"},
		"usrgrp1": {"0x180", "0x190"},
	}

	dug := map[string][]string{
		"usrgrp1": {"0x150", "0x160", "0x170", "0x180"},
		"usrgrp2": {"0x200", "0x210"},
	}

	expectedDup := [3]map[string][]string{du, dg, dug}

	for i, dn := range duplicateNodes {
		for j, d := range dn {
			require.Equal(t, len(expectedDup[i][j]), len(d))
			for _, uid := range d {
				require.Contains(t, expectedDup[i][j], uid)
			}
		}
	}
	require.NoError(t, deleteDuplicatesGroup(hc, duplicateNodes[0]))
	require.NoError(t, deleteDuplicatesGroup(hc, duplicateNodes[1]))
	require.NoError(t, deleteDuplicatesGroup(hc, duplicateNodes[2]))
}
