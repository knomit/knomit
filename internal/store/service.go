package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

	storegit "knomit/internal/store/git"
)

//go:embed schema.sql
var schemaSQL_ string

// BlobObjectType is the go-git integer for plumbing.BlobObject.
const BlobObjectType = 3

// Service is the single entry point for all database access. It opens one
// SQLite file with sqlite-vec + GraphQLite extensions, runs the embedded
// schema, and provides both a go-git Storer and an Index over the shared *sql.DB.
type Service struct {
	db    *sql.DB
	idx   *Index
	gits  *storegit.Storer
	crypt *Crypt // nil if no key material provided
}

// Open opens (or creates) a unified SQLite database at path, initializes the
// schema, and returns a Service that provides access to both the git storer
// and the search index.
func Open(path string, opts ...Option) (*Service, error) {
	cfg := indexConfig{vecDim: 768}
	for _, o := range opts {
		o(&cfg)
	}

	registerVec() // one-time sqlite-vec + GraphQLite driver registration

	dsn := path
	if path == ":memory:" {
		dsn = path + "?_foreign_keys=1"
	} else {
		dsn = path + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"
	}
	db, err := sql.Open("sqlite3_knomit", dsn)
	if err != nil {
		return nil, fmt.Errorf("store.Open: %w", err)
	}
	// SQLite serializes writes internally; limiting the pool avoids SQLITE_BUSY
	// contention between pooled connections competing for the write lock.
	db.SetMaxOpenConns(2)

	// Run embedded schema.
	if _, err := db.Exec(schemaSQL_); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open schema: %w", err)
	}

	// Schema migrations for existing databases.
	migrations := []string{
		`ALTER TABLE remotes ADD COLUMN auth_method TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE remotes ADD COLUMN auth_token TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE review_work_items ADD COLUMN depth INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE facts ADD COLUMN evidence_weight REAL NOT NULL DEFAULT 0`,
		// Pipeline table renames (review_* → pipeline_*)
		`ALTER TABLE review_watermarks RENAME TO pipeline_watermarks`,
		`ALTER TABLE pipeline_watermarks ADD COLUMN tool TEXT NOT NULL DEFAULT 'review'`,
		`ALTER TABLE review_sessions RENAME TO pipeline_sessions`,
		`ALTER TABLE pipeline_sessions ADD COLUMN tool TEXT NOT NULL DEFAULT 'review'`,
		`ALTER TABLE review_work_items RENAME TO pipeline_work_items`,
	}
	for _, m := range migrations {
		db.Exec(m) // ignore "duplicate column" errors
	}

	// Create vec0 virtual table (dimension is configurable).
	vecDDL := fmt.Sprintf(
		`CREATE VIRTUAL TABLE IF NOT EXISTS facts_vec USING vec0(embedding FLOAT[%d] distance_metric=cosine)`,
		cfg.vecDim,
	)
	if _, err := db.Exec(vecDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open vec0: %w", err)
	}

	// Initialize GraphQLite EAV tables.
	if _, err := db.Exec(`SELECT cypher('RETURN 1')`); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open graphqlite: %w", err)
	}

	gits := storegit.NewStorer(db)
	idx := newIndex(db)

	return &Service{db: db, idx: idx, gits: gits}, nil
}

// SetCrypt sets the encryption provider for credential storage.
func (s *Service) SetCrypt(c *Crypt) { s.crypt = c }

// Index returns the search index.
func (s *Service) Index() *Index { return s.idx }

// GitStorer returns the go-git storer.
func (s *Service) GitStorer() *storegit.Storer { return s.gits }

// DB returns the underlying *sql.DB handle.
func (s *Service) DB() *sql.DB { return s.db }

// Close closes the underlying database connection.
func (s *Service) Close() error { return s.db.Close() }

// BeginTx starts a new database transaction.
func (s *Service) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, opts)
}

// GitWriter is the minimal interface needed for DeleteFact.
type GitWriter interface {
	DeleteFile(path, message, operation string) (string, error)
}

// DeleteFact deletes a fact from the git store; the onCommit observer
// handles index cleanup automatically via idx.Sync.
func (s *Service) DeleteFact(gw GitWriter, path, message string) error {
	if _, err := gw.DeleteFile(path, message, "retract"); err != nil {
		return fmt.Errorf("DeleteFact git: %w", err)
	}

	return nil
}
