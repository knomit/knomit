// Package store implements the knomit search index backed by SQLite and
// sqlite-vec. It provides vector similarity search and filtered search over
// a git-backed knowledge base of fact files.
//
// The package is split across several files:
//
//   - index.go   — Core types, interfaces, Index constructor, and schema DDL.
//   - crud.go    — Upsert, Delete, GetByPath, GetEmbedding, meta key-value ops.
//   - search.go  — Vector search, filters, and hybrid scoring.
//   - sync.go    — Git sync (full rebuild + incremental diff).
//   - parse.go   — Fact markdown file parsing (YAML frontmatter + body).
//   - vec.go     — Vector encoding/decoding, pairwise distances, sqlite-vec init.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"

	"knomit/internal/fact"
	"knomit/internal/store/migrate"
)

// ────────────────────────────────────────────────────────────────────────────
// Domain types
// ────────────────────────────────────────────────────────────────────────────

// FactRecord is the stored record — no body, just a blob_hash pointer.
type FactRecord struct {
	Path           string   `json:"path"`
	Title          string   `json:"title"`
	BlobHash       string   `json:"blob_hash"`
	Type           string   `json:"type"`
	Domain         []string `json:"domain"`
	Entities       []string `json:"entities"`
	Confidence     float64  `json:"confidence"`
	Sources        int      `json:"sources"`
	Refs           []string `json:"refs"`
	EvidenceWeight float64  `json:"evidence_weight,omitempty"`
}

// NewFactRecord constructs a FactRecord from a parsed fact and git metadata.
// blobHash is the blob SHA returned by WriteFile.
func NewFactRecord(f fact.Fact, blobHash string) FactRecord {
	return FactRecord{
		Path:           f.Path(),
		Title:          f.Title,
		BlobHash:       blobHash,
		Type:           string(f.Type),
		Domain:         f.Domain,
		Entities:       f.Entities,
		Confidence:     f.Confidence,
		Sources:        f.Sources,
		Refs:           f.Refs,
		EvidenceWeight: f.EvidenceWeight,
	}
}

// FactWithBody is returned by read operations that hydrate the body from git objects.
type FactWithBody struct {
	FactRecord
	Body       string `json:"body"`
	CommitHash string `json:"commit_hash,omitempty"`
}

// ────────────────────────────────────────────────────────────────────────────
// Interfaces
// ────────────────────────────────────────────────────────────────────────────

// Embedder computes vector embeddings for text. When attached to an Index via
// SetEmbedder, Upsert will embed each fact's body and store it in facts_vec.
type Embedder interface {
	Embed(text string) ([]float32, error)
}

// BatchEmbedder extends Embedder with batch inference support.
type BatchEmbedder interface {
	Embedder
	EmbedBatch(texts []string) ([][]float32, error)
}

// GitReader is the interface that Index.Sync requires from the git store.
type GitReader interface {
	// DiffFiles returns paths added, modified, and deleted between fromCommit and HEAD on branch.
	DiffFiles(branch, fromCommit string) (added, modified, deleted []string, err error)
	// ReadFile reads the content of path from the HEAD commit of branch.
	ReadFile(branch, path string) (string, error)
	// ReadFileWithHash returns both the file content and the blob hash for the given path on branch.
	ReadFileWithHash(branch, path string) (content string, blobHash string, err error)
	// HeadCommit returns the hash of the current HEAD commit of branch as a hex string.
	HeadCommit(branch string) (string, error)
	// ListAll returns paths of all .md files from HEAD of branch.
	ListAll(branch string) ([]string, error)
	// ListAllWithHash returns all .md file paths and their blob hashes from HEAD of branch.
	// Single tree walk, no per-file I/O.
	ListAllWithHash(branch string) (paths []string, blobHashes []string, err error)
	// LastCommitForPath returns the hash of the most recent non-merge commit that touched path on branch.
	LastCommitForPath(branch, path string) (string, error)
	// ReadFileAtCommit reads the content of path at the given commit on branch.
	// branch is used for repository context; commitHash uniquely identifies the version.
	ReadFileAtCommit(branch, path, commitHash string) (string, error)
}

