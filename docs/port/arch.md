# Knomit Go Rewrite — Architecture

## 1. Overview

The TypeScript/Bun implementation of Knomit spawns one MCP stdio process per AI client. Each process drives git operations via subprocess calls to the system `git` binary, sharing the same repository on disk. Under concurrent load this produces `.lock` file contention, merge conflicts from racing writes, and requires exponential back-off retry logic baked into every git call. The Go rewrite eliminates this by collapsing all clients into a single long-running HTTP service. One process owns the repository and exposes the MCP protocol over Streamable HTTP transport instead of stdio. A built-in web UI replaces the terminal TUI.

Git objects (commits, trees, blobs, refs) are stored in a SQLite database (`knomit.git.db`) via a custom go-git storer implementation, rather than in a `.git/` directory on disk. SQLite's WAL mode provides concurrent reads without any application-level locking, and atomic transactions replace the fragile multi-file writes of the filesystem git format. A separate SQLite database (`knomit.index.db`) holds the full-text and vector search index. The result is a single binary backed by two database files, serving any number of AI clients simultaneously without contention.

---

## 2. Repository Structure

```
cmd/knomit/             — main entry point; wires everything together, runs cobra CLI
internal/config/        — config struct, env var parsing (KNOMIT_REPO, KNOMIT_PORT, etc.)
internal/gitstorer/     — SQLite-backed go-git storer (objects, refs, kv tables); implements
                          go-git's storer.Storer interface over knomit.git.db
internal/git/           — GitStore: go-git wrapper with logical-sequence mutex, all repo ops
internal/store/         — SearchIndex: SQLite FTS5 + sqlite-vec, incremental sync (knomit.index.db)
internal/embeddings/    — Embedder: ONNX Runtime via yalue/onnxruntime_go; WordPiece tokenizer
internal/cluster/       — UMAP port + HDBSCAN port + FCA metadata splitting
internal/mcp/           — MCP tool handlers (learn, query, why, update, explore, forget)
internal/synthesize/    — recipe parsing, prune/distill step execution, RAPTOR recursion
internal/llm/           — LLMAdapter interface + Anthropic, Gemini, Bedrock, CLI implementations
internal/web/           — chi HTTP handlers for REST API; serves embedded Web UI assets
web/                    — Web UI source (React + Vite); output of `npm run build` is embedded
```

Package naming follows the standard Go convention of short, lowercase, non-plural names. Internal packages are not exported; only `cmd/knomit` is a `main` package.

### TypeScript Artifacts Removed

When the Go port is complete, the following are deleted in a single commit:

- `src/` — entire TypeScript source tree
- `package.json`, `tsconfig.json`, `bun.lockb` — Bun/TypeScript project files at the repo root
- `docs/plans/` — prior TypeScript implementation plans, superseded by `docs/port/`

The following are retained:

- `spec/` — MBEKG specification
- `docs/port/` — Go architecture and tool specs
- `web/` — React frontend (has its own `package.json` for the Vite build, but is part of the Go project, not the TS project)
- `tools/` — development utilities (re-implemented in Go; see Section 18)

---

## 3. Concurrency Model

### SQLite WAL as the Primary Concurrency Mechanism

Because git objects are stored in SQLite (`knomit.git.db`) with WAL journal mode enabled, concurrent reads require no application-level locking at all. SQLite's WAL allows unlimited simultaneous readers while a write is in progress, and its internal write serialisation means two goroutines can never corrupt an object by writing simultaneously — SQLite will return `SQLITE_BUSY` and retry transparently.

This eliminates the need for a mutex around individual git object operations. Reads (`ReadFile`, `ListDir`, `Log`, `Grep`, etc.) go directly to SQLite with no locking overhead.

### Mutex for Logical Sequences

`GitStore` holds a single `sync.Mutex` named `mu` used exclusively for **logical-sequence atomicity** — multi-step operations that must not be interleaved:

- `Sync` (fetch remote objects → merge into agent branch): must be atomic so two concurrent syncs don't produce conflicting merge commits.
- A full tool call sequence (sync → commit N facts → tag → push) in `knomit_learn`, `knomit_update`, `knomit_forget`: held for the entire sequence so another tool call's sync doesn't race with an in-progress commit sequence.
- **Git remote `git push`** (ReceivePackSession): object receipt does **not** acquire the mutex — objects are content-addressed (hash-keyed) and idempotent, so SQLite WAL serialisation is sufficient. Only the final ref update phase (writing the new branch tip and triggering post-receive) acquires the mutex, ensuring a push cannot interleave with a concurrent tool call that is mid-sequence on the same branch.

Individual write operations within a sequence (each `Commit`, each `Tag`) do not acquire additional locks beyond the outer sequence mutex and SQLite's internal write serialisation.

### Synthesize Pipeline

The synthesize pipeline holds the mutex for each coarse phase (checkout branch, each commit run, merge/push). LLM calls — which dominate pipeline time — happen outside the mutex, allowing concurrent MCP tool calls to proceed during synthesis.

### No Queue

Go's HTTP server handles each request in its own goroutine. Mutex back-pressure is sufficient for the expected concurrency (a handful of AI clients). A priority work queue can be added later if needed.

---

## 4. Service Lifecycle

### Startup

`knomit serve` is the primary subcommand. On startup it:

