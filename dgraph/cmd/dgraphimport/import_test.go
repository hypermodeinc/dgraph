//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */
package dgraphimport

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/dgraphtest"
	"github.com/hypermodeinc/dgraph/v24/systest/1million/common"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const expectedSchema = `{
	"schema": [
		{"predicate":"actor.film","type":"uid","count":true,"list":true},
		{"predicate":"country","type":"uid","reverse":true,"list":true},
		{"predicate":"cut.note","type":"string","lang":true},
		{"predicate":"dgraph.user.group","type":"uid","reverse":true,"list":true},
		{"predicate":"director.film","type":"uid","reverse":true,"count":true,"list":true},
		{"predicate":"email","type":"string","index":true,"tokenizer":["exact"],"upsert":true},
		{"predicate":"genre","type":"uid","reverse":true,"count":true,"list":true},
		{"predicate":"initial_release_date","type":"datetime","index":true,"tokenizer":["year"]},
		{"predicate":"loc","type":"geo","index":true,"tokenizer":["geo"]},
		{"predicate":"name","type":"string","index":true,"tokenizer":["hash","term","trigram","fulltext"],"lang":true},
		{"predicate":"performance.actor","type":"uid","list":true},
		{"predicate":"performance.character","type":"uid","list":true},
		{"predicate":"performance.character_note","type":"string","lang":true},
		{"predicate":"performance.film","type":"uid","list":true},
		{"predicate":"rated","type":"uid","reverse":true,"list":true},
		{"predicate":"rating","type":"uid","reverse":true,"list":true},
		{"predicate":"starring","type":"uid","count":true,"list":true},
		{"predicate":"tagline","type":"string","lang":true},
		{"predicate":"xid","type":"string","index":true,"tokenizer":["hash"]}
	]
}`

func TestScanPDirs(t *testing.T) {
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

			got, err := scanPDirs(tt.basePath)
			if (err != nil) != tt.wantErr {
				t.Errorf("scanPDirs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("scanPDirs() = %v, want %v", got, tt.want)
			}

			// Clean up test data directories
			err = os.RemoveAll(tt.basePath)
			require.NoError(t, err)
		})
	}
}

func TestStartSnapshotStreamForSingleNode(t *testing.T) {
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

	pubPort, err := c.GetAlphaGrpcPublicPort()
	require.NoError(t, err)
	client, err := NewClient(dgraphtest.GetLocalHostUrl(pubPort, ""), grpc.WithTransportCredentials(insecure.NewCredentials()))

	require.NoError(t, err)

	resp, err := client.StartSnapshotStream(context.Background())
	require.NoError(t, err)

	require.Equal(t, len(resp.Groups), 1)
	require.Equal(t, resp.Groups, []uint32{1})

	_, err = gc.Query("schema{}")
	require.Error(t, err)
	require.ErrorContains(t, err, "the server is in draining mode")
}

