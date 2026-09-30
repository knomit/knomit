// Commit-log maintenance methods on repoHandler. These were previously on
// searchIndex but moved here because they only touch repoHandler state
// (gits, repo, db) and are called by multiple subsystems.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	storegit "knomit/internal/store/git"
)

// populateCommitLog indexes every commit reachable from the tip of branch
// that is not yet recorded on it: commit_log, branch_commits, commit_parents
// and its path_changes rows. Underived ancestors outside that set are derived
// first (deriveClosure, git only); the new commits are then recorded parents
// first, in transactions of pathChangeBatch commits, each deriving its commits
// in the same transaction. Everything read from git is prepared before each
// transaction opens.
func (rh *repoHandler) populateCommitLog(ctx context.Context, branch string) error {
	if _, err := rh.resolveRef(ctx, branch); err != nil {
		// Branch not found (empty repo) — just mark available if table exists.
		_ = rh.gits.CommitLogAvailable()
		return nil
	}
	if !rh.gits.CommitLogAvailable() {
		return nil
	}
	branchID, err := rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("populateCommitLog: branch %q: %w", branch, err)
	}
	fresh, total, err := rh.reachableCommits(ctx, branch, branchID)
	if err != nil {
		return fmt.Errorf("populateCommitLog: %w", err)
	}
	if len(fresh) == 0 {
		return nil
	}
	if err := rh.recordCommits(ctx, branch, parentsFirst(fresh, commitNode)); err != nil {
		return fmt.Errorf("populateCommitLog: %w", err)
	}
	log.Debug().Int("commits", total).Int("indexed", len(fresh)).Msg("commit_log: populated")
	return nil
}

