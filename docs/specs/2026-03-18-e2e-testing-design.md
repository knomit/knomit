# E2E Testing Framework Design

**Date:** 2026-03-18
**Branch:** feat/e2e
**Framework:** Playwright (TypeScript)
**Approach:** Single framework for UI, MCP, and API testing

## Overview

End-to-end test suite that exercises the knomit binary as a real user would — building
the binary, starting it on a random port with an isolated data directory, and running
Playwright tests against it. Three test layers (UI, MCP, API) all managed by Playwright,
with UI tests as the highest priority since they implicitly cover most API endpoints.

**Priority order:** UI → MCP → API

## Backend Change: KNOMIT_HOME

Currently `Config.RepoPath` (env: `KNOMIT_REPO`) serves as the root directory, with
`repos/` and `models/` already derived from it. However the SSH key fallback is
hardcoded to `~/.knomit/id_ed25519` in two places in main.go (lines 336, 565),
bypassing the config entirely.

**Changes:**

1. Rename `Config.RepoPath` → `Config.Home` (TOML field: `home`, env: `KNOMIT_HOME`)
2. `KNOMIT_REPO` env var becomes an alias for `KNOMIT_HOME` (backward compat)
3. Fix SSH key fallback to use `filepath.Join(cfg.Home, "id_ed25519")` instead of
   hardcoded `~/.knomit/id_ed25519`
