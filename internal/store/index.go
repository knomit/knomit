// Package store implements the knomit search index backed by SQLite FTS5 and
// sqlite-vec. It provides full-text search, vector similarity search, and
// hybrid search over a git-backed knowledge base of fact files.
//
// The package is split across several files:
//
//   - index.go   — Core types, interfaces, Index constructor, and schema DDL.
//   - crud.go    — Upsert, Delete, GetByPath, GetEmbedding, meta key-value ops.
//   - search.go  — FTS5 text search, hybrid vector+text search, filters.
//   - sync.go    — Git sync (full rebuild + incremental diff).
//   - parse.go   — Fact markdown file parsing (YAML frontmatter + body).
//   - vec.go     — Vector encoding/decoding, pairwise distances, sqlite-vec init.
//
// Build with -tags fts5 to enable FTS5 support (required).
package store

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

// ────────────────────────────────────────────────────────────────────────────
// Configuration
// ────────────────────────────────────────────────────────────────────────────

type indexConfig struct {
	vecDim int
}

// Option configures an Index.
type Option func(*indexConfig)

// WithVecDimension sets the dimension of the facts_vec embedding column.
func WithVecDimension(d int) Option {
	return func(c *indexConfig) { c.vecDim = d }
}

// ────────────────────────────────────────────────────────────────────────────
// Domain types
// ────────────────────────────────────────────────────────────────────────────

// FactRecord represents a single fact stored in the index.
type FactRecord struct {
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Domain     []string `json:"domain"`
	Entities   []string `json:"entities"`
	Confidence float64  `json:"confidence"`
	Sources    int      `json:"sources"`
	Refs       []string `json:"refs"`
	CommitHash string   `json:"commit_hash,omitempty"`
}

// ────────────────────────────────────────────────────────────────────────────
// Interfaces
// ────────────────────────────────────────────────────────────────────────────

// Embedder computes vector embeddings for text. When attached to an Index via
// SetEmbedder, Upsert will embed each fact's body and store it in facts_vec.
type Embedder interface {
	Embed(text string) ([]float32, error)
}

// GitReader is the interface that Index.Sync requires from the git store.
type GitReader interface {
	// DiffFiles returns paths added, modified, and deleted between fromCommit and HEAD.
	DiffFiles(fromCommit string) (added, modified, deleted []string, err error)
	// ReadFile reads the content of path from the HEAD commit.
	ReadFile(path string) (string, error)
	// HeadCommit returns the hash of the current HEAD commit as a hex string.
	HeadCommit() (string, error)
	// ListAll returns paths of all .md files from HEAD.
	ListAll() ([]string, error)
}

// ────────────────────────────────────────────────────────────────────────────
// Index (constructor + lifecycle)
// ────────────────────────────────────────────────────────────────────────────

// Index is the search index backed by SQLite with FTS5 and sqlite-vec.
type Index struct {
	db       *sql.DB
	embedder Embedder
}

// SetEmbedder attaches an Embedder to the index. When set, Upsert will call
// Embed on each record's body and persist the result in facts_vec.
func (idx *Index) SetEmbedder(e Embedder) {
	idx.embedder = e
}

// New opens (or creates) a SQLite search index at path.
// Use ":memory:" for an in-memory database (useful in tests).
func New(path string, opts ...Option) (*Index, error) {
	registerVec()

	cfg := indexConfig{vecDim: 768}
	for _, o := range opts {
		o(&cfg)
	}

	dsn := path
	if path != ":memory:" {
		dsn = path + "?_journal_mode=WAL&_busy_timeout=5000"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schemaSQL(cfg.vecDim)); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if _, err = db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES ('schema_version', '3')`); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema_version: %w", err)
	}

	return &Index{db: db}, nil
}

// DB returns the underlying *sql.DB handle.
func (idx *Index) DB() *sql.DB { return idx.db }

// Close closes the underlying database connection.
func (idx *Index) Close() error {
	return idx.db.Close()
}

// ────────────────────────────────────────────────────────────────────────────
// Schema DDL
// ────────────────────────────────────────────────────────────────────────────

// schemaSQL returns the DDL to create all tables: facts (main), facts_fts
// (FTS5 full-text), facts_vec (vec0 embeddings), meta (key-value), and
// synthesis_log.
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
