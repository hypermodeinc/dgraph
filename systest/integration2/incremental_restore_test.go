//go:build integration2

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/dgraphapi"
	"github.com/hypermodeinc/dgraph/v25/dgraphtest"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func TestIncrementalRestore(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(6).WithNumZeros(3).WithReplicas(3).WithACL(time.Hour)
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

	uids := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	_, _, err = gc.AllocateUIDs(context.Background(), uint64(len(uids)))
	require.NoError(t, err)
	require.NoError(t, gc.SetupSchema(`money: [int] @index(int) @count .`))

	for i := 1; i <= len(uids); i++ {
		for j := 1; j <= i; j++ {
			mu := &api.Mutation{SetNquads: []byte(fmt.Sprintf(`<%v> <money> "%v" .`, j, i)), CommitNow: true}
			_, err := gc.Mutate(mu)
			require.NoError(t, err)
		}
		t.Logf("taking backup #%v\n", i)
		require.NoError(t, hc.Backup(c, i == 1, dgraphtest.DefaultBackupDir))
	}

	for i := 2; i <= len(uids); i += 2 {
		t.Logf("restoring backup #%v\n", i)

		incrFrom := i - 1
		require.NoError(t, hc.Restore(c, dgraphtest.DefaultBackupDir, "", incrFrom, i))
		require.NoError(t, dgraphapi.WaitForRestore(c))

		for j := 1; j <= i; j++ {
			resp, err := gc.Query(fmt.Sprintf(`{q(func: uid(%v)) {money}}`, j))
			require.NoError(t, err)

			var data struct {
				Q []struct {
					Money []int
				}
			}
			require.NoError(t, json.Unmarshal(resp.Json, &data))
			sort.Ints(data.Q[0].Money)
			require.Equal(t, uids[j-1:i], data.Q[0].Money)
		}

		// Even when we do an in between mutations, it makes no difference.
		// Incremental restore overwrites any data written in between.
		if i == 10 {
			_, err := gc.Mutate(&api.Mutation{SetNquads: []byte(`<10> <money> "4" .`), CommitNow: true})
			require.NoError(t, err)
		}
	}
}
