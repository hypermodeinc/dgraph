//go:build !oss && integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/dgraphtest"
	"github.com/hypermodeinc/dgraph/v24/x"
	"github.com/stretchr/testify/require"
)

const (
	testSchema             = `project_description_v: float32vector @index(hnsw(exponent: "5", metric: "euclidean")) .`
	testSchemaWithoutIndex = `project_description_v: float32vector .`
	pred                   = "project_description_v"
)

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

func TestVectorDropAll(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	numVectors := 100

	testVectorSimilarTo := func(vectors [][]float32) {
		for _, vector := range vectors {
			_, err := gc.QueryMultipleVectorsUsingSimilarTo(vector, pred, 100)
			require.ErrorContains(t, err, "is not indexed")
			break
		}
	}

	for i := 0; i < 10; i++ {
		require.NoError(t, gc.SetupSchema(testSchema))
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

func TestVectorSnapshot(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(3).WithNumZeros(3).WithReplicas(3).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, c.KillAlpha(1))

	hc, err = c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	gc, cleanup, err = c.AlphaClient(0)
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.SetupSchema(testSchema))

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

func TestVectorDropNamespace(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	numVectors := 500
	for i := 0; i < 6; i++ {
		ns, err := hc.AddNamespace()
		require.NoError(t, err)
		require.NoError(t, gc.SetupSchema(testSchema))
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

func TestVectorIndexRebuilding(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.SetupSchema(testSchema))

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
	require.NoError(t, gc.SetupSchema(testSchema))
	time.Sleep(5 * time.Second)

	result, err = gc.Query(query)
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"vector":[{"count":%v}]}`, numVectors), string(result.GetJson()))

	testVectorQuery(t, gc, vectors, rdfs, pred, numVectors)
}

func TestVectorIndexOnVectorPredWithoutData(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.SetupSchema(testSchema))

	vector := []float32{1.0, 2.0, 3.0}
	_, err = gc.QueryMultipleVectorsUsingSimilarTo(vector, pred, 10)
	require.NoError(t, err)
}

func TestVectorIndexDropPredicate(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)

	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	defer cleanup()
	require.NoError(t, err)

	require.NoError(t, gc.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))

	hc, err := c.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.SetSchema(context.Background(), dgo.RootNamespace, testSchema))
	numVectors := 1000

	// add vectors
	rdfs, vectors := dgraphapi.GenerateRandomVectors(0, numVectors, 100, pred)
	mu := &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	require.NoError(t, gc.SetSchema(context.Background(), dgo.RootNamespace, testSchema))

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
	require.NoError(t, gc.SetSchema(context.Background(), dgo.RootNamespace, testSchemaWithoutIndex))

	// drop predicate
	require.NoError(t, gc.DropPredicate(context.Background(), dgo.RootNamespace, pred))

	// generate random vectors
	rdfs, vectors = dgraphapi.GenerateRandomVectors(0, numVectors, 100, pred)
	mu = &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	// add index back
	require.NoError(t, gc.SetSchema(context.Background(), dgo.RootNamespace, testSchema))

	result, err = gc.Query(query)
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"vector":[{"count":%v}]}`, numVectors), string(result.GetJson()))

	for _, vect := range vectors {
		similarVects, err := gc.QueryMultipleVectorsUsingSimilarTo(vect, pred, 100)
		require.NoError(t, err)
		require.Equal(t, 100, len(similarVects))
	}
}

func TestVectorIndexWithoutSchema(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)

	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	defer cleanup()
	require.NoError(t, err)

	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	numVectors := 1000

	// add vectors
	rdfs, vectors := dgraphapi.GenerateRandomVectors(0, numVectors, 100, pred)
	mu := &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	require.NoError(t, gc.SetupSchema(testSchema))

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

func TestVectorIndexWithoutSchemaWithoutIndex(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)

	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	gc, cleanup, err := c.Client()
	defer cleanup()
	require.NoError(t, err)

	require.NoError(t, gc.LoginIntoNamespace(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	numVectors := 1000

	// add vectors
	rdfs, vectors := dgraphapi.GenerateRandomVectors(0, numVectors, 100, pred)
	mu := &api.Mutation{SetNquads: []byte(rdfs), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	require.NoError(t, gc.SetupSchema(testSchemaWithoutIndex))

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
