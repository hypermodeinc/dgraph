//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package query

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/hypermodeinc/dgraph/v24/dgraphapi"

	"github.com/stretchr/testify/require"
)

type ProjectInput struct {
	Title  string    `json:"title"`
	TitleV []float32 `json:"title_v"`
}

const (
	graphQLVectorSchema = `
	 type Project  {
		 id: ID!
		 title: String!  @search(by: [exact])
		 title_v: [Float!] @embedding @search(by: ["hnsw(metric: %v, exponent: 4)"])
	 } `
)

func generateProjects(count int) []ProjectInput {
	var projects []ProjectInput
	for i := 0; i < count; i++ {
		title := generateUniqueRandomTitle(projects)
		titleV := generateRandomTitleV(5) // Assuming size is fixed at 5
		project := ProjectInput{
			Title:  title,
			TitleV: titleV,
		}
		projects = append(projects, project)
	}
	return projects
}

func isTitleExists(title string, existingTitles []ProjectInput) bool {
	for _, project := range existingTitles {
		if project.Title == title {
			return true
		}
	}
	return false
}

func generateUniqueRandomTitle(existingTitles []ProjectInput) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const titleLength = 10
	title := make([]byte, titleLength)
	for {
		for i := range title {
			title[i] = charset[rand.Intn(len(charset))]
		}
		titleStr := string(title)
		if !isTitleExists(titleStr, existingTitles) {
			return titleStr
		}
	}
}

func generateRandomTitleV(size int) []float32 {
	var titleV []float32
	for i := 0; i < size; i++ {
		value := rand.Float32()
		titleV = append(titleV, value)
	}
	return titleV
}

func addProject(t *testing.T, hc *dgraphapi.HTTPClient, project ProjectInput) {
	query := `
	  mutation addProject($project: AddProjectInput!) {
		  addProject(input: [$project]) {
			  project {
					title
					title_v
			  }
		  }
	  }`

	params := dgraphapi.GraphQLParams{
		Query:     query,
		Variables: map[string]interface{}{"project": project},
	}

	_, err := hc.RunGraphqlQuery(params, false)
	require.NoError(t, err)
}

func queryProjectUsingTitle(t *testing.T, hc *dgraphapi.HTTPClient, title string) ProjectInput {
	query := ` query QueryProject($title: String!) {
		 queryProject(filter: { title: { eq: $title } }) {
		   title
		   title_v
		 }
	   }`

	params := dgraphapi.GraphQLParams{
		Query:     query,
		Variables: map[string]interface{}{"title": title},
	}
	response, err := hc.RunGraphqlQuery(params, false)
	require.NoError(t, err)
	type QueryResult struct {
		QueryProject []ProjectInput `json:"queryProject"`
	}
	var resp QueryResult
	err = json.Unmarshal([]byte(string(response)), &resp)
	require.NoError(t, err)

	return resp.QueryProject[0]
}

func queryProjectsSimilarByEmbedding(t *testing.T, hc *dgraphapi.HTTPClient, vector []float32, topk int) []ProjectInput {
	// query similar project by embedding
	queryProduct := `query QuerySimilarProjectByEmbedding($by: ProjectEmbedding!, $topK: Int!, $vector: [Float!]!) {
		 querySimilarProjectByEmbedding(by: $by, topK: $topK, vector: $vector) {
		   title
		   title_v
		 }
	   }

	 `

	params := dgraphapi.GraphQLParams{
		Query: queryProduct,
		Variables: map[string]interface{}{
			"by":     "title_v",
			"topK":   topk,
			"vector": vector,
		}}
	response, err := hc.RunGraphqlQuery(params, false)
	require.NoError(t, err)
	type QueryResult struct {
		QueryProject []ProjectInput `json:"querySimilarProjectByEmbedding"`
	}
	var resp QueryResult
	err = json.Unmarshal([]byte(string(response)), &resp)
	require.NoError(t, err)

	return resp.QueryProject
}

func TestVectorGraphQLAddVectorPredicate(t *testing.T) {
	require.NoError(t, client.DropAll(context.Background()))

	hc, err := dc.HTTPClient()
	require.NoError(t, err)
	hc.LoginIntoNamespace("groot", "password", 0)
	// add schema
	require.NoError(t, hc.UpdateGQLSchema(fmt.Sprintf(graphQLVectorSchema, "euclidean")))
}

func TestVectorSchema(t *testing.T) {
	require.NoError(t, client.DropAll(context.Background()))

	hc, err := dc.HTTPClient()
	require.NoError(t, err)
	hc.LoginIntoNamespace("groot", "password", 0)

	schema := `type Project  {
		 id: ID!
		 title: String!  @search(by: [exact])
		 title_v: [Float!]
	 }`

	// add schema
	require.NoError(t, hc.UpdateGQLSchema(schema))
	require.Error(t, hc.UpdateGQLSchema(fmt.Sprintf(graphQLVectorSchema, "euclidean")))
}

func TestVectorGraphQlEuclideanIndexMutationAndQuery(t *testing.T) {
	require.NoError(t, client.DropAll(context.Background()))
	hc, err := dc.HTTPClient()
	require.NoError(t, err)
	hc.LoginIntoNamespace("groot", "password", 0)

	schema := fmt.Sprintf(graphQLVectorSchema, "euclidean")
	// add schema
	require.NoError(t, hc.UpdateGQLSchema(schema))
	testVectorGraphQlMutationAndQuery(t, hc)
}

func TestVectorGraphQlCosineIndexMutationAndQuery(t *testing.T) {
	require.NoError(t, client.DropAll(context.Background()))
	hc, err := dc.HTTPClient()
	require.NoError(t, err)
	hc.LoginIntoNamespace("groot", "password", 0)

	schema := fmt.Sprintf(graphQLVectorSchema, "cosine")
	// add schema
	require.NoError(t, hc.UpdateGQLSchema(schema))
	testVectorGraphQlMutationAndQuery(t, hc)
}

func TestVectorGraphQlDotProductIndexMutationAndQuery(t *testing.T) {
	require.NoError(t, client.DropAll(context.Background()))
	hc, err := dc.HTTPClient()
	require.NoError(t, err)
	hc.LoginIntoNamespace("groot", "password", 0)

	schema := fmt.Sprintf(graphQLVectorSchema, "dotproduct")
	// add schema
	require.NoError(t, hc.UpdateGQLSchema(schema))
	testVectorGraphQlMutationAndQuery(t, hc)
}

func testVectorGraphQlMutationAndQuery(t *testing.T, hc *dgraphapi.HTTPClient) {
	var vectors [][]float32
	numProjects := 100
	projects := generateProjects(numProjects)
	for _, project := range projects {
		vectors = append(vectors, project.TitleV)
		addProject(t, hc, project)
	}

	for _, project := range projects {
		p := queryProjectUsingTitle(t, hc, project.Title)
		require.Equal(t, project.Title, p.Title)
		require.Equal(t, project.TitleV, p.TitleV)
	}

	for _, project := range projects {
		p := queryProjectUsingTitle(t, hc, project.Title)
		require.Equal(t, project.Title, p.Title)
		require.Equal(t, project.TitleV, p.TitleV)
	}

	// query similar project by embedding
	for _, project := range projects {
		similarProjects := queryProjectsSimilarByEmbedding(t, hc, project.TitleV, numProjects)
		for _, similarVec := range similarProjects {
			require.Contains(t, vectors, similarVec.TitleV)
		}
	}
}
