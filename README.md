# knomit

Git-backed knowledge base for AI agents. Knowledge + commit.

Knomit stores structured facts as markdown files in a Git repository, organized by an ontological hierarchy. Each agent gets its own branch (derived from its SSH key fingerprint); consensus lives on `main`.

## Requirements

- Go 1.24+
- Node.js / npm (for the web frontend)
- SQLite
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
make e2e-setup # install Playwright browsers (once)
make e2e      # build + run e2e tests
make e2e-ui   # build + run e2e tests (headed browser)
```

## Usage

```sh
knomit serve                  # start HTTP server (default port 3000)
knomit init                   # initialize the default repo
knomit init --name work       # initialize a repo named "work"
knomit rebuild                # rebuild the default repo's search index
knomit rebuild --name work    # rebuild a specific repo's index
knomit reset                  # wipe the default repo
knomit reset --name work      # wipe a specific repo
```

### Data Layout

All data lives under `KNOMIT_HOME` (default `~/.knomit`):

```
~/.knomit/
  repos/
    knomit.db        # default repo (auto-created)
    work.db          # additional repos (discovered at startup)
  models/            # shared ONNX embedder files
  id_ed25519         # SSH identity (shared across repos)
  id_ed25519.pub
```

Repos are discovered by scanning `~/.knomit/repos/` for `*.db` files at startup. The filename (minus `.db`) is the repo name. Names must match `[a-z0-9_-]+`.

The default `knomit` repo is always created if missing.

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

### E2E Testing

The `e2e/` directory contains a Playwright test suite that exercises the built binary end-to-end across UI, MCP, and API layers.

```sh
make e2e-setup   # install deps + Playwright browsers (once)
make e2e         # build binary + run all tests (headless)
make e2e-ui      # same but with a visible browser
```

Tests use `KNOMIT_HOME` pointed at a temp directory for full isolation. A shared instance is seeded with fixture data for read-only tests; mutating tests spin up fresh instances per test.

### Configuration

Place a `knomit.toml` next to the binary or in the data directory. See `dist/knomit.toml` for a full reference. All settings can be overridden via environment variables.

### Remote Sync

Each repo can sync with a git remote. Configure via the web UI (gear icon in the top bar) or `knomit.toml` for the default repo.

Supported auth methods:

| Method | URL format | Credentials |
|--------|-----------|-------------|
| SSH | `git@github.com:user/repo.git` | Knomit's SSH key (`~/.knomit/id_ed25519`) |
| Token | `https://github.com/user/repo.git` | GitHub PAT or similar |
| Basic | `https://github.com/user/repo.git` | Username + password |

Credentials are encrypted at rest using a key derived from the SSH private key.

Sync and push run on independent intervals (default 300s each). If push fails (e.g. read-only token), sync continues working. Remote errors are visible via the gear icon indicator in the UI.

### MCP Server

The MCP endpoint is available per-repo at `/api/v1/{repo}/mcp`. Configure your AI tool to point at it.

Profiles tailor the MCP instructions for different use cases:

| Profile | URL | Use case |
|---------|-----|----------|
| `code` (default) | `/api/v1/knomit/mcp?profile=code` | Code editors — anchors facts to git commits |
| `chat` | `/api/v1/knomit/mcp?profile=chat` | Conversational tools — anchors facts to URLs, documents |
| `generic` | `/api/v1/knomit/mcp?profile=generic` | Minimal instructions for any integration |

#### Claude Code

Add to your project's `.mcp.json` (or `~/.claude/mcp.json` for global):

```json
{
  "mcpServers": {
    "knomit": {
      "type": "streamable-http",
      "url": "http://localhost:3000/api/v1/knomit/mcp"
    }
  }
}
```

Knomit's tool descriptions carry all the behavioral guidance the model needs — no `CLAUDE.md` setup required. The model will automatically query for context and learn decisions as they arise.

#### Claude Desktop

Claude Desktop only supports stdio transports. Use the included `knomit-mcp-remote` bridge (built automatically by `make build`):

Add to `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows):

```json
{
  "mcpServers": {
    "knomit": {
      "command": "/path/to/dist/knomit-mcp-remote",
      "args": ["http://localhost:3000"]
    }
  }
}
```

For multiple repos:

```json
{
  "mcpServers": {
    "knomit": {
      "command": "/path/to/dist/knomit-mcp-remote",
      "args": ["http://localhost:3000"]
    },
    "work-kb": {
      "command": "/path/to/dist/knomit-mcp-remote",
      "args": ["--repo", "work", "--profile", "chat", "http://localhost:3000"]
    }
  }
}
```

Flags: `--repo <name>` (default: `knomit`), `--profile <profile>` (default: `code`).

The bridge reads JSON-RPC from stdin, forwards to the knomit HTTP endpoint, and writes responses to stdout.

Use this initial prompt:

```
You have access to a knowledge base called knomit. Use knomit_learn to save important facts, decisions, and preferences from our conversations. Use knomit_query to check if you already know something before asking me. Use knomit_explore to browse what you know. Use knomit_review to review and maintain the knowledge base — it guides you through evaluating facts step by step.
```

### Web UI

The server embeds a React SPA at `/`. Browse facts, search, trigger synthesis, and monitor tasks in real time via SSE.

The top bar shows a repo selector (when multiple repos exist) and a gear icon for remote origin configuration. Remote sync/push errors are indicated by a red dot on the gear icon.

### Synthesize

Automated knowledge base maintenance — prune stale/duplicate facts and distill higher-order insights using an LLM. Trigger via the web UI or the HTTP API:

```
POST /api/v1/{repo}/synthesize
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
| `knomit_explore` | Browse facts by recency (paginated, 25/page) |
| `knomit_explain` | Explain a fact's provenance graph (paginated BFS) |
| `knomit_update` | Revise an existing fact (confidence, body, refs) |
| `knomit_retract` | Remove a fact (git history retains provenance) |

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

