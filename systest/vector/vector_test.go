//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/dgraphapi"
	"github.com/hypermodeinc/dgraph/v25/dgraphtest"
	"github.com/hypermodeinc/dgraph/v25/x"
)

const (
	testSchemaWithoutIndex = `project_description_v: float32vector .`
	pred                   = "project_description_v"
)

var schemas = map[string]string{
	"hnsw":            `project_description_v: float32vector @index(hnsw(exponent: "5", metric: "euclidean")) .`,
	"partitionedhnsw": `project_description_v: float32vector @index(partitionedhnsw(numClusters: "100", partitionStratOpt: "kmeans", vectorDimention: "100", metric: "euclidean")) .`,
}

func testVectorQuery(t *testing.T, gc *dgraphapi.GrpcClient, vectors [][]float32, rdfs, pred string, topk int) {
	for i, vector := range vectors {
		triple := strings.Split(rdfs, "\n")[i]
		uid := strings.Split(triple, " ")[0]
		queriedVector, err := gc.QuerySingleVectorsUsingUid(uid, pred)
		require.NoError(t, err)
		require.Equal(t, vectors[i], queriedVector[0])

		similarVectors, err := gc.QueryMultipleVectorsUsingSimilarTo(vector, pred, topk)
		require.NoError(t, err)
		for _, similarVector := range similarVectors {
			require.Contains(t, vectors, similarVector)
		}
	}
}

func (vsuite *VectorTestSuite) TestVectorDropAll() {
	t := vsuite.T()
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace))

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.RootNamespace))

	numVectors := 100

	testVectorSimilarTo := func(vectors [][]float32) {
		for _, vector := range vectors {
			_, err := gc.QueryMultipleVectorsUsingSimilarTo(vector, pred, 100)
			require.ErrorContains(t, err, "is not indexed")
			break
		}
	}

	for i := 0; i < 10; i++ {
		require.NoError(t, gc.SetupSchema(vsuite.schema))
		rdfs, vectors := dgraphapi.GenerateRandomVectors(0, numVectors, 100, pred)
		mu := &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
		_, err = gc.Mutate(mu)
		require.NoError(t, err)

		query := `{
			vector(func: has(project_description_v)) {
				   count(uid)
				}
		}`
		result, err := gc.Query(query)
		require.NoError(t, err)
		require.JSONEq(t, fmt.Sprintf(`{"vector":[{"count":%v}]}`, numVectors), string(result.GetJson()))

		testVectorQuery(t, gc, vectors, rdfs, pred, numVectors)
		t.Log("dropping data \n")

		require.NoError(t, gc.DropAll())

		result, err = gc.Query(query)
		require.NoError(t, err)
		require.JSONEq(t, fmt.Sprintf(`{"vector":[{"count":%v}]}`, 0), string(result.GetJson()))
		testVectorSimilarTo(vectors)
	}
}

func (vsuite *VectorTestSuite) TestVectorSnapshot() {
	t := vsuite.T()
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(3).WithNumZeros(3).WithReplicas(3).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace))

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.RootNamespace))

	require.NoError(t, c.KillAlpha(1))

	hc, err = c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.RootNamespace))

	gc, cleanup, err = c.AlphaClient(0)
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace))

	require.NoError(t, gc.SetupSchema(vsuite.schema))

	prevSnapshotTs, err := hc.GetCurrentSnapshotTs(1)
	require.NoError(t, err)

	numVectors := 500
	rdfs, vectors := dgraphapi.GenerateRandomVectors(0, numVectors, 100, pred)
	mu := &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	query := `{
		vector(func: has(project_description_v)) {
			   count(uid)
			}
	}`

	result, err := gc.Query(query)
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"vector":[{"count":%v}]}`, numVectors), string(result.GetJson()))

	_, err = hc.WaitForSnapshot(1, prevSnapshotTs)
	require.NoError(t, err)

	require.NoError(t, c.StartAlpha(1))
	require.NoError(t, c.HealthCheck(false))

	time.Sleep(time.Second)

	gc, cleanup, err = c.AlphaClient(1)
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.Login(context.Background(), dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword))

	result, err = gc.Query(query)
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"vector":[{"count":%v}]}`, numVectors), string(result.GetJson()))

	testVectorQuery(t, gc, vectors, rdfs, pred, numVectors)
}

func (vsuite *VectorTestSuite) TestVectorDropNamespace() {
	t := vsuite.T()
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace))

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.RootNamespace))

	numVectors := 500
	for i := 0; i < 6; i++ {
		ns, err := hc.AddNamespace()
		require.NoError(t, err)
		require.NoError(t, gc.SetupSchema(vsuite.schema))
		rdfs, vectors := dgraphapi.GenerateRandomVectors(0, numVectors, 100, pred)
		mu := &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
		_, err = gc.Mutate(mu)
		require.NoError(t, err)

		query := `{
			vector(func: has(project_description_v)) {
				   count(uid)
				}
		}`

		result, err := gc.Query(query)
		require.NoError(t, err)
		require.JSONEq(t, fmt.Sprintf(`{"vector":[{"count":%v}]}`, numVectors), string(result.GetJson()))

		for _, vector := range vectors {
			similarVectors, err := gc.QueryMultipleVectorsUsingSimilarTo(vector, pred, numVectors)
			require.NoError(t, err)
			for _, similarVector := range similarVectors {
				require.Contains(t, vectors, similarVector)
			}
		}
		_, err = hc.DeleteNamespace(ns)
		require.NoError(t, err)
	}
}

