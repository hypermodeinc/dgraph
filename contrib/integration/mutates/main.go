/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var alpha = flag.String("alpha", "localhost:9080", "Dgraph alpha addr")
var insert = flag.Bool("add", false, "Insert")

func main() {
	flag.Parse()

	// Setup dgraph client
	ctx := context.Background()
	conn, err := grpc.Dial(*alpha, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	pc := api.NewDgraphClient(conn)
	c := dgo.NewDgraphClient(pc)
	err = c.Login(ctx, "groot", "password")
	x.Check(err)

	// Ingest
	if *insert {
		testInsert3Quads(ctx, c)
	} else {
		testQuery3Quads(ctx, c)
	}
}

func testInsert3Quads(ctx context.Context, c *dgo.Dgraph) {
	// Set schema
	x.Check(c.SetSchema(ctx, dgo.RootNamespace, `name: string @index(fulltext) .`))

	txn := c.NewTxn()
	mu := &api.Mutation{}
	quad := &api.NQuad{
		Subject:     "200",
		Predicate:   "name",
		ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: "ok 200"}},
	}
	mu.Set = []*api.NQuad{quad}
	_, err := txn.Mutate(ctx, mu)
	if err != nil {
		log.Fatalf("Error while running mutation: %v\n", err)
	}

	mu = &api.Mutation{}
	quad = &api.NQuad{
		Subject:     "300",
		Predicate:   "name",
		ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: "ok 300"}},
	}
	mu.Set = []*api.NQuad{quad}
	// mu.SetNquads = []byte(`<300> <name> "ok 300" .`)
	_, err = txn.Mutate(ctx, mu)
	if err != nil {
		log.Fatalf("Error while running mutation: %v\n", err)
	}

	mu = &api.Mutation{}
	quad = &api.NQuad{
		Subject:     "400",
		Predicate:   "name",
		ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: "ok 400"}},
	}
	mu.Set = []*api.NQuad{quad}
	// mu.SetNquads = []byte(`<400> <name> "ok 400" .`)
	_, err = txn.Mutate(ctx, mu)
	if err != nil {
		log.Fatalf("Error while running mutation: %v\n", err)
	}

	x.Check(txn.Commit(ctx))
	fmt.Println("Commit OK")
}

func testQuery3Quads(ctx context.Context, c *dgo.Dgraph) {
	txn := c.NewTxn()
	q := `{ me(func: uid(200, 300, 400)) { name }}`
	resp, err := txn.Query(ctx, q)
	if err != nil {
		log.Fatalf("Error while running query: %v\n", err)
	}
	fmt.Printf("Response JSON: %q\n", resp.Json)
	x.AssertTrue(bytes.Equal(resp.Json, []byte(
		"{\"me\":[{\"name\":\"ok 200\"},{\"name\":\"ok 300\"},{\"name\":\"ok 400\"}]}")))
	x.AssertTrue(resp.Txn.StartTs > 0)
	x.Check(txn.Commit(ctx))
}
