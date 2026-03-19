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

Add a `KNOMIT_HOME` env var / config field that becomes the single root for all knomit
data. Everything else derives from it:

| Path | Derivation |
|------|-----------|
| Repos + DBs | `$KNOMIT_HOME/repos/` |
| SSH keypair | `$KNOMIT_HOME/id_ed25519` |
| ONNX model cache | `$KNOMIT_HOME/models/` |
| Config file | `$KNOMIT_HOME/knomit.toml` (searched after exe-adjacent) |

- Default: `~/.knomit` (backward compatible)
- `KNOMIT_REPO` still works as an override for the repos path specifically
- `Config.Home` field added to the struct, layered like other fields: default → TOML → env

This is the only backend change needed to fully isolate test instances.

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
    mcp-client.ts           # Thin MCP protocol helper (HTTP streamable transport)
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
      history.spec.ts       # Timeline rendering, pagination, click commit
      origin.spec.ts        # Remote config modal (fresh instance)
      status-bar.spec.ts    # Index status, sync indicators
      repo-selector.spec.ts # Multi-repo switching
    mcp/
      learn.spec.ts         # knomit_learn tool
      query.spec.ts         # knomit_query tool
      update.spec.ts        # knomit_update tool
      explore.spec.ts       # knomit_explore (session-based queue)
      explain.spec.ts       # knomit_explain tool
      retract.spec.ts       # knomit_retract tool
      profiles.spec.ts      # Profile-specific tool sets and instructions
    api/
      search.spec.ts        # Search edge cases not covered by UI
      history.spec.ts       # Cursor-based pagination
      stats.spec.ts         # Aggregate stats endpoint
      events.spec.ts        # SSE event stream
      repos.spec.ts         # Multi-repo listing
      openapi.spec.ts       # OpenAPI spec serves correctly
    journeys/
      knowledge-lifecycle.spec.ts  # Create → edit → search → find → view history → retract
      browse-and-discover.spec.ts  # Navigate tree → select fact → check entities → search by entity
      synthesis-flow.spec.ts       # Create overlapping facts → synthesize → monitor console → verify
      multi-repo.spec.ts           # Switch repo → create fact → switch back → verify isolation
      remote-setup.spec.ts         # Init fresh → configure origin → verify status
```

## Binary Lifecycle

### Shared Instance (global setup)

1. `make dist` — build binary + ONNX libs (once per run)
2. Create temp dir: `/tmp/knomit-e2e-<run-id>/`
3. Start binary with:
   - `KNOMIT_HOME=/tmp/knomit-e2e-<run-id>`
   - `KNOMIT_PORT=0` (or find a free port)
   - `ONNXRUNTIME_SHARED_LIBRARY=dist/lib/<platform-lib>`
4. Wait for healthy: poll `GET /api/v1/repos` until 200
5. Seed fixture data via `PUT /api/v1/knomit/fact` — known facts across multiple
   domains, entities, confidence levels, and directory depths
6. Export base URL for tests

### Fresh Instance Fixture

A Playwright fixture `freshKnomit` that:
- Creates its own temp `KNOMIT_HOME`
- Starts a knomit process on a different port
- Returns `{ baseURL, apiContext }` to the test
- Kills process + cleans temp dir on teardown

Used by: `fact-edit.spec.ts`, `origin.spec.ts`, all journey specs, all MCP specs.

## Fixture / Seed Data

Seeded via API calls in `global-setup.ts`. Designed to cover:

- Multiple domains (at least 3 directories under ontology root)
- Facts at different tree depths (root, 1-level, 2-level)
- Varying confidence levels (0.3, 0.6, 0.9)
- Facts with entities, refs, and sources
- Multiple commits (seed in batches to create history)
- Enough facts for meaningful search results (20-30 facts)

## MCP Test Helper

Thin HTTP client that speaks the MCP JSON-RPC protocol:

```typescript
class McpClient {
  constructor(baseURL: string, repo: string, profile?: string)
  async initialize(): Promise<void>
  async listTools(): Promise<Tool[]>
  async callTool(name: string, args: Record<string, unknown>): Promise<ToolResult>
  async close(): Promise<void>
}
```

- Manages session ID from initialize response
- Sends proper JSON-RPC envelope with `tools/call` method
- Parses result content from response
- No external MCP SDK dependency — it's just HTTP + JSON

## UI Testing Strategy

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
- **Parallelism:** Serial (no `workers` parallelism) — thoroughness over speed
- **Headless:** Default yes, `--headed` flag for debugging
- **Artifacts:** Screenshots on failure, trace recording in CI
- **Timeouts:** Generous (server startup, ONNX model load can be slow)

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

- Pipeline: `make setup && make dist && make e2e`
- Playwright Docker images with pre-installed browsers
- Platform matrix: macOS (arm64, x86_64), Linux (x64)
- Traces + screenshots uploaded as artifacts on failure
- ONNX libs cached between runs
