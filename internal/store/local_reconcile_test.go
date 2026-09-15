package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdvanceLocalUpstream_FastForwardsMainFromAgent(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, "main", "agent/a"))
	ctx := context.Background()

	res, err := svc.AdvanceLocalUpstream(ctx, "agent/a", "main")
	require.NoError(t, err)
	require.Equal(t, ModeNoop, res.Mode, "fresh init: tips equal")

	_, err = svc.Facts().WriteFact(ctx, "agent/a", "kb/x.md", testFactBody("x", 0.9, nil), "x", "")
	require.NoError(t, err)
	agentTip, err := svc.Branches().HeadCommit(ctx, "agent/a")
	require.NoError(t, err)

	res, err = svc.AdvanceLocalUpstream(ctx, "agent/a", "main")
	require.NoError(t, err)
	require.Equal(t, ModeFF, res.Mode)
	require.Equal(t, agentTip, res.NewTip)
	mainTip, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, agentTip, mainTip)

	// main's index answers for the new fact — notifyCommit ran, so the
	// advance is visible to readers and not just to the ref database.
	got, err := svc.Facts().ReadFact(ctx, "main", "kb/x.md", nil)
	require.NoError(t, err)
	require.Contains(t, got.Content, "x")

	res, err = svc.AdvanceLocalUpstream(ctx, "agent/a", "main")
	require.NoError(t, err)
	require.Equal(t, ModeNoop, res.Mode, "idempotent")
}

func TestAdvanceLocalUpstream_SkipsWhenMainIsNotAnAncestor(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, "main", "agent/a"))
	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/only-main.md", testFactBody("m", 0.9, nil), "m", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "agent/a", "kb/only-agent.md", testFactBody("a", 0.9, nil), "a", "")
	require.NoError(t, err)
	mainBefore, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)

	res, err := svc.AdvanceLocalUpstream(ctx, "agent/a", "main")
	require.NoError(t, err)
	require.Equal(t, ModeNoop, res.Mode)
	mainAfter, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, mainBefore, mainAfter, "diverged: never reset")
	// The fact that only exists on main is still there — the whole point of
	// refusing to reset (kb/gotchas/repos/remote-sync force-reset incident).
	got, err := svc.Facts().ReadFact(ctx, "main", "kb/only-main.md", nil)
	require.NoError(t, err)
	require.NotEmpty(t, got.Content)
}

// The degenerate configuration that caused the force-reset incident — the
// consensus branch IS the agent branch — must be a no-op here too.
func TestAdvanceLocalUpstream_NoopWhenUpstreamIsTheAgentBranch(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, "main", "agent/a"))
	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, "agent/a", "kb/x.md", testFactBody("x", 0.9, nil), "x", "")
	require.NoError(t, err)
	before, err := svc.Branches().HeadCommit(ctx, "agent/a")
	require.NoError(t, err)

	res, err := svc.AdvanceLocalUpstream(ctx, "agent/a", "agent/a")
	require.NoError(t, err)
	require.Equal(t, ModeNoop, res.Mode)
	after, err := svc.Branches().HeadCommit(ctx, "agent/a")
	require.NoError(t, err)
	require.Equal(t, before, after)
}