1. Resolves the repo path (from `--repo` flag or `KNOMIT_REPO` env var, defaulting to `~/.knomit`).
2. Initialises `GitStore`: opens or creates `knomit.git.db` (the SQLite-backed git object store), ensures the agent branch (`agent/<hostname>`) exists.
3. Resolves the SQLite cache directory (`~/.cache/knomit` or `KNOMIT_CACHE_DIR`).
4. Initialises `SearchIndex`: opens or creates `knomit.index.db`, runs schema migrations, conditionally loads the sqlite-vec extension and ONNX model.
5. Performs an initial `SearchIndex.Sync(gitStore)` to catch up the index to HEAD.
6. Wires chi routes and starts `http.ListenAndServe` on the configured port (default 3000, overridden by `--port` or `KNOMIT_PORT`).
7. Logs the listening address and MCP endpoint URL.

### Graceful Shutdown

On `SIGINT` / `SIGTERM`, the server calls `http.Server.Shutdown(ctx)` with a 30-second deadline. In-flight requests complete; new requests are rejected. After HTTP drain, `SearchIndex.Close()` and `GitStore.Close()` are called to flush SQLite WAL and release file locks.

### Other Subcommands

- `knomit init` — initialise a new repo at the given path without starting the server.
- `knomit synthesize <recipe.yaml>` — run a recipe against a running server (or directly if `--no-server` is passed, operating on the repo directly).
- `knomit rebuild` — rebuild the search index from scratch, then exit.

---

## 5. HTTP API Surface

All routes are registered with go-chi. The Web UI is a SPA served from `GET /` (and any unknown path via a catch-all). The MCP endpoint is a single path. REST endpoints power the Web UI.

### MCP

| Method | Path   | Description                              |
|--------|--------|------------------------------------------|
| POST   | `/mcp` | Streamable HTTP MCP transport (all sessions) |

The profile (code / chat / generic) is selected via a query parameter: `POST /mcp?profile=code`. If omitted, `code` is the default.

### Web UI

| Method | Path  | Description                                          |
|--------|-------|------------------------------------------------------|
| GET    | `/`   | Serves the embedded `web/dist/index.html`            |
| GET    | `/assets/*` | Serves embedded static assets (JS, CSS)       |

### REST API (used by Web UI)

| Method | Path                          | Description                                              |
|--------|-------------------------------|----------------------------------------------------------|
| GET    | `/api/browse`                 | List children at `?path=`; mirrors `knomit_explore`      |
| GET    | `/api/fact`                   | Read a single fact at `?path=`; returns frontmatter + body |
| GET    | `/api/search`                 | Hybrid FTS5+vector search; params: `q`, `entities`, `domain`, `path`, `min_confidence` |
| GET    | `/api/history`                | Git log for `?path=`; returns array of log entries       |
| GET    | `/api/stats`                  | Aggregate stats for `?path=` (total facts, domain distribution, avg confidence) |
| GET    | `/api/status`                 | Server health: HEAD commit hash, index last_commit, embeddings enabled, branch name |
| POST   | `/api/synthesize`             | Trigger a synthesis recipe (body: recipe YAML)           |
| GET    | `/api/synthesize/:recipe`     | Poll synthesis run status (progress events via SSE)      |

The REST API returns JSON. The synthesize progress stream uses Server-Sent Events.

---

## 6. MCP Transport

### Library

`github.com/mark3labs/mcp-go` is used for the MCP server. It supports Streamable HTTP transport (the current MCP spec) in addition to stdio. The `mcp.NewServer` is configured with tool registrations, then wrapped in an `http.Handler` that is mounted at `/mcp`.

### Session Handling

Each `POST /mcp` request carries a session ID in the `Mcp-Session-Id` header (assigned by the client on first contact). mcp-go manages session state internally. Multiple concurrent sessions share the same tool handler instances, which are stateless (all state lives in `GitStore` and `SearchIndex`).

### Profile Selection

The `?profile=` query parameter on `/mcp` selects the instruction addendum appended to the base system prompt. The `internal/mcp` package exposes a `ProfileInstructions(profile string) string` function that returns the appropriate addendum. Valid values are `code` (default), `chat`, and `generic`. The profile is resolved at request time when constructing the `mcp.Server` options passed to the handler, or by injecting the instructions into the `knomit-save` prompt resource that is registered on the server.

---

## 7. GitStore

### Responsibility

`GitStore` is the single owner of the git repository. All access to repo state goes through it. No other package imports go-git directly.

### SQLite-Backed go-git Storer (`internal/gitstorer`)

Git objects are not stored in a `.git/` directory. Instead, `internal/gitstorer` implements go-git's `storage.Storer` interface over a SQLite database (`knomit.git.db`). The schema:

```sql
-- git objects: blobs, trees, commits, tags
CREATE TABLE objects (
    hash    TEXT NOT NULL,
    type    INTEGER NOT NULL,  -- plumbing.ObjectType (1=commit,2=tree,3=blob,4=tag)
    size    INTEGER NOT NULL,
    data    BLOB NOT NULL,
    PRIMARY KEY (hash, type)
);

-- git references: branches, tags, HEAD
CREATE TABLE refs (
    name       TEXT PRIMARY KEY,
    target     TEXT NOT NULL,
    is_symbolic INTEGER NOT NULL DEFAULT 0
);

-- catch-all KV for index, config, shallow, modules
CREATE TABLE kv (
    key   TEXT PRIMARY KEY,
    value BLOB NOT NULL
);
```

