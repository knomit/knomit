# sqlite-vec Integration + Cross-Platform Packaging + MCP Profile Selection

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace brute-force vector search with sqlite-vec's `vec0` virtual table, ship a self-contained cross-platform dist package, and wire MCP profile selection via query parameter.

**Architecture:** sqlite-vec is compiled into the Go binary via CGO static bindings (`sqlite-vec-go-bindings/cgo`). No shared library needed for vectors. onnxruntime remains a shared library, bundled in `dist/lib/` and resolved relative to the binary's directory at runtime. MCP profiles are handled by creating per-profile MCP server instances dispatched via `?profile=` query param middleware.

**Tech Stack:** Go, CGO, sqlite-vec-go-bindings/cgo, mattn/go-sqlite3, yalue/onnxruntime_go, mcp-go, chi

**Spec:** `docs/superpowers/specs/2026-03-11-sqlite-vec-packaging-design.md`

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `internal/store/index.go` | Modify | Add sqlite-vec registration, `facts_vec` schema, update Upsert/Delete/Search/GetEmbedding |
| `internal/store/index_test.go` | Modify | Update tests for vec0, add referential integrity test, add vec0 availability test |
| `internal/embeddings/embedder.go` | Modify | Exe-relative library resolution via `os.Executable()` |
| `internal/embeddings/embedder_test.go` | Create | Test `libCandidates()` path generation |
| `internal/mcp/server.go` | Modify | Accept profile-keyed server map or per-profile instructions |
| `internal/mcp/instructions.go` | Create | Profile instructions (base + code/chat/generic addenda) |
| `internal/mcp/instructions_test.go` | Create | Test profile instruction content |
| `internal/web/server.go` | Modify | Mount profile-dispatching MCP middleware |
| `cmd/knomit/main.go` | Modify | Create per-profile MCP servers, pass to router |
| `Makefile` | Modify | Add `dist` target, update `run`/`setup`, add ORT download |
| `go.mod` / `go.sum` | Modify | Add `github.com/asg017/sqlite-vec-go-bindings` dependency |

---

## Chunk 1: sqlite-vec Static Bindings + Schema

### Task 1: Add sqlite-vec dependency

**Files:**
- Modify: `go.mod`

- [ ] **Step 1: Add the dependency**

```bash
cd /Users/knomit/data/mine/knomit && go get github.com/asg017/sqlite-vec-go-bindings/cgo
```

- [ ] **Step 2: Verify it compiles**

```bash
CGO_ENABLED=1 go build -tags sqlite_fts5 ./...
```

