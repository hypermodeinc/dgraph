//go:build integration2

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/hypermodeinc/dgraph/v25/dgraphtest"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/stretchr/testify/require"
)

func TestMCPSSE(t *testing.T) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithMCP()
	c, err := dgraphtest.NewLocalCluster(conf)
	require.NoError(t, err)
	require.NoError(t, c.Start())

	port, err := c.GetAlphaHttpPublicPort(0)
	require.NoError(t, err)
	serverURL := fmt.Sprintf("http://localhost:%s/mcp/sse", port)
	mcpClient, err := client.NewSSEMCPClient(serverURL)
	require.NoError(t, err, "Should create SSE MCP client")

	defer func() {
		mcpClient.Close()
		c.Cleanup(t.Failed())
	}()

	ctx := context.Background()
	startCtx, startCancel := context.WithTimeout(ctx, 60*time.Second)
	defer startCancel()
	err = mcpClient.Start(startCtx)
	require.NoError(t, err, "Should start SSE MCP client")

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "sse-test-client",
		Version: "1.0.0",
	}
	initResult, err := mcpClient.Initialize(context.Background(), initReq)
	require.NoError(t, err, "Failed to initialize client")
	require.NotNil(t, initResult, "Should receive initialization result")

	callTool := func(toolName string, args map[string]interface{}) (string, error) {
		toolRequest := mcp.CallToolRequest{}
		toolRequest.Params.Name = toolName
		toolRequest.Params.Arguments = args
		ctx := context.Background()
		result, err := mcpClient.CallTool(ctx, toolRequest)
		if err != nil {
			return "", fmt.Errorf("tool call failed: %w", err)
		}

		if result == nil || len(result.Content) == 0 {
			return "", fmt.Errorf("empty result from tool %s", toolName)
		}

		if result.IsError {
			return "", fmt.Errorf("tool error: %v", result.Content[0])
		}
		if textContent, ok := result.Content[0].(mcp.TextContent); ok {
			return textContent.Text, nil
		}

		return fmt.Sprintf("%v", result.Content[0]), nil
	}

	t.Run("ServerMetadata", func(t *testing.T) {
		require.Equal(t, "Dgraph MCP Server", initResult.ServerInfo.Name)
		require.NotEmpty(t, initResult.ServerInfo.Version)
	})

	t.Run("ListTools", func(t *testing.T) {
		toolsRequest := mcp.ListToolsRequest{}
		toolListResult, err := mcpClient.ListTools(ctx, toolsRequest)
		require.NoError(t, err, "ListTools should not fail")
		require.NotNil(t, toolListResult, "Should receive tool list result")

		expectedTools := []string{
			"get_schema",
			"run_query",
			"alter_schema",
			"run_mutation",
			"get_common_queries",
		}

		foundTools := make(map[string]bool)
		for _, tool := range toolListResult.Tools {
			foundTools[tool.Name] = true
		}

		for _, expected := range expectedTools {
			require.True(t, foundTools[expected], "Expected tool %s to be available", expected)
		}
	})

	t.Run("ListResources", func(t *testing.T) {
		resourcesRequest := mcp.ListResourcesRequest{}
		resourceListResult, err := mcpClient.ListResources(ctx, resourcesRequest)
		require.NoError(t, err, "ListResources should not fail")
		require.NotNil(t, resourceListResult, "Should receive resource list result")
		expectedResources := []string{
			"dgraph://schema",
			"dgraph://common_queries",
		}

		foundResources := make(map[string]bool)
		for _, resource := range resourceListResult.Resources {
			foundResources[resource.URI] = true
		}

		for _, expected := range expectedResources {
			require.True(t, foundResources[expected], "Expected resource %s to be available", expected)
		}

		for _, resource := range resourceListResult.Resources {
			t.Run(resource.URI, func(t *testing.T) {
				resourceRequest := mcp.ReadResourceRequest{}
				resourceRequest.Params.URI = resource.URI
				resourceResult, err := mcpClient.ReadResource(ctx, resourceRequest)
				require.NoError(t, err, "ReadResource should not fail")
				require.NotNil(t, resourceResult, "Should receive resource result")
				require.NotEmpty(t, resourceResult.Contents, "Should receive resource contents")
			})
		}
	})

	t.Run("ListPrompts", func(t *testing.T) {
		promptsRequest := mcp.ListPromptsRequest{}
		promptListResult, err := mcpClient.ListPrompts(ctx, promptsRequest)
		require.NoError(t, err, "ListPrompts should not fail")
		require.NotNil(t, promptListResult, "Should receive prompt list result")
		expectedPrompts := []string{
			"quick_start_prompt",
		}
		foundPrompts := make(map[string]bool)
		for _, prompt := range promptListResult.Prompts {
			foundPrompts[prompt.Name] = true
		}

		for _, expected := range expectedPrompts {
			require.True(t, foundPrompts[expected], "Expected prompt %s to be available", expected)
		}

		for _, prompt := range promptListResult.Prompts {
			t.Run(prompt.Name, func(t *testing.T) {
				promptRequest := mcp.GetPromptRequest{}
				promptRequest.Params.Name = prompt.Name
				promptResult, err := mcpClient.GetPrompt(ctx, promptRequest)
				require.NoError(t, err, "GetPrompt should not fail")
				require.NotNil(t, promptResult, "Should receive prompt result")
				require.NotEmpty(t, promptResult.Messages, "Should receive prompts")
			})
		}
	})

	t.Run("GetSchema", func(t *testing.T) {
		resultText, err := callTool("get_schema", map[string]interface{}{})

		require.NoError(t, err, "GetSchema should not fail")
		require.NotEmpty(t, resultText, "Should receive schema")
	})

	t.Run("AlterSchema", func(t *testing.T) {
		args := map[string]interface{}{
			"schema": "n: string @index(term) .",
		}
		resultText, err := callTool("alter_schema", args)

		require.NoError(t, err, "AlterSchema should not fail")
		require.NotEmpty(t, resultText, "Should receive alter schema result")
	})

	t.Run("RunMutation", func(t *testing.T) {
		args := map[string]interface{}{
			"mutation": `{
  "set": [
    {
      "uid": "_:1",
      "n": "Foo"
    }
  ]
}`,
		}
		resultText, err := callTool("run_mutation", args)

		require.NoError(t, err, "RunMutation should not fail")
		require.Equal(t, "Mutation completed, 1 UIDs created", resultText, "Should receive run mutation result")
	})

	t.Run("RunQuery", func(t *testing.T) {
		args := map[string]interface{}{
			"query": `{q(func: allofterms(n, "Foo")) { uid }}`,
		}
		resultText, err := callTool("run_query", args)

		require.NoError(t, err, "RunQuery should not fail")
		require.NotEmpty(t, resultText, "Should receive run query result")

		var result map[string][]map[string]string
		err = json.Unmarshal([]byte(resultText), &result)
		require.NoError(t, err, "Should be able to parse JSON response")

		require.Contains(t, result, "q", "Response should contain 'q' field")
		require.Len(t, result["q"], 1, "Should have exactly one result")
		require.Contains(t, result["q"][0], "uid", "Result should have 'uid' field")

		uidValue := result["q"][0]["uid"]
		require.Regexp(t, `^0x[0-9a-f]+$`, uidValue, "UID should be in the format 0x followed by hexadecimal digits")
	})

	t.Run("RunQueryWithEmptyArgs", func(t *testing.T) {
		args := map[string]interface{}{}
		_, err := callTool("run_query", args)

		require.Error(t, err, "RunQueryWithEmptyArgs should fail")
		require.Contains(t, err.Error(), "Query must be present")
	})

	t.Run("RunGetCommonQueries", func(t *testing.T) {
		resultText, err := callTool("get_common_queries", map[string]interface{}{})

		require.NoError(t, err, "RunGetCommonQueries should not fail")
		require.NotEmpty(t, resultText, "Should receive run get common queries result")
	})
}