This is sufficient to implement all of go-git's storer interfaces: `EncodedObjectStorer` (objects table), `ReferenceStorer` (refs table), `IndexStorer` + `ConfigStorer` + `ShallowStorer` + `ModuleStorer` (kv table with namespaced keys).

go-git's conformance test suite (`storage/test/storage_suite.go`) is run against `gitstorer` as part of the test suite to verify correctness.

### No Working Tree on Disk

Knomit never reads or writes fact files through the filesystem. All reads use go-git's object API (traverse commit → tree → blob). All writes use go-git's plumbing API (create blob → build tree → create commit → update ref). A `memfs.New()` (from `go-billy/v5/memfs`) is passed as the worktree to go-git's `Open`/`Init` calls to satisfy the interface; it is never used directly by knomit code.

The `sync` operation (fetch + merge) does write to the memfs worktree internally via go-git. Since the result is only needed in the git object store (already in SQLite), the memfs contents are discarded after sync.

### go-git Operations

- **Open/Init**: `git.Open(storer, memfs)` / `git.Init(storer, memfs)` where `storer` is the SQLite storer.
- **Write fact**: create blob object → get HEAD tree → build modified tree → create commit object → update branch ref.
- **Delete fact**: same as write, but remove the file from the tree.
- **Tag**: create a lightweight tag ref in the `refs` table.
- **Sync**: `remote.Fetch` (writes fetched objects to SQLite) → three-way merge commit into agent branch.
- **Push**: `remote.Push` (reads objects from SQLite, sends over git wire protocol).
- **Log**: traverse commit chain from HEAD ref through parent commit objects in SQLite.
- **ReadFile**: resolve HEAD → tree → blob by path; read blob data from SQLite.
- **ListDir**: resolve HEAD → tree; list entries at path prefix.
- **Grep**: walk tree objects from HEAD, read each blob, apply regex — no filesystem needed.
- **DiffFiles**: compare two commit trees using go-git's `object.DiffTree`.
- **TagsContaining**: iterate refs table for `refs/tags/`, check if commit is reachable.

go-git operates entirely in-process; no subprocess calls are made.

### Sync Semantics

`GitStore.Sync()` replicates the TypeScript `sync()` behaviour:

1. Acquire the logical-sequence mutex.
2. Skip if no remote `origin`.
3. `remote.Fetch` — writes fetched objects into `knomit.git.db`.
4. Check if `origin/main` exists; if not, return `{Synced: false}`.
5. Count commits in `origin/main` not in the agent branch; if zero, return `{Synced: false}`.
6. Perform three-way merge commit into agent branch. On conflict, abort and return conflict details.
7. Return `{Synced: true}`.

### Path Validation

All paths from external callers are validated to be within the `know/` subtree. Path traversal (`../`) is rejected.

### Schema Versioning

The `kv` table stores `schema_version` as a plain integer under the key `"schema_version"`. On startup, `gitstorer.Open` reads the stored version and runs any pending migration functions in sequence. Migrations are append-only and indexed by version number. If no version key exists, the database is freshly initialised and written at version 1 as part of init. The current baseline is version 1.

### Init Behaviour

On first init of a new repo, `GitStore.Init()`:

1. Creates `knomit.git.db` via `gitstorer.New`.
2. `git.Init(storer, memfs)` with default branch `main`.
3. Creates initial commit containing `know.md` (root manifest).
4. Creates and checks out `agent/<hostname>` branch.
5. Stores `user.email = knomit@local`, `user.name = knomit`, `commit.gpgsign = false` in the config KV.

---

## 8. SearchIndex

### Two Separate Databases

The git object store (`knomit.git.db`) and the search index (`knomit.index.db`) are kept as separate SQLite files. They have different access patterns (git objects are append-mostly and immutable; the index is fully mutable and rebuildable), different lifetimes (index can be deleted and rebuilt without touching history), and different vacuum behaviour. Both use WAL mode.

### SQLite Schema

```sql
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE synthesis_log (
    recipe          TEXT PRIMARY KEY,
    last_commit     TEXT NOT NULL,
    run_at          TEXT NOT NULL,
    facts_processed INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE facts (
    path        TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    body        TEXT NOT NULL,
    domain      TEXT NOT NULL,   -- JSON array
    entities    TEXT NOT NULL,   -- JSON array
    confidence  REAL NOT NULL,
    sources     INTEGER NOT NULL,
    refs        TEXT NOT NULL,   -- JSON array
    commit_hash TEXT NOT NULL
);

CREATE VIRTUAL TABLE facts_fts USING fts5(
    title, body, entities, domain,
    content='facts',
    content_rowid='rowid'
);

CREATE VIRTUAL TABLE facts_vec USING vec0(
    path      TEXT PRIMARY KEY,
    embedding FLOAT[768]
);
```

The `meta` table stores the following keys:

| Key              | Value           | Description                                           |
| ---------------- | --------------- | ----------------------------------------------------- |
| `schema_version` | integer string  | Index schema version; drives migration logic          |
| `last_commit`    | git commit hash | HEAD hash at last successful index sync               |
| `embed_model`    | string          | Embedding model name (e.g. `nomic-embed-text-v1.5`)   |
| `embed_dim`      | integer string  | Vector dimension of stored embeddings (e.g. `768`)    |

