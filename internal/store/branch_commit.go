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

// commitSigs returns the author signature (for a given operation) and the
// committer signature (stable per agent) of a commit written ON branch.
//
// Every authored commit — writeFileExact, deleteFile, batchWriteLocked and the
// merge commit in mergeIntoBranch — takes its identity from here, so the rule
// lives in one place: the author is the AGENT whose key signs the commit, not
// the branch it lands on. For an agent branch that is deriveAgentID(branch);
// for an experiment branch it is the experiment's owning agent (#394).
func (rh *repoHandler) commitSigs(ctx context.Context, branch, operation string) (author, committer object.Signature, err error) {
	agentID, err := rh.commitAgentID(ctx, branch)
	if err != nil {
		return object.Signature{}, object.Signature{}, err
	}
	now := time.Now()
	author = object.Signature{Name: agentID, Email: agentID + "+" + operation + "@agents.knomit.io", When: now}
	committer = object.Signature{Name: agentID, Email: agentID + "@agents.knomit.io", When: now}
	return author, committer, nil
}

// commitAgentID names the agent a commit on branch is authored as.
//
// An experiment branch (exp/<name>) is NOT an agent: its commits are signed by
// this instance's key, so they are authored by the agent branch the experiment
// was forked from — its RECORDED parent (experiments.parent_branch). Authoring
// them `exp/<name>` (the old behaviour, deriveAgentID's verbatim fallback) made
// the acceptance gate look up a member record at an id no member has
// (agentIDOfAuthor) and showed experiment names in every per-author count.
//
// It never falls back to the experiment's name. Each case where the owning
// agent cannot be named is an error, and the write is refused before any
// object is built:
//   - no experiments row for the branch (an orphan ref: its parentage is
//     unknowable — ErrOrphanExperimentRef);
//   - a recorded parent that is empty or is itself an experiment branch;
//   - a recorded parent that is not this database's KNOWN agent branch owner
//     (the repo was taken over since the fork — ErrStaleExperimentParent, the
//     same refusal commit and sync give).
//
// An EMPTY owner is unknown, not a mismatch (checkExperimentParentCurrent):
// the recorded parent names the agent then.
//
// deriveAgentID / AgentIDOf keep their meaning; only the experiment case is
// resolved here.
func (rh *repoHandler) commitAgentID(ctx context.Context, branch string) (string, error) {
	name, isExp := ExperimentNameOf(branch)
	if !isExp {
		return deriveAgentID(branch), nil
	}
	exp, ok, err := rh.GetExperiment(ctx, name)
	if err != nil {
		return "", fmt.Errorf("author of %s: %w", branch, err)
	}
	if !ok {
		return "", fmt.Errorf("author of %s: %w: no experiment record names its owning agent", branch, ErrOrphanExperimentRef)
	}
	if exp.Parent == "" || IsExperimentBranch(exp.Parent) {
		return "", fmt.Errorf("author of %s: recorded parent %q is not an agent branch", branch, exp.Parent)
	}
	if err := rh.checkExperimentParentCurrent(ctx, exp); err != nil {
		return "", fmt.Errorf("author of %s: %w", branch, err)
	}
	return deriveAgentID(exp.Parent), nil
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
	// An experiment's "last activity" is defined as any commit on it, and this
	// is the one place every ref mutation passes through — so hanging it here
	// is what makes the two definitions the same definition. It short-circuits
	// on the branch-name prefix before touching SQL, so the cost on every
	// other branch's every commit is one string compare.
	if err := rh.touchExperimentActivity(ctx, branch); err != nil {
		return fmt.Errorf("notifyCommit: %w", err)
	}
	if rh.onCommit != nil {
		rh.onCommit(branch, hash.String())
	}
	return nil
}

// rootCommit walks first-parent ancestry from branch's head to the root commit
// — the repo's stable identity. Lives on repoHandler because repoHandler owns
// git reads; Service.RootCommit and searchIndex.localRepoID both delegate here
// rather than opening a second handle.
//
// First-parent (never wall-clock, never a merge parent) so a repo with a
// grafted or merged history still resolves deterministically.
func (rh *repoHandler) rootCommit(ctx context.Context, branch string) (string, error) {
	head, err := rh.resolveRef(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("rootCommit: resolve %q: %w", branch, err)
	}
	c, err := rh.repo.CommitObject(head)
	if err != nil {
		return "", fmt.Errorf("rootCommit: read head commit: %w", err)
	}
	for c.NumParents() > 0 {
		if c, err = c.Parent(0); err != nil {
			return "", fmt.Errorf("rootCommit: walk parent: %w", err)
		}
	}
	return c.Hash.String(), nil
}