func TestStartSnapshotStreamForHA(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(3).WithNumZeros(1).WithReplicas(3)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	pubPort, err := c.GetAlphaGrpcPublicPort()
	require.NoError(t, err)
	client, err := NewClient(dgraphtest.GetLocalHostUrl(pubPort, ""), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	resp, err := client.StartSnapshotStream(context.Background())
	require.NoError(t, err)

	require.Equal(t, len(resp.Groups), 1)
	require.Equal(t, resp.Groups, []uint32{1})

	for i := 0; i <= 2; i++ {
		gc, cleanup, err := c.AlphaClient(i)
		require.NoError(t, err)
		defer cleanup()
		_, err = gc.Query("schema{}")
		require.Error(t, err)
		require.ErrorContains(t, err, "the server is in draining mode")
	}
}

func TestStartSnapshotStreamForHASharded(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(6).WithNumZeros(1).WithReplicas(3)
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	defer func() { c.Cleanup(t.Failed()) }()
	require.NoError(t, c.Start())

	pubPort, err := c.GetAlphaGrpcPublicPort()
	require.NoError(t, err)
	client, err := NewClient(dgraphtest.GetLocalHostUrl(pubPort, ""),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	resp, err := client.StartSnapshotStream(context.Background())
	require.NoError(t, err)
	require.Equal(t, len(resp.Groups), 2)

	for i := 0; i < 6; i++ {
		gc, cleanup, err := c.AlphaClient(int(i))
		require.NoError(t, err)
		defer cleanup()
		_, err = gc.Query("schema{}")
		require.Error(t, err)
		require.ErrorContains(t, err, "the server is in draining mode")
	}
}

// TestImportApis tests import functionality with different cluster configurations
func TestImportApis(t *testing.T) {
	tests := []struct {
		name           string
		bulkAlphas     int
		targetAlphas   int
		replicasFactor int
	}{
		{"SingleGroupSingleAlpha", 1, 1, 1},
		{"TwoGroupsSingleAlpha", 2, 2, 1},
		{"ThreeGroupsSingleAlpha", 3, 3, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runImportTest(t, tt.bulkAlphas, tt.targetAlphas, tt.replicasFactor)
		})
	}
}

func runImportTest(t *testing.T, bulkAlphas, targetAlphas, replicasFactor int) {
	bulkCluster, baseDir := setupBulkCluster(t, bulkAlphas)
	defer func() { bulkCluster.Cleanup(t.Failed()) }()

	targetCluster, gc, gcCleanup := setupTargetCluster(t, targetAlphas, replicasFactor)
	defer func() { targetCluster.Cleanup(t.Failed()) }()
	defer gcCleanup()

	schemaBefore, err := gc.Query("schema{}")
	require.NoError(t, err)
	fmt.Println("Before streaming schema ==========> \n\n", string(schemaBefore.Json))

	importClient, err := createImportClient(t, targetCluster)
	require.NoError(t, err)

	resp, err := importClient.StartSnapshotStream(context.Background())
	require.NoError(t, err)

	_, err = gc.Query("schema{}")
	require.Error(t, err)
	require.ErrorContains(t, err, "the server is in draining mode")

	outDir := filepath.Join(baseDir, "out")
	err = importClient.SendSnapshot(context.Background(), outDir, resp.Groups)
	require.NoError(t, err)

	verifyImportResults(t, gc)
}

// setupBulkCluster creates and configures a cluster for bulk loading data
func setupBulkCluster(t *testing.T, numAlphas int) (*dgraphtest.LocalCluster, string) {
	baseDir := t.TempDir()
	bulkConf := dgraphtest.NewClusterConfig().
		WithNumAlphas(numAlphas).
		WithNumZeros(1).
		WithReplicas(1).
		WithBulkLoadOutDir(t.TempDir())

	cluster, err := dgraphtest.NewLocalCluster(bulkConf)
	require.NoError(t, err)

	// Download and prepare data files
	dataPath, err := dgraphtest.DownloadDataFiles()
	require.NoError(t, err)
	require.NoError(t, cluster.StartZero(0))

	// Perform bulk load
	schemaFile := filepath.Join(dataPath, "1million.schema")
	rdfFile := filepath.Join(dataPath, "1million.rdf.gz")
	opts := dgraphtest.BulkOpts{
		DataFiles:   []string{rdfFile},
		SchemaFiles: []string{schemaFile},
		OutDir:      filepath.Join(baseDir, "out"),
	}
	require.NoError(t, cluster.BulkLoad(opts))

	return cluster, baseDir
}

// setupTargetCluster creates and starts a cluster that will receive the imported data
func setupTargetCluster(t *testing.T, numAlphas, replicasFactor int) (*dgraphtest.LocalCluster, *dgraphapi.GrpcClient, func()) {
	conf := dgraphtest.NewClusterConfig().
		WithNumAlphas(numAlphas).
		WithNumZeros(1).
		WithReplicas(replicasFactor)

	cluster, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	require.NoError(t, cluster.Start())

	gc, cleanup, err := cluster.Client()
	require.NoError(t, err)

	// Return cluster and client (cleanup will be handled by the caller)
	return cluster, gc, cleanup
}

// createImportClient creates a new client for import operations
func createImportClient(t *testing.T, cluster *dgraphtest.LocalCluster) (*Client, error) {
	pubPort, err := cluster.GetAlphaGrpcPublicPort()
	require.NoError(t, err)

	return NewClient(dgraphtest.GetLocalHostUrl(pubPort, ""),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// verifyImportResults validates the result of an import operation
func verifyImportResults(t *testing.T, gc *dgraphapi.GrpcClient) {
	// Check schema after streaming process
	schemaResp, err := gc.Query("schema{}")
	require.NoError(t, err)
	// Compare the schema response with the expected schema
	var actualSchema, expectedSchemaObj map[string]interface{}
	require.NoError(t, json.Unmarshal(schemaResp.Json, &actualSchema))
	require.NoError(t, json.Unmarshal([]byte(expectedSchema), &expectedSchemaObj))

	// Check if the actual schema contains all the predicates from expected schema
	actualPredicates := getPredicateMap(actualSchema)
	expectedPredicates := getPredicateMap(expectedSchemaObj)

	for predName, predDetails := range expectedPredicates {
		actualPred, exists := actualPredicates[predName]
		require.True(t, exists, "Predicate '%s' not found in actual schema", predName)
		require.Equal(t, predDetails, actualPred, "Predicate '%s' details don't match", predName)
	}

	for _, tt := range common.OneMillionTCs {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		resp, err := gc.Query(tt.Query)
		cancel()

		if ctx.Err() == context.DeadlineExceeded {
			t.Fatal("aborting test due to query timeout")
		}
		require.NoError(t, err)
		dgraphapi.CompareJSON(tt.Resp, string(resp.Json))
	}
}

// getPredicateMap extracts predicates from schema into a map for easier comparison
func getPredicateMap(schema map[string]interface{}) map[string]interface{} {
	predicatesMap := make(map[string]interface{})
	predicates, ok := schema["schema"].([]interface{})
	if !ok {
		return predicatesMap
	}

	for _, pred := range predicates {
		predMap, ok := pred.(map[string]interface{})
		if !ok {
			continue
		}
		name, ok := predMap["predicate"].(string)
		if !ok {
			continue
		}
		predicatesMap[name] = predMap
	}

	return predicatesMap
}
