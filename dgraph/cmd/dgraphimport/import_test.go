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
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hypermodeinc/dgraph/v25/dgraphapi"
	"github.com/hypermodeinc/dgraph/v25/dgraphtest"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/systest/1million/common"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

type testcase struct {
	name           string
	numGroups      int
	targetAlphas   int
	replicasFactor int
	downAlphas     int
	description    string
	err            string
	encrypted      bool
}

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
	t.Skip("Skipping... sometimes the query for schema succeeds even when the server is in draining mode")

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
			connectionString := fmt.Sprintf("dgraph://%s", url)

			dc, err := newClient(connectionString)
			require.NoError(t, err)

			resp, err := initiateSnapshotStream(context.Background(), dc)
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

func TestImportApis(t *testing.T) {
	tests := []testcase{
		{
			name:           "SingleGroupShutTwoAlphasPerGroup",
			numGroups:      1,
			targetAlphas:   3,
			replicasFactor: 3,
			downAlphas:     2,
			description:    "Single group with 3 alphas, shutdown 2 alphas",
			err:            "failed to initiate external snapshot stream",
		},
		{
			name:           "TwoGroupShutTwoAlphasPerGroup",
			numGroups:      2,
			targetAlphas:   6,
			replicasFactor: 3,
			downAlphas:     2,
			description:    "Two groups with 3 alphas each, shutdown 2 alphas per group",
			err:            "failed to initiate external snapshot stream",
		},
		{
			name:           "TwoGroupShutTwoAlphasPerGroupNoPDir",
			numGroups:      1,
			targetAlphas:   6,
			replicasFactor: 3,
			downAlphas:     0,
			description:    "Two groups with 3 alphas each, 1 p directory is not available",
			err:            "p directory does not exist for group [2]",
		},
		{
			name:           "ThreeGroupShutTwoAlphasPerGroup",
			numGroups:      3,
			targetAlphas:   9,
			replicasFactor: 3,
			downAlphas:     2,
			description:    "Three groups with 3 alphas each, shutdown 2 alphas per group",
			err:            "failed to initiate external snapshot stream",
		},
		{
			name:           "SingleGroupShutOneAlpha",
			numGroups:      1,
			targetAlphas:   3,
			replicasFactor: 3,
			downAlphas:     1,
			description:    "Single group with multiple alphas, shutdown 1 alpha",
			err:            "",
		},
		{
			name:           "TwoGroupShutOneAlphaPerGroup",
			numGroups:      2,
			targetAlphas:   6,
			replicasFactor: 3,
			downAlphas:     1,
			description:    "Multiple groups with multiple alphas, shutdown 1 alphas per group",
			err:            "",
		},
		{
			name:           "ThreeGroupShutOneAlphaPerGroup",
			numGroups:      3,
			targetAlphas:   9,
			replicasFactor: 3,
			downAlphas:     1,
			description:    "Three groups with 3 alphas each, shutdown 1 alpha per group",
			err:            "",
		},
		{
			name:           "SingleGroupAllAlphasOnline",
			numGroups:      1,
			targetAlphas:   3,
			replicasFactor: 3,
			downAlphas:     0,
			description:    "Single group with multiple alphas, all alphas are online",
			err:            "",
		},
		{
			name:           "TwoGroupAllAlphasOnline",
			numGroups:      2,
			targetAlphas:   6,
			replicasFactor: 3,
			downAlphas:     0,
			description:    "Multiple groups with multiple alphas, all alphas are online",
			err:            "",
		},
		{
			name:           "ThreeGroupAllAlphasOnline",
			numGroups:      3,
			targetAlphas:   9,
			replicasFactor: 3,
			downAlphas:     0,
			description:    "Three groups with 3 alphas each, all alphas are online",
			err:            "",
		},
		{
			name:           "WithEncryptedData",
			numGroups:      1,
			targetAlphas:   3,
			replicasFactor: 3,
			downAlphas:     0,
			description:    "Single group with multiple alphas, all alphas are online",
			err:            "",
			encrypted:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err != "" {
				t.Logf("Running negative test case: %s", tt.description)
			} else {
				t.Logf("Running test case: %s", tt.description)
			}
			runImportTest(t, tt)
		})
	}
}