// ────────────────────────────────────────────────────────────────────────────
// Index (constructor + lifecycle)
// ────────────────────────────────────────────────────────────────────────────

// Index is the search index backed by SQLite with sqlite-vec.
type Index struct {
	db       *sql.DB
	embedMu  sync.RWMutex
	embedder Embedder
	branches *branchCache
}

// newIndex wraps an existing *sql.DB. Schema must already be applied.
// Used by Service.Open to construct the Index over the shared database.
func newIndex(db *sql.DB) *Index {
	return &Index{db: db, branches: newBranchCache()}
}

// SetEmbedder attaches an Embedder to the index. When set, Upsert will call
// Embed on each record's body and persist the result in facts_vec.
func (idx *Index) SetEmbedder(e Embedder) {
	idx.embedMu.Lock()
	defer idx.embedMu.Unlock()
	idx.embedder = e
}

// EmbedderSet reports whether an Embedder has been attached to this index.
func (idx *Index) EmbedderSet() bool {
	idx.embedMu.RLock()
	defer idx.embedMu.RUnlock()
	return idx.embedder != nil
}

// getEmbedder returns the current Embedder under a read lock.
func (idx *Index) getEmbedder() Embedder {
	idx.embedMu.RLock()
	defer idx.embedMu.RUnlock()
	return idx.embedder
}

// New opens (or creates) a SQLite search index at path and applies all
// migrations including vec0 and GraphQLite.
// Use ":memory:" for an in-memory database (useful in tests).
func New(path string) (*Index, error) {
	registerVec()

	dsn := path
	if path == ":memory:" {
		dsn = path + "?_foreign_keys=1"
	} else {
		dsn = path + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"
	}
	db, err := sql.Open("sqlite3_knomit", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.Exec("PRAGMA optimize")

	if err := migrate.All(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.New: %w", err)
	}

	return &Index{db: db, branches: newBranchCache()}, nil
}

// Close closes the underlying database connection.
func (idx *Index) Close() error {
	return idx.db.Close()
}

// ────────────────────────────────────────────────────────────────────────────
// Schema DDL
// ────────────────────────────────────────────────────────────────────────────

// extractBody strips YAML frontmatter from raw markdown and returns just the body.
// It assumes the format: ---\n...\n---\n# Title\n\nBody
func extractBody(raw []byte) string {
	content := string(raw)
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return content
	}
	afterFrontmatter := strings.TrimSpace(parts[2])
	// Skip the title line (first # heading)
	if idx := strings.Index(afterFrontmatter, "\n"); idx >= 0 {
		return strings.TrimSpace(afterFrontmatter[idx+1:])
	}
	return ""
}


