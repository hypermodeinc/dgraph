package mcp

import (
	"context"
	_ "embed"
	"fmt"
	"sync"
	"time"

	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/x"

	"github.com/golang/glog"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

//go:embed prompt.txt
var promptBytes []byte

var dgraphConnection *dgo.Dgraph
var getConnLock sync.Mutex

func getConn(connectionString string) (*dgo.Dgraph, error) {
	getConnLock.Lock()
	defer getConnLock.Unlock()

	if dgraphConnection != nil {
		return dgraphConnection, nil
	}

	conn, err := dgo.Open(connectionString)
	if err != nil {
		for i := range 3 {
			time.Sleep(time.Second * time.Duration(i))
			conn, err = dgo.Open(connectionString)
			if err == nil {
				break
			}
		}
		if err != nil {
			return nil, fmt.Errorf("error opening connection with Dgraph Alpha: %v", err)
		}
	}
	dgraphConnection = conn
	return conn, nil
}

var True = true
var False = false

// NewMCPServer initializes and returns a new MCPServer instance.
func NewMCPServer(connectionString string, readOnly bool) (*server.MCPServer, error) {
	s := server.NewMCPServer(
		"Dgraph MCP Server",
		x.Version(),
		server.WithResourceCapabilities(true, true),
		server.WithLogging(),
		server.WithRecovery(),
	)

	schemaTool := mcp.NewTool("get_schema",
		mcp.WithDescription("Get Dgraph DQL Schema from dgraph db"),
		mcp.WithToolAnnotation(mcp.ToolAnnotation{
			ReadOnlyHint:    &True,
			DestructiveHint: &False,
			IdempotentHint:  &True,
			OpenWorldHint:   &False,
		}),
	)

	queryTool := mcp.NewTool("run_query",
		mcp.WithDescription("Run Dgraph DQL Query on dgraph db"),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("The query to perform"),
		),
		mcp.WithToolAnnotation(mcp.ToolAnnotation{
			ReadOnlyHint:    &True,
			DestructiveHint: &False,
			IdempotentHint:  &True,
			OpenWorldHint:   &False,
		}),
	)

	if !readOnly {
		alterSchemaTool := mcp.NewTool("alter_schema",
			mcp.WithDescription("Alter Dgraph DQL Schema in dgraph db"),
			mcp.WithString("schema",
				mcp.Required(),
				mcp.Description("Updated schema to insert inside the db"),
			),
			mcp.WithToolAnnotation(mcp.ToolAnnotation{
				ReadOnlyHint:    &False,
				DestructiveHint: &True,
				IdempotentHint:  &False,
				OpenWorldHint:   &False,
			}),
		)

		s.AddTool(alterSchemaTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := request.GetArguments()
			if args == nil {
				return mcp.NewToolResultError("Schema must be present"), nil
			}

			schemaArg, ok := args["schema"]
			if !ok || schemaArg == nil {
				return mcp.NewToolResultError("Schema must be present"), nil
			}

			schema, ok := schemaArg.(string)
			if !ok {
				return mcp.NewToolResultError("Schema must be a string"), nil
			}

			// Execute alter operation
			conn, err := getConn(connectionString)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("Error opening connection with Dgraph Alpha", err), nil
			}
			if err = conn.SetSchema(ctx, dgo.RootNamespace, schema); err != nil {
				return mcp.NewToolResultErrorFromErr("Schema alteration failed", err), nil
			}

			return mcp.NewToolResultText("Schema updated successfully"), nil
		})

		mutationTool := mcp.NewTool("run_mutation",
			mcp.WithDescription("Run DQL Mutation on dgraph db"),
			mcp.WithString("mutation",
				mcp.Required(),
				mcp.Description("The mutation to perform in json format"),
			),
			mcp.WithToolAnnotation(mcp.ToolAnnotation{
				ReadOnlyHint:    &False,
				DestructiveHint: &True,
				IdempotentHint:  &False,
				OpenWorldHint:   &False,
			}),
		)

		s.AddTool(mutationTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			conn, err := getConn(connectionString)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("Error opening connection with Dgraph Alpha", err), nil
			}
			txn := conn.NewTxn()
			defer func() {
				err := txn.Discard(ctx)
				if err != nil {
					glog.Errorf("failed to discard transaction: %v", err)
				}
			}()
			args := request.GetArguments()
			if args == nil {
				return mcp.NewToolResultError("Mutation must be present"), nil
			}

			mutationArg, ok := args["mutation"]
			if !ok || mutationArg == nil {
				return mcp.NewToolResultError("Mutation must be present"), nil
			}

			mutation, ok := mutationArg.(string)
			if !ok {
				return mcp.NewToolResultError("Mutation must be a string"), nil
			}
			resp, err := txn.Mutate(ctx, &api.Mutation{
				SetJson:   []byte(mutation),
				CommitNow: true,
			})
			if err != nil {
				return mcp.NewToolResultErrorFromErr("Error running mutation", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("Mutation completed, %d UIDs created", len(resp.Uids)/2)), nil
		})
	}

	s.AddTool(queryTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		conn, err := getConn(connectionString)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("Error opening connection with Dgraph Alpha", err), nil
		}
		txn := conn.NewTxn()
		defer func() {
			err := txn.Discard(ctx)
			if err != nil {
				glog.Errorf("failed to discard transaction: %v", err)
			}
		}()
		args := request.GetArguments()
		if args == nil {
			return mcp.NewToolResultError("Query must be present"), nil
		}
		queryArg, ok := args["query"]
		if !ok || queryArg == nil {
			return mcp.NewToolResultError("Query must be present"), nil
		}
		op, ok := queryArg.(string)
		if !ok {
			return mcp.NewToolResultError("Query must be a string"), nil
		}
		resp, err := txn.Query(ctx, op)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("Error running query", err), nil
		}
		return mcp.NewToolResultText(string(resp.GetJson())), nil
	})

	s.AddTool(schemaTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		conn, err := getConn(connectionString)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("Error opening connection with Dgraph Alpha", err), nil
		}
		txn := conn.NewTxn()
		defer func() {
			err := txn.Discard(ctx)
			if err != nil {
				glog.Errorf("failed to discard transaction: %v", err)
			}
		}()
		resp, err := txn.Query(ctx, "schema {}")
		if err != nil {
			return mcp.NewToolResultErrorFromErr("Error running query", err), nil
		}
		return mcp.NewToolResultText(string(resp.GetJson())), nil
	})

	schemaResource := mcp.NewResource(
		"dgraph://schema",
		"dgraph_schema",
		mcp.WithResourceDescription("The current Dgraph DQL schema"),
		mcp.WithMIMEType("text/plain"),
	)

	s.AddResource(schemaResource, func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		// Execute operation
		conn, err := getConn(connectionString)
		if err != nil {
			return nil, fmt.Errorf("error opening connection with Dgraph Alpha: %w", err)
		}
		resp, err := conn.NewTxn().Query(ctx, "schema {}")
		if err != nil {
			return nil, fmt.Errorf("error running query: %w", err)
		}

		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      "dgraph://schema",
				MIMEType: "text/plain",
				Text:     string(resp.Json),
			},
		}, nil
	})

	commonQueriesTool := mcp.NewTool("get_common_queries",
		mcp.WithDescription("Get common queries that you can run on the db. If you are seeing issues with your queries, you can check this tool once."),
		mcp.WithToolAnnotation(mcp.ToolAnnotation{
			ReadOnlyHint:    &True,
			DestructiveHint: &False,
			IdempotentHint:  &True,
			OpenWorldHint:   &False,
		}),
	)

	s.AddTool(commonQueriesTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(`
			{
				"shortest_path_query": "
					{
						q(func: eq(guid, "first guid") { // get first uid in form of a query
							a as uid
						}
						q1(func: eq(guid, "second guid")) { // get second uid in form of a query
							b as uid
						}
						path as shortest(from: uid(a), to: uid(b), numpaths: 5, maxheapsize: 10000) {
							connected_to @facets(weight) // Add @facet() to get path by weight, remove it if all edges are same weight
						}
						path(func: uid(path)) {
							uid
						}
					}
				"`,
		), nil
	})

	commonQueries := mcp.NewResource(
		"dgraph://common_queries",
		"dgraph_common_queries",
		mcp.WithResourceDescription("The current Dgraph common queries that you can use to fix your queries"),
		mcp.WithMIMEType("text/plain"),
	)

	s.AddResource(commonQueries, func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      "dgraph://commmon_queries",
				MIMEType: "text/plain",
				Text: `
				{
					"shortest_path_query": "
						{
							q(func: eq(guid, "first guid")) { // get first uid in form of a query
								a as uid
							}
							q1(func: eq(guid, "second guid")) { // get second uid in form of a query
								b as uid
							}

  							path as shortest(from: uid(a), to: uid(b), numpaths: 5, maxheapsize: 10000) {
  								connected_to @facets(weight) // Add @facet() to get path by weight, remove it if all edges are same weight
 							}

  							path(func: uid(path)) {
   								uid
 							}
						}
					",
				}
				`,
			},
		}, nil
	})

	// Add resource with its handler
	s.AddResource(schemaResource, func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		// Execute operation
		conn, err := getConn(connectionString)
		if err != nil {
			return nil, err
		}
		resp, err := conn.NewTxn().Query(ctx, "schema {}")
		if err != nil {
			return nil, fmt.Errorf("failed to get schema: %v", err)
		}

		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      "dgraph://schema",
				MIMEType: "text/plain",
				Text:     string(resp.Json),
			},
		}, nil
	})

	addPrompt(s)

	return s, nil
}

func addPrompt(s *server.MCPServer) {
	prompt := string(promptBytes)
	s.AddPrompt(mcp.NewPrompt("quick_start_prompt",
		mcp.WithPromptDescription("A quick Start prompt for new users and llms"),
	), func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return mcp.NewGetPromptResult(
			"quick_start_prompt",
			[]mcp.PromptMessage{
				mcp.NewPromptMessage(
					mcp.RoleAssistant,
					mcp.NewTextContent(prompt),
				),
			},
		), nil
	})
}