// recordCommits records commits (parents first) on branch: their underived
// ancestors outside the set first, then the commits themselves in batches,
// each derived in the transaction that records it.
func (rh *repoHandler) recordCommits(ctx context.Context, branch string, order []*object.Commit) error {
	d := newIndexingDeriver(rh)
	in := make(map[plumbing.Hash]bool, len(order))
	for _, c := range order {
		in[c.Hash] = true
	}
	var outside []plumbing.Hash
	for _, c := range order {
		for _, p := range c.ParentHashes {
			if !in[p] {
				outside = append(outside, p)
			}
		}
	}
	if _, err := rh.deriveClosure(ctx, d, outside); err != nil {
		return err
	}
	q := conn(ctx, rh.db)
	for i := 0; i < len(order); i += pathChangeBatch {
		chunk := order[i:min(i+pathChangeBatch, len(order))]
		hashes := make([]string, len(chunk))
		prepared := make([]*preparedCommit, len(chunk))
		for j, c := range chunk {
			hashes[j] = c.Hash.String()
			derived, err := d.isDerived(ctx, q, hashes[j])
			if err != nil {
				return err
			}
			if !derived {
				if prepared[j], err = d.prepare(ctx, metaOf(c)); err != nil {
					return err
				}
			}
		}
		// After prepare: the payloads reuse its diffs.
		items, err := rh.indexItems(ctx, d, chunk, false)
		if err != nil {
			return err
		}
		if err := rh.gits.CommitLogApply(ctx, branch, items, storegit.CommitLogApplyOptions{Derive: d.hook(hashes, prepared)}); err != nil {
			return err
		}
	}
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

// payloadsComputed counts commit_log payload diffs (tests: a warm re-populate
// must compute none).
var payloadsComputed atomic.Int64

// indexItems builds CommitLogApply items for commits — OUTSIDE any
// transaction, since the commit_log payload is a git diff: derivation's own,
// when d prepared the commit (commitChanges). A commit already recorded on
// some branch has its commit_log rows, so it gets no payload, unless all is
// set (rebuildCommitLog rewrites commit_log).
func (rh *repoHandler) indexItems(ctx context.Context, d *deriver, commits []*object.Commit, all bool) ([]storegit.CommitLogItem, error) {
	q := conn(ctx, rh.db)
	items := make([]storegit.CommitLogItem, len(commits))
	for i, c := range commits {
		items[i] = storegit.CommitLogItem{Hash: c.Hash.String(), Parents: parentHashes(c)}
		if !all {
			var indexed int
			if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM branch_commits WHERE commit_hash = ?`, items[i].Hash).Scan(&indexed); err != nil {
				return nil, fmt.Errorf("indexed check: %w", err)
			}
			if indexed > 0 {
				continue
			}
		}
		payloadsComputed.Add(1)
		changes, err := d.commitChanges(c)
		if err != nil {
			return nil, fmt.Errorf("changed files %s: %w", c.Hash, err)
		}
		files := make([]changedFileEntry, len(changes))
		for j, e := range changes {
			files[j] = changedFileEntry{path: e.Path, action: e.Action}
		}
		items[i].Entries = commitEntries(c, files)
	}
	return items, nil
}

// swapHook, when set (tests only), runs inside a rewind's or a rebuild's
// swap transaction after its deletions and before it records new commits; a
// non-nil error fails the swap there.
var swapHook func() error

// lastSwapDuration is how long the last swap transaction held the write lock
// (tests assert it stays short).
var lastSwapDuration atomic.Int64

// branchSet returns the commits recorded on the branch.
func (rh *repoHandler) branchSet(ctx context.Context, branchID int64) (map[string]bool, error) {
	rows, err := conn(ctx, rh.db).QueryContext(ctx, `SELECT commit_hash FROM branch_commits WHERE branch_id = ?`, branchID)
	if err != nil {
		return nil, fmt.Errorf("branch set: %w", err)
	}
	defer rows.Close()
	set := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		set[h] = true
	}
	return set, rows.Err()
}

// swapBranch replaces the branch's recorded commits with those reachable from
// its tip, in ONE short transaction proportional to the CHANGE: it deletes
// the dropped commits and records the new ones (both computed outside the
// lock), after before (rebuild's staged commit_log swap). Readers see the old
// branch or the new one, never a mix. Every recorded commit is derived first
// (deriveClosure, short batches), so the hook only verifies.
func (rh *repoHandler) swapBranch(ctx context.Context, branch string, branchID int64, reachable []*object.Commit, before func(ctx context.Context, tx *sql.Tx) error) error {
	current, err := rh.branchSet(ctx, branchID)
	if err != nil {
		return err
	}
	keep := make(map[string]bool, len(reachable))
	var added []*object.Commit
	for _, c := range reachable {
		h := c.Hash.String()
		keep[h] = true
		if !current[h] {
			added = append(added, c)
		}
	}
	var dropped []string
	for h := range current {
		if !keep[h] {
			dropped = append(dropped, h)
		}
	}
	d := newIndexingDeriver(rh)
	tips := make([]plumbing.Hash, len(added))
	for i, c := range added {
		tips[i] = c.Hash
	}
	if _, err := rh.deriveClosure(ctx, d, tips); err != nil {
		return err
	}
	order := parentsFirst(added, commitNode)
	items, err := rh.indexItems(ctx, d, order, false)
	if err != nil {
		return err
	}
	hashes := make([]string, len(order))
	for i, c := range order {
		hashes[i] = c.Hash.String()
	}
	var start time.Time
	defer func() { lastSwapDuration.Store(int64(time.Since(start))) }()
	return rh.gits.CommitLogApply(ctx, branch, items, storegit.CommitLogApplyOptions{
		Before: func(ctx context.Context, tx *sql.Tx) error {
			start = time.Now()
			if before != nil {
				if err := before(ctx, tx); err != nil {
					return err
				}
			}
			for i := 0; i < len(dropped); i += 500 {
				chunk := dropped[i:min(i+500, len(dropped))]
				args := []any{branchID}
				for _, h := range chunk {
					args = append(args, h)
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM branch_commits WHERE branch_id = ? AND commit_hash IN (?`+strings.Repeat(",?", len(chunk)-1)+`)`, args...); err != nil {
					return fmt.Errorf("swap: drop: %w", err)
				}
			}
			if swapHook != nil {
				if err := swapHook(); err != nil {
					return err
				}
			}
			return nil
		},
		Derive: d.hook(hashes, nil),
	})
}

