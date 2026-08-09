// G11 — non-"main" upstream branch. Regression test for the
// previously-hardcoded "main" consensus branch. A bare remote whose default
// branch is "master" (not "main") must bootstrap, sync, and push end-to-end
// without breaking.
//
// This test exercises the full stack: BareRemoteWithBranch sets up the
// remote with master as its symbolic HEAD; Connect drives the production
// InitFromRemote path (which auto-detects upstream when not explicitly
// passed); subsequent agent writes + Sync rounds use master throughout.
package storytests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
	"knomit/test/testenv"
)

// TestReconcile_G11_MasterUpstreamEndToEnd: full bootstrap + sync + push
// against a remote whose consensus branch is master. With the hardcoded
// "main" bug, the connect step would either fail outright (origin/main
// doesn't exist) or silently misconfigure the remotes table; afterwards
// every Sync would error reading origin/main. The fix routes the master
// name through every layer.
func TestReconcile_G11_MasterUpstreamEndToEnd(t *testing.T) {
	t.Log("G11: master-default remote bootstraps, syncs, pushes; no \"main\" anywhere")
	sb := testenv.NewStoryboard(t)

	// Bare remote with master as its symbolic HEAD.
	remote := sb.BareRemoteWithBranch("origin", "master")
	remote.WriteMain("kb/seed.md", testenv.Fact("seed"), "seed on master")
	require.Equal(t, "master", remote.UpstreamBranch())

	// Connect: the production InitFromRemote path runs. It must detect
	// master from the remote's symbolic HEAD (the builder calls
	// store.DetectRemoteUpstreamFromURL) and configure both git refspecs
	// and the remotes table accordingly.
	a := sb.Repo("a").Connect(remote)
	agent := a.Branch("agent/test")
	require.True(t, agent.HasFile("kb/seed.md"), "agent inherited seed from origin/master")

	// Verify the local consensus branch is "master" — not "main".
	masterBranch := a.Branch("master")
	require.True(t, masterBranch.HasFile("kb/seed.md"), "local master holds the seed")

	// Verify Remote.Branch round-tripped through the connect flow
	// (Origins.Set in control.db → the injected origin GetRemote assembles).
	var stored *store.Remote
	a.Instance().WithRead(func(svc *store.Service) {
		var err error
		stored, err = svc.Remote().GetRemote("origin")
		require.NoError(t, err)
	})
	require.NotNil(t, stored)
	require.Equal(t, "master", stored.Branch,
		"remotes.branch must round-trip the detected upstream (not silently rewritten to \"main\")")

	// Agent writes, pushes — master plumbing has to survive the push refspec
	// derivation too.
	agent.Write("kb/local.md", testenv.Fact("local"), "agent-local change")
	push := agent.Push()
	require.True(t, push.Pushed, "agent push must succeed against master-default remote")

	// Remote advances master independently; agent syncs and must pick it up
	// via the merge path (steady state) — proves reconcileMain reads
	// origin/master and reconcileAgentMerge merges from local master.
	remote.WriteMain("kb/promoted.md", testenv.Fact("promoted"), "promoted on master")
	syncRes := agent.Sync()
	require.Contains(t, []store.Mode{store.ModeMerge, store.ModeFF}, syncRes.Agent.Mode,
		"post-push master advance must take the merge/ff path")

	postAgent := a.Branch("agent/test")
	require.True(t, postAgent.HasFile("kb/seed.md"), "seed preserved")
	require.True(t, postAgent.HasFile("kb/local.md"), "agent-local preserved")
	require.True(t, postAgent.HasFile("kb/promoted.md"),
		"master advance must propagate to the agent (the fix this PR ships)")
}
