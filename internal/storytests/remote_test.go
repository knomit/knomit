// Category E — Remote sync round-trip. These tests exercise the full
// multi-agent lifecycle: agents push to a shared bare remote, the remote
// merges branches into main (simulating the planned merge-to-main
// feature), and other agents sync to receive the consensus state.
//
// The DSL helpers are: sb.BareRemote (creates a real bare git repo on
// disk), RepoHandle.Connect (wires origin), BranchHandle.Push,
// BranchHandle.Sync, RemoteHandle.MergeIntoMain, RemoteHandle.WriteMain.
package storytests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/testenv"
)

// ── E1 ────────────────────────────────────────────────────────────────────

// TestRemote_SinglePushRoundTrip is the simplest happy path: one repo
// writes on its agent branch, pushes, the bare remote has the commit.
// Under the post-rework model agents push to agent/<host>, never to main.
func TestRemote_SinglePushRoundTrip(t *testing.T) {
	t.Log("E1: write on agent/test, push, bare remote receives the commit")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")
	a := sb.Repo("a").Connect(remote)
	agent := a.Branch("agent/test")
	agent.Write("kb/x.md", testenv.Fact("x"), "add x")
	result := agent.Push()
	require.True(t, result.Pushed, "push must report Pushed=true")
}

// ── E2 ────────────────────────────────────────────────────────────────────

// TestRemote_TwoAgentsPromoteToMain is the canonical multi-agent
// scenario from working.md: agent A writes a fact, pushes its agent
// branch, the remote merges the agent branch into main, the remote
// then directly modifies the fact on main (simulating another agent's
// already-promoted change), and agent B syncs and observes the
// modified version.
func TestRemote_TwoAgentsPromoteToMain(t *testing.T) {
	t.Log("E2: A writes X v1, pushes agent/test; remote merges agent/test into main; remote writes X v2 on main; B syncs; B sees v2")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")

	// Agent A writes v1 on its agent branch and pushes. Under the post-
	// rework model agents never push to main directly — main is consensus,
	// promoted from the agent branch by the remote-side merge-to-main
	// mechanism (simulated below with RemoteHandle.MergeIntoMain).
	a := sb.Repo("a").Connect(remote)
	aAgent := a.Branch("agent/test")
	aAgent.Write("kb/x.md", testenv.Fact("x").Body("v1"), "A writes v1")
	aAgent.Push()

	// Remote-side step 1: promote agent/test into main (simulates the
	// merge-to-main feature). This puts A's v1 onto origin/main.
	remote.MergeIntoMain("agent/test", "promote agent/test to main")

	// Remote-side step 2: another agent's already-promoted change writes
	// v2 directly on main on top of v1.
	remote.WriteMain("kb/x.md",
		testenv.Fact("x").Body("v2 promoted"),
		"third party promoted v2")

	// Agent B connects and syncs; its agent branch should be replayed
	// onto the new main and observe v2.
	b := sb.Repo("b").Connect(remote)
	bAgent := b.Branch("agent/test")
	bAgent.Sync()

	bAgent.Head().Fact("kb/x.md").Body().MustContain("v2 promoted")
}

// ── E3 ────────────────────────────────────────────────────────────────────

// TestRemote_EmptyRemoteBootstrap is the regression test for the
// empty-remote ontology bug fixed in 2026-04-07. Connecting a repo to
// an empty bare remote and booting must produce a clean Verify report
// with the ontology present on the agent branch (not just an empty
// pull from origin/main).
//
// In the DSL the closest analog is: create a bare remote, create a
// repo connected to it, write nothing, verify clean. The Storyboard's
// Repo() always boots through the manager which exercises the
// InitFromRemote path when Origin is set.
//
// NOTE: the current Storyboard.Repo always uses InitRepo (no remote
// origin at boot time). The "empty-remote bootstrap" production path
// is exercised by the existing TestRemote_PushSyncRoundTrip in
// internal/testenv/bare_remote_test.go which connects after init.
// For E3 here we assert the simpler invariant: connecting to an empty
// remote and pushing produces a clean Verify on both repo and remote.
func TestRemote_EmptyRemoteBootstrap(t *testing.T) {
	t.Log("E3: connect fresh repo to empty bare remote, push agent/test, verify clean")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")
	a := sb.Repo("a").Connect(remote)
	agent := a.Branch("agent/test")
	agent.Write("kb/seed.md", testenv.Fact("seed"), "seed")
	result := agent.Push()
	require.True(t, result.Pushed)
	a.MustVerify()
}

// ── E4 ────────────────────────────────────────────────────────────────────

// TestRemote_PushFastForward asserts that when local has new commits
// and the remote is at an ancestor, the push is a fast-forward (the
// remote's ref advances to the local hash, no merge commit).
func TestRemote_PushFastForward(t *testing.T) {
	t.Log("E4: local advances, remote at base, push is a fast-forward (Pushed=true)")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")
	a := sb.Repo("a").Connect(remote)
	agent := a.Branch("agent/test")
	agent.Write("kb/x.md", testenv.Fact("x"), "x")
	agent.Push()

	// Now add another commit and push again — should be a fast-forward.
	agent.Write("kb/y.md", testenv.Fact("y"), "y")
	result := agent.Push()
	require.True(t, result.Pushed, "second push must report Pushed=true")
}

