/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package dgraphtest

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/pkg/errors"
	"google.golang.org/grpc"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
)

type DCloudCluster struct {
	url   string
	token string
	acl   bool
}

func NewDCloudCluster() (*DCloudCluster, error) {
	url := os.Getenv("TEST_DGRAPH_CLOUD_CLUSTER_URL")
	token := os.Getenv("TEST_DGRAPH_CLOUD_CLUSTER_TOKEN")
	if url == "" || token == "" {
		return nil, errors.New("cloud cluster params needed in env")
	}

	// We assume ACLs are enabled by default
	aclStr := os.Getenv("TEST_DGRAPH_CLOUD_ACL")
	acl := true
	if aclStr != "" {
		var err error
		acl, err = strconv.ParseBool(aclStr)
		if err != nil {
			return nil, errors.Wrap(err, "error parsing env var TEST_DGRAPH_CLOUD_ACL")
		}
	}

	c := &DCloudCluster{url: url, token: token, acl: acl}
	if err := c.init(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *DCloudCluster) init() error {
	_, cleanup, err := c.Client()
	if err != nil {
		return err
	}
	defer cleanup()
	return nil
}

func (c *DCloudCluster) Client() (*dgraphapi.GrpcClient, func(), error) {
	var conns []*grpc.ClientConn
	conn, err := dgo.DialCloud(c.url, c.token)
	if err != nil {
		return nil, nil, errors.Wrap(err, "error creating dgraph client")
	}
	conns = append(conns, conn)

	cleanup := func() {
		for _, conn := range conns {
			if err := conn.Close(); err != nil {
				log.Printf("[WARNING] error closing connection: %v", err)
			}
		}
	}
	client := dgo.NewDgraphClient(api.NewDgraphClient(conn))
	return &dgraphapi.GrpcClient{Dgraph: client}, cleanup, nil
}

func (c *DCloudCluster) HTTPClient() (*dgraphapi.HTTPClient, error) {
	return nil, errNotImplemented
}

func (c *DCloudCluster) AlphasHealth() ([]string, error) {
	return nil, errNotImplemented
}

// AssignUids moves the max assigned UIDs by the given number.
// Note that we this performs dropall after moving the max assigned.
func (c1 *DCloudCluster) AssignUids(client *dgo.Dgraph, num uint64) error {
	// in Dgraph cloud, we can't talk to zero. Therefore, what we instead do
	// is keep doing mutations until the cluster has assigned those many new UIDs

	genData := func() []byte {
		var rdfs bytes.Buffer
		_, _ = rdfs.WriteString("_:root <test_cloud> \"root\" .\n")
		for i := range 1000 {
			rdfs.WriteString(fmt.Sprintf("_:%v <test_cloud> \"0\" .\n", i))
		}
		return rdfs.Bytes()
	}

	var prev uint64
	for i := uint64(0); i < num; {
		log.Printf("[INFO] performing mutation for AssignUID: assigned %v UIDs", i)
		mu := &api.Mutation{SetNquads: genData(), CommitNow: true}

		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		resp, err := client.NewTxn().Mutate(ctx, mu)
		cancel()
		if err != nil {
			return errors.Wrap(err, "error in mutation during AssignUID")
		}

		var max uint64
		for _, uidStr := range resp.Uids {
			uid, err := strconv.ParseUint(uidStr, 0, 64)
			if err != nil {
				return errors.Wrap(err, "error parsing mutation resp during AssignUID")
			}
			if uid > max {
				max = uid
			}
		}

		if prev == 0 {
			i = 1000
			prev = max
			continue
		}
		if max-prev == 0 {
			return errors.New("mutations did not create new UIDs during AssignUID")
		}

		i += max - prev
		prev = max
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if err := client.DropAll(ctx); err != nil {
		return errors.Wrap(err, "error in DropAll during AssignUID")
	}

	return nil
}

func (c *DCloudCluster) GetVersion() string {
	return localVersion
}

// GetRepoDir returns the repositroty directory of the cluster
func (c *DCloudCluster) GetRepoDir() (string, error) {
	return "", errNotImplemented
}

func (c *DCloudCluster) AlphasLogs() ([]string, error) {
	return nil, errNotImplemented
}

func (c *DCloudCluster) GetEncKeyPath() (string, error) {
	return "", errNotImplemented
}