func runImportTest(t *testing.T, tt testcase) {
	bulkCluster, baseDir := setupBulkCluster(t, tt.numGroups, tt.encrypted)
	defer func() { bulkCluster.Cleanup(t.Failed()) }()

	targetCluster, gc, gcCleanup := setupTargetCluster(t, tt.targetAlphas, tt.replicasFactor)
	defer func() { targetCluster.Cleanup(t.Failed()) }()
	defer gcCleanup()

	// Wait for cluster to be fully ready before proceeding
	require.NoError(t, waitForClusterReady(t, targetCluster, gc, 30*time.Second))

	url, err := targetCluster.GetAlphaGrpcEndpoint(0)
	require.NoError(t, err)
	outDir := filepath.Join(baseDir, "out")
	connectionString := fmt.Sprintf("dgraph://%s", url)

	// Get health status for all instances
	hc, err := targetCluster.HTTPClient()
	require.NoError(t, err)
	var state pb.MembershipState

	healthResp, err := hc.GetAlphaState()
	require.NoError(t, err)
	require.NoError(t, protojson.Unmarshal(healthResp, &state))
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
		for i := 0; i < tt.downAlphas; i++ {
			alphaID := alphas[i]
			t.Logf("Shutting down alpha %v from group %v", alphaID, group)
			require.NoError(t, targetCluster.StopAlpha(alphaID))
			time.Sleep(500 * time.Millisecond) // Brief pause between shutdowns
		}
	}

	if tt.downAlphas > 0 && tt.err == "" {
		require.NoError(t, waitForClusterStable(t, targetCluster, 30*time.Second))
	}

	if tt.err != "" {
		err := Import(context.Background(), connectionString, outDir)
		require.Error(t, err)
		require.ErrorContains(t, err, tt.err)
		return
	}

	require.NoError(t, Import(context.Background(), connectionString, outDir))

	for group, alphas := range alphaGroups {
		for i := 0; i < tt.downAlphas; i++ {
			alphaID := alphas[i]
			t.Logf("Starting alpha %v from group %v", alphaID, group)
			require.NoError(t, targetCluster.StartAlpha(alphaID))
			require.NoError(t, waitForAlphaReady(t, targetCluster, alphaID, 30*time.Second))
		}
	}

	require.NoError(t, retryHealthCheck(t, targetCluster, 60*time.Second))

	t.Log("Import completed")

	for i := 0; i < tt.targetAlphas; i++ {
		t.Logf("Verifying import for alpha %v", i)
		gc, cleanup, err := targetCluster.AlphaClient(i)
		require.NoError(t, err)
		defer cleanup()

		require.NoError(t, validateClientConnection(t, gc, 10*time.Second))
		verifyImportResults(t, gc, tt.downAlphas)
	}
}

