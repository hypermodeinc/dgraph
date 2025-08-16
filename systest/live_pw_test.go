//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/testutil"
)

func TestLivePassword(t *testing.T) {
	wrap := func(fn func(*testing.T, *dgo.Dgraph)) func(*testing.T) {
		return func(t *testing.T) {
			dg, err := testutil.DgraphClientWithGroot(testutil.GetSockAddr())
			if err != nil {
				t.Fatalf("Error while getting a dgraph client: %v", err)
			}
			fn(t, dg)
			require.NoError(t, dg.Alter(
				context.Background(), &api.Operation{DropAll: true}))
		}
	}

	t.Run("export", wrap(PasswordExport))
	t.Run("import", wrap(PasswordImport))
}

func PasswordExport(t *testing.T, c *dgo.Dgraph) {
	ctx := context.Background()
	require.NoError(t, c.Alter(ctx, &api.Operation{
		Schema: `secret: password .`,
	}))

	tests := []struct {
		in       string
		mustFail bool
	}{
		{in: `_:uid1 <secret> "123456"^^<xs:int> .`, mustFail: true},
		{in: `_:uid1 <secret> "true"^^<xs:boolean> .`, mustFail: true},
		{in: `_:uid1 <secret> "4.0123"^^<xs:float> .`, mustFail: true},
		{in: `_:uid1 <secret> "2018-12-03"^^<xs:date> .`, mustFail: true},
		{in: `_:uid1 <secret> "string1"^^<xs:string> .`, mustFail: false},
		{in: `_:uid1 <secret> "password1"^^<xs:password> .`, mustFail: false},
	}

	for _, tc := range tests {
		_, err := c.NewTxn().Mutate(ctx, &api.Mutation{
			CommitNow: true,
			SetNquads: []byte(tc.in),
		})
		if tc.mustFail {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	}

	assigned, err := c.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:uid1 <secret> "password1" .
			_:uid2 <secret> "password2" .
			_:uid3 <secret> "password3" .
		`),
	})
	require.NoError(t, err)
	require.Len(t, assigned.Uids, 3)

	txn := c.NewTxn()
	for _, uid := range assigned.Uids {
		resp, err := txn.Query(ctx, `
		{
			q(func: uid(`+uid+`)) {
				secret: checkpwd(secret, "password2")
			}
		}`)
		require.NoError(t, err)
		require.JSONEq(t, fmt.Sprintf(`{"q":[{"secret":%t}]}`, uid == assigned.Uids["uid2"]),
			string(resp.Json))
	}
}

func PasswordImport(t *testing.T, c *dgo.Dgraph) {
	ctx := context.Background()

	// NOTE: we assume a specific bcrypt version '2a' and cost. Future versions of bcrypt
	// could break here if older versions are not supported.
	assigned, err := c.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			<_:uid1> <secret> "$2a$10$0Cv9uJBUhG2FstnCUNw2/.GNH7M89M.yaXn3//Zp8a0.s6zVIJFz6"^^<xs:password> .
			<_:uid2> <secret> "$2a$10$LxWNQhbgcdnJkzWfYnUahuDWkWfs9e8pf7uH8WkdAjMxTeKh8W8V2"^^<xs:password> .
			<_:uid3> <secret> "$2a$10$IXnmk8WSQmhNpHWrAIMtgOnU1QWcndyqgfsUGlzHsVzrpFcrFnUoi"^^<xs:password> .
		`),
	})
	require.NoError(t, err)
	require.Len(t, assigned.Uids, 3)

	txn := c.NewTxn()
	for _, uid := range assigned.Uids {
		resp, err := txn.Query(ctx, `
		{
			q(func: uid(`+uid+`)) {
				secret: checkpwd(secret, "password2")
			}
		}`)
		require.NoError(t, err)
		require.JSONEq(t, fmt.Sprintf(`{"q":[{"secret":%t}]}`, uid == assigned.Uids["uid2"]),
			string(resp.Json))
	}
	require.NoError(t, txn.Discard(ctx))

	resp, err := c.NewTxn().Query(ctx, `
	{
	  q(func: uid(`+assigned.Uids["uid1"]+`)) {
			secret: checkpwd(secret, "invalid")
	  }
	}`)
	require.NoError(t, err)
	require.JSONEq(t, `{"q":[{"secret":false}]}`, string(resp.Json))

	resp, err = c.NewTxn().Query(ctx, `
	{
	  q(func: uid(`+assigned.Uids["uid2"]+`)) {
			secret: checkpwd(secret, "invalid")
	  }
	}`)
	require.NoError(t, err)
	require.JSONEq(t, `{"q":[{"secret":false}]}`, string(resp.Json))

	// NOTE: This tests the _old_ behavior. Passwords were exported as string and used for the
	// encryption value. This is wrong, but shouldn't break.
	assigned, err = c.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			<_:uid1> <secret> "$2a$10$0Cv9uJBUhG2FstnCUNw2/.GNH7M89M.yaXn3//Zp8a0.s6zVIJFz6"^^<xs:string> .
			<_:uid2> <secret> "$2a$10$LxWNQhbgcdnJkzWfYnUahuDWkWfs9e8pf7uH8WkdAjMxTeKh8W8V2"^^<xs:string> .
			<_:uid3> <secret> "$2a$10$IXnmk8WSQmhNpHWrAIMtgOnU1QWcndyqgfsUGlzHsVzrpFcrFnUoi"^^<xs:string> .
		`),
	})
	require.NoError(t, err)
	require.Len(t, assigned.Uids, 3)

	resp, err = c.NewTxn().Query(ctx, `
	{
	  q(func: uid(`+assigned.Uids["uid2"]+`)) {
			secret: checkpwd(secret, "$2a$10$LxWNQhbgcdnJkzWfYnUahuDWkWfs9e8pf7uH8WkdAjMxTeKh8W8V2")
	  }
	}`)
	require.NoError(t, err)
	require.JSONEq(t, `{"q":[{"secret":true}]}`, string(resp.Json))
}
