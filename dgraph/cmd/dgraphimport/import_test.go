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
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hypermodeinc/dgraph/v25/dgraphapi"
	"github.com/hypermodeinc/dgraph/v25/dgraphtest"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/systest/1million/common"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
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

func TestDrainModeAfterStartSnapshotStream(t *testing.T) {
	tests := []struct {
		name        string
		numAlphas   int
		numZeros    int
		replicas    int
		expectedNum int
	}{
		{"SingleNode", 1, 1, 1, 1},
		{"HACluster", 3, 1, 3, 1},
		{"HASharded", 9, 3, 3, 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := dgraphtest.NewClusterConfig().WithNumAlphas(tt.numAlphas).WithNumZeros(tt.numZeros).WithReplicas(tt.replicas)
			c, err := dgraphtest.NewLocalCluster(conf)
			require.NoError(t, err)
			defer func() { c.Cleanup(t.Failed()) }()
			require.NoError(t, c.Start())

			url, err := c.GetAlphaGrpcEndpoint(0)
			require.NoError(t, err)
			dc, err := newClient(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)

			resp, err := startPDirStream(context.Background(), dc)
			require.NoError(t, err)

			require.Equal(t, tt.expectedNum, len(resp.Groups))

			for i := 0; i < tt.numAlphas; i++ {
				gc, cleanup, err := c.AlphaClient(i)
				require.NoError(t, err)
				defer cleanup()
				_, err = gc.Query("schema{}")
				require.Error(t, err)
				require.ErrorContains(t, err, "the server is in draining mode")
			}
		})
	}
}

// TestImportApis tests import functionality with different cluster configurations
func TestImportApis(t *testing.T) {
	tests := []struct {
		name             string
		bulkAlphas       int  // Number of alphas in source cluster
		targetAlphas     int  // Number of alphas in target cluster
		replicasFactor   int  // Number of replicas for each group
		downAlphas       int  // Number of alphas to be shutdown
		negativeTestCase bool // True if this is an expected failure case
		description      string
		err              string
		waitForSnapshot  bool
	}{
		{
			name:             "SingleGroupShutTwoAlphasPerGroup",
			bulkAlphas:       1,
			targetAlphas:     3,
			replicasFactor:   3,
			downAlphas:       2,
			negativeTestCase: true,
			description:      "Single group with 3 alphas, shutdown 2 alphas",
			err:              "failed to initiate pdir stream",
		},
		{
			name:             "TwoGroupShutTwoAlphasPerGroup",
			bulkAlphas:       2,
			targetAlphas:     6,
			replicasFactor:   3,
			downAlphas:       2,
			negativeTestCase: true,
			description:      "Two groups with 3 alphas each, shutdown 2 alphas per group",
			err:              "failed to initiate pdir stream",
		},
		{
			name:             "TwoGroupShutTwoAlphasPerGroupNoPDir",
			bulkAlphas:       1,
			targetAlphas:     6,
			replicasFactor:   3,
			downAlphas:       0,
			negativeTestCase: true,
			description:      "Two groups with 3 alphas each, 1 p directory is not available",
			err:              "p directory does not exist for group [2]",
		},
		{
			name:             "ThreeGroupShutTwoAlphasPerGroup",
			bulkAlphas:       3,
			targetAlphas:     9,
			replicasFactor:   3,
			downAlphas:       2,
			negativeTestCase: true,
			description:      "Three groups with 3 alphas each, shutdown 2 alphas per group",
			err:              "failed to initiate pdir stream",
		},
		{
			name:             "SingleGroupShutOneAlpha",
			bulkAlphas:       1,
			targetAlphas:     3,
			replicasFactor:   3,
			downAlphas:       1,
			negativeTestCase: false,
			description:      "Single group with multiple alphas, shutdown 1 alpha",
			err:              "",
			waitForSnapshot:  true,
		},
		{
			name:             "TwoGroupShutOneAlphaPerGroup",
			bulkAlphas:       2,
			targetAlphas:     6,
			replicasFactor:   3,
			downAlphas:       1,
			negativeTestCase: false,
			description:      "Multiple groups with multiple alphas, shutdown 1 alphas per group",
			err:              "",
			waitForSnapshot:  true,
		},
		{
			name:             "ThreeGroupShutOneAlphaPerGroup",
			bulkAlphas:       3,
			targetAlphas:     9,
			replicasFactor:   3,
			downAlphas:       1,
			negativeTestCase: false,
			description:      "Three groups with 3 alphas each, shutdown 1 alpha per group",
			err:              "",
			waitForSnapshot:  true,
		},
		{
			name:             "SingleGroupAllAlphasOnline",
			bulkAlphas:       1,
			targetAlphas:     3,
			replicasFactor:   3,
			downAlphas:       0,
			negativeTestCase: false,
			description:      "Single group with multiple alphas, all alphas are online",
			err:              "",
		},
		{
			name:             "TwoGroupAllAlphasOnline",
			bulkAlphas:       2,
			targetAlphas:     6,
			replicasFactor:   3,
			downAlphas:       0,
			negativeTestCase: false,
			description:      "Multiple groups with multiple alphas, all alphas are online",
			err:              "",
		},
		{
			name:             "ThreeGroupAllAlphasOnline",
			bulkAlphas:       3,
			targetAlphas:     9,
			replicasFactor:   3,
			downAlphas:       0,
			negativeTestCase: false,
			description:      "Three groups with 3 alphas each, all alphas are online",
			err:              "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.negativeTestCase {
				t.Logf("Running negative test case: %s", tt.description)
			} else {
				t.Logf("Running test case: %s", tt.description)
			}
			runImportTest(t, tt.bulkAlphas, tt.targetAlphas, tt.replicasFactor, tt.downAlphas, tt.negativeTestCase,
				tt.err, tt.waitForSnapshot)
		})
	}
}

