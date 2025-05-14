# Dgraph MCP (Master Control Protocol)

## Project Overview

This is a Dgraph-based Master Control Protocol (MCP) designed to provide advanced graph database
management and querying capabilities. There are two ways you can access the mcp server:

1. Using the dgraph alpha's http endpoint
2. Using the go code

## Key Components

- Dgraph schema management
- Advanced data querying
- Flexible graph database interactions

## Setup Instructions for dgraph alpha

- Install dgraph alpha
- Start dgraph alpha

```json
{
  "dgraph-mcp": {
    "serverUrl": "http://localhost:8080/mcp/sse"
  }
}
```

## Setup Instructions for go code

- Install golang 1.23+
- Clone the repository
- cd mcp/dgraph-mcp
- go build
- Insert the following to your mcp config:

```json
{
  "mcpServers": {
    "dgraph": {
      "command": "<path to dgraph folder>/mcp/dgraph-mcp/dgraph-mcp",
      "env": {
        "DGRAPH_CONNECTION": "dgraph://<host>:443?sslmode=verify-ca&bearertoken=xxxxxxxxxx",
        "DGRAPH_READ_ONLY": "true" / "false",
      }
    }
  }
}
```

### Local Setup for Claude Desktop

You can setup a local experience using Claude app

Install Claude.app from https://claude.ai/download.

On Mac OS the configurations files are in `~/Library/Application\ Support/Claude`. Create
`~/Library/Application\ Support/Claude/claude_desktop_config.json` file and set the content with mcp
config shown above.

Close the Claude app and reopen it.

Check that the tools are listed in Claude desktop (Click on the tools icon).

You are ready to chat and get things done in Dgraph from Claude.

## Troubleshooting

- Check Dgraph connection settings
- Verify Go module dependencies
- Review error logs carefully
