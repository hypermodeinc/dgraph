package main

import (
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/server"

	"github.com/hypermodeinc/dgraph/v25/mcp"
)

func main() {
	// Get connection string from env
	connectionString := os.Getenv("DGRAPH_CONNECTION")
	readOnly := os.Getenv("DGRAPH_READ_ONLY")

	s, err := mcp.NewMCPServer(connectionString, readOnly)
	if err != nil {
		fmt.Printf("Failed to initialize MCPServer: %v\n", err)
		return
	}

	// Start the stdio server
	if err := server.ServeStdio(s); err != nil {
		fmt.Printf("Server error: %v\n", err)
	}
}
