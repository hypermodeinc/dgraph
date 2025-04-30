//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/badger/v4/y"
	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var (
	total = 100000
)

func addBankData(dg *dgo.Dgraph, pred string) error {
	for i := 1; i <= total; {
		bb := &bytes.Buffer{}
		for j := 0; j < 10000; j++ {
			_, err := bb.WriteString(fmt.Sprintf("<%v> <%v> \"%v\" .\n", i, pred, i))
			if err != nil {
				return fmt.Errorf("error in mutation :: %w", err)
			}
			i++
		}
		if err := testutil.RetryMutation(dg, &api.Mutation{
			CommitNow: true,
			SetNquads: bb.Bytes(),
		}); err != nil {
			return fmt.Errorf("error in mutation :: %w", err)
		}
	}

	return nil
}

func TestParallelIndexing(t *testing.T) {
	var dg *dgo.Dgraph
	err := x.RetryUntilSuccess(10, time.Second, func() error {
		var err error
		dg, err = testutil.DgraphClientWithGroot(testutil.SockAddr)
		return err
	})
	require.NoError(t, err)

	if _, _, err := dg.AllocateUIDs(context.Background(), uint64(total*10)); err != nil {
		t.Fatalf("error in assignig UIDs :: %v", err)
	}

	testutil.DropAll(t, dg)
	if err := dg.Alter(context.Background(), &api.Operation{
		Schema: `
			balance_int: int .
			balance_str: string .
			balance_float: float .
		`,
		RunInBackground: true,
	}); err != nil {
		t.Fatalf("error in setting up schema :: %v\n", err)
	}

	fmt.Println("adding integer dataset")
	if err := addBankData(dg, "balance_int"); err != nil {
		t.Fatalf("error in adding integer predicate :: %v\n", err)
	}

	fmt.Println("adding string dataset")
	if err := addBankData(dg, "balance_str"); err != nil {
		t.Fatalf("error in adding string predicate :: %v\n", err)
	}

	fmt.Println("adding float dataset")
	if err := addBankData(dg, "balance_float"); err != nil {
		t.Fatalf("error in adding float predicate :: %v\n", err)
	}

	fmt.Println("building indexes in background for int and string data")
	// Wait until previous indexing is complete.
	for {
		if err := dg.Alter(context.Background(), &api.Operation{
			Schema: `
			balance_int: int @index(int) .
			balance_str: string @index(fulltext, term, exact) .
		`,
			RunInBackground: true,
		}); err != nil && !strings.Contains(err.Error(), "errIndexingInProgress") {
			t.Fatalf("error in adding indexes :: %v\n", err)
		} else if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	// Wait until previous indexing is complete.
	for {
		if err := dg.Alter(context.Background(), &api.Operation{
			Schema: `
			balance_int: int @index(int) .
			balance_str: string @index(fulltext, term, exact) .
		`,
			RunInBackground: true,
		}); err != nil && !strings.Contains(err.Error(), "errIndexingInProgress") {
			t.Fatalf("error in adding indexes :: %v\n", err)
		} else if err == nil {
			break
		}
		time.Sleep(time.Second)
	}

	// Wait until previous indexing is complete.
	for {
		if err := dg.Alter(context.Background(), &api.Operation{
			Schema:          `balance_float: float @index(float) .`,
			RunInBackground: true,
		}); err != nil && !strings.Contains(err.Error(), "errIndexingInProgress") {
			t.Fatalf("error in adding indexes :: %v\n", err)
		} else if err == nil {
			break
		}
		time.Sleep(time.Second)
	}

	fmt.Println("waiting for float indexing to complete")
	waitForSchemaUpdate(`{ q(func: eq(balance_float, "2.0")) {uid}}`, dg)

	// balance should be same as uid.
	checkBalance := func(b int, pred string) error {
		q := fmt.Sprintf(`{ q(func: eq(%v, "%v")) {uid}}`, pred, b)
		resp, err := dg.NewReadOnlyTxn().Query(context.Background(), q)
		if err != nil {
			return fmt.Errorf("error in query: %v ::%w", q, err)
		}
		var data struct {
			Q []struct {
				UID string
			}
		}
		if err := json.Unmarshal(resp.Json, &data); err != nil {
			return fmt.Errorf("error in json.Unmarshal :: %w", err)
		}

		if len(data.Q) != 1 {
			return fmt.Errorf("length not equal :: exp: %v, actual %v", b, data.Q[0])
		}
		v, err := strconv.ParseInt(data.Q[0].UID, 0, 64)
		if err != nil {
			return err
		}
		if b != int(v) {
			return fmt.Errorf("value not equal :: exp: %v, actual %v", b, data.Q[0])
		}

		return nil
	}

	fmt.Println("starting to query")
	var count uint64
	th := y.NewThrottle(50000)
	require.NoError(t, th.Do())
	go func() {
		defer th.Done(nil)
		for {
			time.Sleep(2 * time.Second)
			cur := atomic.LoadUint64(&count)
			fmt.Printf("%v/%v done\n", cur, total*3)
			if int(cur) == total*3 {
				break
			}
		}
	}()

	type pair struct {
		key int
		err string
	}
	ch := make(chan pair, total*3)
	for _, predicate := range []string{"balance_str", "balance_int", "balance_float"} {
		for i := 1; i <= total; i++ {
			require.NoError(t, th.Do())
			go func(bal int, pred string) {
				defer th.Done(nil)
				if err := checkBalance(bal, pred); err != nil {
					ch <- pair{bal, err.Error()}
				}
				atomic.AddUint64(&count, 1)
			}(i, predicate)
		}
	}
	require.NoError(t, th.Finish())

	close(ch)
	for p := range ch {
		t.Errorf("failed for %v, :: %v\n", p.key, p.err)
	}
}
