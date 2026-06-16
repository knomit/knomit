package repos

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// TestHealIndexBranches_RemarksRebuildOnPartialFailure regresses PR #70 review
// finding #1: Rebuild bumps the GLOBAL schema version on each branch it
// completes, so when an earlier branch succeeds and a later branch fails, the
// version reads current and the next startup skips the heal — leaving the
// failed branch's derived state stale forever. The heal must re-mark the schema
// as needing a rebuild whenever any branch's rebuild fails.
func TestHealIndexBranches_RemarksRebuildOnPartialFailure(t *testing.T) {
	ctrl := gomock.NewController(t)
	im := NewMockIndexManager(ctrl)

	// Agent branch heals, upstream fails mid-rebuild.
	im.EXPECT().Rebuild(gomock.Any(), "agent", gomock.Nil()).Return(nil)
	im.EXPECT().Rebuild(gomock.Any(), "main", gomock.Nil()).Return(errors.New("boom"))
	// Because the global version was already bumped by the agent rebuild, the
	// heal must re-arm the stale flag so the next startup retries every branch.
	im.EXPECT().MarkRebuildNeeded(gomock.Any()).Return(nil)

	ok := healIndexBranches(context.Background(), im, "repo", []string{"agent", "main"}, true, nil)
	require.False(t, ok, "a rebuild failure must report the heal as NOT ok so the caller can surface 'error'")
}

// TestHealIndexBranches_NoRemarkWhenAllRebuildsSucceed pins that a fully
// successful heal does NOT re-mark the schema (which would force a redundant
// rebuild on every subsequent startup). MarkRebuildNeeded has no EXPECT, so the
// gomock controller fails the test if it is called.
func TestHealIndexBranches_NoRemarkWhenAllRebuildsSucceed(t *testing.T) {
	ctrl := gomock.NewController(t)
	im := NewMockIndexManager(ctrl)

	im.EXPECT().Rebuild(gomock.Any(), "agent", gomock.Nil()).Return(nil)
	im.EXPECT().Rebuild(gomock.Any(), "main", gomock.Nil()).Return(nil)

	ok := healIndexBranches(context.Background(), im, "repo", []string{"agent", "main"}, true, nil)
	require.True(t, ok, "a fully successful heal must report ok")
}

// TestHealIndexBranches_SyncsWhenNotStale pins the non-heal path: when the
// schema is current, each branch is incrementally Synced and the schema is
// never re-marked (a Sync failure is not a schema-version problem). An
// upstream-only (index > 0) Sync failure is non-fatal — the local agent index
// is usable and the reconcile loop owns upstream convergence — so the heal
// still reports ok.
func TestHealIndexBranches_SyncsWhenNotStale(t *testing.T) {
	ctrl := gomock.NewController(t)
	im := NewMockIndexManager(ctrl)

	im.EXPECT().Sync(gomock.Any(), "agent").Return(nil)
	im.EXPECT().Sync(gomock.Any(), "main").Return(errors.New("transient"))

	ok := healIndexBranches(context.Background(), im, "repo", []string{"agent", "main"}, false, nil)
	require.True(t, ok, "an upstream-only sync failure must NOT flag the index as error")
}

// TestHealIndexBranches_AgentSyncFailureIsNotOk pins that a failed initial Sync
// of the agent branch (index 0 — the one local reads depend on) reports the
// heal as NOT ok, so the caller surfaces an index "error" rather than falsely
// reporting "ready".
func TestHealIndexBranches_AgentSyncFailureIsNotOk(t *testing.T) {
	ctrl := gomock.NewController(t)
	im := NewMockIndexManager(ctrl)

	im.EXPECT().Sync(gomock.Any(), "agent").Return(errors.New("agent index broken"))
	im.EXPECT().Sync(gomock.Any(), "main").Return(nil)

	ok := healIndexBranches(context.Background(), im, "repo", []string{"agent", "main"}, false, nil)
	require.False(t, ok, "an agent-branch sync failure must flag the index as error")
}
