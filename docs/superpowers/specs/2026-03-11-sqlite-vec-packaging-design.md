# sqlite-vec Integration + Cross-Platform Packaging

## Problem

Vector search uses brute-force cosine similarity over BLOBs stored in the `facts` table. This is O(N) per query and doesn't leverage sqlite-vec's ANN indexing. Additionally, the build depends on Homebrew for onnxruntime and has no cross-platform packaging story — the `dist/` output is just the Go binary.

## Goals

1. Replace BLOB-based vector storage with sqlite-vec's `vec0` virtual table
2. Ship a self-contained `dist/` package with all native dependencies for macOS, Linux, and Windows
3. No system-level package manager required to run knomit

## Design

### 1. sqlite-vec: Static Go Bindings

**Dependency:** `github.com/asg017/sqlite-vec-go-bindings/cgo`

This compiles sqlite-vec directly into the Go binary via CGO. No shared library needed.

**Explicit registration required** (`internal/store/index.go`):
```go
import sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
```

Unlike some SQLite extensions, sqlite-vec does **not** auto-register via `init()`. The function `sqlite_vec.Auto()` must be called before any database connection is opened. This call is made once via `sync.Once` at the top of `New()`, before `sql.Open`.

```go
var vecOnce sync.Once

func New(path string) (*Index, error) {
    vecOnce.Do(func() { sqlite_vec.Auto() })
    // ... sql.Open, schema migrations, etc.
}
```

### 2. Schema (Fresh Start)

No migration logic — the project is early-stage. The schema is always created fresh. The `facts` table has no `vec_data` column.

New virtual table — vec0 requires an **integer primary key** (TEXT PRIMARY KEY is not supported):
```sql
CREATE VIRTUAL TABLE IF NOT EXISTS facts_vec USING vec0(
    embedding FLOAT[768] distance_metric=cosine
);
```

The `rowid` of `facts_vec` corresponds to the `rowid` of the `facts` table. Joins between the two use this shared rowid.

Using `distance_metric=cosine` means vec0 returns cosine distance directly (0 = identical, 1 = orthogonal) for unit vectors. This eliminates the need to convert from L2 distance.

#### Referential Integrity

When a fact is deleted, all corresponding data must be cleaned up. The `Delete()` method handles this in a single transaction:

1. Delete from `facts_vec` (by rowid lookup from `facts`)
2. Delete from `facts_fts` (existing FTS5 shadow table cleanup)
3. Delete from `facts`

All three deletions happen inside the same transaction. If any step fails, the entire transaction rolls back. This ensures no orphaned vector or FTS entries remain.

### 3. Changed Methods in `internal/store/index.go`

#### `Upsert(rec FactRecord)`

Inside the existing transaction, after inserting into `facts` + `facts_fts`:
1. Compute embedding: `vec, err := idx.embedder.Embed(rec.Body)`
2. Serialize to `[]byte` via `float32SliceToBytes` (vec0 accepts raw little-endian float32 blobs)
3. `INSERT OR REPLACE INTO facts_vec(rowid, embedding) VALUES ((SELECT rowid FROM facts WHERE path=?), ?)` with the path and blob

The `facts` table has no `vec_data` column — embeddings are stored exclusively in `facts_vec`.

#### `Delete(path string)`

Add inside the existing transaction, before deleting from `facts`:

```sql
DELETE FROM facts_vec WHERE rowid = (SELECT rowid FROM facts WHERE path = ?)
```

#### `Search(q SearchQuery)`

Replace the brute-force vector augmentation block (lines 740–766) with a single vec0 KNN query:

```go
// Vec0 KNN search — single SQL query replaces per-result GetEmbedding + dotProduct.
if queryVec != nil {
    vecBlob := float32SliceToBytes(queryVec)
    rows, err := idx.db.Query(
        `SELECT rowid, distance FROM facts_vec WHERE embedding MATCH ? AND k = ?`,
        vecBlob, limit*5,
    )
    // Build rowid→distance map, join with facts to get paths.
    // distance_metric=cosine: distance is cosine distance [0,1].
    // cosine_similarity = 1 - cosine_distance.
}
```

Note: vec0 KNN queries require the `AND k = ?` constraint (not just `LIMIT`). This is the portable syntax that works across all SQLite versions.

The merge logic:
- FTS results scored as before (BM25 normalized to [0,1])
- Vec results: `cosine_similarity = 1 - cosine_distance` (direct from vec0, no L2 conversion needed)
- Combined: `0.6*bm25 + 0.4*cosine` (unchanged formula)
- Vec-only results (not in FTS set) included if cosine_similarity > 0.2
- Union is sorted by combined score, normalized to [0,100], capped at limit

