# knomit

Git-backed knowledge base for AI agents. Knowledge + commit.

Knomit stores structured facts as markdown files in a Git repository, organized by an ontological hierarchy. Each agent gets its own branch; consensus lives on `main`.

## Requirements

- Go 1.24+
- Node.js / npm (for the web frontend)
- SQLite with FTS5 support
- ONNX Runtime (downloaded automatically via `make setup`)

## Building

```sh
make setup    # download ONNX Runtime
make build    # build Go binary + React frontend → dist/knomit
```

Individual targets:

```sh
make web      # build React frontend only
make test     # run Go tests
make dist     # full distribution package (ORT + binary)
make clean    # remove build artifacts
```

## Usage

```sh
knomit serve       # start HTTP server (default port 3000)
knomit init        # initialize a new repo
knomit rebuild     # rebuild the search index
knomit reset       # wipe all databases and start fresh
```

### Development

```sh
make run              # build + run server (default: serve)
make run CMD=init     # run a different subcommand
make dev              # Vite dev server for frontend (HMR)
```

Seed test data (requires the server running):

```sh
go run ./tools/seed/   # seed base facts
```

### MCP Server

The MCP endpoint is available at `/mcp` on the HTTP server. Configure your AI tool to point at it.

Profiles tailor the MCP instructions for different use cases:

| Profile | URL | Use case |
|---------|-----|----------|
| `code` (default) | `/mcp` or `/mcp?profile=code` | Code editors — anchors facts to git commits |
| `chat` | `/mcp?profile=chat` | Conversational tools — anchors facts to URLs, documents |
| `generic` | `/mcp?profile=generic` | Minimal instructions for any integration |

#### Claude Code

Add to your project's `.mcp.json` (or `~/.claude/mcp.json` for global):

```json
{
  "mcpServers": {
    "knomit": {
      "type": "streamable-http",
      "url": "http://localhost:3000/mcp"
    }
  }
}
```

Knomit's tool descriptions carry all the behavioral guidance the model needs — no `CLAUDE.md` setup required. The model will automatically query for context and learn decisions as they arise.

#### Claude Desktop

Add to `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows):

```json
{
  "mcpServers": {
    "knomit": {
      "type": "streamable-http",
      "url": "http://localhost:3000/mcp?profile=chat"
    }
  }
}
```

### Web UI

The server embeds a React SPA at `/`. Browse facts, search, trigger synthesis, and monitor tasks in real time via SSE.

### Synthesize

Automated knowledge base maintenance — prune stale/duplicate facts and distill higher-order insights using an LLM. Trigger via the web UI or the HTTP API:

```
POST /api/v1/synthesize
```

#### LLM Configuration

Set the model and API key via environment variables:

| Provider   | Variables                                                                                                |
|------------|----------------------------------------------------------------------------------------------------------|
| Anthropic  | `KNOMIT_LLM_MODEL=claude-sonnet-4-6` `ANTHROPIC_API_KEY=...`                                            |
| Gemini     | `KNOMIT_LLM_MODEL=gemini-2.0-flash` `GOOGLE_AI_API_KEY=...`                                             |
| Bedrock    | `KNOMIT_LLM_MODEL=us.anthropic.claude-sonnet-4-6-v1` `AWS_ACCESS_KEY_ID=...` `AWS_SECRET_ACCESS_KEY=...` |
| Claude CLI | `KNOMIT_LLM_PROVIDER=claude-cli` — uses the `claude` CLI (no API key needed, works with Anthropic Max)   |
| Gemini CLI | `KNOMIT_LLM_PROVIDER=gemini-cli` — uses the `gemini` CLI (no API key needed, works with Google AI Pro)   |

The default model is `claude-sonnet-4-6` (Anthropic). The provider is auto-detected from the model name for API providers. CLI providers must be set explicitly via `KNOMIT_LLM_PROVIDER`.

#### Recipes

Recipes are YAML files in `<repo>/.knomit/synthesize/`. Example:

```yaml
name: cve-review
prompt: "Review security CVEs for staleness and patterns"
scope:
  domain: [security]
  entities: [libfoo]
