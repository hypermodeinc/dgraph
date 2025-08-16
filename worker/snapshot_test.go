//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/testutil"
)

func TestSnapshot(t *testing.T) {
	snapshotTs := uint64(0)

	dg1, err := testutil.DgraphClient(testutil.GetSockAddr())
	if err != nil {
		t.Fatalf("Error while getting a dgraph client: %v", err)
	}
	require.NoError(t, dg1.Alter(context.Background(), &api.Operation{
		DropOp: api.Operation_ALL,
	}))
	require.NoError(t, dg1.Alter(context.Background(), &api.Operation{
		Schema: `
			value: int .
			name: string .
			address: string @index(term) .`,
	}))

	t.Logf("Stopping alpha2.\n")
	require.NoError(t, testutil.DockerRun("alpha2", testutil.Stop))

	// Update the name predicate to include an index.
	require.NoError(t, dg1.Alter(context.Background(), &api.Operation{
		Schema: `name: string @index(term) .`,
	}))

	// Delete the address predicate.
	require.NoError(t, dg1.Alter(context.Background(), &api.Operation{
		DropOp:    api.Operation_ATTR,
		DropValue: "address",
	}))

	for i := 1; i <= 200; i++ {
		err := testutil.RetryMutation(dg1, &api.Mutation{
			SetNquads: []byte(fmt.Sprintf(`_:node <value> "%d" .`, i)),
			CommitNow: true,
		})
		require.NoError(t, err)
	}
	t.Logf("Mutations done.\n")
	snapshotTs = waitForSnapshot(t, snapshotTs)

	t.Logf("Starting alpha2.\n")
	require.NoError(t, testutil.DockerRun("alpha2", testutil.Start))

	// Wait for the container to start.
	if err := testutil.CheckHealthContainer(testutil.ContainerAddr("alpha2", 8080)); err != nil {
		t.Fatalf("error while getting alpha container health: %v", err)
	}
	dg2, err := testutil.DgraphClient(testutil.ContainerAddr("alpha2", 9080))
	if err != nil {
		t.Fatalf("Error while getting a dgraph client: %v", err)
	}
	verifySnapshot(t, dg2, 200)

	t.Logf("Stopping alpha2.\n")
	require.NoError(t, testutil.DockerRun("alpha2", testutil.Stop))

	for i := 201; i <= 400; i++ {
		err := testutil.RetryMutation(dg1, &api.Mutation{
			SetNquads: []byte(fmt.Sprintf(`_:node <value> "%d" .`, i)),
			CommitNow: true,
		})
		require.NoError(t, err)
	}
	const testSchema = "type Person { name: String }"
	// uploading new schema while alpha2 is not running so we can
	// test whether the stopped alpha gets new schema in snapshot
	testutil.UpdateGQLSchema(t, testutil.GetSockAddrHttp(), testSchema)
	_ = waitForSnapshot(t, snapshotTs)

	t.Logf("Starting alpha2.\n")
	require.NoError(t, testutil.DockerRun("alpha2", testutil.Start))
	if err := testutil.CheckHealthContainer(testutil.ContainerAddr("alpha2", 8080)); err != nil {
		t.Fatalf("error while getting alpha container health: %v", err)
	}

	dg2, err = testutil.DgraphClient(testutil.ContainerAddr("alpha2", 9080))
	if err != nil {
		t.Fatalf("Error while getting a dgraph client: %v", err)
	}
	verifySnapshot(t, dg2, 400)
	resp := testutil.GetGQLSchema(t, testutil.ContainerAddr("alpha2", 8080))
	// comparing uploaded graphql schema and schema acquired from stopped container
	require.Equal(t, testSchema, resp)
}

func verifySnapshot(t *testing.T, dg *dgo.Dgraph, num int) {
	expectedSum := (num * (num + 1)) / 2

	q1 := `
	{
		values(func: has(value)) {
			value
		}
	}`

	resMap := make(map[string][]map[string]int)
	resp, err := testutil.RetryQuery(dg, q1)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(resp.Json, &resMap))

	sum := 0
	require.Equal(t, num, len(resMap["values"]))
	for _, item := range resMap["values"] {
		sum += item["value"]
	}
	require.Equal(t, expectedSum, sum)

	// Perform a query using the updated index in the schema.
	q2 := `
	{
		names(func: anyofterms(name, Mike)) {
			name
		}
	}`
	resMap = make(map[string][]map[string]int)
	_, err = testutil.RetryQuery(dg, q2)
	require.NoError(t, err)

	// Trying to perform a query using the address index should not work since that
	// predicate was deleted.
	q3 := `
	{
		addresses(func: anyofterms(address, Mike)) {
			address
		}
	}`
	resMap = make(map[string][]map[string]int)
	_, err = testutil.RetryBadQuery(dg, q3)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Attribute address is not indexed")
}

func waitForSnapshot(t *testing.T, prevSnapTs uint64) uint64 {
	snapPattern := `"snapshotTs":"([0-9]*)"`
	for {
		res, err := http.Get("http://" + testutil.GetSockAddrZeroHttp() + "/state")
		require.NoError(t, err)
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		require.NoError(t, err)

		regex, err := regexp.Compile(snapPattern)
		require.NoError(t, err)

		matches := regex.FindAllStringSubmatch(string(body), 1)
		if len(matches) == 0 {
			time.Sleep(time.Second)
			continue
		}

		snapshotTs, err := strconv.ParseUint(matches[0][1], 10, 64)
		require.NoError(t, err)
		if snapshotTs > prevSnapTs {
			return snapshotTs
		}

		time.Sleep(time.Second)
	}
}