4. Config file search: `KNOMIT_HOME` is resolved from env/default BEFORE config file
   loading. The config file cannot override `Home` (it's the search root).

| Path | Derivation |
| ---- | ---------- |
| Repos + DBs | `$KNOMIT_HOME/repos/` |
| SSH keypair | `$KNOMIT_HOME/id_ed25519` |
| ONNX model cache | `$KNOMIT_HOME/models/` |
| Config file | `$KNOMIT_HOME/knomit.toml` (searched after exe-adjacent) |

Default: `~/.knomit` — fully backward compatible since the old `RepoPath` pointed
there and `repos/`, `models/` were already derived from it via `filepath.Join`.

**Call-site updates in main.go:** Currently the code does
`filepath.Join(cfg.RepoPath, "repos")` and `filepath.Join(cfg.RepoPath, "models")`.
After the rename to `cfg.Home`, these call sites remain unchanged — `cfg.Home` replaces
`cfg.RepoPath` as the root, and the `repos/` and `models/` suffixes are still appended
by the call sites. There is no separate `Config.RepoPath` derived field — we simply
rename the field and fix the SSH fallback. This avoids any double-nesting risk.

**Migration:** `KNOMIT_REPO=/some/path` continues to work as a direct alias for
`KNOMIT_HOME`. The semantics are identical to the old behavior.

## Project Layout

```
e2e/
  package.json              # Playwright + TypeScript deps
  tsconfig.json
  playwright.config.ts      # Headless Chromium default, all browsers in CI
  global-setup.ts           # Build binary, start shared instance, seed fixture
  global-teardown.ts        # Kill process, clean temp dir
  fixtures/
    knomit.ts               # Playwright fixtures (shared instance, fresh instance, API helpers)
    seed.ts                 # Seed data definitions + seeding logic
  helpers/
    mcp-client.ts           # MCP protocol helper (HTTP streamable transport + SSE parsing)
  pages/
    browse.page.ts          # BrowsePage — directory nav, breadcrumbs, search
    fact-panel.page.ts      # FactPanel — view/edit/delete facts
    history.page.ts         # HistoryTimeline — commits, pagination
  tests/
    ui/
      browse.spec.ts        # Directory navigation, breadcrumbs, expand/collapse
      fact-view.spec.ts     # View fact, markdown rendering, frontmatter display, historical commit
      fact-edit.spec.ts     # Create/update/delete facts (fresh instance)
      search.spec.ts        # Full-text search, entity/domain filters, empty results
      recent.spec.ts        # Recent facts list, search within recent, clear button
      history.spec.ts       # Timeline rendering, pagination, click commit → view commit detail
      origin.spec.ts        # Remote config modal (fresh instance)
      status-bar.spec.ts    # Index status (/status endpoint), sync indicators
      repo-selector.spec.ts # Multi-repo switching
    mcp/
      learn.spec.ts         # knomit_learn tool
      query.spec.ts         # knomit_query tool
      update.spec.ts        # knomit_update tool
      explore.spec.ts       # knomit_explore (session-based queue)
      explain.spec.ts       # knomit_explain tool
      retract.spec.ts       # knomit_retract tool
      review.spec.ts        # knomit_review tool (always listed; verify error when no LLM configured)
      profiles.spec.ts      # Profile-specific tool sets, default-to-code, unknown profile fallback
    api/
      activity.spec.ts      # Activity endpoint
      search.spec.ts        # Search edge cases not covered by UI
      history.spec.ts       # Cursor-based pagination
      commit.spec.ts        # Commit detail endpoint
      stats.spec.ts         # Aggregate stats endpoint
      events.spec.ts        # SSE event stream
      repos.spec.ts         # Multi-repo listing
      rebuild.spec.ts       # Rebuild endpoint (async task + SSE monitoring)
      synthesize.spec.ts    # Synthesize endpoint (async task + SSE progress events)
      origin.spec.ts        # Origin CRUD (get/set remote config, validation, error cases)
      recent.spec.ts        # Recent endpoint edge cases (query params, empty state)
      status.spec.ts        # Status response shape (head commit, branch, embeddings flag, ontology root)
      openapi.spec.ts       # OpenAPI spec serves correctly
    journeys/
      knowledge-lifecycle.spec.ts  # Create → edit → search → find → view history → retract
      browse-and-discover.spec.ts  # Navigate tree → select fact → check entities → search by entity
      synthesis-flow.spec.ts       # Create overlapping facts → synthesize → monitor console → verify
      multi-repo.spec.ts           # Switch repo → create fact → switch back → verify isolation
      remote-setup.spec.ts         # Init fresh → configure origin → verify status
```

**Explicitly excluded:** `/git` smart HTTP remote (different protocol, not user-facing),
`/docs` Swagger UI (static asset serving, low value), SPA static asset routes
(`/assets/*`, `/*` fallback — framework-level routing, not application logic).

## Binary Lifecycle

### Port Discovery

The test setup uses a free-port finder (e.g. `get-port` npm package) to discover an
available port BEFORE starting the binary, then passes it via `KNOMIT_PORT`. This
avoids needing backend changes to expose the bound port.

### ONNX Model Handling

On first startup with a fresh `KNOMIT_HOME`, the binary downloads the ONNX embedding
model to `$KNOMIT_HOME/models/`. For test speed and CI reliability:
- Global setup pre-populates the model cache by copying from a known location
  (e.g., `dist/models/` or the developer's `~/.knomit/models/`)
- If the model is unavailable, the server starts with embeddings disabled — search
  tests that depend on vector similarity should skip gracefully

### Shared Instance (global setup)

1. `make dist` — build binary + ONNX libs (once per run)
2. Find a free port via `get-port`
3. Create temp dir: `/tmp/knomit-e2e-<run-id>/`
4. Optionally copy ONNX model cache into temp dir's `models/`
5. Start binary with:
   - `KNOMIT_HOME=/tmp/knomit-e2e-<run-id>`
   - `KNOMIT_PORT=<discovered-port>`
   - `ONNXRUNTIME_SHARED_LIBRARY=<abs-path-to>/dist/lib/<platform-lib>`
6. Wait for healthy: poll `GET /api/v1/repos` until 200 (timeout: 60s to account for
   git init + index build + optional model download). Note: repo initialization is
   synchronous before `ListenAndServe`, so a 200 response means repos are fully loaded.
7. Seed fixture data via `PUT /api/v1/knomit/fact`
8. Export base URL for tests via env file

### Fresh Instance Fixture

A Playwright fixture `freshKnomit` that:
- Finds a free port
- Creates its own temp `KNOMIT_HOME`
- Copies ONNX model cache if available
- Starts a knomit process
- Waits for healthy (60s timeout)
- Returns `{ baseURL, apiContext }` to the test
- Kills process + cleans temp dir on teardown

Used by: `fact-edit.spec.ts`, `origin.spec.ts`, all journey specs, all MCP specs.

## Fixture / Seed Data

Seeded via `PUT /api/v1/knomit/fact` calls in `global-setup.ts`. Each call writes a
complete markdown file (frontmatter YAML + body) to a given path. The seed helper
constructs valid frontmatter including domain, entities, confidence, refs, and sources.

Designed to cover:

- Multiple domains (at least 3 directories under ontology root)
- Facts at different tree depths (root, 1-level, 2-level)
- Varying confidence levels (0.3, 0.6, 0.9)
- Facts with entities, refs, and sources
- Multiple commits — seed in sequential batches with explicit waits between batches
  to create distinct commits for history testing (each PUT creates its own git commit)
- Enough facts for meaningful search results (20-30 facts)

The shared fixture is **read-mostly** — browse, search, recent, history, and status
tests do not mutate it.

## MCP Test Helper

HTTP client that speaks the MCP JSON-RPC protocol over streamable HTTP transport.
Responses may arrive as SSE event streams (not plain JSON), so the client must parse
SSE `data:` lines and extract JSON-RPC results.

```typescript
class McpClient {
  // URL constructed as: ${baseURL}/api/v1/${repo}/mcp?profile=${profile}
  constructor(baseURL: string, repo: string, profile?: string)
  async initialize(): Promise<void>   // POST initialize, capture Mcp-Session-Id header
  async listTools(): Promise<Tool[]>  // POST tools/list with session header
  async callTool(name: string, args: Record<string, unknown>): Promise<ToolResult>
  async close(): Promise<void>
}
```

- Manages `Mcp-Session-Id` header from initialize response
- Sends JSON-RPC envelopes via POST
- Parses SSE event stream responses (mcp-go uses SSE for streaming)
- No external MCP SDK dependency

## UI Testing Strategy

### Fact Deletion

There is no `DELETE /api/v1/{repo}/fact` REST endpoint. Fact deletion in the UI triggers
the MCP `knomit_retract` tool or uses the fact write API to remove the file. Tests that
exercise delete should verify the actual mechanism the frontend uses.

### Page Objects

Thin wrappers for the three main UI areas (BrowsePage, FactPanel, HistoryTimeline).
Encapsulate selectors and common interactions. Keep them minimal — add methods as
tests need them, not speculatively.

### Network Assertions

For key flows, assert the correct API calls were made using `page.waitForResponse`.
This bridges UI and API testing — verifies both UI behavior and correct API usage.

### Journey Tests

Multi-step flows that chain operations as a real user would. Serial within each spec
(each step depends on the prior). Use fresh instances since they mutate state heavily.

## Playwright Configuration

- **Browsers:** Chromium only locally, all three (Chromium/Firefox/WebKit) in CI
- **Parallelism:** Read-only specs against the shared fixture (browse, search, recent,
  history, status-bar, repo-selector) can run in parallel. Fresh-instance specs and
  journey tests run serially.
- **Headless:** Default yes, `--headed` flag for debugging
- **Artifacts:** Screenshots on failure, trace recording in CI
- **Timeouts:** 60s for server startup, 30s for navigation/actions

## Makefile Additions

```makefile
e2e: dist
	cd e2e && npx playwright test

e2e-ui: dist
	cd e2e && npx playwright test --headed

e2e-setup:
	cd e2e && npm ci && npx playwright install
```

## CI Considerations (design only)

- Pipeline: `make setup && make dist && make e2e-setup && make e2e`
- Playwright Docker images with pre-installed browsers
- Platform matrix: macOS (arm64, x86_64), Linux (x64)
- Traces + screenshots uploaded as artifacts on failure
- ONNX libs and model cache cached between runs