func runImportTest(t *testing.T, bulkAlphas, targetAlphas, replicasFactor, numDownAlphas int, negative bool,
	errStr string, waitForSnapshot bool) {
	bulkCluster, baseDir := setupBulkCluster(t, bulkAlphas)
	defer func() { bulkCluster.Cleanup(t.Failed()) }()

	targetCluster, gc, gcCleanup := setupTargetCluster(t, targetAlphas, replicasFactor)
	defer func() { targetCluster.Cleanup(t.Failed()) }()
	defer gcCleanup()

	_, err := gc.Query("schema{}")
	require.NoError(t, err)

	url, err := targetCluster.GetAlphaGrpcEndpoint(0)
	require.NoError(t, err)
	outDir := filepath.Join(baseDir, "out")

	// Get group information for all alphas
	_, cleanup, err := targetCluster.Client()
	require.NoError(t, err)
	defer cleanup()

	// Get health status for all instances
	hc, err := targetCluster.HTTPClient()
	require.NoError(t, err)
	var state pb.MembershipState

	healthResp, err := hc.GetAlphaState()
	require.NoError(t, err)
	require.NoError(t, protojson.Unmarshal(healthResp, &state))
	fmt.Println("Health response: ", string(healthResp))

	// Group alphas by their group number
	alphaGroups := make(map[uint32][]int)
	for _, group := range state.Groups {
		for _, member := range group.Members {
			if strings.Contains(member.Addr, "alpha0") {
				continue
			}
			alphaNum := strings.TrimPrefix(member.Addr, "alpha")
			alphaNum = strings.TrimSuffix(alphaNum, ":7080")
			alphaID, err := strconv.Atoi(alphaNum)
			require.NoError(t, err)
			alphaGroups[member.GroupId] = append(alphaGroups[member.GroupId], alphaID)
		}
	}

	// Shutdown specified number of alphas from each group
	for group, alphas := range alphaGroups {
		for i := 0; i < numDownAlphas; i++ {
			alphaID := alphas[i]
			t.Logf("Shutting down alpha %v from group %v", alphaID, group)
			require.NoError(t, targetCluster.StopAlpha(alphaID))
		}
	}

	if negative {
		err := Import(context.Background(), url, grpc.WithTransportCredentials(insecure.NewCredentials()), outDir)
		require.Error(t, err)
		fmt.Println("Error: ", err)
		require.ErrorContains(t, err, errStr)
		return
	}

	require.NoError(t, Import(context.Background(), url,
		grpc.WithTransportCredentials(insecure.NewCredentials()), outDir))

	for group, alphas := range alphaGroups {
		for i := 0; i < numDownAlphas; i++ {
			alphaID := alphas[i]
			t.Logf("Starting alpha %v from group %v", alphaID, group)
			require.NoError(t, targetCluster.StartAlpha(alphaID))
		}
	}

	require.NoError(t, targetCluster.HealthCheck(false))

	if waitForSnapshot {
		for grp, alphas := range alphaGroups {
			for i := 0; i < numDownAlphas; i++ {
				fmt.Println("Waiting for snapshot for alpha", alphas[i], "group", grp)
				hc, err := targetCluster.GetAlphaHttpClient(alphas[i])
				require.NoError(t, err)

				prevTs, err := hc.GetCurrentSnapshotTs(uint64(grp))
				require.NoError(t, err)
				// no need to check error because the cluster may have already taken a snapshot
				_, _ = hc.WaitForSnapshot(uint64(grp), prevTs)
			}
		}
	}

	t.Log("Import completed")

	for i := 0; i < targetAlphas; i++ {
		gc, cleanup, err := targetCluster.AlphaClient(i)
		require.NoError(t, err)
		defer cleanup()
		verifyImportResults(t, gc)
	}
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

	require.NoError(t, cluster.StartZero(0))

	// Perform bulk load
	oneMillion := dgraphtest.GetDataset(dgraphtest.OneMillionDataset)
	opts := dgraphtest.BulkOpts{
		DataFiles:   []string{oneMillion.DataFilePath()},
		SchemaFiles: []string{oneMillion.SchemaPath()},
		OutDir:      filepath.Join(baseDir, "out"),
	}
	require.NoError(t, cluster.BulkLoad(opts))

	return cluster, baseDir
}

