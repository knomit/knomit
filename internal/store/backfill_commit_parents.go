package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"
)

// commitParentsBackfillKey is the meta-table sentinel that records whether
// the one-time commit_parents backfill has run for this database. Once set,
// CommitLogSync maintains commit_parents incrementally for every new commit.
const commitParentsBackfillKey = "commit_parents_backfilled"

// backfillCommitParents populates commit_parents for every commit already
// recorded in branch_commits, reading parent edges via go-git. Idempotent:
// guarded by a meta-table sentinel so it runs at most once per database.
//
// Why this is needed: commit_parents was added after commit_log, so existing
// repos boot with commit_log populated but commit_parents empty. The
// recursive-CTE walk in resolveActiveCommitForPath would short-circuit at
// depth 1 without this. New commits are appended in-line by CommitLogSync;
// only the historical backfill needs this one-off pass.
func backfillCommitParents(ctx context.Context, rh *repoHandler) error {
	db := rh.db
	var done string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, commitParentsBackfillKey).Scan(&done)
	if err == nil && done == "1" {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("backfillCommitParents: probe meta: %w", err)
	}

	// Pull every distinct commit hash visible on any branch. branch_commits is
	// the canonical visibility record; commit_log alone would miss visibility-
	// only rows (commits with no file changes).
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT commit_hash FROM branch_commits`)
	if err != nil {
		return fmt.Errorf("backfillCommitParents: query branch_commits: %w", err)
	}
	hashes := make([]string, 0, 1024)
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return fmt.Errorf("backfillCommitParents: scan: %w", err)
		}
		hashes = append(hashes, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backfillCommitParents: rows: %w", err)
	}

	if len(hashes) == 0 {
		// Empty repo — nothing to backfill. Still mark done so we don't probe
		// on every Open.
		return markCommitParentsBackfilled(ctx, db)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("backfillCommitParents: begin: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO commit_parents (commit_hash, parent_order, parent_hash) VALUES (?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("backfillCommitParents: prepare: %w", err)
	}
	var inserted int
	for _, h := range hashes {
		c, err := rh.repo.CommitObject(plumbing.NewHash(h))
		if err != nil {
			// A commit indexed in branch_commits but missing from the git
			// object store is a deeper integrity problem — skip rather than
			// abort so the rest of the backfill still helps.
			log.Warn().Err(err).Str("commit", h).Msg("commit_parents backfill: skipping unreadable commit")
			continue
		}
		for i, p := range c.ParentHashes {
			if _, err := stmt.ExecContext(ctx, h, i, p.String()); err != nil {
				stmt.Close()
				tx.Rollback()
				return fmt.Errorf("backfillCommitParents: insert %s parent %d: %w", h, i, err)
			}
			inserted++
		}
	}
	stmt.Close()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, '1') ON CONFLICT(key) DO UPDATE SET value='1'`,
		commitParentsBackfillKey); err != nil {
		tx.Rollback()
		return fmt.Errorf("backfillCommitParents: mark done: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("backfillCommitParents: commit: %w", err)
	}

	log.Info().Int("commits", len(hashes)).Int("edges", inserted).Msg("commit_parents: backfilled")
	return nil
}

func markCommitParentsBackfilled(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, '1') ON CONFLICT(key) DO UPDATE SET value='1'`,
		commitParentsBackfillKey); err != nil {
		return fmt.Errorf("markCommitParentsBackfilled: %w", err)
	}
	return nil
}
