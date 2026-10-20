package sync

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/store/merge"
)

// The parent's white-box tests drive the reconcile cores directly, with a
// hand-set watermark or a main moved by hand, which no production path does.
// Each wrapper below is the unexported function unchanged and panics outside
// a test binary, so no production caller can take the door.

func mustBeTest(name string) {
	if !testing.Testing() {
		panic("sync." + name + " called outside a test binary")
	}
}

// ReadAgentBaseForTest is readAgentBase.
func (rh *Repo) ReadAgentBaseForTest(agentBranch string) (plumbing.Hash, error) {
	mustBeTest("ReadAgentBaseForTest")
	return rh.readAgentBase(agentBranch)
}

// ReconcileMainForTest is reconcileMain.
func (rh *Repo) ReconcileMainForTest(ctx context.Context, upstreamMain string) (MainReconcileResult, error) {
	mustBeTest("ReconcileMainForTest")
	return rh.reconcileMain(ctx, upstreamMain)
}

// ReconcileAgentForTest is reconcileAgent.
func (rh *Repo) ReconcileAgentForTest(ctx context.Context, agentBranch, upstreamMain string, strategy merge.ConflictStrategy, mainRewound bool) (merge.AgentReconcileResult, error) {
	mustBeTest("ReconcileAgentForTest")
	return rh.reconcileAgent(ctx, agentBranch, upstreamMain, strategy, mainRewound)
}

// ReconcileAgentMergeForTest is reconcileAgentMerge.
func (rh *Repo) ReconcileAgentMergeForTest(ctx context.Context, agentBranch, upstreamMain string, strategy merge.ConflictStrategy) (merge.AgentReconcileResult, error) {
	mustBeTest("ReconcileAgentMergeForTest")
	return rh.reconcileAgentMerge(ctx, agentBranch, upstreamMain, strategy)
}

// ReconcileAgentRebaseForTest is reconcileAgentRebase.
func (rh *Repo) ReconcileAgentRebaseForTest(ctx context.Context, agentBranch, upstreamMain string, strategy merge.ConflictStrategy) (merge.AgentReconcileResult, error) {
	mustBeTest("ReconcileAgentRebaseForTest")
	return rh.reconcileAgentRebase(ctx, agentBranch, upstreamMain, strategy)
}

// ReplayOntoUpstreamForTest is replayOntoUpstream.
func (rh *Repo) ReplayOntoUpstreamForTest(ctx context.Context, localBranch string, upstreamTip, explicitBase plumbing.Hash, strategy merge.ConflictStrategy) (merge.AgentReconcileResult, error) {
	mustBeTest("ReplayOntoUpstreamForTest")
	return rh.replayOntoUpstream(ctx, localBranch, upstreamTip, explicitBase, strategy)
}

// ReplayCommitForTest is replayCommit.
func (rh *Repo) ReplayCommitForTest(ctx context.Context, orig *object.Commit, ontoHash plumbing.Hash, strategy merge.ConflictStrategy, extraParents ...plumbing.Hash) (plumbing.Hash, error) {
	mustBeTest("ReplayCommitForTest")
	return rh.replayCommit(ctx, orig, ontoHash, strategy, extraParents...)
}

// UnpushedCommitsForTest is unpushedCommits.
func (rh *Repo) UnpushedCommitsForTest(localTip, upstreamTip, explicitBase plumbing.Hash) ([]*object.Commit, bool, error) {
	mustBeTest("UnpushedCommitsForTest")
	return rh.unpushedCommits(localTip, upstreamTip, explicitBase)
}

// ReadOriginIdentityForTest is readOriginIdentity.
func (rh *Repo) ReadOriginIdentityForTest() (string, error) {
	mustBeTest("ReadOriginIdentityForTest")
	return rh.readOriginIdentity()
}

// ReconcileNowForTest is reconcileNow.
func (ri *Index) ReconcileNowForTest(ctx context.Context, agentBranch, upstreamMain string) (SyncResult, error) {
	mustBeTest("ReconcileNowForTest")
	return ri.reconcileNow(ctx, agentBranch, upstreamMain)
}

// UpdateRemoteStatusForTest is updateRemoteStatus.
func (ri *Index) UpdateRemoteStatusForTest(name, status string, syncErr *string) error {
	mustBeTest("UpdateRemoteStatusForTest")
	return ri.updateRemoteStatus(name, status, syncErr)
}

// UpdateRemotePushStatusForTest is updateRemotePushStatus.
func (ri *Index) UpdateRemotePushStatusForTest(name, status string, pushErr *string) error {
	mustBeTest("UpdateRemotePushStatusForTest")
	return ri.updateRemotePushStatus(name, status, pushErr)
}

// OriginIdentityForTest is originIdentity.
func OriginIdentityForTest(raw string) string {
	mustBeTest("OriginIdentityForTest")
	return originIdentity(raw)
}

// AgentBaseRefNameForTest is agentBaseRefName.
func AgentBaseRefNameForTest(agentBranch string) plumbing.ReferenceName {
	mustBeTest("AgentBaseRefNameForTest")
	return agentBaseRefName(agentBranch)
}

// ClassifyMainRewindForTest is classifyMainRewind, flattened to its two
// answers: whether the histories are disjoint and the MergeBase error.
func ClassifyMainRewindForTest(local, origin *object.Commit) (disjoint bool, baseErr error) {
	mustBeTest("ClassifyMainRewindForTest")
	c := classifyMainRewind(local, origin)
	return c.Disjoint, c.BaseErr
}

// SetStoreHooksForTest installs the push and sync test seams (both may be
// nil) and returns the restore. afterSnapshot runs in Push after the
// snapshot is taken and the write lock released, before any network;
// afterFetch fails a Sync round right after a successful fetch.
func SetStoreHooksForTest(afterSnapshot func(branch string, snap plumbing.Hash), afterFetch func() error) (restore func()) {
	mustBeTest("SetStoreHooksForTest")
	storeHooksMu.Lock()
	storeTestHooks = storeHooks{afterSnapshot: afterSnapshot, afterFetch: afterFetch}
	storeHooksMu.Unlock()
	return func() {
		storeHooksMu.Lock()
		storeTestHooks = storeHooks{}
		storeHooksMu.Unlock()
	}
}
