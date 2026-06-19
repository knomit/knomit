# knomit

Git-backed knowledge base for AI agents. Knowledge + commit.

Knomit stores structured facts as markdown files in a Git repository, organized by an ontological hierarchy. Each agent gets its own branch (derived from its SSH key fingerprint); consensus lives on `main`.

## Requirements

- **Go 1.24+** — the entire backend
- **Node.js + npm** — builds the embedded React frontend
- **Git CLI** — knomit shells out to `git`
- **A C compiler** — Xcode Command Line Tools on macOS (`xcode-select --install`), `gcc`/`build-essential` on Linux. The build is CGO-based (SQLite via `mattn/go-sqlite3` and the ONNX bindings are compiled in).

Two native shared libraries — **ONNX Runtime** (embeddings) and **graphqlite** (graph queries) — are downloaded automatically into `dist/lib/` by `make setup` / `make build`; you don't install them by hand.

## Quick start

```sh
git clone <your-knomit-remote-url> knomit
cd knomit
make setup    # one-time: download ONNX Runtime + graphqlite into dist/lib/
make build    # build the web frontend, then the Go binaries
make run      # start the server on http://localhost:19278
```

Open <http://localhost:19278/> for the web UI. The default repo is created automatically on first run.

## Building

```sh
make setup    # download native libs (ONNX Runtime + graphqlite)
make build    # build React frontend + Go binaries (CGO)
```

Build artifacts are written under a per-platform directory,
`dist/<goos>-<goarch>/` (e.g. `dist/darwin-arm64/`, `dist/linux-arm64/`), so
builds for different platforms coexist without clobbering each other — Wails
(CGO + the OS-native webview) cannot cross-compile, so each platform is built in
its own native environment.

`make build` produces the server/CLI and the stdio bridge, plus stable top-level
symlinks → the host platform's build (so config that references a fixed path,
e.g. `.mcp.json` and the e2e harness, keeps working):

- `dist/knomit` → `dist/<platform>/knomit` — the main server / CLI binary
- `dist/knomit-bridge` → `dist/<platform>/knomit-bridge` — stdio↔HTTP adapter for stdio-only MCP clients

`make desktop` produces the native desktop app (Wails v3) **only** under the
platform dir (no top-level symlink — nothing references it by a fixed path):

- macOS: `dist/<platform>/Knomit.app` — launch with `make desktop-run` (or `open dist/darwin-arm64/Knomit.app`)
- Linux/Windows: `dist/<platform>/knomit-desktop`

Individual targets:

```sh
make web      # build React frontend only
make test     # run Go tests
make desktop  # build the native desktop app (Wails v3, CGO)
make docker   # build the self-contained cloud server image
make dist     # full distribution package (ORT + binary)
make clean    # remove build artifacts
make e2e-setup # install Playwright browsers (once)
make e2e      # build + run e2e tests
make e2e-ui   # build + run e2e tests (headed browser)
```

### Testing

```sh
# Unit + scenario tests (fast, always on).
go test ./...

# Same, with the race detector — used in CI.
go test ./... -race

# Run just the scenario tests in the Storyboard DSL.
go test ./internal/storytests/ -v

# Run just the Verify tool's unit tests.
go test ./internal/store/ -run TestVerify -v
```

**Property tests** (opt-in) exercise random op sequences against real store state. They live in
[internal/storytests/property_test.go](internal/storytests/property_test.go)
and are gated behind `KNOMIT_PROPTESTS=1`:

```sh
# Run all property tests (time-based seed, new coverage every run).
KNOMIT_PROPTESTS=1 go test ./internal/storytests/ -run TestProperty -v

# Reproduce a failure deterministically — the seed is logged on every run.
KNOMIT_PROPTESTS=1 KNOMIT_PROPTEST_SEED=1712345678 \
  go test ./internal/storytests/ -run TestProperty -v
```

The `knomit verify` CLI subcommand runs the same integrity checks against a live on-disk repo:

```sh
knomit verify                 # verify the default repo
knomit verify --name work     # verify a specific repo
knomit verify --all --deep    # every repo, including fact-format check
```

## Usage

```sh
knomit serve                  # start HTTP server (default port 19278)
knomit reset                  # wipe the default repo
knomit reset --name work      # wipe a specific repo
```