// setupTargetCluster creates and starts a cluster that will receive the imported data
func setupTargetCluster(t *testing.T, numAlphas, replicasFactor int) (*dgraphtest.LocalCluster, *dgraphapi.GrpcClient, func()) {
	conf := dgraphtest.NewClusterConfig().
		WithNumAlphas(numAlphas).
		WithNumZeros(3).
		WithReplicas(replicasFactor)

	cluster, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	require.NoError(t, cluster.Start())

	gc, cleanup, err := cluster.Client()
	require.NoError(t, err)

	// Return cluster and client (cleanup will be handled by the caller)
	return cluster, gc, cleanup
}

// verifyImportResults validates the result of an import operation with retry logic
func verifyImportResults(t *testing.T, gc *dgraphapi.GrpcClient) {
	maxRetries := 10
	retryDelay := 500 * time.Millisecond
	hasAllPredicates := true

	// Get expected predicates first
	var expectedSchemaObj map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(expectedSchema), &expectedSchemaObj))
	expectedPredicates := getPredicateMap(expectedSchemaObj)

	for i := 0; i < maxRetries; i++ {
		schemaResp, err := gc.Query("schema{}")
		require.NoError(t, err)

		// Parse schema response
		var actualSchema map[string]interface{}
		require.NoError(t, json.Unmarshal(schemaResp.Json, &actualSchema))

		// Get actual predicates
		actualPredicates := getPredicateMap(actualSchema)

		// Check if all expected predicates are present
		for predName := range expectedPredicates {
			if _, exists := actualPredicates[predName]; !exists {
				hasAllPredicates = false
				break
			}
		}

		if hasAllPredicates {
			break
		}

		if i < maxRetries-1 {
			t.Logf("Not all predicates found yet, retrying in %v", retryDelay)
			time.Sleep(retryDelay)
			retryDelay *= 2
		}
	}

	if !hasAllPredicates {
		t.Fatalf("Not all predicates found in schema")
	}

	for _, tt := range common.OneMillionTCs {
		resp, err := gc.Query(tt.Query)
		require.NoError(t, err)
		require.NoError(t, dgraphapi.CompareJSON(tt.Resp, string(resp.Json)))
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
