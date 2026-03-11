package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

const schema = `
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
`

// FactRecord represents a single fact stored in the index.
type FactRecord struct {
	Path       string
	Title      string
	Body       string
	Domain     []string
	Entities   []string
	Confidence float64
	Sources    int
	Refs       []string
	CommitHash string
}

// Index is the search index backed by SQLite with FTS5.
type Index struct {
	db *sql.DB
}

// New opens (or creates) a SQLite search index at path.
// Use ":memory:" for an in-memory database (useful in tests).
func New(path string) (*Index, error) {
	dsn := path
	if path != ":memory:" {
		dsn = path + "?_journal_mode=WAL&_busy_timeout=5000"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Index{db: db}, nil
}

// Close closes the underlying database connection.
func (idx *Index) Close() error {
	return idx.db.Close()
}

// Upsert inserts or replaces a FactRecord, keeping the FTS5 index in sync.
func (idx *Index) Upsert(rec FactRecord) error {
	domainJSON, err := json.Marshal(rec.Domain)
	if err != nil {
		return fmt.Errorf("marshal domain: %w", err)
	}
	entitiesJSON, err := json.Marshal(rec.Entities)
	if err != nil {
		return fmt.Errorf("marshal entities: %w", err)
	}
	refsJSON, err := json.Marshal(rec.Refs)
	if err != nil {
		return fmt.Errorf("marshal refs: %w", err)
	}

	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Step 1: Delete old FTS row if the path already exists
	if _, err := tx.Exec(
		`DELETE FROM facts_fts WHERE rowid = (SELECT rowid FROM facts WHERE path=?)`,
		rec.Path,
	); err != nil {
		return fmt.Errorf("delete old fts row: %w", err)
	}

	// Step 2: Insert or replace into facts
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO facts(path, title, body, domain, entities, confidence, sources, refs, commit_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Path, rec.Title, rec.Body,
		string(domainJSON), string(entitiesJSON),
		rec.Confidence, rec.Sources,
		string(refsJSON), rec.CommitHash,
	); err != nil {
		return fmt.Errorf("upsert fact: %w", err)
	}

	// Step 3: Insert into FTS using the new rowid
	if _, err := tx.Exec(
		`INSERT INTO facts_fts(rowid, title, body, entities, domain)
		 VALUES ((SELECT rowid FROM facts WHERE path=?), ?, ?, ?, ?)`,
		rec.Path, rec.Title, rec.Body, string(entitiesJSON), string(domainJSON),
	); err != nil {
		return fmt.Errorf("insert fts row: %w", err)
	}

	return tx.Commit()
}

// Delete removes a fact and its FTS entry by path.
func (idx *Index) Delete(path string) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`DELETE FROM facts_fts WHERE rowid = (SELECT rowid FROM facts WHERE path=?)`,
		path,
	); err != nil {
		return fmt.Errorf("delete fts row: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM facts WHERE path=?`, path); err != nil {
		return fmt.Errorf("delete fact: %w", err)
	}

	return tx.Commit()
}

// GetByPath retrieves a FactRecord by its path. Returns nil, nil if not found.
func (idx *Index) GetByPath(path string) (*FactRecord, error) {
	row := idx.db.QueryRow(
		`SELECT path, title, body, domain, entities, confidence, sources, refs, commit_hash
		 FROM facts WHERE path=?`,
		path,
	)
	return scanFactRecord(row)
}

// GetEmbedding returns the stored embedding vector for a fact.
// Returns nil, nil if not set (vector storage added in Task 9).
func (idx *Index) GetEmbedding(path string) ([]float32, error) {
	return nil, nil
}

// SetLastCommit stores the last processed commit hash in the meta table.
func (idx *Index) SetLastCommit(hash string) error {
	_, err := idx.db.Exec(
		`INSERT OR REPLACE INTO meta(key, value) VALUES ('last_commit', ?)`,
		hash,
	)
	return err
}

// GetLastCommit returns the last processed commit hash, or "" if not set.
func (idx *Index) GetLastCommit() (string, error) {
	var hash string
	err := idx.db.QueryRow(`SELECT value FROM meta WHERE key='last_commit'`).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return hash, nil
}

// SearchText queries the FTS5 index and returns matching FactRecords.
func (idx *Index) SearchText(query string, limit int) ([]FactRecord, error) {
	rows, err := idx.db.Query(
		`SELECT f.path, f.title, f.body, f.domain, f.entities, f.confidence, f.sources, f.refs, f.commit_hash
		 FROM facts_fts
		 JOIN facts f ON facts_fts.rowid = f.rowid
		 WHERE facts_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		query, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("fts query: %w", err)
	}
	defer rows.Close()

	var results []FactRecord
	for rows.Next() {
		rec, err := scanFactRecordFromRows(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, *rec)
	}
	return results, rows.Err()
}

// scanFactRecord scans a single FactRecord from a *sql.Row.
func scanFactRecord(row *sql.Row) (*FactRecord, error) {
	var rec FactRecord
	var domainJSON, entitiesJSON, refsJSON string
	err := row.Scan(
		&rec.Path, &rec.Title, &rec.Body,
		&domainJSON, &entitiesJSON,
		&rec.Confidence, &rec.Sources,
		&refsJSON, &rec.CommitHash,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan fact: %w", err)
	}
	if err := json.Unmarshal([]byte(domainJSON), &rec.Domain); err != nil {
		return nil, fmt.Errorf("unmarshal domain: %w", err)
	}
	if err := json.Unmarshal([]byte(entitiesJSON), &rec.Entities); err != nil {
		return nil, fmt.Errorf("unmarshal entities: %w", err)
	}
	if err := json.Unmarshal([]byte(refsJSON), &rec.Refs); err != nil {
		return nil, fmt.Errorf("unmarshal refs: %w", err)
	}
	return &rec, nil
}

// scanFactRecordFromRows scans a FactRecord from *sql.Rows.
func scanFactRecordFromRows(rows *sql.Rows) (*FactRecord, error) {
	var rec FactRecord
	var domainJSON, entitiesJSON, refsJSON string
	err := rows.Scan(
		&rec.Path, &rec.Title, &rec.Body,
		&domainJSON, &entitiesJSON,
		&rec.Confidence, &rec.Sources,
		&refsJSON, &rec.CommitHash,
	)
	if err != nil {
		return nil, fmt.Errorf("scan fact row: %w", err)
	}
	if err := json.Unmarshal([]byte(domainJSON), &rec.Domain); err != nil {
		return nil, fmt.Errorf("unmarshal domain: %w", err)
	}
	if err := json.Unmarshal([]byte(entitiesJSON), &rec.Entities); err != nil {
		return nil, fmt.Errorf("unmarshal entities: %w", err)
	}
	if err := json.Unmarshal([]byte(refsJSON), &rec.Refs); err != nil {
		return nil, fmt.Errorf("unmarshal refs: %w", err)
	}
	return &rec, nil
}
