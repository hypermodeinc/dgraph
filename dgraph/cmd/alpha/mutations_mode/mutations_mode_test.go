//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/testutil"
)

// Tests in this file require a cluster running with the --limit "mutations=<mode>;" flag.

func runOn(conn *grpc.ClientConn, fn func(*testing.T, *dgo.Dgraph)) func(*testing.T) {
	return func(t *testing.T) {
		dg := dgo.NewDgraphClient(api.NewDgraphClient(conn))
		fn(t, dg)
	}
}

func dropAllDisallowed(t *testing.T, dg *dgo.Dgraph) {
	err := dg.DropAll(context.Background())
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "no mutations allowed")
}

func dropAllAllowed(t *testing.T, dg *dgo.Dgraph) {
	require.NoError(t, dg.DropAll(context.Background()))
}

func mutateNewDisallowed(t *testing.T, dg *dgo.Dgraph) {
	ctx := context.Background()

	txn := dg.NewTxn()
	_, err := txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
			_:a <name> "Alice" .
		`),
	})

	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "no mutations allowed")
}

func mutateNewDisallowed2(t *testing.T, dg *dgo.Dgraph) {
	ctx := context.Background()

	txn := dg.NewTxn()
	_, err := txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
			_:a <name> "Alice" .
		`),
	})

	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "schema not defined for predicate")
}

func addPredicateDisallowed(t *testing.T, dg *dgo.Dgraph) {
	ctx := context.Background()
	err := dg.SetSchema(ctx, dgo.RootNamespace, `name: string @index(exact) .`)
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "no mutations allowed")
}

func addPredicateAllowed1(t *testing.T, dg *dgo.Dgraph) {
	ctx := context.Background()
	err := dg.SetSchema(ctx, dgo.RootNamespace, `name: string @index(exact) .`)
	require.NoError(t, err)
}

func addPredicateAllowed2(t *testing.T, dg *dgo.Dgraph) {
	ctx := context.Background()
	err := dg.SetSchema(ctx, dgo.RootNamespace, `size: string @index(exact) .`)
	require.NoError(t, err)
}

func mutateExistingDisallowed(t *testing.T, dg *dgo.Dgraph) {
	ctx := context.Background()

	txn := dg.NewTxn()
	_, err := txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
			_:a <dgraph.xid> "XID00001" .
		`),
	})

	require.NoError(t, txn.Discard(ctx))
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "no mutations allowed")
}

func mutateExistingAllowed1(t *testing.T, dg *dgo.Dgraph) {
	ctx := context.Background()

	txn := dg.NewTxn()
	_, err := txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
			_:a <name> "Alice" .
		`),
	})

	require.NoError(t, txn.Commit(ctx))
	require.NoError(t, err)
}

func mutateExistingAllowed2(t *testing.T, dg *dgo.Dgraph) {
	ctx := context.Background()

	txn := dg.NewTxn()
	_, err := txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
			_:s <size> "small" .
		`),
	})

	require.NoError(t, txn.Commit(ctx))
	require.NoError(t, err)
}

func TestMutationsDisallow(t *testing.T) {
	a := testutil.ContainerAddr("alpha1", 9080)
	conn, err := grpc.Dial(a, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Cannot perform drop all op: %s", err.Error())
	}
	defer conn.Close()

	t.Run("disallow drop all in no mutations mode",
		runOn(conn, dropAllDisallowed))
	t.Run("disallow mutate new predicate in no mutations mode",
		runOn(conn, mutateNewDisallowed))
	t.Run("disallow add predicate in no mutations mode",
		runOn(conn, addPredicateDisallowed))
	t.Run("disallow mutate existing predicate in no mutations mode",
		runOn(conn, mutateExistingDisallowed))
}

func TestMutationsStrict(t *testing.T) {
	a1 := testutil.ContainerAddr("alpha2", 9080)
	conn1, err := grpc.Dial(a1, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Cannot perform drop all op: %s", err.Error())
	}
	defer conn1.Close()

	a2 := testutil.ContainerAddr("alpha3", 9080)
	conn2, err := grpc.Dial(a2, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Cannot perform drop all op: %s", err.Error())
	}
	defer conn2.Close()

	t.Run("allow group1 drop all in strict mutations mode",
		runOn(conn1, dropAllAllowed))
	t.Run("allow group2 drop all in strict mutations mode",
		runOn(conn2, dropAllAllowed))
	t.Run("disallow group1 mutate new predicate in strict mutations mode",
		runOn(conn1, mutateNewDisallowed2))
	t.Run("disallow group2 mutate new predicate in strict mutations mode",
		runOn(conn2, mutateNewDisallowed2))
	t.Run("allow group1 add predicate in strict mutations mode",
		runOn(conn1, addPredicateAllowed1))
	t.Run("allow group2 add predicate in strict mutations mode",
		runOn(conn2, addPredicateAllowed2))
	t.Run("allow group1 mutate group1 predicate in strict mutations mode",
		runOn(conn1, mutateExistingAllowed1))
	t.Run("allow group2 mutate group1 predicate in strict mutations mode",
		runOn(conn2, mutateExistingAllowed1))
	t.Run("allow group1 mutate group2 predicate in strict mutations mode",
		runOn(conn1, mutateExistingAllowed2))
	t.Run("allow group2 mutate group2 predicate in strict mutations mode",
		runOn(conn2, mutateExistingAllowed2))
}
