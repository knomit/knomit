package store

import "database/sql"

// FactRow holds the minimal fields needed to replay a fact into another store.
type FactRow struct {
	Path       string
	BlobHash   string
	CommitHash string
}

// FactsIter is a cursor-based iterator over the branch_facts table. It yields
// facts one at a time, newest-first (by fact_id DESC), deduplicating by path
// so that only the latest version of each fact is returned. It never loads all
// facts into memory.
type FactsIter struct {
	rows *sql.Rows
	seen map[string]struct{}
}

// Next returns the next unique fact, or nil when iteration is complete.
// It skips paths that have already been yielded (dedup).
func (it *FactsIter) Next() (*FactRow, error) {
	for it.rows.Next() {
		var row FactRow
		if err := it.rows.Scan(&row.Path, &row.BlobHash, &row.CommitHash); err != nil {
			return nil, err
		}
		if _, dup := it.seen[row.Path]; dup {
			continue
		}
		it.seen[row.Path] = struct{}{}
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
