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
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"sync"
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

func TestStringIndex(t *testing.T) {
	total := 100000
	numAccts := uint64(total)
	acctsBal := make(map[int]int, numAccts)
	var lock sync.Mutex

	var dg *dgo.Dgraph
	err := x.RetryUntilSuccess(10, time.Second, func() error {
		var err error
		dg, err = testutil.DgraphClientWithGroot(testutil.SockAddr)
		return err
	})
	require.NoError(t, err)

	testutil.DropAll(t, dg)
	if err := dg.Alter(context.Background(), &api.Operation{
		Schema: "balance: string .",
	}); err != nil {
		t.Fatalf("error in setting up schema :: %v\n", err)
	}

	if _, _, err := dg.AllocateUIDs(context.Background(), uint64(total*10)); err != nil {
		t.Fatalf("error in assigning UIDs :: %v", err)
	}

	// first insert bank accounts
	fmt.Println("inserting accounts")
	for i := 1; i <= int(numAccts); {
		bb := &bytes.Buffer{}
		for j := 0; j < 10000; j++ {
			acctsBal[i] = rand.Intn(total * 100)
			_, err := bb.WriteString(fmt.Sprintf("<%v> <balance> \"%v\" .\n", i, acctsBal[i]))
			if err != nil {
				t.Fatalf("error in mutation %v\n", err)
			}
			i++
		}
		if err := testutil.RetryMutation(dg, &api.Mutation{
			CommitNow: true,
			SetNquads: bb.Bytes(),
		}); err != nil {
			t.Fatalf("error in mutation :: %v", err)
		}
	}

	fmt.Println("building indexes in background")
	if err := dg.Alter(context.Background(), &api.Operation{
		Schema:          "balance: string @index(fulltext, term, exact) .",
		RunInBackground: true,
	}); err != nil {
		t.Fatalf("error in adding indexes :: %v\n", err)
	}

	// perform mutations until ctrl+c
	mutateUID := func(uid int) {
		nb := rand.Intn(total * 100)
		switch uid % 3 {
		case 0:
			// change the balance to new random value.
			if _, err := dg.NewTxn().Mutate(context.Background(), &api.Mutation{
				CommitNow: true,
				SetNquads: []byte(fmt.Sprintf(`<%v> <balance> "%v" .`, uid, nb)),
			}); err != nil && !errors.Is(err, dgo.ErrAborted) {
				t.Fatalf("error in mutation :: %v\n", err)
			} else if errors.Is(err, dgo.ErrAborted) {
				return
			}
		case 1:
			// delete this uid.
			if _, err := dg.NewTxn().Mutate(context.Background(), &api.Mutation{
				CommitNow: true,
				DelNquads: []byte(fmt.Sprintf(`<%v> <balance> * .`, uid)),
			}); err != nil && !errors.Is(err, dgo.ErrAborted) {
				t.Fatalf("error in deletion :: %v\n", err)
			} else if errors.Is(err, dgo.ErrAborted) {
				return
			}
			nb = -1
		case 2:
			// add new uid.
			uid = int(atomic.AddUint64(&numAccts, 1))
			if _, err := dg.NewTxn().Mutate(context.Background(), &api.Mutation{
				CommitNow: true,
				SetNquads: []byte(fmt.Sprintf(`<%v> <balance> "%v" .`, uid, nb)),
			}); err != nil && !errors.Is(err, dgo.ErrAborted) {
				t.Fatalf("error in insertion :: %v\n", err)
			} else if errors.Is(err, dgo.ErrAborted) {
				return
			}
		}

		lock.Lock()
		acctsBal[uid] = nb
		lock.Unlock()
	}

	// perform mutations until ctrl+c
	var swg sync.WaitGroup
	var counter uint64
	quit := make(chan struct{})
	runLoop := func() {
		defer swg.Done()
		for {
			select {
			case <-quit:
				return
			default:
				n := int(atomic.LoadUint64(&numAccts))
				mutateUID(rand.Intn(n) + 1)
				atomic.AddUint64(&counter, 1)
			}
		}
	}

	swg.Add(101)
	for i := 0; i < 100; i++ {
		go runLoop()
	}
	go printStats(&counter, quit, &swg)
	waitForSchemaUpdate(`{ q(func: anyoftext(balance, "example")) {uid}}`, dg)
	close(quit)
	swg.Wait()
	fmt.Println("mutations done")

	// compute index
	balIndex := make(map[int][]int)
	for uid, bal := range acctsBal {
		balIndex[bal] = append(balIndex[bal], uid)
	}
	for _, aa := range balIndex {
		sort.Ints(aa)
	}

	checkDelete := func(uid int) error {
		q := fmt.Sprintf(`{ q(func: uid(%v)) {balance}}`, uid)
		resp, err := dg.NewReadOnlyTxn().Query(context.Background(), q)
		if err != nil {
			return fmt.Errorf("error in query: %v :: %w", q, err)
		}
		var data struct {
			Q []struct {
				Balance string
			}
		}
		if err := json.Unmarshal(resp.Json, &data); err != nil {
			return fmt.Errorf("error in json.Unmarshal :: %w", err)
		}

		if len(data.Q) != 0 {
			return fmt.Errorf("found a deleted UID, %v", uid)
		}
		return nil
	}

	checkBalance := func(b int, uids []int) error {
		q := fmt.Sprintf(`{ q(func: anyoftext(balance, "%v")) {uid}}`, b)
		resp, err := dg.NewReadOnlyTxn().Query(context.Background(), q)
		if err != nil {
			return fmt.Errorf("error in query: %v :: %w", q, err)
		}
		var data struct {
			Q []struct {
				UID string
			}
		}
		if err := json.Unmarshal(resp.Json, &data); err != nil {
			return fmt.Errorf("error in json.Unmarshal :: %w", err)
		}

		actual := make([]int, len(data.Q))
		for i, ui := range data.Q {
			v, err := strconv.ParseInt(ui.UID, 0, 64)
			if err != nil {
				return err
			}
			actual[i] = int(v)
		}
		sort.Ints(actual)

		if len(actual) != len(uids) {
			return fmt.Errorf("length not equal :: exp: %v, actual %v", uids, actual)
		}
		for i := range uids {
			if uids[i] != actual[i] {
				return fmt.Errorf("value not equal :: exp: %v, actual %v", uids, actual)
			}
		}

		return nil
	}

	type pair struct {
		key int
		err string
	}
	ch := make(chan pair, numAccts)

	fmt.Println("starting to query")
	var count uint64
	th := y.NewThrottle(50000)
	require.NoError(t, th.Do())
	go func() {
		defer th.Done(nil)
		for {
			time.Sleep(2 * time.Second)
			cur := atomic.LoadUint64(&count)
			fmt.Printf("%v/%v done\n", cur, len(balIndex))
			if int(cur) == len(balIndex) {
				break
			}
		}
	}()

	for balance, uids := range balIndex {
		require.NoError(t, th.Do())
		go func(bal int, uidList []int) {
			defer th.Done(nil)
			if bal == -1 {
				for _, uid := range uidList {
					if err := checkDelete(uid); err != nil {
						ch <- pair{uid, err.Error()}
					}
				}
			} else {
				if err := checkBalance(bal, uidList); err != nil {
					ch <- pair{bal, err.Error()}
				}
			}
			atomic.AddUint64(&count, 1)
		}(balance, uids)
	}
	require.NoError(t, th.Finish())

	close(ch)
	for p := range ch {
		t.Fatalf("failed for %v, :: %v\n", p.key, p.err)
	}
}