On `SearchIndex.Open`, after running schema migrations, the stored `embed_model` and `embed_dim` are compared against the currently configured embedder. If either differs, the vector index is considered stale: all rows are deleted from `facts_vec`, `embed_model` and `embed_dim` are updated to the new values, and `last_commit` is cleared to force a full re-index on the next `Sync` call. A warning is logged: `"embedding model changed from X to Y; vector index cleared, rebuilding"`. The FTS5 index is unaffected — only the vector data needs rebuilding.

### FTS5 Manual Content Sync

Because the FTS5 table is a content-shadow table (`content='facts'`), inserts and updates to `facts` do not automatically propagate. The `SearchIndex.Upsert` method manually:

1. Fetches the existing FTS row by rowid (if any) and issues a `'delete'` command to the FTS index.
2. Upserts the `facts` row.
3. Fetches the new rowid and inserts into the FTS index.

This is wrapped in a transaction. Vector embedding is computed and stored after the transaction commits (non-critical, errors are logged and ignored).

### Hybrid Search

When a text query is present and embeddings are enabled:

1. Run FTS5 BM25 search: `SELECT ... FROM facts_fts WHERE facts_fts MATCH ? ORDER BY rank LIMIT N`.
2. Embed the query text using the ONNX model.
3. Run vector ANN search: `SELECT path, distance FROM facts_vec WHERE embedding MATCH ? ORDER BY distance LIMIT N`.
4. Normalize BM25 scores (negative, closer to 0 is better) to 0–1 range.
5. Normalize cosine distances (0 = identical, 1 = orthogonal) to similarity.
6. Combine: `score = 0.6 * normBM25 + 0.4 * (1 - cosineDistance)`.
7. Merge FTS and vector result sets, include vec-only results with distance < 0.8.
8. Sort descending by combined score.
9. Normalize final top-N scores to 0–100 range; filter out results below 10.

When embeddings are disabled (sqlite-vec not loaded, or ONNX model not present), only FTS5 BM25 is used.

### Index Schema Versioning

The `meta` table stores `schema_version` as a string integer. On startup, `SearchIndex.Open` reads it and runs pending migrations in sequence. Because the index is fully rebuildable (`knomit rebuild`), destructive migrations (dropping and recreating a virtual table) are acceptable without data loss. The current baseline is version 1.

### Incremental Sync

`SearchIndex.Sync(gitStore)` compares `meta.last_commit` to the current `HEAD`:

- If no `last_commit` stored → full rebuild.
- If `last_commit == HEAD` → no-op (returns `false`).
- Otherwise → `gitStore.DiffFiles(lastCommit)` returns added/modified/deleted paths. Added and modified `.md` files are re-indexed; deleted paths are removed. `last_commit` is updated to `HEAD`.

### Extension Loading (CGO)

go-sqlite3 is a CGO library. The sqlite-vec extension is a shared library (`.so` / `.dylib` / `.dll`) loaded at runtime via `db.LoadExtension`. On macOS, the system SQLite lacks extension support; the binary must link against Homebrew's SQLite or bundle a custom build. This is handled at startup: the binary searches known paths (bundled lib dir, Homebrew prefix), calls `sqlite3.Version()` to confirm, and logs a warning if extension support is unavailable (embeddings degrade gracefully to FTS5-only).

---

## 9. Embeddings

### Model

nomic-embed-text-v1.5 (768-dimensional float32 embeddings). Chosen over all-MiniLM-L6-v2 for its 8192-token context window (vs 256 tokens), which ensures full fact content is embedded without truncation. The ONNX model file and `tokenizer.json` (HuggingFace format) are distributed alongside the binary (bundled into a `data/` directory, or downloaded on first use to the cache dir).

### ONNX Runtime

`github.com/yalue/onnxruntime_go` wraps the ONNX Runtime C API. The native `libonnxruntime` shared library must be locatable at runtime. The binary resolves the path from a relative `data/lib/` directory next to the binary, or from an embedded path configured at build time via `ldflags`. `ONNXRUNTIME_SHARED_LIBRARY` can override detection.

### Tokenizer

`github.com/daulet/tokenizers` wraps the HuggingFace tokenizers Rust library via CGO. It reads `tokenizer.json` directly, handling nomic-embed-text's BPE tokenizer without any hand-ported tokenizer logic. The native `libtokenizers` shared library is bundled in `data/lib/` alongside `libonnxruntime`.

### Inference

`Embedder.Embed(text string) ([]float32, error)` tokenizes the input using `daulet/tokenizers`, runs inference with the resulting `input_ids` and `attention_mask` tensors, performs mean-pooling over the `last_hidden_state` output, and L2-normalises the result. nomic-embed-text-v1.5 does not use `token_type_ids`.

---

## 10. Clustering

### UMAP Port

UMAP is ported from `umap-js` (MIT). The port lives in `internal/cluster/umap.go`. Key math dependencies from `gonum.org/v1/gonum`:

- `gonum.org/v1/gonum/mat` for dense matrix operations.
- `gonum.org/v1/gonum/stat` for nearest-neighbour computations.
- `gonum.org/v1/gonum/optimize` for the embedding optimisation loop.

Parameters mirror the TypeScript call site: `nComponents` (default 5), `nNeighbors` (min(15, count/2)), `minDist` (0.1), `spread` (1.0).

### HDBSCAN Port