func (vsuite *VectorTestSuite) TestVectorIndexRebuilding() {
	t := vsuite.T()
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace))

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.RootNamespace))

	require.NoError(t, gc.SetupSchema(vsuite.schema))

	numVectors := 1000
	rdfs, vectors := dgraphapi.GenerateRandomVectors(0, numVectors, 100, pred)
	mu := &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)
	query := `{
		vector(func: has(project_description_v)) {
			   count(uid)
			}
	}`

	result, err := gc.Query(query)
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"vector":[{"count":%v}]}`, numVectors), string(result.GetJson()))

	testVectorQuery(t, gc, vectors, rdfs, pred, numVectors)

	// drop index
	require.NoError(t, gc.SetupSchema(testSchemaWithoutIndex))

	time.Sleep(5 * time.Second)
	// rebuild index
	require.NoError(t, gc.SetupSchema(vsuite.schema))
	time.Sleep(5 * time.Second)

	result, err = gc.Query(query)
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"vector":[{"count":%v}]}`, numVectors), string(result.GetJson()))

	testVectorQuery(t, gc, vectors, rdfs, pred, numVectors)
}

func (vsuite *VectorTestSuite) TestVectorIndexOnVectorPredWithoutData() {
	t := vsuite.T()
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace))

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.RootNamespace))

	require.NoError(t, gc.SetupSchema(vsuite.schema))

	vector := []float32{1.0, 2.0, 3.0}
	_, err = gc.QueryMultipleVectorsUsingSimilarTo(vector, pred, 10)
	require.NoError(t, err)
}

func (vsuite *VectorTestSuite) TestVectorIndexDropPredicate() {
	t := vsuite.T()
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)

	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	defer cleanup()
	require.NoError(t, err)

	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace))

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.RootNamespace))

	require.NoError(t, gc.SetupSchema(vsuite.schema))
	numVectors := 1000

	// add vectors
	rdfs, vectors := dgraphapi.GenerateRandomVectors(0, numVectors, 100, pred)
	mu := &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	require.NoError(t, gc.SetupSchema(vsuite.schema))

	for _, vect := range vectors {
		similarVects, err := gc.QueryMultipleVectorsUsingSimilarTo(vect, pred, 2)
		require.NoError(t, err)
		require.Equal(t, 2, len(similarVects))
	}

	query := `{
		vector(func: has(project_description_v)) {
			   count(uid)
			}
	}`

	result, err := gc.Query(query)
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"vector":[{"count":%v}]}`, numVectors), string(result.GetJson()))

	// remove index from vector predicate
	require.NoError(t, gc.SetupSchema(testSchemaWithoutIndex))

	// drop predicate
	op := &api.Operation{
		DropAttr: pred,
	}
	require.NoError(t, gc.Alter(context.Background(), op))

	// generate random vectors
	rdfs, vectors = dgraphapi.GenerateRandomVectors(0, numVectors, 100, pred)
	mu = &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	// add index back
	require.NoError(t, gc.SetupSchema(vsuite.schema))

	result, err = gc.Query(query)
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"vector":[{"count":%v}]}`, numVectors), string(result.GetJson()))

	for _, vect := range vectors {
		similarVects, err := gc.QueryMultipleVectorsUsingSimilarTo(vect, pred, 100)
		require.NoError(t, err)
		require.Equal(t, 100, len(similarVects))
	}
}

func (vsuite *VectorTestSuite) TestVectorIndexWithoutSchema() {
	t := vsuite.T()
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)

	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	defer cleanup()
	require.NoError(t, err)

	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace))

	numVectors := 1000

	// add vectors
	rdfs, vectors := dgraphapi.GenerateRandomVectors(0, numVectors, 100, pred)
	mu := &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	require.NoError(t, gc.SetupSchema(vsuite.schema))

	for _, vect := range vectors {
		similarVects, err := gc.QueryMultipleVectorsUsingSimilarTo(vect, pred, 100)
		require.NoError(t, err)
		require.Equal(t, 100, len(similarVects))
	}

	query := `{
		vector(func: has(project_description_v)) {
			   count(uid)
			}
	}`

	result, err := gc.Query(query)
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"vector":[{"count":%v}]}`, numVectors), string(result.GetJson()))
}

func (vsuite *VectorTestSuite) TestVectorIndexWithoutSchemaWithoutIndex() {
	t := vsuite.T()
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)

	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	defer cleanup()
	require.NoError(t, err)

	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace))

	numVectors := 1000

	// add vectors
	rdfs, vectors := dgraphapi.GenerateRandomVectors(0, numVectors, 100, pred)
	mu := &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	require.NoError(t, gc.SetupSchema(vsuite.schema))

	for i, vect := range vectors {
		triple := strings.Split(rdfs, "\n")[i]
		uid := strings.Split(triple, " ")[0]
		queriedVector, err := gc.QuerySingleVectorsUsingUid(uid, pred)
		require.NoError(t, err)
		require.Equal(t, vect, queriedVector[0])
	}

	query := `{
		vector(func: has(project_description_v)) {
			   count(uid)
			}
	}`

	result, err := gc.Query(query)
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"vector":[{"count":%v}]}`, numVectors), string(result.GetJson()))
}

type VectorTestSuite struct {
	suite.Suite
	schema string
}

func TestVectorSuite(t *testing.T) {
	for _, schema := range schemas {
		var ssuite VectorTestSuite
		ssuite.schema = schema
		suite.Run(t, &ssuite)
		if t.Failed() {
			x.Panic(errors.New("vector tests failed"))
		}
	}
}
