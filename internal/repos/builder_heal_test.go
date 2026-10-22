package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/index"
)

// healFixture is a booted repo's real index and two branches that really exist
// in it (the agent branch and main), plus a name that exists nowhere. A heal
// step on a real branch succeeds; on the phantom, Rebuild and SyncLocked fail
// at the ref lookup, which is how these tests fail one branch of a heal
// without a double of the index. progress records every Rebuild phase that
// ran: a completed Rebuild reports the start of its graph phase exactly once,
// Sync reports nothing, so that count is the number of branches that were
// REBUILT rather than synced.
type healFixture struct {
	im      *index.Index
	real0   string
	real1   string
	phantom string
	started int
}

func newHealFixture(t *testing.T) *healFixture {
	t.Helper()
	m := newTestManager(t)
	ri := bootRepo(t, m)
	f := &healFixture{
		im:      testService(t, ri).IndexManager(),
		real0:   ri.AgentBranch(),
		real1:   "main",
		phantom: "agent/never-created",
	}
	_, err := testService(t, ri).HeadCommit(context.Background(), f.real1)
	require.NoError(t, err, "fixture: main must exist")
	return f
}

// progress is the heal's RebuildProgress: it counts Rebuilds that began.
func (f *healFixture) progress(phase string, done, total int) {
	if phase == "graph" && done == 0 {
		f.started++
	}
}

// stale/fresh build the heal's per-branch work list, which is what setupIndex
// produces from one NeedsRebuild call per branch.
func stale(names ...string) []healBranch {
	out := make([]healBranch, 0, len(names))
	for _, n := range names {
		out = append(out, healBranch{name: n, stale: true})
	}
	return out
}

func fresh(names ...string) []healBranch {
	out := make([]healBranch, 0, len(names))
	for _, n := range names {
		out = append(out, healBranch{name: n, stale: false})
	}
	return out
}

// TestHealIndexBranches_UpstreamRebuildFailureIsNotOkButIsNotFatal pins the
// non-fatal half of the rebuild path: the upstream branch's index is not on the
// local read path and the reconcile loop owns its convergence, so its failure
// must not flag the whole repo index-failed.
//
// It ALSO pins what replaced PR #70's MarkRebuildNeeded: the heal re-arms
// nothing. The schema version is keyed per branch, so the failed branch simply
// never got its bump and reports stale again by itself.
func TestHealIndexBranches_UpstreamRebuildFailureIsNotOkButIsNotFatal(t *testing.T) {
	f := newHealFixture(t)
	ctx := context.Background()

	// Agent branch heals, upstream fails mid-rebuild.
	ok := healIndexBranches(ctx, f.im, "repo", stale(f.real0, f.phantom), f.progress)
	require.True(t, ok, "an upstream-only rebuild failure must NOT flag the whole index as error")
	require.Equal(t, 1, f.started, "the agent branch must have been rebuilt")

	// Nothing was re-armed for the branch that was rebuilt, and the heal wrote
	// no staleness marker for the one that failed: the next heal's per-branch
	// question is still just "is this branch's own version behind".
	staleAgent, err := f.im.NeedsRebuild(ctx, f.real0)
	require.NoError(t, err)
	require.False(t, staleAgent, "the rebuilt branch reads current")
}

// TestHealIndexBranches_AgentRebuildFailureIsNotOk pins the fatal half of the
// rebuild path: the agent branch (index 0) is what local reads use, so its
// failure must surface as an index "error".
func TestHealIndexBranches_AgentRebuildFailureIsNotOk(t *testing.T) {
	f := newHealFixture(t)

	ok := healIndexBranches(context.Background(), f.im, "repo", stale(f.phantom, f.real1), f.progress)
	require.False(t, ok, "an agent-branch rebuild failure must flag the index as error")
	require.Equal(t, 1, f.started, "the failure of branch 0 must not stop the upstream branch's rebuild")
}

// TestHealIndexBranches_RebuildsOnlyTheStaleBranches is the regression for PR
// #73 review finding #1: a branch whose own version is current must be Synced,
// not Rebuilt, even while a sibling branch is being rebuilt.
//
// Under the old global version this shape was unreachable — one stale answer
// applied to every branch — which is exactly why a permanently-failing upstream
// dragged the healthy agent branch through a full re-index on every boot,
// forever. The phantom upstream fails before its rebuild reports anything, so
// a Rebuild that began can only be the agent branch's: none may have.
func TestHealIndexBranches_RebuildsOnlyTheStaleBranches(t *testing.T) {
	f := newHealFixture(t)

	branches := []healBranch{{name: f.real0}, {name: f.phantom, stale: true}}
	ok := healIndexBranches(context.Background(), f.im, "repo", branches, f.progress)
	require.True(t, ok, "a stale upstream must not flag the index as error")
	require.Zero(t, f.started, "a current branch must be synced, never escalated to a full rebuild")
}

// TestHealIndexBranches_AllRebuildsSucceed pins the ordinary stale path.
func TestHealIndexBranches_AllRebuildsSucceed(t *testing.T) {
	f := newHealFixture(t)

	ok := healIndexBranches(context.Background(), f.im, "repo", stale(f.real0, f.real1), f.progress)
	require.True(t, ok, "a fully successful heal must report ok")
	require.Equal(t, 2, f.started, "every stale branch must be rebuilt")
}

// TestHealIndexBranches_SyncsWhenNotStale pins the non-heal path: when a
// branch's schema version is current it is incrementally Synced. An
// upstream-only (index > 0) Sync failure is non-fatal — the local agent index is
// usable and the reconcile loop owns upstream convergence — so the heal still
// reports ok. The heal uses SyncLocked (not bare Sync) so its background index
// mutation holds lockBranch and can't race an inline write or the observer.
func TestHealIndexBranches_SyncsWhenNotStale(t *testing.T) {
	f := newHealFixture(t)

	ok := healIndexBranches(context.Background(), f.im, "repo", fresh(f.real0, f.phantom), f.progress)
	require.True(t, ok, "an upstream-only sync failure must NOT flag the index as error")
	require.Zero(t, f.started, "a branch that is not stale is synced, not rebuilt")
}

// TestHealIndexBranches_AgentSyncFailureIsNotOk pins that a failed initial Sync
// of the agent branch (index 0 — the one local reads depend on) reports the
// heal as NOT ok, so the caller surfaces an index "error" rather than falsely
// reporting "ready".
func TestHealIndexBranches_AgentSyncFailureIsNotOk(t *testing.T) {
	f := newHealFixture(t)

	ok := healIndexBranches(context.Background(), f.im, "repo", fresh(f.phantom, f.real1), f.progress)
	require.False(t, ok, "an agent-branch sync failure must flag the index as error")
	require.Zero(t, f.started, "a branch that is not stale is synced, not rebuilt")
}
