package repos

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

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
// never got its bump and reports stale again by itself. gomock fails the test if
// any un-EXPECTed call is made, so this asserts the heal touches no global
// retry switch.
func TestHealIndexBranches_UpstreamRebuildFailureIsNotOkButIsNotFatal(t *testing.T) {
	ctrl := gomock.NewController(t)
	im := NewMockIndexManager(ctrl)

	// Agent branch heals, upstream fails mid-rebuild.
	im.EXPECT().Rebuild(gomock.Any(), "agent", gomock.Nil()).Return(nil)
	im.EXPECT().Rebuild(gomock.Any(), "main", gomock.Nil()).Return(errors.New("boom"))

	ok := healIndexBranches(context.Background(), im, "repo", stale("agent", "main"), nil)
	require.True(t, ok, "an upstream-only rebuild failure must NOT flag the whole index as error")
}

// TestHealIndexBranches_AgentRebuildFailureIsNotOk pins the fatal half of the
// rebuild path: the agent branch (index 0) is what local reads use, so its
// failure must surface as an index "error".
func TestHealIndexBranches_AgentRebuildFailureIsNotOk(t *testing.T) {
	ctrl := gomock.NewController(t)
	im := NewMockIndexManager(ctrl)

	im.EXPECT().Rebuild(gomock.Any(), "agent", gomock.Nil()).Return(errors.New("boom"))
	im.EXPECT().Rebuild(gomock.Any(), "main", gomock.Nil()).Return(nil)

	ok := healIndexBranches(context.Background(), im, "repo", stale("agent", "main"), nil)
	require.False(t, ok, "an agent-branch rebuild failure must flag the index as error")
}

// TestHealIndexBranches_RebuildsOnlyTheStaleBranches is the regression for PR
// #73 review finding #1: a branch whose own version is current must be Synced,
// not Rebuilt, even while a sibling branch is being rebuilt.
//
// Under the old global version this shape was unreachable — one stale answer
// applied to every branch — which is exactly why a permanently-failing upstream
// dragged the healthy agent branch through a full re-index on every boot,
// forever. Rebuild has no EXPECT for "agent", so gomock fails the test if the
// heal ever escalates a current branch to a full rebuild.
func TestHealIndexBranches_RebuildsOnlyTheStaleBranches(t *testing.T) {
	ctrl := gomock.NewController(t)
	im := NewMockIndexManager(ctrl)

	im.EXPECT().SyncLocked(gomock.Any(), "agent").Return(nil)
	im.EXPECT().Rebuild(gomock.Any(), "main", gomock.Nil()).Return(errors.New("no such ref"))

	branches := []healBranch{{name: "agent"}, {name: "main", stale: true}}
	ok := healIndexBranches(context.Background(), im, "repo", branches, nil)
	require.True(t, ok, "a stale upstream must not flag the index as error")
}

// TestHealIndexBranches_AllRebuildsSucceed pins the ordinary stale path.
func TestHealIndexBranches_AllRebuildsSucceed(t *testing.T) {
	ctrl := gomock.NewController(t)
	im := NewMockIndexManager(ctrl)

	im.EXPECT().Rebuild(gomock.Any(), "agent", gomock.Nil()).Return(nil)
	im.EXPECT().Rebuild(gomock.Any(), "main", gomock.Nil()).Return(nil)

	ok := healIndexBranches(context.Background(), im, "repo", stale("agent", "main"), nil)
	require.True(t, ok, "a fully successful heal must report ok")
}

// TestHealIndexBranches_SyncsWhenNotStale pins the non-heal path: when a
// branch's schema version is current it is incrementally Synced. An
// upstream-only (index > 0) Sync failure is non-fatal — the local agent index is
// usable and the reconcile loop owns upstream convergence — so the heal still
// reports ok. The heal uses SyncLocked (not bare Sync) so its background index
// mutation holds lockBranch and can't race an inline write or the observer.
func TestHealIndexBranches_SyncsWhenNotStale(t *testing.T) {
	ctrl := gomock.NewController(t)
	im := NewMockIndexManager(ctrl)

	im.EXPECT().SyncLocked(gomock.Any(), "agent").Return(nil)
	im.EXPECT().SyncLocked(gomock.Any(), "main").Return(errors.New("transient"))

	ok := healIndexBranches(context.Background(), im, "repo", fresh("agent", "main"), nil)
	require.True(t, ok, "an upstream-only sync failure must NOT flag the index as error")
}

// TestHealIndexBranches_AgentSyncFailureIsNotOk pins that a failed initial Sync
// of the agent branch (index 0 — the one local reads depend on) reports the
// heal as NOT ok, so the caller surfaces an index "error" rather than falsely
// reporting "ready".
func TestHealIndexBranches_AgentSyncFailureIsNotOk(t *testing.T) {
	ctrl := gomock.NewController(t)
	im := NewMockIndexManager(ctrl)

	im.EXPECT().SyncLocked(gomock.Any(), "agent").Return(errors.New("agent index broken"))
	im.EXPECT().SyncLocked(gomock.Any(), "main").Return(nil)

	ok := healIndexBranches(context.Background(), im, "repo", fresh("agent", "main"), nil)
	require.False(t, ok, "an agent-branch sync failure must flag the index as error")
}