HDBSCAN is ported from `hdbscan-ts` (MIT). The port lives in `internal/cluster/hdbscan.go`. The minimum spanning tree computation uses `gonum.org/v1/gonum/graph/spanning` (Prim's algorithm). Parameters: `MinClusterSize` (default 3), `MinSamples` (defaults to `MinClusterSize`). Labels are int; `-1` denotes noise.

### FCA Metadata Splitting

After HDBSCAN produces raw cluster labels, each cluster is passed through `SplitByMetadata`, a Union-Find algorithm that splits a cluster into connected components based on shared `domain` or `entities` tags. Components below `MinClusterSize` are reclassified as noise. This is a direct port of the TypeScript `splitByMetadata` function.

### Entry Point

`cluster.ClusterFacts(facts []FactForLLM, embeddings map[string][]float32, opts ClusterOptions) ClusterResult` is the single public function. It returns `ClusterResult{Clusters map[int][]FactForLLM, Noise []FactForLLM}`.

---

## 11. LLM Adapters

### Interface

```go
type Message struct {
    Role    string // "user" | "assistant"
    Content string
}

type LLMAdapter interface {
    Complete(ctx context.Context, system string, messages []Message, onChunk func(string)) (string, error)
}
```

`onChunk` is optional (nil disables streaming). When non-nil, partial text is forwarded to the caller as it arrives; the method still returns the full accumulated text on completion.

### Implementations

- **Anthropic** (`internal/llm/anthropic.go`): uses `github.com/anthropics/anthropic-sdk-go`. Supports streaming via the SDK's streaming API. Reads `ANTHROPIC_API_KEY`.
- **Gemini** (`internal/llm/gemini.go`): uses `github.com/google/generative-ai-go`. Reads `GOOGLE_AI_API_KEY`.
- **Bedrock** (`internal/llm/bedrock.go`): uses `github.com/aws/aws-sdk-go-v2/service/bedrockruntime`. Reads `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`. Wraps the Claude-on-Bedrock API.
- **claude-cli** (`internal/llm/claudecli.go`): runs `claude` subprocess, pipes stdin/stdout. Matches TypeScript behaviour.
- **gemini-cli** (`internal/llm/geminicli.go`): runs `gemini` subprocess, pipes stdin/stdout.

### Provider Resolution

`llm.ResolveProvider(model string, explicit string) (string, error)` infers the provider from the model prefix (`claude-*` → anthropic, `gemini-*` → gemini, `anthropic.*` / `us.*` / `eu.*` → bedrock) or uses the explicit override. Returns an error for unrecognised model names without an explicit provider.

### Config

`llm.ConfigFromEnv()` reads `KNOMIT_LLM_PROVIDER`, `KNOMIT_LLM_MODEL` (default `claude-sonnet-4-6`), and the provider-specific credentials. Returns an `LLMConfig` struct used by `llm.NewAdapter(cfg)`.

---

## 12. Synthesize Pipeline

### Recipe Format

Recipes are YAML files validated against the `Recipe` struct:

```
name:       string (required)
prompt:     string (optional, recipe-level context)
scope:      optional; if absent, auto-discovery mode is used
  domain:   []string
  entities: []string
  search:   []string
  path:     string
auto_merge: bool (default false)
steps:
  - mode:             "prune" | "distill"
    model:            string (optional, overrides global LLM)
    prompt:           string (optional, step-level instructions)
    max_depth:        int (distill only, default 1, RAPTOR recursion depth)
    umap_dimensions:  int (distill only, default 5)
    min_cluster_size: int (distill only, default 3)
```

### Pipeline Execution

`synthesize.Run(ctx, gitStore, searchIndex, recipe, onProgress)`:

1. Record the original branch.
2. Delete any stale `synthesize/<name>` branch from a previous interrupted run.
3. Checkout a new `synthesize/<name>` branch (acquires mutex).
4. For each step:
   a. Re-index `SearchIndex` from the git repo (picks up prior step's commits).
   b. Gather facts: either by scope query or by delta since last synthesis run.
   c. Dispatch to `executePruneStep` or `executeDistillStep`.
5. Record synthesis run in `synthesis_log`.
6. If `auto_merge`: checkout original branch, merge, delete synthesis branch.
7. Else: push synthesis branch to origin, checkout original branch.

### Prune Step

1. Chunk facts into ≤100KB groups.
2. For each chunk: send to LLM with the prune prompt, parse JSON response.
3. Apply decisions: `keep` (no-op), `forget` (delete + index removal), `update` (patch confidence).
4. Apply merges: commit merged fact, delete source facts.
5. Tag `learn/synthesize-<name>-prune` at HEAD.

### Distill Step (RAPTOR)

1. Fetch embeddings from `SearchIndex` for all scoped facts.
2. For each RAPTOR depth (1 to `max_depth`):
   a. Cluster facts using UMAP + HDBSCAN + FCA split.
   b. For each cluster: send to LLM with the distill prompt, parse JSON response.
   c. Collect new synthesized facts and facts to forget.
   d. If more depth remains: embed the new synthesized facts in-process, use as input for next depth.
3. Commit all synthesized facts, delete all subsumed facts.
4. Tag `learn/synthesize-<name>-distill` at HEAD.

### Ref Resolution

Before committing synthesized or merged facts, file-path refs are resolved to `knomit:blob/<commit>/<path>` URIs. Refs that already contain `://` or start with `knomit:` are passed through unchanged. This is done by `resolveRefs(ctx, gitStore, refs)`.

### Progress Events

`onProgress` is a `func(ProgressEvent)` callback. `ProgressEvent` is a discriminated union (Go: interface with a `Phase() string` method, or a struct with a `Phase` string field + sub-fields). Events cover: `step-start`, `gather`, `cluster`, `raptor-depth`, `llm`, `llm-stream`, `llm-done`, `apply`, `detail-*`, `reindex`, `merge`, `push`, `done`. The HTTP synthesize endpoint streams these as SSE events to the Web UI.

---

## 13. Web UI

### Technology Choice

The Web UI should be built with **React + Vite** (TypeScript). The reasons:

- The TUI state machine (`state.ts`) is a pure reducer over well-defined state: `currentPath`, `selectedIndex`, `currentFact`, `rightPanelMode`, `historyMode`, `searchActive`, `navStack`. This maps naturally to React's `useReducer`.
- The UI has enough interactive complexity (keyboard navigation, ref following, search, history, synthesis progress) that plain HTML+HTMX would require non-trivial client-side state management anyway.
- The existing TUI panels (LeftPanel, RightPanel, TopBar, StatusBar) translate directly to React components.
- Vite produces a `dist/` directory of static assets that are straightforward to embed.

### Embedded Assets

`web/dist/` is embedded into the Go binary using `//go:embed web/dist`. The `internal/web` package serves the embedded filesystem via `http.FileServer(http.FS(embeddedFS))`. The SPA catch-all route serves `index.html` for any path not matched by an API route or static asset route, enabling client-side routing.

### UI Panels

The Web UI reproduces the TUI layout as a browser application:

- **TopBar**: current HEAD commit hash, sync status indicator, manual sync button.
- **LeftPanel**: breadcrumb path navigator, list of worlds/facts at current path; search input (text or domain).
- **RightPanel**: three modes — summary (stats + domain/entity distribution), fact (frontmatter + rendered markdown body + refs), history (git log with commit markers for learn/ tags).
- **StatusBar**: total facts, average confidence, embeddings enabled indicator.
- **SynthesisModal**: progress stream display for running recipes.

### State Management

The Web UI state machine mirrors the TypeScript `AppState` / `Action` / `reducer` pattern. The same state transitions (navigate up/down, open item, go up, follow ref, nav back, search, history toggle) are re-implemented in React with `useReducer`. Data is fetched from the REST API on state transitions that require it (e.g. listing children when `currentPath` changes, loading fact content when `currentFact` changes).

---

## 14. Migration from stdio

### Current MCP Client Config (TypeScript / stdio)

Clients currently configure knomit like this (example for Claude Desktop / claude-code):

```json
{
  "mcpServers": {
    "knomit": {
      "command": "knomit",
      "args": ["mcp"],
      "env": { "KNOMIT_REPO": "/path/to/repo" }
    }
  }
}
```

### New Config (Go / HTTP)

After the rewrite, `knomit serve` must be running as a background service. Clients switch to the HTTP transport:

```json
{
  "mcpServers": {
    "knomit": {
      "url": "http://localhost:3000/mcp"
    }
  }
}
```

For profile selection:

```json
{
  "mcpServers": {
    "knomit-chat": {
      "url": "http://localhost:3000/mcp?profile=chat"
    }
  }
}
```

### Backward Compatibility

The `knomit mcp` subcommand should remain as a thin shim for a transition period. It can:

- Start a local HTTP server on a random port and proxy stdio to it (complex), or
- Simply print a deprecation notice instructing the user to switch to `knomit serve` and update their MCP client config.

The simpler option (deprecation notice) is recommended. The stdio transport is the root cause of the concurrency problem; preserving it defeats the purpose of the rewrite.

### Process Management

Users need a way to keep `knomit serve` running persistently. Distribution should include:

- A `launchd` plist template for macOS (`com.knomit.serve.plist`).
- A `systemd` unit file template for Linux.
- Documentation covering manual background launch (`knomit serve &`) as a fallback.

---

## 15. Build and Distribution

### CGO Requirements

Three dependencies require CGO:

- `github.com/mattn/go-sqlite3`: SQLite with FTS5 and the extension loading API. FTS5 must be compiled in (it is by default with go-sqlite3). The `-tags fts5` build tag is not needed since go-sqlite3 enables FTS5 by default, but `-tags sqlite_json` may be needed depending on SQLite version.
- `github.com/yalue/onnxruntime_go`: wraps the pre-built `libonnxruntime` native library via CGO. Not statically linked; bundled in `data/lib/`.
- `github.com/daulet/tokenizers`: wraps the HuggingFace tokenizers Rust library (`libtokenizers`) via CGO. Handles nomic-embed-text's BPE tokenizer by reading `tokenizer.json` directly. Not statically linked; bundled in `data/lib/`.

### Native Library Bundling

The distribution archive (`.tar.gz` or `.zip`) contains:

```
knomit                         — the Go binary
data/
  models/
    nomic-embed-text-v1.5.onnx — embedding model
    tokenizer.json             — BPE vocab (HuggingFace format)
  lib/
    libonnxruntime.so.1        — Linux
    libonnxruntime.1.24.3.dylib — macOS
    onnxruntime.dll            — Windows
    libtokenizers.so           — Linux (daulet/tokenizers)
    libtokenizers.dylib        — macOS (daulet/tokenizers)
    tokenizers.dll             — Windows (daulet/tokenizers)
    libsqlite3.dylib           — macOS only (Homebrew extension-capable build)
```

The binary locates `data/` relative to its own executable path using `os.Executable()`. If the binary is run from a development checkout, `data/` is expected adjacent to the binary in `cmd/knomit/`.

### Build Process

A `Makefile` (or `build.sh`) handles the multi-step build:

1. `cd web && npm ci && npm run build` — produces `web/dist/`.
2. `go generate ./internal/web/` — runs `go:generate` directive to verify the embed path exists (optional sanity check).
3. `CGO_ENABLED=1 go build -o dist/knomit ./cmd/knomit/` — produces the binary.
4. Copy `data/` into `dist/`.
5. Archive `dist/` into `knomit-<version>-<os>-<arch>.tar.gz`.

### Cross-Compilation Challenges

CGO makes true cross-compilation difficult:

- **Linux → macOS / Windows**: requires a cross-compiling C toolchain (e.g. `osxcross`, `mingw-w64`). Complex to maintain.
- **macOS → Linux**: possible with a Linux sysroot but non-trivial.
- **Recommended approach**: use CI matrix builds (GitHub Actions `macos-latest`, `ubuntu-latest`, `windows-latest`). Each platform natively produces its own binary. The ONNX Runtime and SQLite native libraries are downloaded from their respective release pages during the CI build step for each target platform.

### SQLite on macOS

The macOS system SQLite (`/usr/lib/libsqlite3.dylib`) does not support extension loading (needed for sqlite-vec). The binary must link against a SQLite build that has extension support — either Homebrew's SQLite or a bundled build.

Recommended approach: link against Homebrew's SQLite at build time (`CGO_CFLAGS=-I$(brew --prefix sqlite)/include CGO_LDFLAGS=-L$(brew --prefix sqlite)/lib`). For distribution, bundle the Homebrew SQLite dylib in `data/lib/libsqlite3.dylib` and use `install_name_tool` to set the rpath so the binary finds it at runtime relative to its own path.

Note: both `knomit.git.db` and `knomit.index.db` are opened via go-sqlite3. The git object store does not use any SQLite extensions; only the search index needs sqlite-vec. A single SQLite build satisfies both.

### Minimum Go Version

Go 1.22 or later is required (for `net/http` routing improvements and `slices`/`maps` standard library packages used in the implementation).

---

## 16. Git Remote Protocol Support

Knomit can act as a full git remote, allowing standard git CLI operations against the knowledge base repo:

```sh
git clone http://localhost:3000/git knomit-local
git pull knomit http://localhost:3000/git
git push http://localhost:3000/git agent/laptop
```

Because all git objects already live in the SQLite storer (`knomit.git.db`), the protocol handlers read from and write to the same store as every other knomit operation — no additional data path is needed.

### Smart HTTP (HTTP/S)

go-git provides `plumbing/transport/server` with a ready-made HTTP backend. Two endpoints implement the [Smart HTTP protocol](https://www.git-scm.com/docs/http-backend):

| Method | Path                                      | Description                                       |
|--------|-------------------------------------------|---------------------------------------------------|
| GET    | `/git/info/refs?service=git-upload-pack`  | Advertise refs for `git clone` / `git fetch`      |
| POST   | `/git/git-upload-pack`                    | Send objects to client (`git clone`, `git fetch`) |
| GET    | `/git/info/refs?service=git-receive-pack` | Advertise refs for `git push`                     |
| POST   | `/git/git-receive-pack`                   | Receive objects from client (`git push`)          |

These are mounted at `/git/` in the chi router. The handler is constructed by wrapping go-git's `server.NewUploadPackSession` and `server.NewReceivePackSession` with a loader that returns the SQLite-backed `*git.Repository`:

```go
type repoLoader struct{ store *gitstorer.Storer }

func (l *repoLoader) Load(ep *transport.Endpoint) (transport.Repository, error) {
    // knomit serves a single repo; ignore ep.Path
    return git.Open(l.store, memory.NewStorage())
}

handler := server.NewHTTPBackend(server.NewServer(repoLoader{store: gitStore.Storer()}))
r.Mount("/git", handler)
```

The `server.HTTPBackend` from go-git handles content negotiation, request parsing, and response packing. It calls into the `repoLoader` to obtain the repository, then reads/writes objects directly to SQLite.

### Native git:// Protocol (Optional)

go-git also supports the native `git://` protocol (port 9418) via `plumbing/transport/server`. A TCP listener is opened alongside the HTTP server when `--git-port` is set (default: disabled):

```go
ln, _ := net.Listen("tcp", ":9418")
srv := server.NewServer(repoLoader{store: gitStore.Storer()})
for {
    conn, _ := ln.Accept()
    go srv.ServeConn(conn)
}
```

The native protocol is unauthenticated by default and is only suitable for local or trusted-network use. Smart HTTP is preferred for any network-accessible deployment.

### Access Control

Both protocols share the same authentication surface. Two modes:

- **Read-only by default**: `git-upload-pack` (fetch/clone) is always allowed. `git-receive-pack` (push) requires an API key passed as the HTTP password (`Authorization: Bearer <KNOMIT_API_KEY>`). With the native git:// protocol, `git-receive-pack` is disabled unless explicitly enabled via `--allow-git-push`.
- **Disabled**: `KNOMIT_GIT_REMOTE=false` disables all `/git/` routes and the git:// listener entirely. Useful when the HTTP port is publicly exposed.

### Push Behaviour and Locking

`git push` has two distinct phases with different locking requirements:

1. **Object receipt** (no mutex): incoming pack data — blobs, trees, commits — is unpacked and written to the `objects` table in `knomit.git.db`. Objects are content-addressed (keyed by SHA hash) and immutable once written, so concurrent pushes writing the same or different objects are always safe. SQLite WAL serialises the row writes internally; no application mutex is needed. Concurrent `git fetch` / `git clone` requests, API reads, and MCP query calls continue without any blocking during this phase.

2. **Ref update** (mutex acquired): once all objects are safely stored, `GitStore.mu` is acquired to update the branch ref (`refs/heads/agent/<name>`) to the new tip commit. The mutex is held until the ref is written and the post-receive async index sync is dispatched. This prevents a concurrent `knomit_learn` tool call from updating the same ref simultaneously and producing an inconsistent branch state.

Pushes to `main` are rejected before the mutex is acquired. The post-receive `SearchIndex.Sync` runs asynchronously after the mutex is released so it does not block subsequent requests.

### Effect on HTTP API Surface

The git routes are added to Section 5's API surface table (see Smart HTTP routes above). They are served by go-git's `HTTPBackend` handler, not by the chi REST handlers, so they bypass JSON serialisation and chi middleware except for the authentication middleware which wraps the `/git/` mount point.

---

## 17. Configuration Reference

All configuration is via environment variables. No config file in V1.

### Server

| Variable            | Default           | Description                                                                                     |
|---------------------|-------------------|-------------------------------------------------------------------------------------------------|
| `KNOMIT_REPO`       | `~/.knomit`       | Path to the repo directory; contains `knomit.git.db`                                            |
| `KNOMIT_PORT`       | `3000`            | HTTP listen port                                                                                |
| `KNOMIT_CACHE_DIR`  | `~/.cache/knomit` | Directory for `knomit.index.db` and temporary files                                             |
| `KNOMIT_API_KEY`    | —                 | Bearer token required for write operations (REST and git push); if unset, writes are open       |
| `KNOMIT_GIT_REMOTE` | `true`            | Set to `false` to disable all `/git/` Smart HTTP routes                                         |
| `KNOMIT_GIT_PORT`   | —                 | If set, opens a native `git://` TCP listener on this port (unauthenticated, local use only)     |

### LLM

| Variable               | Default              | Description                                                      |
|------------------------|----------------------|------------------------------------------------------------------|
| `KNOMIT_LLM_PROVIDER`  | inferred from model  | `anthropic`, `gemini`, `bedrock`, `claude-cli`, `gemini-cli`     |
| `KNOMIT_LLM_MODEL`     | `claude-sonnet-4-6`  | Model name passed to the provider                                |
| `ANTHROPIC_API_KEY`    | —                    | Anthropic API key (`anthropic` provider)                         |
| `GOOGLE_AI_API_KEY`    | —                    | Google AI API key (`gemini` provider)                            |
| `AWS_REGION`           | —                    | AWS region (`bedrock` provider)                                  |
| `AWS_ACCESS_KEY_ID`    | —                    | AWS access key (`bedrock` provider)                              |
| `AWS_SECRET_ACCESS_KEY`| —                    | AWS secret key (`bedrock` provider)                              |

### Remote (client-side git auth)

Used when knomit pushes to / fetches from `origin`. Token takes precedence over Basic if both are set; SSH key takes precedence over agent.

| Variable                  | Default | Description                                                 |
|---------------------------|---------|-------------------------------------------------------------|
| `KNOMIT_REMOTE_TOKEN`     | —       | Bearer token / PAT for HTTP remote auth                     |
| `KNOMIT_REMOTE_USER`      | —       | Username for HTTP Basic auth to the remote                  |
| `KNOMIT_REMOTE_PASSWORD`  | —       | Password for HTTP Basic auth to the remote                  |
| `KNOMIT_REMOTE_SSH_KEY`   | —       | SSH private key path; falls back to `SSH_AUTH_SOCK` agent   |
| `SSH_AUTH_SOCK`           | —       | SSH agent socket; used when `KNOMIT_REMOTE_SSH_KEY` is unset|

### Runtime libraries

| Variable                    | Default          | Description                                                           |
|-----------------------------|------------------|-----------------------------------------------------------------------|
| `ONNXRUNTIME_SHARED_LIBRARY`| auto-detected    | Explicit path to `libonnxruntime`; overrides detection from `data/lib/` |

---

## 18. Development Seed Data

`tools/seed/main.go` seeds a fresh repo with sample facts for testing and exercises all clustering paths in the synthesize pipeline. It calls the running knomit server over MCP HTTP (`POST /mcp`, `knomit_learn` tool).

The same two phases are preserved:

- **base** — people, projects, decisions, debugging, and conventions facts
- **distill** — clustering test cases: tight cluster, cross-path same domain, shared entities, FCA split, orphan fact

Usage (with `knomit serve` already running):

```sh
go run ./tools/seed/ [base|distill|all] [http://localhost:3000]
```

The fact data is preserved verbatim from the original `scripts/seed.ts`.