The directory tree under the ontology root (`kb/` by default) forms an ontological hierarchy. Facts placed at higher levels apply to everything below them — a fact at `kb/geography/` is inherited by `kb/geography/europe/uk/london/`.

Each learning moment is an atomic git commit tagged with `learn/<moment-name>`, giving full provenance tracking.

### Agent Identity

Each knomit instance generates an Ed25519 SSH keypair at `~/.knomit/id_ed25519`. The key fingerprint determines the agent branch name (e.g. `agent/hostname-abc123`), ensuring each machine gets its own branch. The same key is used for SSH remote auth and commit signing.

When opening an existing repo that uses an older branch naming convention, knomit automatically creates a new branch from the current HEAD and switches to it.

### Refs and the `knomit:` URI scheme

Refs anchor facts to their source material using the `knomit:` URI scheme:

| Form | Meaning | Example |
|------|---------|---------|
| Relative (no authority) | Current knowledge base | `knomit:/kb/technology/debugging/pool-fix.md` |
| Absolute (with host) | External repo | `knomit://github.com/org/repo/src/main.ts` |
| Plain URL | Any web resource | `https://example.com/doc` |

Synthesize automatically resolves file-path refs to `knomit:/<path>` URIs.

## HTTP API

All repo-scoped endpoints use the pattern `/api/v1/{repo}/...`.

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/v1/repos` | List all repos |
| GET | `/api/v1/{repo}/browse` | Browse ontology hierarchy |
| GET | `/api/v1/{repo}/search` | Full-text + vector search |
| GET | `/api/v1/{repo}/fact` | Read a single fact |
| GET | `/api/v1/{repo}/history` | Commit history |
| GET | `/api/v1/{repo}/stats` | Knowledge base statistics |
| GET | `/api/v1/{repo}/status` | System status |
| POST | `/api/v1/{repo}/synthesize` | Trigger synthesis |
| POST | `/api/v1/{repo}/sync` | Sync with remote |
| GET | `/api/v1/{repo}/origin` | Get remote origin config |
| PUT | `/api/v1/{repo}/origin` | Set remote origin config |
| GET | `/api/v1/{repo}/events` | SSE event stream |
| ALL | `/api/v1/{repo}/mcp` | MCP server endpoint |
| GET | `/docs` | Swagger UI |

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `KNOMIT_HOME` | `~/.knomit` | Path to the data directory |
| `KNOMIT_REPO` | — | Alias for `KNOMIT_HOME` (backward compat) |
| `KNOMIT_PORT` | `3000` | HTTP server port |
| `KNOMIT_LLM_MODEL` | `claude-sonnet-4-6` | Model name for synthesis |
| `KNOMIT_LLM_PROVIDER` | auto-detected | LLM provider: `anthropic`, `gemini`, `bedrock`, `claude-cli`, `gemini-cli` |
| `KNOMIT_GIT_ORIGIN` | — | Remote origin URL (seeds default repo) |
| `KNOMIT_GIT_SERVE` | `false` | Enable git smart-HTTP remote at `/git` |
| `KNOMIT_API_KEY` | — | API key for LLM and git remote auth |
| `KNOMIT_REMOTE_AUTH` | auto-detected | Remote auth method: `token`, `basic`, `ssh` |
| `KNOMIT_REMOTE_TOKEN` | — | Token for remote auth |
| `KNOMIT_REMOTE_SSH_KEY` | — | Path to SSH key (default: `~/.knomit/id_ed25519`) |
| `KNOMIT_LLM_TRACE` | — | File path to log LLM requests |
| `ANTHROPIC_API_KEY` | — | Anthropic API key |
| `GOOGLE_AI_API_KEY` | — | Gemini API key |
| `AWS_ACCESS_KEY_ID` | — | AWS access key for Bedrock |
| `AWS_SECRET_ACCESS_KEY` | — | AWS secret key for Bedrock |
| `AWS_REGION` | `us-east-1` | AWS region for Bedrock |
| `ORT_LIB_PATH` | — | Override ONNX Runtime library path |
