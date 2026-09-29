// Commit-log maintenance methods on repoHandler. These were previously on
// searchIndex but moved here because they only touch repoHandler state
// (gits, repo, db) and are called by multiple subsystems.
package store

import (
	"context"
	"database/sql"
	"fmt"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	storegit "knomit/internal/store/git"
)

// populateCommitLog indexes every commit reachable from the tip of branch
// that is not yet recorded on it: commit_log, branch_commits, commit_parents
// and — in the same transaction — its path_changes rows. Commits are recorded
// parents first (go-git's log order is children first), in transactions of
// pathChangeBatch commits; everything read from git objects is prepared
// before each transaction opens.
func (rh *repoHandler) populateCommitLog(ctx context.Context, branch string) error {
	return rh.populate(ctx, branch, true)
}

// populate is populateCommitLog; oneTimePass=false skips deriveUnderived.
func (rh *repoHandler) populate(ctx context.Context, branch string, oneTimePass bool) error {
	if _, err := rh.resolveRef(ctx, branch); err != nil {
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
	fresh, total, err := rh.reachableCommits(ctx, branch, branchID)
	if err != nil {
		return fmt.Errorf("populateCommitLog: %w", err)
	}
	order := parentsFirst(fresh)
	d := newDeriver(rh)
	for i := 0; i < len(order); i += pathChangeBatch {
		items, prepared, err := rh.indexItems(ctx, d, order[i:min(i+pathChangeBatch, len(order))], false)
		if err != nil {
			return fmt.Errorf("populateCommitLog: %w", err)
		}
		if err := rh.gits.CommitLogApply(ctx, branch, items, storegit.CommitLogApplyOptions{Derive: d.hook(prepared)}); err != nil {
			return fmt.Errorf("populateCommitLog: %w", err)
		}
	}
	log.Debug().Int("commits", total).Int("indexed", len(order)).Msg("commit_log: populated")
	return nil
}

// reachableCommits walks the branch tip and returns the commits NOT yet
// recorded on the branch (all of them when branchID is 0), and how many
// commits it walked.
func (rh *repoHandler) reachableCommits(ctx context.Context, branch string, branchID int64) ([]*object.Commit, int, error) {
	hash, err := rh.resolveRef(ctx, branch)
	if err != nil {
		return nil, 0, nil
	}
	logIter, err := rh.repo.Log(&gogit.LogOptions{From: hash, Order: gogit.LogOrderDefault})
	if err != nil {
		return nil, 0, fmt.Errorf("log: %w", err)
	}
	defer logIter.Close()
	q := conn(ctx, rh.db)
	var fresh []*object.Commit
	total := 0
	err = logIter.ForEach(func(c *object.Commit) error {
		total++
		if branchID != 0 {
			var n int
			if err := q.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM branch_commits WHERE branch_id = ? AND commit_hash = ?`,
				branchID, c.Hash.String()).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return nil
			}
		}
		fresh = append(fresh, c)
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("walk: %w", err)
	}
	return fresh, total, nil
}

// indexItems prepares commits (already in parents-first order) for
// CommitLogApply — OUTSIDE any transaction, since it reads git objects.
// A commit already derived (commit_fp) was indexed before, so its commit_log
// rows exist: it gets no payload diff and no path_changes preparation, unless
// all is set (rebuildCommitLog, which clears and rewrites both).
func (rh *repoHandler) indexItems(ctx context.Context, d *deriver, commits []*object.Commit, all bool) ([]storegit.CommitLogItem, []*preparedCommit, error) {
	q := conn(ctx, rh.db)
	items := make([]storegit.CommitLogItem, len(commits))
	prepared := make([]*preparedCommit, len(commits))
	for i, c := range commits {
		items[i] = storegit.CommitLogItem{Hash: c.Hash.String(), Parents: parentHashes(c)}
		if !all {
			var marked int
			if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM commit_fp WHERE commit_hash = ?`, items[i].Hash).Scan(&marked); err != nil {
				return nil, nil, fmt.Errorf("derived mark: %w", err)
			}
			if marked > 0 {
				continue
			}
		}
		files, err := changedFilesInCommit(c)
		if err != nil {
			return nil, nil, fmt.Errorf("changed files %s: %w", c.Hash, err)
		}
		items[i].Entries = commitEntries(c, files)
		if prepared[i], err = d.prepare(c, markdownChanges(items[i].Entries)); err != nil {
			return nil, nil, err
		}
	}
	return items, prepared, nil
}

// hook is the CommitLogApply Derive hook for prepared items: apply, SQL only.
// An item left unprepared was already derived; apply's mark check skips it.
func (d *deriver) hook(prepared []*preparedCommit) func(ctx context.Context, tx *sql.Tx, i int) error {
	return func(ctx context.Context, tx *sql.Tx, i int) error {
		if prepared[i] == nil {
			return nil
		}
		return d.apply(ctx, tx, prepared[i])
	}
}

// repopulateBranch replaces the branch's commit visibility with the commits
// reachable from its tip — purge, then record — in ONE short transaction, so a
// reader never sees the branch empty or half-indexed in between (a rewind of
// main, the rebase replay after one). Everything read from git objects is
// prepared before the transaction opens.
func (rh *repoHandler) repopulateBranch(ctx context.Context, branch string) error {
	if !rh.gits.CommitLogAvailable() {
		return nil
	}
	if err := rh.deriveUnderived(ctx); err != nil {
		return fmt.Errorf("repopulateBranch: %w", err)
	}
	branchID, err := rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("repopulateBranch: branch %q: %w", branch, err)
	}
	all, _, err := rh.reachableCommits(ctx, branch, 0)
	if err != nil {
		return fmt.Errorf("repopulateBranch: %w", err)
	}
	d := newDeriver(rh)
	items, prepared, err := rh.indexItems(ctx, d, parentsFirst(all), false)
	if err != nil {
		return fmt.Errorf("repopulateBranch: %w", err)
	}
	return rh.gits.CommitLogApply(ctx, branch, items, storegit.CommitLogApplyOptions{
		Before: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM branch_commits WHERE branch_id = ?`, branchID)
			return err
		},
		Derive: d.hook(prepared),
	})
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
// Everything is prepared from git first; then the clear and the re-record run
// in ONE transaction, so readers keep the old rows until the rebuilt ones
// commit, never an empty branch.
//
// Scope is per-branch: only rows for commits visible to THIS branch are
// cleared, and every one of them is re-recorded (and re-derived) on this
// branch. Commits unique to other branches are untouched.
func (rh *repoHandler) rebuildCommitLog(ctx context.Context, branch string) error {
	if !rh.gits.CommitLogAvailable() {
		return nil
	}
	branchID, err := rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("rebuildCommitLog: branch id: %w", err)
	}
	all, _, err := rh.reachableCommits(ctx, branch, 0)
	if err != nil {
		return fmt.Errorf("rebuildCommitLog: %w", err)
	}
	d := newDeriver(rh)
	items, prepared, err := rh.indexItems(ctx, d, parentsFirst(all), true)
	if err != nil {
		return fmt.Errorf("rebuildCommitLog: %w", err)
	}
	return rh.gits.CommitLogApply(ctx, branch, items, storegit.CommitLogApplyOptions{
		Before: func(ctx context.Context, tx *sql.Tx) error {
			// Every DELETE selects through branch_commits, so it goes last.
			for _, stmt := range []struct{ what, sql string }{
				{"commit_log", `DELETE FROM commit_log WHERE commit_hash IN (SELECT commit_hash FROM branch_commits WHERE branch_id = ?)`},
				{"path_changes", `DELETE FROM path_changes WHERE commit_hash IN (SELECT commit_hash FROM branch_commits WHERE branch_id = ?)`},
				{"path_change_links", `DELETE FROM path_change_links WHERE commit_hash IN (SELECT commit_hash FROM branch_commits WHERE branch_id = ?)`},
				{"commit_fp", `DELETE FROM commit_fp WHERE commit_hash IN (SELECT commit_hash FROM branch_commits WHERE branch_id = ?)`},
				{"branch_commits", `DELETE FROM branch_commits WHERE branch_id = ?`},
			} {
				if _, err := tx.ExecContext(ctx, stmt.sql, branchID); err != nil {
					return fmt.Errorf("rebuildCommitLog: clear %s: %w", stmt.what, err)
				}
			}
			return nil
		},
		Derive: d.hook(prepared),
	})
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
	c, err := rh.repo.CommitObject(plumbing.NewHash(hashStr))
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
	d := newDeriver(rh)
	items, prepared, err := rh.indexItems(ctx, d, []*object.Commit{c}, false)
	if err != nil {
		return fmt.Errorf("AppendCommitLog: %w", err)
	}
	if err := rh.gits.CommitLogApply(ctx, branch, items, storegit.CommitLogApplyOptions{Derive: d.hook(prepared)}); err != nil {
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
