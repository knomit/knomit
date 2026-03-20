package store

import (
	"database/sql"
)

// FactRow holds the minimal fields needed to replay a fact into another store.
type FactRow struct {
	Path       string
	BlobHash   string
	CommitHash string
}

// FactsIter is a cursor-based iterator over the facts table. It yields facts
// one at a time, newest-first (by rowid DESC), deduplicating by path so that
// only the latest version of each fact is returned. It never loads all facts
// into memory.
type FactsIter struct {
	rows *sql.Rows
	seen map[string]bool
}

// NewFactsIter opens a cursor over the facts table ordered by rowid DESC.
// The caller must call Close() when done to release the underlying database cursor.
func NewFactsIter(db *sql.DB) (*FactsIter, error) {
	rows, err := db.Query(`SELECT path, blob_hash, commit_hash FROM facts ORDER BY rowid DESC`)
	if err != nil {
		return nil, err
	}
	return &FactsIter{rows: rows, seen: make(map[string]bool)}, nil
}

// Next returns the next unique fact, or nil when iteration is complete.
// It skips paths that have already been yielded (dedup).
func (it *FactsIter) Next() (*FactRow, error) {
	for it.rows.Next() {
		var row FactRow
		if err := it.rows.Scan(&row.Path, &row.BlobHash, &row.CommitHash); err != nil {
			return nil, err
		}
		if it.seen[row.Path] {
			continue
		}
		it.seen[row.Path] = true
		return &row, nil
	}
	return nil, it.rows.Err()
}

// Close releases the underlying database cursor. It is safe to call multiple times.
func (it *FactsIter) Close() error {
	if it.rows != nil {
		err := it.rows.Close()
		it.rows = nil
		return err
	}
	return nil
}
