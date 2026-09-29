// Commit-log maintenance methods on repoHandler. These were previously on
// searchIndex but moved here because they only touch repoHandler state
// (gits, repo, db) and are called by multiple subsystems.
package store

import (
	"context"
	"fmt"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	storegit "knomit/internal/store/git"
)

// populateCommitLog indexes every commit reachable from the tip of branch
// that is not yet recorded on it: commit_log, branch_commits, commit_parents
// and — in the same transaction, through the Derive hook — its path_changes
// rows. Commits are fed parents first, so each is derived after its parents
// (go-git's log order is children first). When ctx carries a transaction the
// whole population joins it.
func (rh *repoHandler) populateCommitLog(ctx context.Context, branch string) error {
	return rh.populate(ctx, branch, true)
}

// populate is populateCommitLog; oneTimePass=false skips deriveUnderived, for
// rebuildCommitLog, whose cleared commits all re-enter through the Derive hook
// right after their commit_log rows (the one-time pass would derive the ones
// shared with other branches before those rows exist).
func (rh *repoHandler) populate(ctx context.Context, branch string, oneTimePass bool) error {
	hash, err := rh.resolveRef(ctx, branch)
	if err != nil {
		// Branch not found (empty repo) — just mark available if table exists.
		_ = rh.gits.CommitLogAvailable()
		return nil
	}
	if !rh.gits.CommitLogAvailable() {
		return nil
	}
	// The one-time pass (a database that predates path_changes, or a
	// derivation version bump) runs first: the commits indexed below may
	// have parents indexed before path_changes existed.
	if oneTimePass {
		if err := rh.deriveUnderived(ctx); err != nil {
			return fmt.Errorf("populateCommitLog: %w", err)
		}
	}
	branchID, err := rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("populateCommitLog: branch %q: %w", branch, err)
	}

	logIter, err := rh.repo.Log(&gogit.LogOptions{
		From:  hash,
		Order: gogit.LogOrderDefault,
	})
	if err != nil {
		return fmt.Errorf("populateCommitLog: log: %w", err)
	}
	defer logIter.Close()

	// Collect the commits this branch has not recorded (one indexed lookup
	// each), then order them parents first.
	q := conn(ctx, rh.db)
	var fresh []*object.Commit
	count := 0
	if err := logIter.ForEach(func(c *object.Commit) error {
		count++
		var n int
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM branch_commits WHERE branch_id = ? AND commit_hash = ?`,
			branchID, c.Hash.String()).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			fresh = append(fresh, c)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("populateCommitLog: walk: %w", err)
	}
	if len(fresh) == 0 {
		return nil
	}
	order := parentsFirst(fresh)

	// The payload is a thunk: changedFilesInCommit is an object.DiffTree
	// costing ~2 ms per commit, and CommitLogSyncWith calls it only for a
	// commit it will actually insert.
	d := newDeriver(rh)
	i := 0
	err = rh.gits.CommitLogSyncWith(ctx, branch, func() (string, storegit.CommitLogPayload, error) {
		if i == len(order) {
			return "", nil, nil
		}
		c := order[i]
		i++
		return c.Hash.String(), func() ([]string, []storegit.CommitLogEntry, error) {
			files, err := changedFilesInCommit(c)
			if err != nil {
				return nil, nil, err
			}
			return parentHashes(c), commitEntries(c, files), nil
		}, nil
	}, storegit.CommitLogSyncOptions{Derive: d.derive, Batch: pathChangeBatch})
	if err != nil {
		return fmt.Errorf("populateCommitLog: sync: %w", err)
	}
	log.Debug().Int("commits", count).Int("indexed", len(order)).Msg("commit_log: populated")
	return nil
}

// repopulateBranch replaces the branch's commit visibility with the commits
// reachable from its tip — purge, then populate — in ONE transaction, so a
// reader never sees the branch empty or half-indexed in between (a rewind of
// main, the rebase replay after one).
func (rh *repoHandler) repopulateBranch(ctx context.Context, branch string) error {
	ctx, tx, own, err := beginTxIfNeeded(ctx, rh.db)
	if err != nil {
		return fmt.Errorf("repopulateBranch: begin: %w", err)
	}
	if own {
		defer tx.Rollback() //nolint:errcheck
	}
	if err := rh.purgeBranchCommits(ctx, branch); err != nil {
		return err
	}
	if err := rh.populateCommitLog(ctx, branch); err != nil {
		return err
	}
	if own {
		return tx.Commit()
	}
	return nil
}

// rebuildCommitLog rewrites this branch's commit_log — and re-derives the
// path_changes rows of its commits — from git. populateCommitLog alone cannot
// refresh existing rows: CommitLogSync dedups on branch_commits and commit_log
// uses INSERT OR IGNORE, so any commit already recorded is skipped and its row
// kept as-is (e.g. a row written before a column existed). Clearing this
// branch's rows first forces a full re-walk, so author identity and other
// per-commit metadata are re-read from the source of truth, and :rebuild can
// repair path_changes.
//
// It runs in ONE transaction: readers keep seeing the old rows until the
// rebuilt ones commit, never an empty branch.
//
// Scope is per-branch: only rows for commits visible to THIS branch are
// cleared, and every one of them re-enters the index on this branch, so the
// Derive hook re-derives each right after its commit_log rows. Commits
// unique to other branches are untouched.
func (rh *repoHandler) rebuildCommitLog(ctx context.Context, branch string) error {
	if !rh.gits.CommitLogAvailable() {
		return nil
	}
	branchID, err := rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("rebuildCommitLog: branch id: %w", err)
	}
	ctx, tx, own, err := beginTxIfNeeded(ctx, rh.db)
	if err != nil {
		return fmt.Errorf("rebuildCommitLog: begin: %w", err)
	}
	if own {
		defer tx.Rollback() //nolint:errcheck
	}
	// Every DELETE selects through branch_commits, so branch_commits goes last.
	for _, stmt := range []struct{ what, sql string }{
		{"commit_log", `DELETE FROM commit_log WHERE commit_hash IN (SELECT commit_hash FROM branch_commits WHERE branch_id = ?)`},
		{"path_changes", `DELETE FROM path_changes WHERE commit_hash IN (SELECT commit_hash FROM branch_commits WHERE branch_id = ?)`},
		{"path_change_links", `DELETE FROM path_change_links WHERE commit_hash IN (SELECT commit_hash FROM branch_commits WHERE branch_id = ?)`},
		{"commit_fp_up", `DELETE FROM commit_fp_up WHERE commit_hash IN (SELECT commit_hash FROM branch_commits WHERE branch_id = ?)`},
		{"commit_fp", `DELETE FROM commit_fp WHERE commit_hash IN (SELECT commit_hash FROM branch_commits WHERE branch_id = ?)`},
		{"branch_commits", `DELETE FROM branch_commits WHERE branch_id = ?`},
	} {
		if _, err := tx.ExecContext(ctx, stmt.sql, branchID); err != nil {
			return fmt.Errorf("rebuildCommitLog: clear %s: %w", stmt.what, err)
		}
	}
	if err := rh.populate(ctx, branch, false); err != nil {
		return err
	}
	if own {
		return tx.Commit()
	}
	return nil
}

// AppendCommitLog indexes a single new commit (and derives its path_changes
// rows in the same transaction). When one of its parents is not derived yet —
// it was never indexed on any branch — the branch is populated instead, which
// indexes the missing ancestors first.
// Returns an error so callers (notifyCommit) can propagate append failures
// — previously the error was swallowed to a log.Warn which let silent
// branches drift out of commit_log parity. The property test P3 surfaced
// a case where AppendCommitLog failed mid-sequence on a freshly-created
// child branch and the resulting gap was only caught by a later Verify.
func (rh *repoHandler) AppendCommitLog(ctx context.Context, branch, hashStr string) error {
	if !rh.gits.CommitLogAvailable() {
		return nil
	}
	hash := plumbing.NewHash(hashStr)
	c, err := rh.repo.CommitObject(hash)
	if err != nil {
		return fmt.Errorf("AppendCommitLog: get commit %s: %w", hashStr, err)
	}
	q := conn(ctx, rh.db)
	for _, p := range c.ParentHashes {
		var n int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM commit_fp WHERE commit_hash = ?`, p.String()).Scan(&n); err != nil {
			return fmt.Errorf("AppendCommitLog: parent mark: %w", err)
		}
		if n == 0 {
			if _, oerr := rh.repo.CommitObject(p); oerr == nil {
				return rh.populateCommitLog(ctx, branch)
			}
		}
	}
	done := false
	d := newDeriver(rh)
	// Same lazy-payload shape as populateCommitLog: diffing the commit
	// against its parent is skipped entirely when it is already recorded on
	// this branch.
	if err := rh.gits.CommitLogSyncWith(ctx, branch, func() (string, storegit.CommitLogPayload, error) {
		if done {
			return "", nil, nil
		}
		done = true
		return hash.String(), func() ([]string, []storegit.CommitLogEntry, error) {
			files, err := changedFilesInCommit(c)
			if err != nil {
				return nil, nil, fmt.Errorf("changed files %s: %w", hashStr, err)
			}
			return parentHashes(c), commitEntries(c, files), nil
		}, nil
	}, storegit.CommitLogSyncOptions{Derive: d.derive}); err != nil {
		return fmt.Errorf("AppendCommitLog: sync %s: %w", hashStr, err)
	}
	return nil
}

// parentHashes extracts the ordered parent commit hashes from a go-git
// commit object. Returns nil for root commits. parents[0] is the canonical
// first parent (the "ours" side on a merge commit), matching git's
// first-parent semantics used by branch-local history walks.
func parentHashes(c *object.Commit) []string {
	if c == nil || len(c.ParentHashes) == 0 {
		return nil
	}
	out := make([]string, len(c.ParentHashes))
	for i, h := range c.ParentHashes {
		out[i] = h.String()
	}
	return out
}
