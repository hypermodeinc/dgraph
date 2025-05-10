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
	"strings"
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

func TestCountIndex(t *testing.T) {
	total := 10000
	numUIDs := uint64(total)
	edgeCount := make([]int, total+100000)
	uidLocks := make([]sync.Mutex, total+100000)

	var dg *dgo.Dgraph
	err := x.RetryUntilSuccess(10, time.Second, func() error {
		var err error
		dg, err = testutil.DgraphClientWithGroot(testutil.SockAddr)
		return err
	})
	require.NoError(t, err)
	testutil.DropAll(t, dg)
	if err := dg.Alter(context.Background(), &api.Operation{
		Schema: "value: [string] .",
	}); err != nil {
		t.Fatalf("error in setting up schema :: %v\n", err)
	}

	if _, _, err := dg.AllocateUIDs(context.Background(), uint64(total*10)); err != nil {
		t.Fatalf("error in assigning UIDs :: %v", err)
	}

	fmt.Println("inserting values")
	th := y.NewThrottle(10000)
	for i := 1; i <= int(numUIDs); i++ {
		require.NoError(t, th.Do())

		go func(uid int) {
			defer th.Done(nil)
			bb := &bytes.Buffer{}
			edgeCount[uid] = rand.Intn(1000)
			for j := 0; j < edgeCount[uid]; j++ {
				_, err := bb.WriteString(fmt.Sprintf("<%v> <value> \"%v\" .\n", uid, j))
				x.Panic(err)
			}
			if err := testutil.RetryMutation(dg, &api.Mutation{
				CommitNow: true,
				SetNquads: bb.Bytes(),
			}); err != nil {
				panic(fmt.Sprintf("error in mutation :: %v", err))
			}
		}(i)
	}
	require.NoError(t, th.Finish())

	fmt.Println("building indexes in background")
	if err := dg.Alter(context.Background(), &api.Operation{
		Schema:          "value: [string] @count .",
		RunInBackground: true,
	}); err != nil {
		t.Fatalf("error in adding indexes :: %v\n", err)
	}

	// perform mutations until ctrl+c
	mutateUID := func(uid int) {
		uidLocks[uid].Lock()
		defer uidLocks[uid].Unlock()
		ec := edgeCount[uid]
		switch rand.Intn(1000) % 3 {
		case 0:
			// add new edge
			if _, err := dg.NewTxn().Mutate(context.Background(), &api.Mutation{
				CommitNow: true,
				SetNquads: []byte(fmt.Sprintf(`<%v> <value> "%v" .`, uid, ec)),
			}); err != nil && !errors.Is(err, dgo.ErrAborted) {
				t.Fatalf("error in mutation :: %v\n", err)
			} else if errors.Is(err, dgo.ErrAborted) {
				return
			}
			ec++
		case 1:
			if ec <= 0 {
				return
			}
			// delete an edge
			if _, err := dg.NewTxn().Mutate(context.Background(), &api.Mutation{
				CommitNow: true,
				DelNquads: []byte(fmt.Sprintf(`<%v> <value> "%v" .`, uid, ec-1)),
			}); err != nil && (errors.Is(err, dgo.ErrAborted) ||
				strings.Contains(err.Error(), "Properties of guardians group and groot user cannot be deleted")) {
				return
			} else if err != nil {
				t.Fatalf("error in deletion :: %v\n", err)
			}
			ec--
		case 2:
			// new uid with one edge
			uid = int(atomic.AddUint64(&numUIDs, 1))
			if _, err := dg.NewTxn().Mutate(context.Background(), &api.Mutation{
				CommitNow: true,
				SetNquads: []byte(fmt.Sprintf(`<%v> <value> "0" .`, uid)),
			}); err != nil && !errors.Is(err, dgo.ErrAborted) {
				t.Fatalf("error in insertion :: %v\n", err)
			} else if errors.Is(err, dgo.ErrAborted) {
				return
			}
			ec = 1
		}

		edgeCount[uid] = ec
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
				n := int(atomic.LoadUint64(&numUIDs))
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
	waitForSchemaUpdate(`{ q(func: eq(count(value), "3")) {uid}}`, dg)
	close(quit)
	swg.Wait()
	fmt.Println("mutations done")

	// compute count index
	countIndex := make(map[int][]int)
	for uid := 1; uid <= int(numUIDs); uid++ {
		val := edgeCount[uid]
		countIndex[val] = append(countIndex[val], uid)
	}
	for _, aa := range countIndex {
		sort.Ints(aa)
	}

	checkDelete := func(uid int) error {
		q := fmt.Sprintf(`{ q(func: uid(%v)) {value:count(value)}}`, uid)
		resp, err := dg.NewReadOnlyTxn().Query(context.Background(), q)
		if err != nil {
			return fmt.Errorf("error in query: %v :: %w", q, err)
		}
		var data struct {
			Q []struct {
				Count int
			}
		}
		if err := json.Unmarshal(resp.Json, &data); err != nil {
			return fmt.Errorf("error in json.Unmarshal :: %w", err)
		}

		if len(data.Q) != 1 && data.Q[0].Count != 0 {
			return fmt.Errorf("found a deleted UID, %v", uid)
		}
		return nil
	}

	checkValue := func(b int, uids []int) error {
		q := fmt.Sprintf(`{ q(func: eq(count(value), "%v")) {uid}}`, b)
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
	ch := make(chan pair, numUIDs)

	fmt.Println("starting to query")
	var count uint64
	th = y.NewThrottle(50000)
	require.NoError(t, th.Do())
	go func() {
		defer th.Done(nil)
		for {
			time.Sleep(2 * time.Second)
			cur := atomic.LoadUint64(&count)
			fmt.Printf("%v/%v done\n", cur, len(countIndex))
			if int(cur) == len(countIndex) {
				break
			}
		}
	}()

	for value, uids := range countIndex {
		require.NoError(t, th.Do())
		go func(val int, uidList []int) {
			defer th.Done(nil)
			if val <= 0 {
				for _, uid := range uidList {
					if err := checkDelete(uid); err != nil {
						ch <- pair{uid, err.Error()}
					}
				}
			} else {
				if err := checkValue(val, uidList); err != nil {
					ch <- pair{val, err.Error()}
				}
			}
			atomic.AddUint64(&count, 1)
		}(value, uids)
	}
	require.NoError(t, th.Finish())

	close(ch)
	for p := range ch {
		t.Errorf("failed for %v, :: %v\n", p.key, p.err)
	}
}
