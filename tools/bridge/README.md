# knomit-bridge

`knomit-bridge` is a stdio↔HTTP adapter that lets stdio-only MCP clients (Claude Desktop, VS Code extensions, and any other client that only supports process-based MCP) talk to a running knomit server.

## Why it exists

The knomit server speaks MCP over streamable-HTTP (`/api/v1/{repo}/mcp`). Many MCP clients only support stdio transport — they launch a subprocess, write JSON-RPC to its stdin, and read responses from its stdout. `knomit-bridge` is that subprocess: it translates between the two transports so no client-side changes are needed.

```
MCP client (stdio)
      │  JSON-RPC over stdin/stdout
      ▼
knomit-bridge
      │  POST /api/v1/{repo}/mcp?profile={profile}
      ▼
knomit server (HTTP)
```

## How it works

1. Reads newline-delimited JSON-RPC messages from stdin.
2. POSTs each message to the knomit MCP endpoint, carrying the `Mcp-Session-Id` header once the session is established.
3. The server responds with either `application/json` (simple response) or `text/event-stream` (SSE, used for streaming tool results). Both are normalised to newline-delimited JSON written to stdout.
4. Notifications (HTTP 202, no body) are silently acknowledged.
5. HTTP errors are translated into JSON-RPC error responses so the client always receives well-formed output.

Port discovery follows this priority:

1. Positional `base-url` argument (explicit override).
2. Lockfile written by `knomit-tray` (`~/Library/Application Support/knomit/server.json` on macOS, `$XDG_STATE_HOME/knomit/server.json` on Linux).
3. Default `http://localhost:19278`.

## Usage

```
knomit-bridge [--repo <name>] [--profile <profile>] [base-url]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--repo` | `knomit` | Repository name |
| `--profile` | `code` | MCP profile (`code`, `chat`, `generic`) |
| `base-url` | `http://localhost:19278` | Base URL of the knomit server |

## MCP client configuration

### Claude Desktop

Add to `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows):

```json
{
  "mcpServers": {
    "knomit": {
      "command": "/path/to/dist/knomit-bridge"
    }
  }
}
```

Multiple repos, different profiles:

```json
{
  "mcpServers": {
    "knomit": {
      "command": "/path/to/dist/knomit-bridge"
    },
    "work-kb": {
      "command": "/path/to/dist/knomit-bridge",
      "args": ["--repo", "work", "--profile", "chat"]
    }
  }
}
```

## Debugging

Set `KNOMIT_MCP_DEBUG=1` to log traffic to stderr:

```
KNOMIT_MCP_DEBUG=1 knomit-bridge
```

Each stdin message, outgoing HTTP request, response status, session ID capture, and stdout write is logged with direction arrows (`←` stdin, `→` stdout).

## Building

```
make build        # produces dist/knomit-bridge
go build -o dist/knomit-bridge ./tools/bridge/
```