Expected: clean build (the import isn't used yet, but the module should resolve)

- [ ] **Step 3: Tidy modules**

```bash
go mod tidy
```

- [ ] **Step 4: Add `dist/lib/` to `.gitignore`**

Append to `.gitignore` (create if absent):

```
dist/lib/
```

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum .gitignore
git commit -m "deps: add sqlite-vec-go-bindings/cgo for vec0 support"
```

---

### Task 2: Register sqlite-vec, add `facts_vec` schema, and `WithVecDimension` option

**Files:**
- Modify: `internal/store/index.go`
- Modify: `internal/store/index_test.go`

This task introduces sqlite-vec registration, the `facts_vec` virtual table, and the `WithVecDimension` option in a single commit. This avoids a broken window where existing embedding tests (384-dim stubs) fail against a 768-dim `facts_vec` table.

- [ ] **Step 1: Write the vec0 availability test**

Add to `internal/store/index_test.go`:

```go
func TestVec0Available(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	var version string
	err = idx.DB().QueryRow("SELECT vec_version()").Scan(&version)
	if err != nil {
		t.Fatalf("vec_version() failed: %v — sqlite-vec not registered", err)
	}
	if version == "" {
		t.Fatal("vec_version() returned empty string")
	}
	t.Logf("sqlite-vec version: %s", version)
}
```

Note: This test requires a `DB()` accessor on `Index`. We'll add it in the next step.

- [ ] **Step 2: Run test to verify it fails**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -run TestVec0Available ./internal/store/
```

Expected: FAIL — `DB()` method doesn't exist yet, and sqlite-vec isn't registered.

- [ ] **Step 3: Implement sqlite-vec registration, schema, and `WithVecDimension`**

In `internal/store/index.go`:

1. Add import:

```go
sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
```

2. Add `sync.Once` registration before `New()`:

```go
var vecOnce sync.Once

func registerVec() {
	vecOnce.Do(func() { sqlite_vec.Auto() })
}
```

3. Add functional option types:

```go
type indexConfig struct {
	vecDim int
}

// Option configures Index creation.
type Option func(*indexConfig)

// WithVecDimension overrides the default 768-dim embedding for facts_vec.
// Useful in tests with small stub vectors.
func WithVecDimension(d int) Option {
	return func(c *indexConfig) { c.vecDim = d }
}
```

4. Update `New()` signature to `New(path string, opts ...Option) (*Index, error)`:
   - Call `registerVec()` as the first line.
   - Apply options to config (default `vecDim: 768`).
   - Use `schemaSQL(cfg.vecDim)` instead of the `schema` constant.

5. Replace the `schema` constant with a function:

```go
func schemaSQL(vecDim int) string {
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS synthesis_log (
    recipe          TEXT PRIMARY KEY,
    last_commit     TEXT NOT NULL,
    run_at          TEXT NOT NULL,
    facts_processed INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS facts (
    path        TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    body        TEXT NOT NULL,
    domain      TEXT NOT NULL,
    entities    TEXT NOT NULL,
    confidence  REAL NOT NULL,
    sources     INTEGER NOT NULL,
    refs        TEXT NOT NULL,
    commit_hash TEXT NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS facts_fts USING fts5(
    title, body, entities, domain,
    content='facts', content_rowid='rowid'
);
CREATE VIRTUAL TABLE IF NOT EXISTS facts_vec USING vec0(
    embedding FLOAT[%d] distance_metric=cosine
);`, vecDim)
}
```

   Note: The `facts` table no longer has `vec_data BLOB`. Remove `vec_data` from the `Upsert` INSERT column list and values too (the Upsert vec0 write is added in Task 3).

6. Add `DB()` accessor:

```go
func (idx *Index) DB() *sql.DB { return idx.db }
```

7. Remove the `migrateV2` constant and the entire v1→v2 migration block in `New()` (the `ALTER TABLE facts ADD COLUMN vec_data BLOB` logic and the schema_version check/update). Replace with:

```go
if _, err = db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES ('schema_version', '3')`); err != nil {
    db.Close()
    return nil, fmt.Errorf("init schema_version: %w", err)
}
```