auto_merge: false
steps:
  - mode: prune
    prompt: "Find stale or superseded CVEs"
  - mode: distill
    prompt: "Identify vulnerability patterns"
```

| Field | Description |
|-------|-------------|
| `name` | Recipe identifier (used for branch names and logging) |
| `prompt` | Global context passed to every step |
| `scope` | Filter facts by `domain`, `entities`, `search` queries, or `path` prefix |
| `auto_merge` | `true` merges results back automatically; `false` pushes a branch for review |
| `steps` | Pipeline of `prune` and/or `distill` steps. Each can override `model`. |

Running synthesis with no recipe uses a built-in default: prune + distill on all facts changed since the last run, with auto-merge enabled.

## MCP Tools

| Tool | Description |
|------|-------------|
| `knomit_learn` | Persist facts — preferences, decisions, conclusions |
| `knomit_query` | Search by free text, entity, domain, or path |
| `knomit_explore` | Browse the ontology hierarchy |
| `knomit_why` | Explain a fact's provenance and learning moment |
| `knomit_update` | Revise an existing fact (confidence, body, refs) |
| `knomit_forget` | Remove a fact (git history retains provenance) |

## How it works

Facts are markdown files with YAML frontmatter:

```markdown
---
confidence: 0.8
domain: [music]
entities: [alice]
sources: 1
refs:
  - knomit://github.com/org/repo/src/preferences.ts
---
Alice prefers rock music over jazz.
```

The directory tree under `know/` forms an ontological hierarchy. Facts placed at higher levels apply to everything below them — a fact at `know/earth/` is inherited by `know/earth/uk/london/`.

Each learning moment is an atomic git commit tagged with `learn/<moment-name>`, giving full provenance tracking.

### Refs and the `knomit:` URI scheme

Refs anchor facts to their source material using the `knomit:` URI scheme:

| Form | Meaning | Example |
|------|---------|---------|
| Relative (no authority) | Current knowledge base | `knomit:/know/debugging/pool-fix.md` |
| Absolute (with host) | External repo | `knomit://github.com/org/repo/src/main.ts` |
| Plain URL | Any web resource | `https://example.com/doc` |

Synthesize automatically resolves file-path refs to `knomit:/<path>` URIs.

## HTTP API

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/v1/browse` | Browse ontology hierarchy |
| GET | `/api/v1/search` | Full-text + vector search |
| GET | `/api/v1/fact` | Read a single fact |
| GET | `/api/v1/history` | Commit history |
| GET | `/api/v1/stats` | Knowledge base statistics |
| GET | `/api/v1/status` | System status |
| POST | `/api/v1/synthesize` | Trigger synthesis |
| POST | `/api/v1/sync` | Sync with remote |
| GET | `/api/v1/events` | SSE event stream |
| ALL | `/mcp` | MCP server endpoint |
| GET | `/docs` | Swagger UI |

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `KNOMIT_REPO` | `~/.knomit` | Path to the data directory |
| `KNOMIT_CACHE_DIR` | `~/.cache/knomit` | Model cache directory |
| `KNOMIT_PORT` | `3000` | HTTP server port |
| `KNOMIT_LLM_MODEL` | `claude-sonnet-4-6` | Model name for synthesis |
| `KNOMIT_LLM_PROVIDER` | auto-detected | LLM provider: `anthropic`, `gemini`, `bedrock`, `claude-cli`, `gemini-cli` |
| `KNOMIT_GIT_REMOTE` | `false` | Enable git remote endpoint |
| `KNOMIT_GIT_PORT` | — | Git remote port |
| `KNOMIT_API_KEY` | — | API key for git remote auth |
| `ANTHROPIC_API_KEY` | — | Anthropic API key |
| `GOOGLE_AI_API_KEY` | — | Gemini API key |
| `AWS_ACCESS_KEY_ID` | — | AWS access key for Bedrock |
| `AWS_SECRET_ACCESS_KEY` | — | AWS secret key for Bedrock |
| `AWS_REGION` | `us-east-1` | AWS region for Bedrock |
| `ORT_LIB_PATH` | — | Override ONNX Runtime library path |
