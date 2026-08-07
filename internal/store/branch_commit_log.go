// Commit-log maintenance methods on repoHandler. These were previously on
// searchIndex but moved here because they only touch repoHandler state
// (gits, repo, db) and are called by multiple subsystems.
package store

import (
	"context"
	"fmt"
	"io"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	storegit "knomit/internal/store/git"
)

// populateCommitLog backfills commit_log from the tip of branch.
func (rh *repoHandler) populateCommitLog(ctx context.Context, branch string) error {
	hash, err := rh.resolveRef(ctx, branch)
	if err != nil {
		// Branch not found (empty repo) — just mark available if table exists.
		_ = rh.gits.CommitLogAvailable()
		return nil
	}

	logIter, err := rh.repo.Log(&gogit.LogOptions{
		From:  hash,
		Order: gogit.LogOrderDefault,
	})
	if err != nil {
		return fmt.Errorf("populateCommitLog: log: %w", err)
	}
	defer logIter.Close()

	// The payload is a thunk, not a value: changedFilesInCommit is an
	// object.DiffTree costing ~300 object loads (~2 ms) per commit, and on a
	// warm open every commit here is already recorded. CommitLogSync calls the
	// thunk only for commits it will actually insert, so a no-op re-walk of a
	// populated branch costs one indexed lookup per commit instead of a diff.
	var count, computed int
	err = rh.gits.CommitLogSync(branch, func() (string, storegit.CommitLogPayload, error) {
		c, err := logIter.Next()
		if err == io.EOF {
			return "", nil, nil
		}
		if err != nil {
			return "", nil, err
		}
		count++
		return c.Hash.String(), func() ([]string, []storegit.CommitLogEntry, error) {
			computed++
			files, err := changedFilesInCommit(c)
			if err != nil {
				return nil, nil, err
			}
			return parentHashes(c), commitEntries(c, files), nil
		}, nil
	})
	if err != nil {
		return fmt.Errorf("populateCommitLog: sync: %w", err)
	}

	log.Debug().Int("commits", count).Int("computed", computed).Msg("commit_log: populated")
	return nil
}

// rebuildCommitLog rewrites this branch's commit_log from git. populateCommitLog
// alone cannot refresh existing rows: CommitLogSync dedups on branch_commits and
// commit_log uses INSERT OR IGNORE, so any commit already recorded is skipped and
// its row kept as-is (e.g. a row written before a column existed). Clearing this
// branch's commit_log + branch_commits rows first forces a full re-walk, so author
// identity and other per-commit metadata are re-read from the source of truth.
//
// Scope is per-branch: only commit_log rows for commits visible to THIS branch are
// cleared. Commits shared with other branches are re-inserted by the re-walk (their
// rows reappear with refreshed data); commits unique to other branches are untouched.
func (rh *repoHandler) rebuildCommitLog(ctx context.Context, branch string) error {
	if !rh.gits.CommitLogAvailable() {
		return nil
	}
	branchID, err := rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("rebuildCommitLog: branch id: %w", err)
	}
	c := conn(ctx, rh.db)
	// Delete commit_log rows BEFORE branch_commits — the subquery needs the
	// branch_commits rows to identify which commits are visible to this branch.
	if _, err := c.ExecContext(ctx,
		`DELETE FROM commit_log WHERE commit_hash IN (SELECT commit_hash FROM branch_commits WHERE branch_id = ?)`,
		branchID); err != nil {
		return fmt.Errorf("rebuildCommitLog: clear commit_log: %w", err)
	}
	if _, err := c.ExecContext(ctx,
		`DELETE FROM branch_commits WHERE branch_id = ?`, branchID); err != nil {
		return fmt.Errorf("rebuildCommitLog: clear branch_commits: %w", err)
	}
	return rh.populateCommitLog(ctx, branch)
}

// AppendCommitLog inserts a single new commit into commit_log.
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
	done := false
	// Same lazy-payload shape as populateCommitLog: reading the commit object
	// and diffing it against its parent is skipped entirely when the commit is
	// already recorded on this branch.
	if err := rh.gits.CommitLogSync(branch, func() (string, storegit.CommitLogPayload, error) {
		if done {
			return "", nil, nil
		}
		done = true
		return hash.String(), func() ([]string, []storegit.CommitLogEntry, error) {
			c, err := rh.repo.CommitObject(hash)
			if err != nil {
				return nil, nil, fmt.Errorf("get commit %s: %w", hashStr, err)
			}
			files, err := changedFilesInCommit(c)
			if err != nil {
				return nil, nil, fmt.Errorf("changed files %s: %w", hashStr, err)
			}
			return parentHashes(c), commitEntries(c, files), nil
		}, nil
	}); err != nil {
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