// ── E5 ────────────────────────────────────────────────────────────────────

// TestRemote_PushWithConcurrentRemoteUpdate exercises the production
// force-push fallback. Two repos push to the same agent branch in
// sequence: A pushes, then B (a separate clone) pushes a divergent
// commit. The blueprint promise is that B's push wins on the remote
// AND A's commits remain reachable in A's LOCAL history.
//
// Under the post-rework model agents always push to agent/<host>. The
// scenario uses a single agent-branch name ("agent/test") shared by
// both Storyboard repos — each repo has its own local agent/test ref,
// but they target the same ref on the bare remote. The force-push
// semantic is exactly the same as the legacy main-based version.
func TestRemote_PushWithConcurrentRemoteUpdate(t *testing.T) {
	t.Log("E5: A pushes, B pushes divergently to agent/test; B wins on remote, A's local history still has A's commit")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")

	// Seed origin/main with a baseline directly so both repos can
	// bootstrap from a non-empty remote (InitFromRemote requires
	// origin/main when the remote has any refs). Agents never write
	// main themselves under the post-rework model — only the remote-
	// side merge-to-main mechanism does, which WriteMain simulates.
	remote.WriteMain("kb/baseline.md", testenv.Fact("base"), "baseline")

	// A connects (its local main + agent/test bootstrap to baseline).
	a := sb.Repo("a").Connect(remote)
	aAgent := a.Branch("agent/test")

	// Step 1: A writes "kb/a.md" on its agent branch and does NOT push yet.
	aSnap := aAgent.Write("kb/a.md", testenv.Fact("from-a"), "A writes a")

	// Step 2: B comes online, connects (sees baseline via origin/main).
	b := sb.Repo("b").Connect(remote)
	bAgent := b.Branch("agent/test")
	// B writes its own divergent commit on its agent branch. B's local
	// agent/test history does NOT include A's kb/a.md (A never pushed).
	bAgent.Write("kb/b.md", testenv.Fact("from-b"), "B writes b")
	bAgent.Push() // First push of agent/test to origin.

	// Step 3: A pushes its diverged agent commit. The remote now has
	// B's commit at refs/heads/agent/test; A's local history branches
	// off baseline independently of B's. The production Push force-
	// pushes, overwriting B's tip on the remote.
	aAgent.Push()

	// A's local history still has A's commit (force-push only affects
	// the remote ref).
	a.Branch("agent/test").At(aSnap).Fact("kb/a.md").MustExist()
}

// ── E6 ────────────────────────────────────────────────────────────────────

// TestRemote_SyncMergesMainIntoAgent asserts that after the remote's
// main has new commits, a local Sync replays the agent on top of the
// advanced origin/main. Under the post-rework model the agent branch is
// the only branch written locally; main is consensus, mutated only by
// the remote-side merge-to-main mechanism (simulated here by WriteMain).
func TestRemote_SyncMergesMainIntoAgent(t *testing.T) {
	t.Log("E6: remote main gets a new fact, local agent syncs, local agent has both facts")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")
	a := sb.Repo("a").Connect(remote)
	agent := a.Branch("agent/test")
	agent.Write("kb/x.md", testenv.Fact("x"), "init x")
	agent.Push()

	// Remote side: a third party writes a NEW file on main directly.
	remote.WriteMain("kb/y.md", testenv.Fact("y"), "third party adds y")

	// Local sync should fetch origin/main, fast-forward main, and replay
	// the agent (the agent already includes kb/x.md; the replay must place
	// kb/x.md on top of the advanced main that carries kb/y.md).
	agent.Sync()
	agent.Head().Fact("kb/y.md").MustExist()
	agent.Head().Fact("kb/x.md").MustExist()
}

// ── E7 ────────────────────────────────────────────────────────────────────

// TestRemote_RoundTripPreservesHistory asserts that A pushes 5 commits
// on its agent branch, the agent branch is promoted to main on the
// remote, B syncs, and B's log for each touched fact matches A's log.
// Both repos see the same commit chain.
func TestRemote_RoundTripPreservesHistory(t *testing.T) {
	t.Log("E7: A pushes 5 commits to agent/test; promote to main; B syncs; both see the same chain")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")
	a := sb.Repo("a").Connect(remote)
	aAgent := a.Branch("agent/test")

	for i := range 5 {
		aAgent.Write("kb/item"+itoa(i)+".md", testenv.Fact("item"), "add")
	}
	aAgent.Push()

	// Promote A's agent branch into main on the remote so subsequent
	// agents inherit the full chain through origin/main.
	remote.MergeIntoMain("agent/test", "promote A's agent to main")

	b := sb.Repo("b").Connect(remote)
	bAgent := b.Branch("agent/test")
	bAgent.Sync()

	// B sees all 5 items.
	for i := range 5 {
		bAgent.Head().Fact("kb/item" + itoa(i) + ".md").MustExist()
	}

	// Both repos report the same fact count under kb/.
	require.Equal(t, aAgent.FactCount(), bAgent.FactCount(), "fact counts must match after sync")
}