// repopulateBranch replaces the branch's commit visibility with the commits
// reachable from its tip (a rewind of main, the rebase replay after one).
// Derived rows are never deleted.
func (rh *repoHandler) repopulateBranch(ctx context.Context, branch string) error {
	if !rh.gits.CommitLogAvailable() {
		return nil
	}
	branchID, err := rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("repopulateBranch: branch %q: %w", branch, err)
	}
	all, _, err := rh.reachableCommits(ctx, branch, 0)
	if err != nil {
		return fmt.Errorf("repopulateBranch: %w", err)
	}
	if err := rh.swapBranch(ctx, branch, branchID, all, nil); err != nil {
		return fmt.Errorf("repopulateBranch: %w", err)
	}
	return nil
}

// rebuildCommitLog rewrites this branch's commit_log from git (populate alone
// cannot refresh existing rows: it dedups on branch_commits and commit_log
// uses INSERT OR IGNORE), so author identity and other per-commit metadata are
// re-read from the source of truth; re-derives the history rows that were
// derived around missing objects (degraded), which repairs them once the
// objects are back; and swaps the branch's visibility.
//
// The rewrite is STAGED outside the write lock (commit_log_stage, short
// batches) and swapped in by bulk statements in one short transaction. Like
// dev's rebuild, every commit_log row of the branch's commits ends up as git
// says: a reachable commit keeps exactly its staged rows (none, if it changed
// nothing), and a dropped commit on no other branch loses its rows. A dropped
// commit still on another branch keeps them: they are that branch's. Only rows
// that differ are written, so an unchanged branch's swap writes nothing.
// Derived rows are immutable per hash and never deleted here.
func (rh *repoHandler) rebuildCommitLog(ctx context.Context, branch string) (err error) {
	if !rh.gits.CommitLogAvailable() {
		return nil
	}
	if _, err := rh.rederiveDegraded(ctx); err != nil {
		return fmt.Errorf("rebuildCommitLog: %w", err)
	}
	branchID, err := rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("rebuildCommitLog: branch id: %w", err)
	}
	all, _, err := rh.reachableCommits(ctx, branch, 0)
	if err != nil {
		return fmt.Errorf("rebuildCommitLog: %w", err)
	}
	// Derive whatever reachable commit is missing its rows (a lookup each
	// when none is), in short batches, before anything takes the lock.
	tips := make([]plumbing.Hash, len(all))
	for i, c := range all {
		tips[i] = c.Hash
	}
	if _, err := rh.deriveClosure(ctx, newIndexingDeriver(rh), tips); err != nil {
		return fmt.Errorf("rebuildCommitLog: %w", err)
	}
	// The stage is deleted when the rebuild returns, after the swap and
	// outside its lock, whether it succeeded or not. A crash in between is
	// cleared by the next open (openHistory).
	defer func() {
		if _, cerr := conn(ctx, rh.db).ExecContext(context.WithoutCancel(ctx), `DELETE FROM commit_log_stage WHERE branch_id = ?`, branchID); cerr != nil {
			log.Warn().Err(cerr).Str("branch", branch).Msg("rebuildCommitLog: stage cleanup failed")
		}
	}()
	if err := rh.stageCommitLog(ctx, branchID, all); err != nil {
		return fmt.Errorf("rebuildCommitLog: %w", err)
	}
	return rh.swapBranch(ctx, branch, branchID, all, func(ctx context.Context, tx *sql.Tx) error {
		for _, stmt := range []string{
			// Before branch_commits changes: the old visibility names the
			// dropped commits. Only rows that DIFFER from the stage are
			// deleted (an identical staged row keeps its row: IS matches
			// NULLs), so an unchanged branch writes nothing here; the
			// INSERT OR IGNORE then fills in what was deleted or missing.
			`DELETE FROM commit_log WHERE (commit_hash IN (SELECT commit_hash FROM commit_log_stage WHERE branch_id = ?1)
			    OR commit_hash IN (SELECT commit_hash FROM branch_commits WHERE branch_id = ?1
			        AND commit_hash NOT IN (SELECT commit_hash FROM branch_commits WHERE branch_id != ?1)))
			  AND NOT EXISTS (SELECT 1 FROM commit_log_stage s WHERE s.branch_id = ?1
			        AND s.commit_hash = commit_log.commit_hash AND s.path = commit_log.path
			        AND s.message IS commit_log.message AND s.operation IS commit_log.operation
			        AND s.author_name IS commit_log.author_name AND s.author_email IS commit_log.author_email
			        AND s.action IS commit_log.action AND s.committed_at IS commit_log.committed_at)`,
			`INSERT OR IGNORE INTO commit_log (commit_hash, path, message, operation, author_name, author_email, action, committed_at)
			 SELECT commit_hash, path, message, operation, author_name, author_email, action, committed_at FROM commit_log_stage WHERE branch_id = ?1 AND path != ''`,
		} {
			if _, err := tx.ExecContext(ctx, stmt, branchID); err != nil {
				return fmt.Errorf("rebuildCommitLog: swap commit_log: %w", err)
			}
		}
		return nil
	})
}