#### `GetEmbedding(path string)`

Query `facts_vec` via rowid join:
```go
var blob []byte
err := idx.db.QueryRow(
    `SELECT fv.embedding FROM facts_vec fv JOIN facts f ON fv.rowid = f.rowid WHERE f.path = ?`,
    path,
).Scan(&blob)
```
Returns `bytesToFloat32Slice(blob)`.

### 4. Removed Code

- `dotProduct` function — replaced by vec0 KNN query
- `migrateV2` constant — no migration logic needed
- Per-result `GetEmbedding` call in the search loop — replaced by batch vec0 query
- `vec_data BLOB` column — removed from schema entirely (embeddings live in `facts_vec`)

### 5. Kept Code

- `float32SliceToBytes` / `bytesToFloat32Slice` — still needed for vec0 blob format and `GetEmbedding` return
- `Embedder` interface — unchanged
- FTS5 search — unchanged
- Hybrid scoring formula — unchanged
- Graceful degradation when embedder is nil — unchanged

### 6. Cross-Platform Dist Layout

```
dist/
  knomit                          # binary
  lib/
    libonnxruntime.1.24.3.dylib   # macOS (arm64)
    libonnxruntime.so.1.24.3      # Linux (x86_64)
    onnxruntime.1.24.3.dll        # Windows (x86_64)
```

Only onnxruntime needs a shared library. sqlite-vec is compiled in.

#### Library Resolution (`internal/embeddings/embedder.go`)

Update `candidateLibraryPaths` to resolve relative to the binary's directory using `os.Executable()`:

```go
func initORT() error {
    ortOnce.Do(func() {
        if p := os.Getenv("ORT_LIB_PATH"); p != "" {
            ort.SetSharedLibraryPath(p)
        } else {
            exeDir := filepath.Dir(mustExePath())
            candidates := libCandidates(exeDir)
            for _, c := range candidates {
                if _, err := os.Stat(c); err == nil {
                    ort.SetSharedLibraryPath(c)
                    break
                }
            }
        }
        ortInitErr = ort.InitializeEnvironment()
    })
    return ortInitErr
}
```

Platform-specific candidates (runtime `GOOS` check):
- macOS: `<exe_dir>/lib/libonnxruntime.dylib`, `/opt/homebrew/lib/libonnxruntime.dylib`
- Linux: `<exe_dir>/lib/libonnxruntime.so`, `/usr/local/lib/libonnxruntime.so`
- Windows: `<exe_dir>/lib/onnxruntime.dll`

#### Makefile: `dist` target

```makefile
ORT_VERSION := 1.24.3

dist: build
    @mkdir -p dist/lib
    $(call download-ort)

# Per-platform onnxruntime download
define download-ort
    # Detect OS/arch, download from GitHub releases, extract to dist/lib/
endef
```

The download target fetches from `https://github.com/microsoft/onnxruntime/releases/download/v$(ORT_VERSION)/` and extracts the shared library to `dist/lib/`.

#### `make run` uses `dist/lib`

The `run` target sets `ORT_LIB_PATH` to `dist/lib/` so local dev uses the same bundled onnxruntime that ships in the package. No Homebrew dependency for running:

```makefile
run: dist
	CGO_ENABLED=1 ORT_LIB_PATH=dist/lib go run -tags sqlite_fts5 ./cmd/knomit/ serve
```

`make setup` is simplified to just download onnxruntime to `dist/lib/` (no Homebrew). The `dist` target handles this automatically.

### 7. Build Tags

No new build tags needed. The existing `-tags sqlite_fts5` stays.

### 8. Testing

- **`internal/store/index_test.go`**: update `TestSearchHybrid` and `TestGetEmbedding` to work with `facts_vec` instead of `facts.vec_data`. Test fixtures must declare vec0 tables with dimensions matching the stub vectors (e.g. `FLOAT[4]` for 4-dim stubs, not `FLOAT[768]`), since vec0 rejects inserts with mismatched dimensions. The test helper `New(":memory:")` handles this by using the same schema as production but with a configurable dimension, or the tests override the table creation.
- **Vec0 availability test**: `SELECT vec_version()` should return a version string.
- **Referential integrity test**: delete a fact and verify no orphaned rows remain in `facts_vec` or `facts_fts`.

### Not Changed

- `internal/embeddings/*` — embedder code unchanged (except library path resolution)
- `internal/synthesize/*` — calls `GetEmbedding`, which still returns `[]float32`
- Web UI, TaskHub, SSE, handlers — no changes
- ONNX model download — already handled by `embeddings.EnsureModel`
