//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */
package edgraph

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/dgraphtest"
	"github.com/hypermodeinc/dgraph/v24/x"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestGetPDiPdirrectories(t *testing.T) {
	tests := []struct {
		name     string
		basePath string
		want     map[uint32]string
		wantErr  bool
	}{
		{
			name:     "valid directory structure",
			basePath: "testdata/valid",
			want: map[uint32]string{
				1: "testdata/valid/1/p",
				2: "testdata/valid/2/p",
			},
			wantErr: false,
		},
		{
			name:     "invalid directory structure",
			basePath: "testdata/invalid",
			want:     nil,
			wantErr:  true,
		},
		{
			name:     "non-existent directory",
			basePath: "testdata/non-existent",
			want:     nil,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test data directories
			if tt.name == "valid directory structure" {
				err := os.MkdirAll(tt.basePath+"/1/p", 0755)
				require.NoError(t, err)
				err = os.MkdirAll(tt.basePath+"/2/p", 0755)
				require.NoError(t, err)
			}

			got, err := getPDiPdirrectories(tt.basePath)
			if (err != nil) != tt.wantErr {
				t.Errorf("getPDiPdirrectories() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getPDiPdirrectories() = %v, want %v", got, tt.want)
			}

			// Clean up test data directories
			err = os.RemoveAll(tt.basePath)
			require.NoError(t, err)
		})
	}
}

func TestInitiateSnapshotStreamForSingleNode(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).
		WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.Login(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	pubPort, err := c.GetAlphaGrpcPublicPort()
	require.NoError(t, err)
	client, err := NewImportClient(dgraphtest.GetLocalHostUrl(pubPort, ""), grpc.WithTransportCredentials(insecure.NewCredentials()))

	require.NoError(t, err)

	resp, err := client.InitiateSnapShotStream(context.Background())
	require.NoError(t, err)

	require.Equal(t, `leader_alphas:{key:1  value:"localhost:9080"}`, resp.String())

	_, err = gc.Query("schema{}")
	require.Error(t, err)
	require.ErrorContains(t, err, "the server is in draining mode")
}

func TestInitiateSnapshotStreamForHA(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(3).WithNumZeros(1).WithReplicas(3)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	state, err := hc.GetAlphaState()
	require.NoError(t, err)

	pubPort, err := c.GetAlphaGrpcPublicPort()
	require.NoError(t, err)
	client, err := NewImportClient(dgraphtest.GetLocalHostUrl(pubPort, ""), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	resp, err := client.InitiateSnapShotStream(context.Background())
	require.NoError(t, err)

	for _, group := range state.Groups {
		for _, node := range group.Members {
			if node.Leader {
				require.Equal(t, resp.LeaderAlphas[node.GroupId], node.GrpcAddr)
			}
		}
	}

	for i := 0; i <= 2; i++ {
		gc, cleanup, err := c.AlphaClient(i)
		require.NoError(t, err)
		defer cleanup()
		_, err = gc.Query("schema{}")
		require.Error(t, err)
		require.ErrorContains(t, err, "the server is in draining mode")
	}
}

func TestInitiateSnapshotStreamForHASharded(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(6).WithNumZeros(1).WithReplicas(3)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	hc, err := c.HTTPClient()
	require.NoError(t, err)

	state, err := hc.GetAlphaState()
	require.NoError(t, err)
	pubPort, err := c.GetAlphaGrpcPublicPort()
	require.NoError(t, err)
	client, err := NewImportClient(dgraphtest.GetLocalHostUrl(pubPort, ""),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	resp, err := client.InitiateSnapShotStream(context.Background())
	require.NoError(t, err)
	for _, group := range state.Groups {
		for _, node := range group.Members {
			if node.Leader {
				require.Equal(t, resp.LeaderAlphas[node.GroupId], node.GrpcAddr)
			}
		}
	}

	for i := 0; i < 6; i++ {
		gc, cleanup, err := c.AlphaClient(int(i))
		require.NoError(t, err)
		defer cleanup()
		_, err = gc.Query("schema{}")
		require.Error(t, err)
		require.ErrorContains(t, err, "the server is in draining mode")
	}
}

func TestImportApis(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	// check current leader of cluster
	state, err := hc.GetAlphaState()
	require.NoError(t, err)

	gc, cleanup, err := c.Client()
	require.NoError(t, err)
	defer cleanup()
	// check schema before streaming process
	schemaresp, err := gc.Query("schema{}")
	require.NoError(t, err)

	fmt.Println("before streaming  schema==========>", string(schemaresp.Json))

	pubPort, err := c.GetAlphaGrpcPublicPort()
	require.NoError(t, err)

	// create a import client
	importClient, err := NewImportClient(dgraphtest.GetLocalHostUrl(pubPort, ""),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	// initiating snapshot
	resp, err := importClient.InitiateSnapShotStream(context.Background())
	require.NoError(t, err)

	for _, group := range state.Groups {
		for _, node := range group.Members {
			if node.Leader {
				require.Equal(t, resp.LeaderAlphas[node.GroupId], node.GrpcAddr)
			}
		}
	}

	_, err = gc.Query("schema{}")
	require.Error(t, err)
	require.ErrorContains(t, err, "the server is in draining mode")

	// get public port of alpha because we are using docker here
	pub, err := c.GetAlphaGrpcPublicPort()
	require.NoError(t, err)

	url := dgraphtest.GetLocalHostUrl(pub, "")

	resp.LeaderAlphas[1] = url

	// start streaming sp directory
	err = importClient.StreamSnapshot(context.Background(), "/home/shiva/workspace/dgraph-work/benchmarks/data/out", resp.LeaderAlphas)
	require.NoError(t, err)

	// require.NoError(t, c.StopAlpha(0))
	// require.NoError(t, c.StartAlpha(0))
	// require.NoError(t, c.HealthCheck(false))

	gc, cleanup, err = c.Client()
	require.NoError(t, err)
	defer cleanup()
	schemaresp, err = gc.Query("schema{}")
	require.NoError(t, err)

	fmt.Println("after streaming==========>", string(schemaresp.Json))
}