// stageCommitLog writes the git-derived commit_log rows of commits into
// commit_log_stage for branchID, in short transactions, outside any lock the
// swap will hold. Every commit also gets a row with an empty path, so the swap
// knows the whole reachable set, including commits that changed nothing.
func (rh *repoHandler) stageCommitLog(ctx context.Context, branchID int64, commits []*object.Commit) error {
	d := newDeriver(rh, activeTables)
	if _, err := conn(ctx, rh.db).ExecContext(ctx, `DELETE FROM commit_log_stage WHERE branch_id = ?`, branchID); err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	const insert = `INSERT OR IGNORE INTO commit_log_stage (branch_id, commit_hash, path, message, operation, author_name, author_email, action, committed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	for i := 0; i < len(commits); i += pathChangeBatch {
		chunk := commits[i:min(i+pathChangeBatch, len(commits))]
		items, err := rh.indexItems(ctx, d, chunk, true)
		if err != nil {
			return err
		}
		tctx, tx, own, err := beginTxIfNeeded(ctx, rh.db)
		if err != nil {
			return fmt.Errorf("stage: %w", err)
		}
		for _, it := range items {
			_, err := tx.ExecContext(tctx, insert, branchID, it.Hash, "", "", "", "", "", "", 0)
			for _, e := range it.Entries {
				if err != nil {
					break
				}
				_, err = tx.ExecContext(tctx, insert, branchID, e.Hash, e.Path, e.Message, e.Operation, e.AuthorName, e.AuthorEmail, e.Action, e.CommittedAt)
			}
			if err != nil {
				if own {
					tx.Rollback() //nolint:errcheck
				}
				return fmt.Errorf("stage: %w", err)
			}
		}
		if own {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("stage: %w", err)
			}
		}
	}
	return nil
}

// AppendCommitLog records a single new commit on branch. Every caller moved
// the ref only after deriveBeforeAdvance derived the commit and its ancestors,
// so recording derives nothing: the hook only verifies. When a parent is not
// recorded on this branch — a gap left by an older index — the branch is
// populated instead, which records every missing ancestor (all derived too).
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
	branchID, err := rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("AppendCommitLog: branch %q: %w", branch, err)
	}
	q := conn(ctx, rh.db)
	for _, p := range c.ParentHashes {
		var n int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM branch_commits WHERE branch_id = ? AND commit_hash = ?`, branchID, p.String()).Scan(&n); err != nil {
			return fmt.Errorf("AppendCommitLog: parent visibility: %w", err)
		}
		if n == 0 {
			if _, oerr := rh.repo.CommitObject(p); oerr == nil {
				return rh.populateCommitLog(ctx, branch)
			}
		}
	}
	if err := rh.recordCommits(ctx, branch, []*object.Commit{c}); err != nil {
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
