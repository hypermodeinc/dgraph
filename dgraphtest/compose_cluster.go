/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package dgraphtest

import (
	"github.com/dgraph-io/dgo/v250"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/hypermodeinc/dgraph/v25/dgraphapi"
	"github.com/hypermodeinc/dgraph/v25/testutil"
)

type ComposeCluster struct{}

func NewComposeCluster() *ComposeCluster {
	return &ComposeCluster{}
}

func (c *ComposeCluster) Client() (*dgraphapi.GrpcClient, func(), error) {
	dg, err := dgo.NewClient(testutil.GetSockAddr(),
		dgo.WithGrpcOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return nil, nil, err
	}
	return &dgraphapi.GrpcClient{Dgraph: dg}, func() { dg.Close() }, nil
}

// HTTPClient creates an HTTP client
func (c *ComposeCluster) HTTPClient() (*dgraphapi.HTTPClient, error) {
	httpClient, err := dgraphapi.GetHttpClient(testutil.GetSockAddrHttp(), testutil.GetSockAddrZeroHttp())
	if err != nil {
		return nil, err
	}
	httpClient.HttpToken = &dgraphapi.HttpToken{}
	return httpClient, nil
}

func (c *ComposeCluster) AlphasHealth() ([]string, error) {
	return nil, errNotImplemented
}

func (c *ComposeCluster) AlphasLogs() ([]string, error) {
	return nil, errNotImplemented
}

func (c *ComposeCluster) AssignUids(client *dgo.Dgraph, num uint64) error {
	return testutil.AssignUids(num)
}

func (c *ComposeCluster) GetVersion() string {
	return localVersion
}

func (c *ComposeCluster) GetEncKeyPath() (string, error) {
	return "", errNotImplemented
}

// GetRepoDir returns the repositroty directory of the cluster
func (c *ComposeCluster) GetRepoDir() (string, error) {
	return "", errNotImplemented
}
