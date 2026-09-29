// Commit-log maintenance methods on repoHandler. These were previously on
// searchIndex but moved here because they only touch repoHandler state
// (gits, repo, db) and are called by multiple subsystems.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"

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
	// A database that predates path_changes, or a version bump: derive the
	// history of every branch tip first (one lookup when current).
	if err := rh.ensureAllDerived(ctx); err != nil {
		return fmt.Errorf("populateCommitLog: %w", err)
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
	if err := rh.recordCommits(ctx, branch, parentsFirst(fresh)); err != nil {
		return fmt.Errorf("populateCommitLog: %w", err)
	}
	log.Debug().Int("commits", total).Int("indexed", len(fresh)).Msg("commit_log: populated")
	return nil
}

// recordCommits records commits (parents first) on branch: their underived
// ancestors outside the set first, then the commits themselves in batches,
// each derived in the transaction that records it.
func (rh *repoHandler) recordCommits(ctx context.Context, branch string, order []*object.Commit) error {
	d := newDeriver(rh)
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
	for i := 0; i < len(order); i += pathChangeBatch {
		chunk := order[i:min(i+pathChangeBatch, len(order))]
		items, err := rh.indexItems(ctx, chunk, false)
		if err != nil {
			return err
		}
		hashes := make([]string, len(chunk))
		prepared := make([]*preparedCommit, len(chunk))
		q := conn(ctx, rh.db)
		for j, c := range chunk {
			hashes[j] = c.Hash.String()
			var marked int
			if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM commit_fp WHERE commit_hash = ?`, hashes[j]).Scan(&marked); err != nil {
				return fmt.Errorf("derived mark: %w", err)
			}
			if marked == 0 {
				if prepared[j], err = d.prepare(ctx, c, items[j].Entries); err != nil {
					return err
				}
			}
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
// transaction, since the commit_log payload is a git diff. A commit already
// recorded on some branch has its commit_log rows, so it gets no payload,
// unless all is set (rebuildCommitLog rewrites commit_log).
func (rh *repoHandler) indexItems(ctx context.Context, commits []*object.Commit, all bool) ([]storegit.CommitLogItem, error) {
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
		files, err := changedFilesInCommit(c)
		if err != nil {
			return nil, fmt.Errorf("changed files %s: %w", c.Hash, err)
		}
		items[i].Entries = commitEntries(c, files)
	}
	return items, nil
}

// swapHook, when set (tests only), runs inside a rewind's or a rebuild's
// swap transaction after it cleared the branch and before it re-records it.
var swapHook func()

// swapBranch replaces the branch's recorded commits with items — the
// "before" deletions and the re-record in ONE short transaction, so readers
// see the old branch or the new one, never an empty or half-recorded one.
// Every item must already be derived: the history is derived first, in its
// own short batches (deriveClosure), so the swap holds the write lock only
// for its SQL.
func (rh *repoHandler) swapBranch(ctx context.Context, branch string, items []storegit.CommitLogItem, clear []string) error {
	branchID, err := rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("branch %q: %w", branch, err)
	}
	hashes := make([]string, len(items))
	for i, it := range items {
		hashes[i] = it.Hash
	}
	d := newDeriver(rh)
	return rh.gits.CommitLogApply(ctx, branch, items, storegit.CommitLogApplyOptions{
		Before: func(ctx context.Context, tx *sql.Tx) error {
			for _, stmt := range clear {
				if _, err := tx.ExecContext(ctx, stmt, branchID); err != nil {
					return fmt.Errorf("clear: %w", err)
				}
			}
			if swapHook != nil {
				swapHook()
			}
			return nil
		},
		Derive: d.hook(hashes, nil),
	})
}

// repopulateBranch replaces the branch's commit visibility with the commits
// reachable from its tip (a rewind of main, the rebase replay after one):
// derive what is missing in short batches, then swap in one short
// transaction. Derived rows are never deleted.
func (rh *repoHandler) repopulateBranch(ctx context.Context, branch string) error {
	if !rh.gits.CommitLogAvailable() {
		return nil
	}
	if err := rh.ensureAllDerived(ctx); err != nil {
		return fmt.Errorf("repopulateBranch: %w", err)
	}
	all, _, err := rh.reachableCommits(ctx, branch, 0)
	if err != nil {
		return fmt.Errorf("repopulateBranch: %w", err)
	}
	order := parentsFirst(all)
	tips := make([]plumbing.Hash, len(order))
	for i, c := range order {
		tips[i] = c.Hash
	}
	if _, err := rh.deriveClosure(ctx, newDeriver(rh), tips); err != nil {
		return fmt.Errorf("repopulateBranch: %w", err)
	}
	items, err := rh.indexItems(ctx, order, false)
	if err != nil {
		return fmt.Errorf("repopulateBranch: %w", err)
	}
	return rh.swapBranch(ctx, branch, items, []string{`DELETE FROM branch_commits WHERE branch_id = ?`})
}

// rebuildCommitLog rewrites this branch's commit_log from git (populate alone
// cannot refresh existing rows: it dedups on branch_commits and commit_log
// uses INSERT OR IGNORE), so author identity and other per-commit metadata are
// re-read from the source of truth. Missing path_changes rows are derived
// first, in short batches; existing ones are immutable per hash and never
// deleted (a pathChangesVersion bump is the only reset). The commit_log
// rewrite and branch_commits swap run in ONE short transaction.
//
// Scope is per-branch: commit_log rows for commits visible to THIS branch are
// rewritten (identically, for commits other branches share).
func (rh *repoHandler) rebuildCommitLog(ctx context.Context, branch string) error {
	if !rh.gits.CommitLogAvailable() {
		return nil
	}
	if err := rh.ensureAllDerived(ctx); err != nil {
		return fmt.Errorf("rebuildCommitLog: %w", err)
	}
	all, _, err := rh.reachableCommits(ctx, branch, 0)
	if err != nil {
		return fmt.Errorf("rebuildCommitLog: %w", err)
	}
	order := parentsFirst(all)
	tips := make([]plumbing.Hash, len(order))
	for i, c := range order {
		tips[i] = c.Hash
	}
	if _, err := rh.deriveClosure(ctx, newDeriver(rh), tips); err != nil {
		return fmt.Errorf("rebuildCommitLog: %w", err)
	}
	items, err := rh.indexItems(ctx, order, true)
	if err != nil {
		return fmt.Errorf("rebuildCommitLog: %w", err)
	}
	// commit_log's DELETE selects through branch_commits, so it goes first.
	return rh.swapBranch(ctx, branch, items, []string{
		`DELETE FROM commit_log WHERE commit_hash IN (SELECT commit_hash FROM branch_commits WHERE branch_id = ?)`,
		`DELETE FROM branch_commits WHERE branch_id = ?`,
	})
}

// AppendCommitLog indexes a single new commit, deriving its path_changes rows
// in the same transaction (and any underived ancestors first, from git). When
// a parent is not recorded on this branch — a gap left by an older index —
// the branch is populated instead, which records every missing ancestor.
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