The default repo (`trunk`) is created automatically the first time you run
`knomit serve`. Additional repos are created, archived, restored, and purged
through the web UI ("Manage repos") or the REST API — see
[Managing repos](#managing-repos). There is no CLI command to create a repo.

### Data Layout

All data lives under `KNOMIT_HOME` (default `~/.knomit`):

```text
~/.knomit/
  repos/
    trunk.db         # default repo (auto-created)
    work.db          # additional repos (created via the API/UI)
    archive/         # archived repos (<ksuid>.db + <ksuid>.json manifest)
  models/            # shared ONNX embedder files
  id_ed25519         # SSH identity (shared across repos)
  id_ed25519.pub
```

### Managing repos

Repos are created and managed at runtime — no CLI, no restart. Use the web UI
("Manage repos" in the top-bar menu) or the REST API:

```sh
# Create a repo (streams newline-delimited JSON progress).
# mode is one of: preset | custom | clone
curl -N -X POST http://localhost:19278/api/v1/repos \
  -H 'Content-Type: application/json' \
  -d '{"name":"work","mode":"preset","ontology_preset":"default"}'

# Archive (recoverable), list archived, restore, purge.
# Archiving returns an opaque archive id (a ksuid); list archived to find it.
curl -X DELETE http://localhost:19278/api/v1/repos/work
curl http://localhost:19278/api/v1/archived
curl -X POST http://localhost:19278/api/v1/archived/2cVcW8aQk1bE9fG0hJ2kL3mN4pQ/restore
curl -X DELETE http://localhost:19278/api/v1/archived/2cVcW8aQk1bE9fG0hJ2kL3mN4pQ
```

The startup scan and the rescan endpoint still pick up `*.db` files that appear
out-of-band (e.g. a restored backup copied into `~/.knomit/repos/`):

```sh
curl -X POST http://localhost:19278/api/v1/repos:rescan
# {"added":["work"],"skipped":["trunk"],"errors":[],"_links":{...}}
```

Already-open repos are reported in `skipped`; per-repo open failures appear
in `errors[]` without aborting the scan.

Repos are discovered by scanning `~/.knomit/repos/` for `*.db` files at startup. The filename (minus `.db`) is the repo name. Names must match `[a-z0-9_-]+`.

The default `trunk` repo is always created if missing.

### Development

```sh
make run              # build + run server (default: serve)
make run CMD=init     # run a different subcommand
make dev              # Vite dev server for frontend (HMR)
```

**Editor setup (VS Code):** install the Go extension (`golang.go`); it uses `gopls`. A C compiler must be on `PATH` so gopls and `go test` can build the CGO packages.

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

Tests use `KNOMIT_HOME` pointed at a temp directory for full isolation.

### Configuration

Place a `knomit.toml` next to the binary or in the data directory (`~/.knomit/knomit.toml`). All settings can also be set via environment variables (see below).

### Remote Sync

Each repo can sync with a git remote. Configure via the web UI (gear icon in the top bar) or environment variables.

Supported auth methods:

| Method | URL format | Credentials |
|--------|------------|-------------|
| SSH | `git@github.com:user/repo.git` | Knomit's SSH key (`~/.knomit/id_ed25519`) |
| Token | `https://github.com/user/repo.git` | GitHub PAT or similar |
| Basic | `https://github.com/user/repo.git` | Username + password |

Credentials are encrypted at rest using a key derived from the SSH private key.

Sync and push run on independent intervals. Remote errors are visible via the gear icon indicator in the UI.

### MCP Server

The MCP endpoint is branch-scoped:

```
/api/v1/repos/{repo}/branches/{branch}/mcp?profile={profile}
```

The agent branch is logged on startup (`branch=agent/hostname-abc123`) and is also available via `GET /api/v1/repos/{repo}` (the `agent_branch` field). Branch names use `:` in place of `/` in URL path segments (e.g. `agent:hostname-abc123`).

Profiles tailor the MCP instructions for different use cases:

| Profile | Use case |
|---------|----------|
| `code` (default) | Code editors — anchors facts to git commits |
| `chat` | Conversational tools — anchors facts to URLs, documents |
| `generic` | Minimal instructions for any integration |

#### Claude Code

The simplest setup uses `knomit-bridge` over stdio — no need to look up the agent branch. Add to your project's `.mcp.json` (or `~/.claude/mcp.json` for global); this repo already ships one:

```json
{
  "mcpServers": {
    "knomit": {
      "command": "dist/knomit-bridge",
      "args": ["--repo", "trunk", "--source", "trunk", "--profile", "code"]
    }
  }
}
```

`--source` is the slug used in `src://` refs (defaults to `--repo` if omitted). With the server running, the bridge discovers the agent branch automatically.

Alternatively, connect directly over streamable-HTTP, substituting the branch logged on startup (`/` → `:` in the URL):

```json
{
  "mcpServers": {
    "knomit": {
      "type": "streamable-http",
      "url": "http://localhost:19278/api/v1/repos/trunk/branches/agent:hostname-abc123/mcp"
    }
  }
}
```

Knomit's tool descriptions carry all the behavioral guidance the model needs — no `CLAUDE.md` setup required.

#### Other stdio clients (Claude Desktop, VS Code, Cursor, …)

Claude Desktop and other stdio-only MCP clients use the same `knomit-bridge` adapter built by `make build`. It auto-discovers the agent branch from the server. See [tools/bridge/README.md](tools/bridge/README.md) for setup and configuration.

### Web UI

The server embeds a React SPA at `/`. Browse facts, search, trigger synthesis, and monitor tasks in real time via SSE.

The top bar shows a repo selector (when multiple repos exist) and a gear icon for remote origin configuration.

### Desktop app (macOS / Windows / Linux)

`knomit-desktop` is a native [Wails v3](https://v3.wails.io) app: a system-tray icon plus a native webview window showing the knomit UI. It runs the knomit server **in-process** (API/MCP only) on a looknomitck port (prefers 19278) and serves the UI from embedded assets — so the port is a pure API/MCP endpoint that Claude Code and other MCP clients can call. Build with `make desktop`. See [tools/desktop/README.md](tools/desktop/README.md).

### Synthesize

Automated knowledge base maintenance — prune stale/duplicate facts and distill higher-order insights using an LLM. Trigger via the web UI or:

```
POST /api/v1/repos/{repo}/branches/{branch}/synthesis-runs
```

#### LLM Configuration

Set the model and API key via environment variables:

| Provider   | Variables |
|------------|-----------|
| Gemini     | `KNOMIT_LLM_MODEL=gemini-2.5-flash` `GOOGLE_AI_API_KEY=...` |
| Anthropic  | `KNOMIT_LLM_MODEL=claude-sonnet-4-6` `ANTHROPIC_API_KEY=...` |
| Bedrock    | `KNOMIT_LLM_MODEL=us.anthropic.claude-sonnet-4-6-v1` `AWS_ACCESS_KEY_ID=...` `AWS_SECRET_ACCESS_KEY=...` |
| Claude CLI | `KNOMIT_LLM_PROVIDER=claude-cli` — uses the `claude` CLI (no API key needed, works with Anthropic Max) |
| Gemini CLI | `KNOMIT_LLM_PROVIDER=gemini-cli` — uses the `gemini` CLI (no API key needed, works with Google AI Pro) |

The default model is `gemini-2.5-flash`. The provider is auto-detected from the model name for API providers; CLI providers must be set explicitly via `KNOMIT_LLM_PROVIDER`.

Synthesis is incremental — it only processes facts that changed since the last run. The pipeline prunes stale/duplicate facts first, then distills higher-order insights using RAPTOR (recursive abstractive processing) across multiple depth levels.

## MCP Tools

| Tool | Description |
|------|-------------|
| `knomit_learn` | Write one or more facts to the knowledge base in a single commit |
| `knomit_query` | Search by free text, entity, domain, path, or confidence threshold; use `sort=recent` to browse by recency (paginated) |
| `knomit_explain` | Traverse a fact's provenance graph via its refs (paginated BFS) |
| `knomit_update` | Revise an existing fact |
| `knomit_retract` | Remove a fact (git history retains provenance) |
| `knomit_review` | Interactive session to review and maintain the knowledge base |
| `knomit_hypothesize` | Generate hypotheses from synthesis facts |

## How it works

Facts are markdown files with YAML frontmatter:

```markdown
---
type: observation
confidence: 0.8
domain: [music]
entities: [alice]
sources: 1
refs:
  - knomit://github.com/org/repo/src/preferences.ts
---
Alice prefers rock music over jazz.
```

The `type` field classifies the kind of knowledge: `observation` (default), `concept`, `process`, `principle`, `pattern`, `reference`, `synthesis`, `insight`, `hypothesis`, or `methodology`.

The directory tree under the ontology root (`kb/` by default) forms an ontological hierarchy. Facts placed at higher levels apply to everything below them — a fact at `kb/geography/` is inherited by `kb/geography/europe/uk/london/`.

Each learning moment is an atomic git commit, giving full provenance tracking.

### Agent Identity

Each knomit instance generates an Ed25519 SSH keypair at `~/.knomit/id_ed25519`. The key fingerprint determines the agent branch name (`agent/hostname-<fingerprint>`), ensuring each machine gets its own branch. The same key is used for SSH remote auth and commit signing.

### Refs and the `knomit:` URI scheme

Refs anchor facts to their source material using the `knomit:` URI scheme:

| Form | Meaning | Example |
|------|---------|---------|
| Relative (no authority) | Current knowledge base | `knomit:/kb/technology/debugging/pool-fix.md` |
| Absolute (with host) | External repo | `knomit://github.com/org/repo/src/main.ts` |
| Plain URL | Any web resource | `https://example.com/doc` |

## HTTP API

All endpoints are under `/api/v1`. The API follows HAL+JSON — start at `GET /api/v1` and follow `_links`.

Branch names use `:` in place of `/` in URL path segments.

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/v1/repos` | List repos |
| GET | `/api/v1/repos/{repo}` | Repo detail + agent branch |
| GET | `/api/v1/repos/{repo}/branches` | List branches |
| GET | `/api/v1/repos/{repo}/branches/{branch}` | Branch detail + links |
| GET | `/api/v1/repos/{repo}/branches/{branch}/facts` | List facts |
| POST | `/api/v1/repos/{repo}/branches/{branch}/facts` | Create fact |
| GET | `/api/v1/repos/{repo}/branches/{branch}/facts/*` | Read fact |
| PUT | `/api/v1/repos/{repo}/branches/{branch}/facts/*` | Update fact |
| DELETE | `/api/v1/repos/{repo}/branches/{branch}/facts/*` | Delete fact |
| GET | `/api/v1/repos/{repo}/branches/{branch}/search` | Full-text + vector search |
| GET | `/api/v1/repos/{repo}/branches/{branch}/topics` | Browse ontology hierarchy |
| GET | `/api/v1/repos/{repo}/branches/{branch}/domains` | List domains |
| GET | `/api/v1/repos/{repo}/branches/{branch}/stats` | Knowledge base statistics |
| GET | `/api/v1/repos/{repo}/branches/{branch}/commits` | Commit history |
| GET | `/api/v1/repos/{repo}/branches/{branch}/events` | SSE event stream |
| POST | `/api/v1/repos/{repo}/branches/{branch}/synthesis-runs` | Start synthesis |
| POST | `/api/v1/repos/{repo}/branches/{branch}/index-rebuilds` | Rebuild search index |
| ALL | `/api/v1/repos/{repo}/branches/{branch}/mcp` | MCP endpoint |
| GET | `/api/v1/repos/{repo}/origin` | Get remote origin |
| PUT | `/api/v1/repos/{repo}/origin` | Set remote origin |
| DELETE | `/api/v1/repos/{repo}/origin` | Delete remote origin |
| GET | `/api/v1/openapi.yaml` | OpenAPI spec |

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `KNOMIT_HOME` | `~/.knomit` | Path to the data directory |
| `KNOMIT_REPO` | — | Alias for `KNOMIT_HOME` (backward compat) |
| `KNOMIT_HOST` | `""` | Listen host |
| `KNOMIT_PORT` | `19278` | HTTP server port |
| `KNOMIT_SOCKET` | — | Unix socket path; enables socket listener when set |
| `KNOMIT_LLM_MODEL` | `gemini-2.5-flash` | Model name for synthesis |
| `KNOMIT_LLM_PROVIDER` | auto-detected | LLM provider: `anthropic`, `gemini`, `bedrock`, `claude-cli`, `gemini-cli` |
| `KNOMIT_API_KEY` | — | LLM API key |
| `KNOMIT_LLM_CACHE` | `false` | Enable LLM prompt caching |
| `KNOMIT_LLM_BATCH` | `false` | Enable LLM batch processing |
| `KNOMIT_GIT_ORIGIN` | — | Remote origin URL (seeds default repo) |
| `KNOMIT_GIT_SERVE` | `false` | Enable git smart-HTTP at `/git` |
| `KNOMIT_GIT_PORT` | — | Port for git smart-HTTP listener |
| `KNOMIT_REMOTE_AUTH` | auto-detected | Remote auth method: `token`, `basic`, `ssh` |
| `KNOMIT_REMOTE_TOKEN` | — | Token for remote auth |
| `KNOMIT_REMOTE_USER` | — | Username for basic auth |
| `KNOMIT_REMOTE_PASSWORD` | — | Password for basic auth |
| `KNOMIT_REMOTE_SSH_KEY` | `~/.knomit/id_ed25519` | Path to SSH key |
| `ANTHROPIC_API_KEY` | — | Anthropic API key |
| `GOOGLE_AI_API_KEY` | — | Gemini API key |
| `AWS_ACCESS_KEY_ID` | — | AWS access key for Bedrock |
| `AWS_SECRET_ACCESS_KEY` | — | AWS secret key for Bedrock |
| `AWS_REGION` | `us-east-1` | AWS region for Bedrock |
| `ONNXRUNTIME_SHARED_LIBRARY` | — | Override ONNX Runtime library path |