// Completions returns autocomplete suggestions for a given filter category and prefix,
// scoped to the given branch.
// Supported categories: "domain", "entity", "type", "ep", "path".
func (idx *Index) Completions(ctx context.Context, branch, category, prefix string, limit int) ([]string, error) {
	branchID, err := idx.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("completions: %w", err)
	}

	switch category {
	case "domain":
		return idx.queryDistinct(ctx,
			`SELECT DISTINCT fd.domain FROM fact_domains fd
			 JOIN branch_facts bf ON bf.fact_id = fd.fact_id
			 WHERE bf.branch_id = ? AND fd.domain LIKE ? LIMIT ?`,
			branchID, prefix+"%", limit)
	case "entity":
		return idx.queryDistinct(ctx,
			`SELECT DISTINCT fe.entity FROM fact_entities fe
			 JOIN branch_facts bf ON bf.fact_id = fe.fact_id
			 WHERE bf.branch_id = ? AND fe.entity LIKE ? LIMIT ?`,
			branchID, prefix+"%", limit)
	case "type":
		return []string{"observation", "concept", "process", "principle", "pattern", "reference", "synthesis", "hypothesis", "methodology"}, nil
	case "ep":
		return []string{"learn", "update", "retract", "subsume", "synthesize", "sync"}, nil
	case "path":
		rows, err := conn(ctx, idx.db).QueryContext(ctx,
			`SELECT DISTINCT f.path
			 FROM branch_facts bf
			 JOIN facts f ON f.id = bf.fact_id
			 WHERE bf.branch_id = ? AND f.path LIKE ? LIMIT 500`,
			branchID, prefix+"%")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		dirs := map[string]bool{}
		prefixLen := len(prefix)
		for rows.Next() {
			var p string
			rows.Scan(&p)
			// Find the next '/' after the prefix to extract directory components
			rest := p[prefixLen:]
			if i := strings.Index(rest, "/"); i >= 0 {
				dirs[p[:prefixLen+i]] = true
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		vals := make([]string, 0, len(dirs))
		for d := range dirs {
			vals = append(vals, d)
		}
		sort.Strings(vals)
		if len(vals) > limit {
			vals = vals[:limit]
		}
		return vals, nil
	default:
		return nil, fmt.Errorf("unknown completion category: %s", category)
	}
}

// queryDistinct executes a query and returns distinct non-empty string values.
func (idx *Index) queryDistinct(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := conn(ctx, idx.db).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var vals []string
	for rows.Next() {
		var v string
		rows.Scan(&v)
		if v != "" {
			vals = append(vals, v)
		}
	}
	return vals, rows.Err()
}

// StatsResult holds aggregate statistics computed from the facts table.
type StatsResult struct {
	Total         int            `json:"total"`
	AvgConfidence float64        `json:"avg_confidence"`
	Domains       map[string]int `json:"domains"`
	Entities      map[string]int `json:"entities"`
}

// Stats returns aggregate statistics over all indexed facts on a branch,
// optionally filtered to those whose path starts with pathPrefix.
func (idx *Index) Stats(ctx context.Context, branch, pathPrefix string) (StatsResult, error) {
	res := StatsResult{
		Domains:  make(map[string]int),
		Entities: make(map[string]int),
	}

	branchID, err := idx.branchID(ctx, branch)
	if err != nil {
		return res, fmt.Errorf("stats: %w", err)
	}

	// Total count and average confidence.
	var avgConf *float64
	q := `SELECT COUNT(*), AVG(f.confidence)
	      FROM branch_facts bf
	      JOIN facts f ON f.id = bf.fact_id
	      WHERE bf.branch_id = ?`
	args := []any{branchID}
	if pathPrefix != "" {
		q += ` AND f.path LIKE ?`
		args = append(args, pathPrefix+"%")
	}
	if err := conn(ctx, idx.db).QueryRowContext(ctx, q, args...).Scan(&res.Total, &avgConf); err != nil {
		return res, err
	}
	if avgConf != nil {
		// Round to 2 decimal places.
		res.AvgConfidence = float64(int(*avgConf*100+0.5)) / 100
	}

	// Domain counts via json_each.
	dq := `SELECT d.value, COUNT(*)
	       FROM branch_facts bf
	       JOIN facts f ON f.id = bf.fact_id, json_each(f.domain) d
	       WHERE bf.branch_id = ? AND d.value IS NOT NULL`
	dargs := []any{branchID}
	if pathPrefix != "" {
		dq += ` AND f.path LIKE ?`
		dargs = append(dargs, pathPrefix+"%")
	}
	dq += ` GROUP BY d.value`
	drows, err := conn(ctx, idx.db).QueryContext(ctx, dq, dargs...)
	if err != nil {
		return res, err
	}
	defer drows.Close()
	for drows.Next() {
		var k string
		var n int
		if err := drows.Scan(&k, &n); err != nil {
			return res, err
		}
		res.Domains[k] = n
	}

	// Entity counts via json_each.
	eq := `SELECT e.value, COUNT(*)
	       FROM branch_facts bf
	       JOIN facts f ON f.id = bf.fact_id, json_each(f.entities) e
	       WHERE bf.branch_id = ? AND e.value IS NOT NULL`
	eargs := []any{branchID}
	if pathPrefix != "" {
		eq += ` AND f.path LIKE ?`
		eargs = append(eargs, pathPrefix+"%")
	}
	eq += ` GROUP BY e.value`
	erows, err := conn(ctx, idx.db).QueryContext(ctx, eq, eargs...)
	if err != nil {
		return res, err
	}
	defer erows.Close()
	for erows.Next() {
		var k string
		var n int
		if err := erows.Scan(&k, &n); err != nil {
			return res, err
		}
		res.Entities[k] = n
	}

	return res, nil
}