// setupBulkCluster creates and configures a cluster for bulk loading data
func setupBulkCluster(t *testing.T, numAlphas int, encrypted bool) (*dgraphtest.LocalCluster, string) {
	if runtime.GOOS != "linux" && os.Getenv("DGRAPH_BINARY") == "" {
		fmt.Println("You can set the DGRAPH_BINARY environment variable to path of a native dgraph binary to run these tests")
		t.Skip("Skipping test on non-Linux platforms due to dgraph binary dependency")
	}

	baseDir := t.TempDir()
	bulkConf := dgraphtest.NewClusterConfig().
		WithNumAlphas(numAlphas).
		WithNumZeros(1).
		WithReplicas(1).
		WithBulkLoadOutDir(t.TempDir())

	if encrypted {
		bulkConf.WithEncryption()
	}

	cluster, err := dgraphtest.NewLocalCluster(bulkConf)
	require.NoError(t, err)

	require.NoError(t, retryStartZero(t, cluster, 0, 30*time.Second))

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
func verifyImportResults(t *testing.T, gc *dgraphapi.GrpcClient, downAlphas int) {
	maxRetries := 1
	if downAlphas > 0 {
		maxRetries = 10
	}

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

// waitForClusterReady ensures the cluster is fully operational before proceeding
func waitForClusterReady(t *testing.T, cluster *dgraphtest.LocalCluster, gc *dgraphapi.GrpcClient, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	retryDelay := 500 * time.Millisecond

	for time.Now().Before(deadline) {
		if _, err := gc.Query("schema{}"); err != nil {
			t.Logf("Cluster not ready yet: %v, retrying in %v", err, retryDelay)
			time.Sleep(retryDelay)
			retryDelay = min(retryDelay*2, 5*time.Second)
			continue
		}

		if err := cluster.HealthCheck(false); err != nil {
			t.Logf("Health check failed: %v, retrying in %v", err, retryDelay)
			time.Sleep(retryDelay)
			retryDelay = min(retryDelay*2, 5*time.Second)
			continue
		}

		t.Log("Cluster is ready")
		return nil
	}

	return fmt.Errorf("cluster not ready within %v timeout", timeout)
}

// waitForClusterStable ensures remaining alphas are accessible after some are shut down
func waitForClusterStable(t *testing.T, cluster *dgraphtest.LocalCluster, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	retryDelay := 1 * time.Second

	for time.Now().Before(deadline) {
		if err := cluster.HealthCheck(false); err != nil {
			t.Logf("Cluster not stable yet: %v, retrying in %v", err, retryDelay)
			time.Sleep(retryDelay)
			retryDelay = min(retryDelay*2, 5*time.Second)
			continue
		}

		t.Log("Cluster is stable")
		return nil
	}

	return fmt.Errorf("cluster not stable within %v timeout", timeout)
}

// waitForAlphaReady waits for a specific alpha to be ready after startup
func waitForAlphaReady(t *testing.T, cluster *dgraphtest.LocalCluster, alphaID int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	retryDelay := 500 * time.Millisecond

	for time.Now().Before(deadline) {
		gc, cleanup, err := cluster.AlphaClient(alphaID)
		if err != nil {
			t.Logf("Alpha %d not ready yet: %v, retrying in %v", alphaID, err, retryDelay)
			time.Sleep(retryDelay)
			retryDelay = min(retryDelay*2, 3*time.Second)
			continue
		}

		_, queryErr := gc.Query("schema{}")
		cleanup()

		if queryErr != nil {
			t.Logf("Alpha %d query failed: %v, retrying in %v", alphaID, queryErr, retryDelay)
			time.Sleep(retryDelay)
			retryDelay = min(retryDelay*2, 3*time.Second)
			continue
		}

		t.Logf("Alpha %d is ready", alphaID)
		return nil
	}

	return fmt.Errorf("alpha %d not ready within %v timeout", alphaID, timeout)
}

// retryHealthCheck performs health check with retry logic
func retryHealthCheck(t *testing.T, cluster *dgraphtest.LocalCluster, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	retryDelay := 1 * time.Second

	for time.Now().Before(deadline) {
		if err := cluster.HealthCheck(false); err != nil {
			t.Logf("Health check failed: %v, retrying in %v", err, retryDelay)
			time.Sleep(retryDelay)
			retryDelay = min(retryDelay*2, 5*time.Second)
			continue
		}

		t.Log("Health check passed")
		return nil
	}

	return fmt.Errorf("health check failed within %v timeout", timeout)
}

// validateClientConnection ensures the client connection is working before use
func validateClientConnection(t *testing.T, gc *dgraphapi.GrpcClient, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	retryDelay := 200 * time.Millisecond

	for time.Now().Before(deadline) {
		if _, err := gc.Query("schema{}"); err != nil {
			t.Logf("Client connection validation failed: %v, retrying in %v", err, retryDelay)
			time.Sleep(retryDelay)
			retryDelay = min(retryDelay*2, 2*time.Second)
			continue
		}

		return nil
	}

	return fmt.Errorf("client connection validation failed within %v timeout", timeout)
}

// retryStartZero attempts to start zero with retry logic for port conflicts
func retryStartZero(t *testing.T, cluster *dgraphtest.LocalCluster, zeroID int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	retryDelay := 1 * time.Second

	for time.Now().Before(deadline) {
		err := cluster.StartZero(zeroID)
		if err == nil {
			t.Logf("Zero %d started successfully", zeroID)
			return nil
		}

		if strings.Contains(err.Error(), "bind: address already in use") {
			t.Logf("Port conflict starting zero %d: %v, retrying in %v", zeroID, err, retryDelay)
			time.Sleep(retryDelay)
			retryDelay = min(retryDelay*2, 10*time.Second)
			continue
		}

		return fmt.Errorf("failed to start zero %d: %v", zeroID, err)
	}

	return fmt.Errorf("failed to start zero %d within %v timeout due to port conflicts", zeroID, timeout)
}