8. Update existing tests that use embeddings to pass `WithVecDimension`:
   - `TestGetEmbedding`: change 384-dim stub to 4-dim, use `store.New(":memory:", store.WithVecDimension(4))`
   - `TestSearchHybrid`: already uses 4-dim vectors, add `store.WithVecDimension(4)`
   - All other `store.New(":memory:")` calls: no change needed (default 768 is fine since they don't insert embeddings)

- [ ] **Step 4: Run the vec0 availability test and full store test suite**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 ./internal/store/
```

Expected: ALL PASS — `vec_version()` returns a version string, existing tests still pass with the new schema.

- [ ] **Step 5: Commit**

```bash
git add internal/store/index.go internal/store/index_test.go
git commit -m "feat(store): register sqlite-vec, add facts_vec schema with WithVecDimension option"
```

---

### Task 3: Update Upsert to write embeddings to `facts_vec`

**Files:**
- Modify: `internal/store/index.go`

- [ ] **Step 1: Write the test**

Update `TestGetEmbedding` in `internal/store/index_test.go` to verify embeddings are stored in `facts_vec` (not `facts.vec_data`). The existing test should still pass after changes, but add an explicit check:

```go
func TestGetEmbeddingFromVec0(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	const dims = 4
	known := []float32{0.5, 0.5, 0.5, 0.5}
	idx.SetEmbedder(&stubEmb{vec: known})

	rec := store.FactRecord{
		Path: "know/test/emb.md", Title: "Embedding test",
		Body: "body text", Domain: []string{"test"}, Entities: []string{},
		Confidence: 1.0, Sources: 1, CommitHash: "emb1",
	}
	if err := idx.Upsert(rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := idx.GetEmbedding("know/test/emb.md")
	if err != nil {
		t.Fatalf("GetEmbedding: %v", err)
	}
	if len(got) != dims {
		t.Fatalf("expected %d-dim vector, got %d", dims, len(got))
	}
	for i, v := range got {
		if v != known[i] {
			t.Fatalf("mismatch at %d: got %v, want %v", i, v, known[i])
		}
	}

	// Verify it's in facts_vec, not facts.vec_data
	var rowCount int
	err = idx.DB().QueryRow("SELECT count(*) FROM facts_vec").Scan(&rowCount)
	if err != nil {
		t.Fatalf("count facts_vec: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("expected 1 row in facts_vec, got %d", rowCount)
	}
}
```

**Note:** `WithVecDimension(4)` was already introduced in Task 2. Use `store.New(":memory:", store.WithVecDimension(4))` for this test.

- [ ] **Step 2: Run tests to verify they fail**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -run TestGetEmbeddingFromVec0 ./internal/store/
```

Expected: FAIL — Upsert doesn't write to `facts_vec` yet.

- [ ] **Step 3: Update Upsert to write to `facts_vec`**

In `internal/store/index.go`, modify `Upsert()`:

1. Remove `vec_data` from the `INSERT OR REPLACE INTO facts(...)` column list and values.
2. After the FTS5 insert (step 4), add a vec0 insert if embedding was computed:

```go
// Step 5: Insert embedding into facts_vec.
// vec0 may not support INSERT OR REPLACE, so delete first then insert.
if vecData != nil {
    newRowid := int64(0)
    _ = tx.QueryRow(`SELECT rowid FROM facts WHERE path=?`, rec.Path).Scan(&newRowid)
    if newRowid > 0 {
        tx.Exec(`DELETE FROM facts_vec WHERE rowid = ?`, newRowid)
        if _, err := tx.Exec(
            `INSERT INTO facts_vec(rowid, embedding) VALUES (?, ?)`,
            newRowid, vecData,
        ); err != nil {
            return fmt.Errorf("insert vec row: %w", err)
        }
    }
}
```

- [ ] **Step 4: Run tests**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -run TestGetEmbedding ./internal/store/
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/index.go internal/store/index_test.go
git commit -m "feat(store): write embeddings to facts_vec via vec0"
```

---

### Task 4: Update GetEmbedding to read from `facts_vec`

**Files:**
- Modify: `internal/store/index.go`

- [ ] **Step 1: The test from Task 3 already covers this. Update GetEmbedding implementation.**

Replace the current `GetEmbedding` method:

```go
func (idx *Index) GetEmbedding(path string) ([]float32, error) {
	var blob []byte
	err := idx.db.QueryRow(
		`SELECT fv.embedding FROM facts_vec fv
		 JOIN facts f ON fv.rowid = f.rowid
		 WHERE f.path = ?`,
		path,
	).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get embedding: %w", err)
	}
	if len(blob) == 0 {
		return nil, nil
	}
	return bytesToFloat32Slice(blob)
}
```

- [ ] **Step 2: Run tests**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -run TestGetEmbedding ./internal/store/
```

Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/store/index.go
git commit -m "feat(store): read embeddings from facts_vec instead of facts.vec_data"
```

---

### Task 5: Update Delete with referential integrity

**Files:**
- Modify: `internal/store/index.go`

- [ ] **Step 1: Write referential integrity test**

Add to `internal/store/index_test.go`:

```go
func TestDeleteReferentialIntegrity(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	idx.SetEmbedder(&stubEmb{vec: []float32{1, 0, 0, 0}})

	rec := store.FactRecord{
		Path: "know/test/ri.md", Title: "RI test", Body: "referential integrity",
		Domain: []string{"test"}, Entities: []string{},
		Confidence: 0.9, Sources: 1, CommitHash: "ri1",
	}
	if err := idx.Upsert(rec); err != nil {
		t.Fatal(err)
	}

	// Verify all three tables have data
	var count int
	idx.DB().QueryRow("SELECT count(*) FROM facts").Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 fact, got %d", count)
	}
	idx.DB().QueryRow("SELECT count(*) FROM facts_vec").Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 vec row, got %d", count)
	}

	// Delete
	if err := idx.Delete("know/test/ri.md"); err != nil {
		t.Fatal(err)
	}

	// Verify all three tables are clean
	idx.DB().QueryRow("SELECT count(*) FROM facts").Scan(&count)
	if count != 0 {
		t.Fatalf("expected 0 facts after delete, got %d", count)
	}
	idx.DB().QueryRow("SELECT count(*) FROM facts_vec").Scan(&count)
	if count != 0 {
		t.Fatalf("expected 0 vec rows after delete, got %d", count)
	}

	// FTS should also be clean
	results, err := idx.SearchText("referential", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 FTS results after delete, got %d", len(results))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -run TestDeleteReferentialIntegrity ./internal/store/
```

Expected: FAIL — Delete doesn't clean `facts_vec`.

- [ ] **Step 3: Update Delete method**

In `internal/store/index.go`, add vec0 cleanup inside `Delete()`, after the FTS5 delete and before the `DELETE FROM facts`:

```go
// Delete from facts_vec (must happen before facts row is deleted).
if err == nil { // err == nil means we found the old row
    if _, err := tx.Exec(
        `DELETE FROM facts_vec WHERE rowid = ?`, oldRowid,
    ); err != nil {
        return fmt.Errorf("delete vec row: %w", err)
    }
}
```

- [ ] **Step 4: Run test**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -run TestDeleteReferentialIntegrity ./internal/store/
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/index.go internal/store/index_test.go
git commit -m "feat(store): ensure referential integrity on delete (facts_vec + facts_fts)"
```

---

### Task 6: Replace brute-force vector search with vec0 KNN

**Files:**
- Modify: `internal/store/index.go`

- [ ] **Step 1: Update TestSearchHybrid**

The existing `TestSearchHybrid` test uses 4-dim vectors with `dispatchEmb`. It should still work with vec0 KNN, but update the test expectations to reflect vec0 behavior. The test asserts fact A ranks above fact B when query vector matches A — this should still hold with vec0 KNN since cosine distance to A will be 0 and to B will be ~1.

No test changes needed — `TestSearchHybrid` already uses `store.WithVecDimension(4)` from Task 2.

- [ ] **Step 2: Run existing hybrid test to see current state**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -run TestSearchHybrid ./internal/store/
```

Note the result — it may already pass or fail depending on whether the 4-dim vec0 table was created.

- [ ] **Step 3: Replace the brute-force vector block in Search()**

In `internal/store/index.go`, replace the vector augmentation block (the `for i, fr := range ftsResults` loop that calls `idx.GetEmbedding` per result) with a vec0 KNN query:

```go
// ── Optional vector augmentation via vec0 KNN ─────────────────────────────
var queryVec []float32
if idx.embedder != nil {
    queryVec, err = idx.embedder.Embed(q.Text)
    if err != nil {
        queryVec = nil
    }
}

type candidate struct {
    rec   FactRecord
    score float64
}

// Build path→bm25Score map from FTS results
ftsScoreByPath := make(map[string]float64, len(ftsResults))
ftsRecByPath := make(map[string]FactRecord, len(ftsResults))
for i, fr := range ftsResults {
    ftsScoreByPath[fr.rec.Path] = bm25Scores[i]
    ftsRecByPath[fr.rec.Path] = fr.rec
}

// Vec0 KNN search — two-step: get rowid+distance from vec0, then join with facts.
// vec0 KNN queries are restrictive and JOINs inside the MATCH query may not work.
vecSimByPath := make(map[string]float64)
if queryVec != nil {
    vecBlob := float32SliceToBytes(queryVec)
    rows, err := idx.db.Query(
        `SELECT rowid, distance FROM facts_vec WHERE embedding MATCH ? AND k = ?`,
        vecBlob, limit*5,
    )
    if err == nil {
        type vecHit struct {
            rowid int64
            dist  float64
        }
        var hits []vecHit
        for rows.Next() {
            var h vecHit
            if err := rows.Scan(&h.rowid, &h.dist); err != nil {
                break
            }
            hits = append(hits, h)
        }
        rows.Close()

        // Resolve rowids to paths
        for _, h := range hits {
            var path string
            err := idx.db.QueryRow(`SELECT path FROM facts WHERE rowid = ?`, h.rowid).Scan(&path)
            if err == nil {
                vecSimByPath[path] = 1.0 - h.dist // cosine_similarity = 1 - cosine_distance
            }
        }
    }
}

// Merge FTS + vec results
seen := make(map[string]bool)
candidates := make([]candidate, 0, len(ftsResults)+len(vecSimByPath))

// FTS results with optional vec boost
for _, fr := range ftsResults {
    bm25 := ftsScoreByPath[fr.rec.Path]
    score := bm25
    if cosine, ok := vecSimByPath[fr.rec.Path]; ok {
        score = 0.6*bm25 + 0.4*cosine
    }
    candidates = append(candidates, candidate{rec: fr.rec, score: score})
    seen[fr.rec.Path] = true
}

// Vec-only results (not in FTS set) if cosine_similarity > 0.2
for path, cosine := range vecSimByPath {
    if seen[path] || cosine <= 0.2 {
        continue
    }
    rec, err := idx.GetByPath(path)
    if err != nil || rec == nil {
        continue
    }
    candidates = append(candidates, candidate{rec: *rec, score: 0.4 * cosine})
}
```

Also remove the `dotProduct` function — it's no longer used.

- [ ] **Step 4: Run all store tests**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 ./internal/store/
```

Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/index.go
git commit -m "feat(store): replace brute-force vector search with vec0 KNN"
```

---

### Task 7: Run full test suite

- [ ] **Step 1: Run all tests**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 ./...
```

Expected: ALL PASS. If any package fails due to the schema change (e.g. `vec_data` column references), fix those callers.

- [ ] **Step 2: Check for remaining `vec_data` references**

```bash
grep -rn "vec_data" internal/ cmd/
```

If any references remain, update them to use `facts_vec` or remove them.

- [ ] **Step 3: Fix any broken references and re-test**

- [ ] **Step 4: Commit any fixes**

```bash
git add -u
git commit -m "fix: remove remaining vec_data references after vec0 migration"
```

---

## Chunk 2: Cross-Platform Packaging

### Task 8: Update onnxruntime library resolution to use exe-relative paths

**Files:**
- Modify: `internal/embeddings/embedder.go`
- Create: `internal/embeddings/embedder_test.go`

- [ ] **Step 1: Write test for library candidate paths**

Create `internal/embeddings/embedder_test.go`:

```go
package embeddings

import (
	"runtime"
	"testing"
)

func TestLibCandidates(t *testing.T) {
	candidates := libCandidates("/usr/local/bin")

	if len(candidates) == 0 {
		t.Fatal("expected at least one candidate path")
	}

	// First candidate should be exe-relative
	if candidates[0] != "/usr/local/bin/lib/libonnxruntime.dylib" &&
		candidates[0] != "/usr/local/bin/lib/libonnxruntime.so" &&
		candidates[0] != "/usr/local/bin/lib/onnxruntime.dll" {
		t.Fatalf("first candidate should be exe-relative, got %q", candidates[0])
	}

	// Verify platform-specific candidates
	switch runtime.GOOS {
	case "darwin":
		found := false
		for _, c := range candidates {
			if c == "/opt/homebrew/lib/libonnxruntime.dylib" {
				found = true
			}
		}
		if !found {
			t.Fatal("expected Homebrew fallback on darwin")
		}
	case "linux":
		found := false
		for _, c := range candidates {
			if c == "/usr/local/lib/libonnxruntime.so" {
				found = true
			}
		}
		if !found {
			t.Fatal("expected /usr/local/lib fallback on linux")
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -run TestLibCandidates ./internal/embeddings/
```

Expected: FAIL — `libCandidates` doesn't exist yet.

- [ ] **Step 3: Implement exe-relative library resolution**

In `internal/embeddings/embedder.go`:

1. Add imports: `"path/filepath"`, `"runtime"`

2. Replace the `candidateLibraryPaths` variable and update `initORT()`:

```go
// mustExePath returns the directory containing the running executable.
func mustExePath() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	// Resolve symlinks so dev `go run` still works
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return filepath.Dir(exe)
	}
	return filepath.Dir(resolved)
}

// libCandidates returns ORT shared library paths to try, in priority order.
// The exe-relative path is always first, followed by platform-specific fallbacks.
func libCandidates(exeDir string) []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			filepath.Join(exeDir, "lib", "libonnxruntime.dylib"),
			"/opt/homebrew/lib/libonnxruntime.dylib",
		}
	case "linux":
		return []string{
			filepath.Join(exeDir, "lib", "libonnxruntime.so"),
			"/usr/local/lib/libonnxruntime.so",
			"/usr/lib/libonnxruntime.so",
		}
	case "windows":
		return []string{
			filepath.Join(exeDir, "lib", "onnxruntime.dll"),
		}
	default:
		return nil
	}
}

func initORT() error {
	ortOnce.Do(func() {
		if p := os.Getenv("ORT_LIB_PATH"); p != "" {
			ort.SetSharedLibraryPath(p)
		} else {
			exeDir := mustExePath()
			for _, c := range libCandidates(exeDir) {
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

3. Remove the old `candidateLibraryPaths` variable.

- [ ] **Step 4: Run test**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -run TestLibCandidates ./internal/embeddings/
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/embeddings/embedder.go internal/embeddings/embedder_test.go
git commit -m "feat(embeddings): resolve onnxruntime relative to binary directory"
```

---

### Task 9: Update Makefile with `dist` target and `make run` using dist/lib

**Files:**
- Modify: `Makefile`

- [ ] **Step 1: Update the Makefile**

Replace the entire `Makefile` with:

```makefile
.PHONY: build web test clean run dev setup dist download-ort

ORT_VERSION := 1.24.3
UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)

# Detect platform for ORT download
ifeq ($(UNAME_S),Darwin)
  ifeq ($(UNAME_M),arm64)
    ORT_PLATFORM := osx-arm64
    ORT_LIB_NAME := libonnxruntime.dylib
    ORT_LIB_VERSIONED := libonnxruntime.$(ORT_VERSION).dylib
  else
    ORT_PLATFORM := osx-x86_64
    ORT_LIB_NAME := libonnxruntime.dylib
    ORT_LIB_VERSIONED := libonnxruntime.$(ORT_VERSION).dylib
  endif
else ifeq ($(UNAME_S),Linux)
  ORT_PLATFORM := linux-x64
  ORT_LIB_NAME := libonnxruntime.so
  ORT_LIB_VERSIONED := libonnxruntime.so.$(ORT_VERSION)
else
  ORT_PLATFORM := win-x64
  ORT_LIB_NAME := onnxruntime.dll
  ORT_LIB_VERSIONED := onnxruntime.dll
endif

ORT_URL := https://github.com/microsoft/onnxruntime/releases/download/v$(ORT_VERSION)/onnxruntime-$(ORT_PLATFORM)-$(ORT_VERSION).tgz

setup: download-ort
	@echo "Setup complete. Run 'make run' to start the server."

download-ort:
	@mkdir -p dist/lib
	@if [ ! -f dist/lib/$(ORT_LIB_NAME) ]; then \
		echo "Downloading onnxruntime $(ORT_VERSION) for $(ORT_PLATFORM)..."; \
		curl -sL $(ORT_URL) | tar xz -C /tmp; \
		cp /tmp/onnxruntime-$(ORT_PLATFORM)-$(ORT_VERSION)/lib/$(ORT_LIB_VERSIONED) dist/lib/$(ORT_LIB_NAME); \
		rm -rf /tmp/onnxruntime-$(ORT_PLATFORM)-$(ORT_VERSION); \
		echo "onnxruntime installed to dist/lib/"; \
	fi

build: web
	CGO_ENABLED=1 go build -tags sqlite_fts5 -o dist/knomit ./cmd/knomit/

web:
	cd web && npm ci && npm run build

test:
	CGO_ENABLED=1 go test -tags sqlite_fts5 ./...

dist: download-ort build
	@echo "Distribution package ready in dist/"

run: download-ort
	CGO_ENABLED=1 ORT_LIB_PATH=dist/lib/$(ORT_LIB_NAME) go run -tags sqlite_fts5 ./cmd/knomit/ serve

dev:
	cd web && npm run dev

clean:
	rm -rf dist/ web/dist/
```

- [ ] **Step 2: Verify `make download-ort` works**

```bash
make download-ort
ls -la dist/lib/
```

Expected: `libonnxruntime.dylib` (or platform equivalent) present in `dist/lib/`.

- [ ] **Step 3: Verify `make run` starts the server with bundled ORT**

```bash
make run &
sleep 3
curl -s http://localhost:3000/api/v1/status | head -20
kill %1
```

Expected: Server starts and responds. The status response should show `embeddingsEnabled: true` if the ORT library loads successfully.

- [ ] **Step 4: Commit**

```bash
git add Makefile
git commit -m "feat(build): add dist target with ORT download, make run uses dist/lib"
```

---

## Chunk 3: MCP Profile Selection

### Task 10: Create profile instructions

**Files:**
- Create: `internal/mcp/instructions.go`
- Create: `internal/mcp/instructions_test.go`

- [ ] **Step 1: Write the test**

Create `internal/mcp/instructions_test.go`:

```go
package mcp

import "testing"

func TestProfileInstructions(t *testing.T) {
	// All three profiles should return non-empty strings
	for _, p := range []string{"code", "chat", "generic"} {
		got := ProfileInstructions(p)
		if got == "" {
			t.Fatalf("ProfileInstructions(%q) returned empty string", p)
		}
	}

	// Unknown profile should fall back to "code"
	code := ProfileInstructions("code")
	unknown := ProfileInstructions("unknown")
	if code != unknown {
		t.Fatal("unknown profile should fall back to code")
	}

	// Profiles should differ
	chat := ProfileInstructions("chat")
	if code == chat {
		t.Fatal("code and chat profiles should differ")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -run TestProfileInstructions ./internal/mcp/
```

Expected: FAIL — `ProfileInstructions` doesn't exist.

- [ ] **Step 3: Create instructions.go**

Create `internal/mcp/instructions.go`:

```go
package mcp

// ProfileInstructions returns the MCP server instructions for the given profile.
// Valid profiles: "code", "chat", "generic". Unknown profiles fall back to "code".
func ProfileInstructions(profile string) string {
	addendum, ok := profileAddenda[profile]
	if !ok {
		addendum = profileAddenda["code"]
	}
	return baseInstructions + "\n\n" + addendum
}

const baseInstructions = `You are connected to a knomit knowledge base. Use the available tools to learn, query, and manage knowledge.

Key concepts:
- Facts are stored as markdown files under know/ with YAML frontmatter (domain, entities, confidence, sources, refs)
- Each fact has a path like know/topic/subtopic/fact-name.md
- Use knomit_learn to store new knowledge, knomit_query to search, knomit_why for provenance
- Use knomit_update to modify existing facts, knomit_forget to remove outdated knowledge
- Use knomit_explore to browse the knowledge tree`

var profileAddenda = map[string]string{
	"code": `You are assisting with software development. When learning new facts, prefer structured technical knowledge: architecture decisions, API contracts, debugging findings, conventions, and system behaviors. Use domain tags like "architecture", "debugging", "conventions", "api". Reference source code locations in refs when applicable.`,

	"chat": `You are in a conversational context. When learning new facts, capture insights, preferences, decisions, and context from the conversation. Use natural language for fact bodies. Prefer broader domain tags. Keep confidence scores conservative for subjective knowledge.`,

	"generic": `You are a general-purpose knowledge assistant. Store and retrieve knowledge across any domain. Use descriptive domain and entity tags. Maintain clear, self-contained fact bodies that can be understood without additional context.`,
}
```

- [ ] **Step 4: Run test**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 -run TestProfileInstructions ./internal/mcp/
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/mcp/instructions.go internal/mcp/instructions_test.go
git commit -m "feat(mcp): add profile-specific instructions (code/chat/generic)"
```

---

### Task 11: Wire profile query parameter to MCP handler

**Files:**
- Modify: `internal/mcp/server.go`
- Modify: `internal/web/server.go`
- Modify: `cmd/knomit/main.go`

- [ ] **Step 1: Update NewServer to use profile instructions**

In `internal/mcp/server.go`, use the profile to set server instructions:

```go
func NewServer(gs GitStore, idx SearchIndex, llmAdapter llm.LLMAdapter, profile string) *server.MCPServer {
	s := server.NewMCPServer("knomit", "1.0.0",
		server.WithInstructions(ProfileInstructions(profile)),
	)

	s.AddTool(learnTool(), LearnHandler(gs, idx))
	s.AddTool(queryTool(), QueryHandler(gs, idx))
	s.AddTool(whyTool(), WhyHandler(gs))
	s.AddTool(updateTool(), UpdateHandler(gs, idx))
	s.AddTool(exploreTool(), ExploreHandler(gs))
	s.AddTool(forgetTool(), ForgetHandler(gs, idx))

	return s
}
```

Remove the `_ = mcpgo.NewTool`, `_ = llmAdapter`, `_ = profile` lines.

Note: Check if `server.WithInstructions` exists in mcp-go v0.45.0. If not, use `server.WithResourceCapabilities` or set instructions via a prompt resource. Verify:

```bash
grep -rn "WithInstructions" "$(go env GOMODCACHE)/github.com/mark3labs/mcp-go@v0.45.0/"
```

If `WithInstructions` doesn't exist, register a prompt resource instead:

```go
s.AddPrompt(mcpgo.Prompt{
    Name:        "instructions",
    Description: "Knowledge base usage instructions",
}, func(ctx context.Context, req mcpgo.GetPromptRequest) (*mcpgo.GetPromptResult, error) {
    return &mcpgo.GetPromptResult{
        Messages: []mcpgo.PromptMessage{{
            Role:    mcpgo.RoleUser,
            Content: mcpgo.TextContent{Text: ProfileInstructions(profile)},
        }},
    }, nil
})
```

- [ ] **Step 2: Create per-profile MCP servers in main.go**

In `cmd/knomit/main.go`, replace the single MCP server creation with a map:

```go
// 6. Create per-profile MCP servers
profiles := []string{"code", "chat", "generic"}
mcpServers := make(map[string]http.Handler, len(profiles))
for _, p := range profiles {
    srv := mcp.NewServer(gs, idx, llmAdapter, p)
    mcpServers[p] = mcpserver.NewStreamableHTTPServer(srv)
}
```

Update the `NewRouter` call to accept `mcpServers` instead of a single `mcpHandler`.

- [ ] **Step 3: Add profile-dispatching middleware in web/server.go**

In `internal/web/server.go`, update `NewRouter` to accept `map[string]http.Handler`:

```go
func NewRouter(gs GitStore, idx SearchIndex, hub *TaskHub, synthDeps *SynthDeps,
    mcpHandlers map[string]http.Handler, gitHandler http.Handler, embeddingsEnabled bool) http.Handler {

    r := chi.NewRouter()
    r.Use(middleware.Recoverer)

    if len(mcpHandlers) > 0 {
        r.Mount("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            profile := r.URL.Query().Get("profile")
            if profile == "" {
                profile = "code"
            }
            handler, ok := mcpHandlers[profile]
            if !ok {
                handler = mcpHandlers["code"]
            }
            handler.ServeHTTP(w, r)
        }))
    }
    // ... rest unchanged
```

- [ ] **Step 4: Update any callers of NewRouter**

Check `internal/web/` for test files that call `NewRouter` and update the signature:

```bash
grep -rn "NewRouter" internal/web/
```

Update test helpers to pass `map[string]http.Handler` instead of a single handler.

- [ ] **Step 5: Run all tests**

```bash
CGO_ENABLED=1 go test -tags sqlite_fts5 ./...
```

Expected: ALL PASS

- [ ] **Step 6: Commit**

```bash
git add internal/mcp/server.go internal/web/server.go cmd/knomit/main.go
git add internal/web/*_test.go  # if any test files changed
git commit -m "feat(mcp): wire ?profile= query param for code/chat/generic profiles"
```

---

## Chunk 4: Final Verification

### Task 12: Full build + test + manual smoke test

- [ ] **Step 1: Clean build**

```bash
make clean && make dist
```

Expected: `dist/knomit` binary and `dist/lib/libonnxruntime.dylib` (or platform equivalent).

- [ ] **Step 2: Full test suite**

```bash
make test
```

Expected: ALL PASS

- [ ] **Step 3: Manual smoke test**

```bash
make run &
sleep 5

# Test status endpoint
curl -s http://localhost:3000/api/v1/status

# Test MCP with default profile
curl -s -X POST http://localhost:3000/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}},"id":1}'

# Test MCP with chat profile
curl -s -X POST "http://localhost:3000/mcp?profile=chat" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}},"id":1}'

kill %1
```

Expected: Both MCP requests return valid initialize responses. Status shows embeddings enabled.

- [ ] **Step 4: Verify no Homebrew dependency for running**

The `make run` target should work without `brew install onnxruntime` — it downloads ORT to `dist/lib/` and sets `ORT_LIB_PATH`.

- [ ] **Step 5: Final commit if any loose changes**

```bash
git status
# If clean, no commit needed
```
