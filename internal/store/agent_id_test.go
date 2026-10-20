package store

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// D18/P1b: an authored commit names the PERSISTED agent id the store was given
// (SetAgentID), not "the branch minus agent/". The id here deliberately
// differs from the branch's name, so a derivation from the name is visible.
// Covers the agent branch and an experiment forked from it (whose author is
// the owning agent, #394). Sabotage: return the fixture fallback (branch minus
// "agent/") from ownAgentID even when an id is set — red: author "test".
func TestCommitAuthor_IsThePersistedAgentIDNotTheBranchName(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)
	svc.SetSigner(newTestSigner(t))
	const persisted = "opaque-7f00" // not "test", which testAgentBranch would derive to
	require.NotEqual(t, TestFallbackAgentID(testAgentBranch), persisted, "fixture: the id must differ from the name-derived one")
	svc.SetAgentID(persisted)
	require.NoError(t, svc.Branches().SetAgentBranchOwner(ctx, testAgentBranch))
	base, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)

	content := "---\ntype: observation\n---\n# a\n\nbody\n"
	_, err = svc.Facts().WriteFact(ctx, testAgentBranch, "kb/a.md", content, "learn: a", "learn")
	require.NoError(t, err)
	_, err = svc.Experiments().OpenExperiment(ctx, "e", "", testAgentBranch)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "exp/e", "kb/b.md", content, "learn: b", "learn")
	require.NoError(t, err)

	tip, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	onAgent := commitsSince(t, svc, tip, base)
	require.Len(t, onAgent, 1, "fixture: one write on the agent branch")
	requireAuthoredBy(t, onAgent, persisted)
	require.Equal(t, persisted+"+learn@agents.knomit.io", onAgent[0].Author.Email)

	expTip, err := svc.Branches().HeadCommit(ctx, "exp/e")
	require.NoError(t, err)
	onExp := commitsSince(t, svc, expTip, tip)
	require.Len(t, onExp, 1, "fixture: one write on the experiment")
	requireAuthoredBy(t, onExp, persisted)
}

// Without an agent id and without the test fixture fallback, an authored
// write is refused (ErrNoAgentID) before any object is written: production has
// no name-derived fallback. Sabotage: fall back to the branch name in
// ownAgentID — red.
func TestCommitAuthor_NoAgentIDRefuses(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)
	svc.SetSigner(newTestSigner(t))
	prev := TestFallbackAgentForTest()
	SetTestFallbackAgentID(nil)
	t.Cleanup(func() {
		if prev != nil {
			SetTestFallbackAgentID(*prev)
		}
	})
	var objs int
	require.NoError(t, svc.rh.DB().QueryRow(`SELECT COUNT(*) FROM objects`).Scan(&objs))
	_, err := svc.Facts().WriteFact(ctx, testAgentBranch, "kb/a.md", "---\ntype: observation\n---\n# a\n\nbody\n", "learn: a", "learn")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoAgentID), "err = %v", err)
	var after int
	require.NoError(t, svc.rh.DB().QueryRow(`SELECT COUNT(*) FROM objects`).Scan(&after))
	require.Equal(t, objs, after, "a refused write leaves no object")
}
