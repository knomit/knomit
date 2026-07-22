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
// Called INSIDE the branch lock: every caller (writeFile, deleteFile,
// batchWrite, MergeBranch, remote reconcile) holds lockBranch(branch) across
// this call, so the ref advance, commit_log append, and im.Sync are atomic
// w.r.t. concurrent readers and other index mutations on the branch. im.Sync is
// therefore the lock-FREE primitive — out-of-band callers (the commit observer,
// the startup heal) must use im.SyncLocked instead. The onCommit observer only
// schedules a debounced timer (obs.Notify returns immediately); its own
// SyncLocked runs later, after this lock has been released.
//
// Returns an error iff the index sync fails. Callers must propagate the
// error so the failing operation is visible at its own call site.
//
// Caller cancellation is dropped here (context.WithoutCancel keeps values,
// deadlines of the surrounding work aside). By the time notifyCommit is
// reached the branch ref has ALREADY been advanced by a SetReference that
// takes no ctx, so honoring cancellation could only produce the torn state
// this function exists to prevent: the commit lives in git while commit_log /
// branch_facts / facts_vec / the graph never learn of it, and the caller is
// told the write failed. This sits at the shared choke point rather than in
// each caller so every mutation path (writeFile, deleteFile, batchWrite,
// MergeBranch, remote reconcile) is covered by one rule. Nothing is lost by
// it: we are inside the branch lock, so returning early would not release
// anything sooner, and callers that want to stop a long run still observe
// their own ctx between operations.
func (rh *repoHandler) notifyCommit(ctx context.Context, branch string, hash plumbing.Hash) error {
	ctx = context.WithoutCancel(ctx)
	if err := rh.AppendCommitLog(ctx, branch, hash.String()); err != nil {
		return fmt.Errorf("notifyCommit: AppendCommitLog(%s): %w", branch, err)
	}
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
