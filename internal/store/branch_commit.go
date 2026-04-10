// Commit signature helpers and commit-notification glue on repoHandler.
// These were previously on factIndex but moved here because they are shared
// between factIndex writes and remoteIndex.Sync, and reaching SIDEWAYS through
// a sibling subsystem is not allowed.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// authorSig returns the author signature for a given operation.
func (rh *repoHandler) authorSig(branch, operation string) object.Signature {
	agentID := deriveAgentID(branch)
	return object.Signature{
		Name:  agentID,
		Email: agentID + "+" + operation + "@agents.knomit.io",
		When:  time.Now(),
	}
}

// committerSig returns the committer signature (stable per agent).
func (rh *repoHandler) committerSig(branch string) object.Signature {
	agentID := deriveAgentID(branch)
	return object.Signature{
		Name:  agentID,
		Email: agentID + "@agents.knomit.io",
		When:  time.Now(),
	}
}

// notifyCommit runs the post-commit side effects for a new commit on branch:
//
//  1. Appends the commit to commit_log (branch-agnostic row + branch_commits
//     visibility row).
//  2. Calls im.Sync(ctx, branch) so branch_facts / facts_vec / graph catch
//     up with the new tree at HEAD. This is the contract EVERY mutation path
//     (WriteFact, DeleteFact, BatchWriteFacts, MergeBranch, remote Sync) must
//     honor — skipping it leaves per-branch tables stale relative to the git
//     tree and trips the facts-coherence Verify check.
//  3. Calls the external onCommit observer if registered (e.g. SSE broadcast).
//
// Called outside the branch lock — Sync may call back into Service for reads,
// and the observer runs user code.
//
// Returns an error iff the index sync fails. Callers must propagate the
// error so the failing operation is visible at its own call site.
func (rh *repoHandler) notifyCommit(ctx context.Context, branch string, hash plumbing.Hash) error {
	rh.AppendCommitLog(ctx, branch, hash.String())
	if rh.im != nil {
		if err := rh.im.Sync(ctx, branch); err != nil {
			return fmt.Errorf("notifyCommit: im.Sync(%s): %w", branch, err)
		}
	}
	if rh.onCommit != nil {
		rh.onCommit(branch, hash.String())
	}
	return nil
}
