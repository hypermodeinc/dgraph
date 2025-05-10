//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package zero

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/conn"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/testutil"
)

func TestRemoveNode(t *testing.T) {
	server := &Server{
		state: &pb.MembershipState{
			Groups: map[uint32]*pb.Group{1: {Members: map[uint64]*pb.Member{}}},
		},
	}
	_, err := server.RemoveNode(context.TODO(), &pb.RemoveNodeRequest{NodeId: 3, GroupId: 1})
	require.Error(t, err)
	_, err = server.RemoveNode(context.TODO(), &pb.RemoveNodeRequest{NodeId: 1, GroupId: 2})
	require.Error(t, err)
}

func TestIdLeaseOverflow(t *testing.T) {
	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	con, err := grpc.NewClient(testutil.SockAddrZero, dialOpts...)
	require.NoError(t, err)
	zc := pb.NewZeroClient(con)

	_, err = zc.AssignIds(context.Background(), &pb.Num{Val: 100, Type: pb.Num_UID})
	require.NoError(t, err)

	_, err = zc.AssignIds(context.Background(), &pb.Num{Val: math.MaxUint64 - 10, Type: pb.Num_UID})
	require.Error(t, err)
	require.Contains(t, err.Error(), "limit has reached")
}

func TestIdBump(t *testing.T) {
	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	con, err := grpc.NewClient(testutil.SockAddrZero, dialOpts...)
	require.NoError(t, err)
	zc := pb.NewZeroClient(con)

	ctx := context.Background()
	res, err := zc.AssignIds(ctx, &pb.Num{Val: 10, Type: pb.Num_UID})
	require.NoError(t, err)
	require.Equal(t, uint64(10), res.GetEndId()-res.GetStartId()+1)

	// Next assignment's startId should be greater than 10.
	res, err = zc.AssignIds(ctx, &pb.Num{Val: 50, Type: pb.Num_UID})
	require.NoError(t, err)
	require.Greater(t, res.GetStartId(), uint64(10))
	require.Equal(t, uint64(50), res.GetEndId()-res.GetStartId()+1)

	bumpTo := res.GetEndId() + 100000

	// Bump the lease to (last result + 100000).
	_, err = zc.AssignIds(ctx, &pb.Num{Val: bumpTo, Type: pb.Num_UID, Bump: true})
	require.NoError(t, err)

	// Next assignment's startId should be greater than bumpTo.
	res, err = zc.AssignIds(ctx, &pb.Num{Val: 10, Type: pb.Num_UID})
	require.NoError(t, err)
	require.Greater(t, res.GetStartId(), bumpTo)
	require.Equal(t, uint64(10), res.GetEndId()-res.GetStartId()+1)

	// If bump request is less than maxLease, then it should result in no-op.
	_, err = zc.AssignIds(ctx, &pb.Num{Val: 10, Type: pb.Num_UID, Bump: true})
	require.Contains(t, err.Error(), "Nothing to be leased")
}

func TestProposalKey(t *testing.T) {
	id := uint64(2)
	node := &node{Node: &conn.Node{Id: id}, ctx: context.Background(), closer: z.NewCloser(1)}
	require.NoError(t, node.initProposalKey(node.Id))

	pkey := proposalKey
	nodeIdFromKey := proposalKey >> 48
	require.Equal(t, id, nodeIdFromKey, "id extracted from proposal key is not equal to initial value")

	valueOf48thBit := int(pkey & (1 << 48))
	require.Equal(t, 0, valueOf48thBit, "48th bit is not set to zero on initialisation")

	node.uniqueKey()
	require.Equal(t, pkey+1, proposalKey, "proposal key should increment by 1 at each call of unique key")

	uniqueKeys := make(map[uint64]struct{})
	for i := 0; i < 10; i++ {
		node.uniqueKey()
		uniqueKeys[proposalKey] = struct{}{}
	}
	require.Equal(t, len(uniqueKeys), 10, "each iteration should create unique key")
}

func TestZeroHealth(t *testing.T) {
	client := http.Client{Timeout: 3 * time.Second}
	u := &url.URL{
		Scheme: "http",
		Host:   testutil.ContainerAddr("zero1", 6080),
		Path:   "health",
	}

	// JSON format
	req, err := http.NewRequest("GET", u.String(), nil)
	require.NoError(t, err)

	req.Header.Add("Accept", `application/json`)
	start := time.Now().Unix()
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var r map[string]interface{}
	err = json.Unmarshal(body, &r)
	require.NoError(t, err)

	require.Equal(t, "zero", r["instance"].(string))
	require.Equal(t, "zero1:5080", r["address"].(string))
	require.Equal(t, "healthy", r["status"].(string))
	require.NotEqual(t, 0, len(r["version"].(string)))
	require.Greater(t, r["uptime"].(float64), 0.0)
	require.GreaterOrEqual(t, int64(r["lastEcho"].(float64)), start)

	// String format
	req, err = http.NewRequest("GET", u.String(), nil)
	require.NoError(t, err)

	resp, err = client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, string(body), "OK")
}
