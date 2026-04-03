package store

import (
	"context"
	"database/sql"
)

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

// FactsIter opens a cursor over facts for the given branch ordered by
// fact_id DESC. The caller must call Close() when done to release the
// underlying database cursor.
func (s *Service) FactsIter(ctx context.Context, branch string) (*FactsIter, error) {
	branchID, err := s.idx.rh.branchID(ctx, branch)
	if err != nil {
		return nil, err
	}
	rows, err := conn(ctx, s.db).QueryContext(ctx,
		`SELECT bf.path, f.blob_hash, bf.commit_hash
		 FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 WHERE bf.branch_id = ?
		 ORDER BY bf.fact_id DESC`,
		branchID,
	)
	if err != nil {
		return nil, err
	}
	return &FactsIter{rows: rows, seen: make(map[string]struct{})}, nil
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
