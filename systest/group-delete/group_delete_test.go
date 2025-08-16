//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/testutil"
)

func NodesSetup(t *testing.T, c *dgo.Dgraph) {
	ctx := context.Background()

	// Retry DropAll to make sure the nodes is up and running.
	var err error
	for i := 0; i < 3; i++ {
		if err = c.Alter(ctx, &api.Operation{DropAll: true}); err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}
	require.NoError(t, err, "error while dropping all the data")

	schema, err := os.ReadFile(`../data/goldendata.schema`)
	require.NoError(t, err)
	require.NoError(t, c.Alter(ctx, &api.Operation{Schema: string(schema)}))

	fp, err := os.Open(`../data/goldendata_export.rdf.gz`)
	require.NoError(t, err)
	defer fp.Close()

	gz, err := gzip.NewReader(fp)
	require.NoError(t, err)
	defer gz.Close()

	var (
		cnt int
		bb  bytes.Buffer
	)

	reader := bufio.NewReader(gz)
	for {
		b, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
		}
		bb.Write(b)
		cnt++
		if cnt%100 == 0 {
			_, err = c.NewTxn().Mutate(ctx, &api.Mutation{
				CommitNow: true,
				SetNquads: bb.Bytes(),
			})
			require.NoError(t, err)
			bb.Reset()
		}
	}
}

func doTestQuery(t *testing.T, c *dgo.Dgraph) {
	resp, err := c.NewTxn().Query(context.Background(), `
  {
  q(func:anyofterms(name@en, "good bad"), first: -5) {
    name@en
  }
  }`)
	require.NoError(t, err)

	testutil.CompareJSON(t, `
  {
    "q": [
      {
        "name@en": "Good Grief"
      },
      {
        "name@en": "Half Good Killer"
      },
      {
        "name@en": "Bad Friend"
      },
      {
        "name@en": "Ace of Spades: Bad Destiny"
      },
      {
        "name@en": "Bad Girls 6"
      }
    ]
  }`, string(resp.GetJson()))
}

func getError(rc io.ReadCloser) error {
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return errors.Wrapf(err, "while reading")
	}
	if bytes.Contains(b, []byte("Error")) {
		return errors.Errorf("%s", string(b))
	}
	return nil
}

func TestNodes(t *testing.T) {
	var dg *dgo.Dgraph
	var err error
	for i := 0; i < 3; i++ {
		dg, err = testutil.GetClientToGroup("1")
		if err == nil {
			break
		}
		time.Sleep(5 * time.Second)
	}
	require.NoError(t, err, "error while getting connection to group 1")

	NodesSetup(t, dg)

	state1, err := testutil.GetState()
	require.NoError(t, err)

	for pred := range state1.Groups["3"].Tablets {
		testutil.AssertMoveTablet(t, pred, 2)
		time.Sleep(time.Second)
	}

	state2, err := testutil.GetState()
	require.NoError(t, err)

	if len(state2.Groups["3"].Tablets) > 0 {
		t.Errorf("moving tablets failed")
	}

	groupNodes, err := testutil.GetNodesInGroup("3")
	require.NoError(t, err)
	nodeId, err := strconv.ParseUint(groupNodes[0], 10, 64)
	require.NoError(t, err)
	testutil.AssertRemoveNode(t, nodeId, 3)

	state2, err = testutil.GetState()
	require.NoError(t, err)

	group, ok := state2.Groups["3"]
	require.True(t, ok, "group 3 is removed")

	require.Equal(t, len(group.Members), int(0),
		fmt.Sprintf("Expected 0 members in group 3 but got %d", len(group.Members)))

	doTestQuery(t, dg)

	state1, err = testutil.GetState()
	require.NoError(t, err)

	for pred := range state1.Groups["2"].Tablets {
		moveUrl := fmt.Sprintf("http://"+testutil.GetSockAddrZeroHttp()+"/moveTablet?tablet=%s&group=1",
			url.QueryEscape(pred))
		resp, err := http.Get(moveUrl)
		require.NoError(t, err)
		require.NoError(t, getError(resp.Body))
		time.Sleep(time.Second)
	}

	state2, err = testutil.GetState()
	require.NoError(t, err)

	if len(state2.Groups["2"].Tablets) > 0 {
		t.Errorf("moving tablets failed")
	}

	groupNodes, err = testutil.GetNodesInGroup("2")
	require.NoError(t, err)
	resp, err := http.Get("http://" + testutil.GetSockAddrZeroHttp() + "/removeNode?group=2&id=" +
		groupNodes[0])
	require.NoError(t, err)
	require.NoError(t, getError(resp.Body))

	state2, err = testutil.GetState()
	require.NoError(t, err)

	group, ok = state2.Groups["2"]
	require.True(t, ok, "group 2 is removed")
	if len(group.Members) != 0 {
		t.Errorf("Expected 0 members in group 2 but got %d", len(group.Members))
	}

	doTestQuery(t, dg)
}
