/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var addrs = flag.String("addrs", "", "comma separated dgraph addresses")
var mode = flag.String("mode", "", "mode to run in ('mutate' or 'query')")
var conc = flag.Int("j", 1, "number of operations to run in parallel")

func init() {
	rand.Seed(time.Now().Unix())
}

var ctx = context.Background()

func main() {
	flag.Parse()
	c := makeClient()

	// Install the schema automatically on the first run. This allows the same
	// command to be used when running this program for the first and
	// subsequent times. We guess if it's the first run based on the number of
	// schema items.
	resp, err := c.NewTxn().Query(ctx, "schema {}")
	x.Check(err)
	if len(resp.Json) < 5 {
		// Run each schema alter separately so that there is an even
		// distribution among all groups.
		for _, s := range schema() {
			x.Check(c.SetSchema(ctx, dgo.RootNamespace, s))
		}
		x.Check2(c.NewTxn().Mutate(ctx, &api.Mutation{
			CommitNow: true,
			SetNquads: []byte(initialData()),
		}))
	}

	switch *mode {
	case "mutate":
		var errCount int64
		var mutateCount int64
		for range *conc {
			go func() {
				for {
					err := mutate(c)
					if err == nil {
						atomic.AddInt64(&mutateCount, 1)
					} else {
						atomic.AddInt64(&errCount, 1)
					}
				}
			}()
		}
		for {
			time.Sleep(time.Second)
			fmt.Printf("Status: success_mutations=%d errors=%d\n",
				atomic.LoadInt64(&mutateCount), atomic.LoadInt64(&errCount))
		}
	case "query":
		var errCount int64
		var queryCount int64
		for range *conc {
			go func() {
				for {
					err := showNode(c)
					if err == nil {
						atomic.AddInt64(&queryCount, 1)
					} else {
						atomic.AddInt64(&errCount, 1)
					}
				}
			}()
		}
		for {
			time.Sleep(time.Second)
			fmt.Printf("Status: success_queries=%d errors=%d\n",
				atomic.LoadInt64(&queryCount), atomic.LoadInt64(&errCount))
		}
	default:
		fmt.Printf("unknown mode: %q\n", *mode)
		os.Exit(1)
	}
}

func schema() []string {
	s := []string{"xid: string @index(exact) .\n"}
	for char := 'a'; char <= 'z'; char++ {
		s = append(s, fmt.Sprintf("count_%c: int .\n", char))
	}
	for char := 'a'; char <= 'z'; char++ {
		s = append(s, fmt.Sprintf("attr_%c: string .\n", char))
	}
	return s
}

func initialData() string {
	rdfs := "_:root <xid> \"root\" .\n"
	for char := 'a'; char <= 'z'; char++ {
		rdfs += fmt.Sprintf("_:root <count_%c> \"0\" .\n", char)
	}
	return rdfs
}

func makeClient() *dgo.Dgraph {
	var dgcs []api.DgraphClient
	for _, addr := range strings.Split(*addrs, ",") {
		c, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		x.Check(err)
		dgcs = append(dgcs, api.NewDgraphClient(c))
	}
	return dgo.NewDgraphClient(dgcs...)
}

type runner struct {
	txn *dgo.Txn
}

func mutate(c *dgo.Dgraph) error {
	r := &runner{
		txn: c.NewTxn(),
	}
	defer func() { _ = r.txn.Discard(ctx) }()

	char := 'a' + rune(rand.Intn(26))

	var result struct {
		Q []struct {
			Uid   *string
			Count *int
		}
	}
	if err := r.query(&result, `
	{
		q(func: eq(xid, "root")) {
			uid
			count: count_%c
		}
	}
	`, char); err != nil {
		return err
	}

	x.AssertTrue(len(result.Q) > 0 && result.Q[0].Count != nil && result.Q[0].Uid != nil)

	if _, err := r.txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(fmt.Sprintf("<%s> <count_%c> \"%d\" .\n",
			*result.Q[0].Uid, char, *result.Q[0].Count+1)),
	}); err != nil {
		return err
	}

	rdfs := fmt.Sprintf("_:node <xid> \"%c_%d\" .\n", char, *result.Q[0].Count)
	for char := 'a'; char <= 'z'; char++ {
		if rand.Float64() < 0.9 {
			continue
		}
		payload := make([]byte, 16+rand.Intn(16))
		if _, err := rand.Read(payload); err != nil {
			return err
		}
		rdfs += fmt.Sprintf("_:node <attr_%c> \"%s\" .\n", char, url.QueryEscape(string(payload)))
	}
	if _, err := r.txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(rdfs),
	}); err != nil {
		return err
	}

	return r.txn.Commit(ctx)
}

func showNode(c *dgo.Dgraph) error {
	r := &runner{
		txn: c.NewTxn(),
	}
	defer func() { _ = r.txn.Discard(ctx) }()

	char := 'a' + rune(rand.Intn(26))
	var result struct {
		Q []struct {
			Count *int
		}
	}

	q := fmt.Sprintf(`
	{
		q(func: eq(xid, "root")) {
			uid
			count: count_%c
		}
	}
	`, char)
	resp, err := r.txn.Query(ctx, q)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(resp.Json, &result); err != nil {
		return err
	}
	x.AssertTruef(len(result.Q) > 0 && result.Q[0].Count != nil, "%v %+v", string(resp.Json), result)

	var m map[string]interface{}
	return r.query(&m, `
	{
		q(func: eq(xid, "%c_%d")) {
			expand(_all_)
		}
	}
	`, char, rand.Intn(*result.Q[0].Count))
}

func (r *runner) query(out interface{}, q string, args ...interface{}) error {
	q = fmt.Sprintf(q, args...)
	resp, err := r.txn.Query(ctx, q)
	if err != nil {
		return err
	}
	return json.Unmarshal(resp.Json, out)
}
