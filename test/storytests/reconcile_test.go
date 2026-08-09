// Category G — Origin reconcile rework scenarios. These tests assert the
// four bootstrap/reconcile scenarios from the 2026-05-11 origin-sync rework
// plan plus the recovery paths and the post-push main-advance regression:
//
//	G1 brand-new local repo (no origin) — agent is usable; main is dormant.
//	G2 origin set later, history disjoint — replay all local onto origin/main.
//	G3 origin set, common ancestor, no remote agent — replay delta onto origin/main.
//	G4 origin set, common ancestor, remote agent exists — adopt origin/agent/<host>.
//	G5 token-expiry resume — local advanced offline; reconnect pushes deltas.
//	G6 origin/main rewind — agent re-migrates onto new origin/main.
//	G7 post-push main advance — agent must keep consuming main updates after
//	   its own first push (the design bug fixed by the watermark rework).
//	G8 adopt origin/agent after main advanced — watermark must seed to the
//	   merge base so subsequent main scrubs propagate (the bootstrap-watermark
//	   bug fixed by the InitFromRemote MergeBase seed).
package storytests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
	"knomit/test/testenv"
)

// G1: brand-new local repo (no origin). InitRepo path produces a state in
// which the agent branch is fully usable for local edits without touching
// any remote. Main exists (it's the consensus branch) but writes on the
// agent do not advance main locally.
func TestReconcile_G1_BrandNewLocalNoOrigin(t *testing.T) {
	t.Log("G1: fresh repo, no origin; agent + main co-exist; commits land on agent only")
	sb := testenv.NewStoryboard(t)
	a := sb.Repo("a") // no Connect — no origin

	agent := a.Branch("agent/test")
	mainBefore := a.Branch("main").Head().CommitHash()

	agent.Write("kb/x.md", testenv.Fact("x"), "add x")

	// Agent advanced; main is unchanged.
	require.True(t, agent.HasFile("kb/x.md"), "agent has the local write")
	require.Equal(t, mainBefore, a.Branch("main").Head().CommitHash(),
		"main does not advance from local agent writes")
	require.NotEqual(t, mainBefore, agent.Head().CommitHash(),
		"agent moved past the shared root")
}

// G2: origin set later with disjoint history. Local accumulates work on the
// agent branch with no origin configured. Then ConnectKeepingWork wires an
// origin whose main is unrelated to the local chain. Reconcile must replay
// the local commits onto origin/main, producing an agent branch that
// contains BOTH the local files AND origin's main content. The replay
// rewrites commit hashes, so the post-replay agent tip differs from the
// pre-migration head.
func TestReconcile_G2_DisjointHistoryReplaysOntoOriginMain(t *testing.T) {
	t.Log("G2: local agent has work; origin has disjoint history; replay puts local on top of origin/main")
	sb := testenv.NewStoryboard(t)

	a := sb.Repo("a") // no origin yet
	agent := a.Branch("agent/test")
	agent.Write("kb/local-a.md", testenv.Fact("local-a"), "local A")
	agent.Write("kb/local-b.md", testenv.Fact("local-b"), "local B")
	preMigrationHead := agent.HeadCommit()

	// Build a bare remote with disjoint history on main.
	remote := sb.BareRemote("origin")
	remote.WriteMain("kb/remote-x.md", testenv.Fact("remote-x"), "remote X (disjoint root)")

	// Now connect — triggers Origins.Set + ActivateSync (reconcile).
	a.ConnectKeepingWork(remote)

	postAgent := a.Branch("agent/test")
	require.NotEqual(t, preMigrationHead, postAgent.HeadCommit(),
		"replayed commits have new hashes")
	require.True(t, postAgent.HasFile("kb/local-a.md"), "local A survived replay")
	require.True(t, postAgent.HasFile("kb/local-b.md"), "local B survived replay")
	require.True(t, postAgent.HasFile("kb/remote-x.md"), "remote X is now in agent")
}

// G3: origin set later, common ancestor exists (the seed), no remote agent
// branch yet. The agent's local delta replays onto the advanced
// origin/main; the result is linear history with seed + remote-advanced
// + local additions.
func TestReconcile_G3_CommonAncestorNoRemoteAgent(t *testing.T) {
	t.Log("G3: local agent shares root with origin/main; new commits replay onto advanced origin/main")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")
	remote.WriteMain("kb/seed.md", testenv.Fact("seed"), "seed")

	// Agent clones (gets seed via InitFromRemote), then accumulates local work.
	a := sb.Repo("a").Connect(remote)
	agent := a.Branch("agent/test")
	require.True(t, agent.HasFile("kb/seed.md"), "agent inherited seed")
	agent.Write("kb/local.md", testenv.Fact("local"), "local change")

	// Remote main advances (some other agent's work was promoted).
	remote.WriteMain("kb/promoted.md", testenv.Fact("promoted"), "promoted by other")

	// Next sync triggers reconcile. With the merge-based design, divergent
	// histories (agent has kb/local.md, main has kb/promoted.md, sharing
	// the seed root) produce a single merge commit.
	syncRes := agent.Sync()
	require.Equal(t, store.ModeMerge, syncRes.Agent.Mode,
		"diverged histories produce one merge commit")

	postAgent := a.Branch("agent/test")
	require.True(t, postAgent.HasFile("kb/seed.md"), "seed survived")
	require.True(t, postAgent.HasFile("kb/promoted.md"), "promoted change pulled in")
	require.True(t, postAgent.HasFile("kb/local.md"), "local change preserved via merge")
}

// G4: origin/agent/<host> already exists (e.g. another instance pushed
// from the same hostname). A new repo on the same agent branch picks up
// origin/agent/<host> as upstream, not origin/main.
func TestReconcile_G4_ResumesFromRemoteAgentBranch(t *testing.T) {
	t.Log("G4: remote already has agent/test with content; this repo picks it up as upstream")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")
	remote.WriteMain("kb/seed.md", testenv.Fact("seed"), "seed")

	// First instance: writes and pushes the agent branch.
	first := sb.Repo("first").Connect(remote)
	firstAgent := first.Branch("agent/test")
	firstAgent.Write("kb/persisted.md", testenv.Fact("persisted"), "persistence test")
	firstAgent.Push()

	// Second instance on the SAME agent branch: connects and should see
	// origin/agent/test, use it as upstream, end up with persisted content.
	second := sb.Repo("second").Connect(remote)
	secondAgent := second.Branch("agent/test")
	require.True(t, secondAgent.HasFile("kb/persisted.md"),
		"second instance reads upstream from origin/agent/test")
	require.True(t, secondAgent.HasFile("kb/seed.md"),
		"second instance also has the main seed (reachable through agent chain)")
}

// G5: token-expiry resume. Local advances while origin was unreachable.
// When sync resumes, the deltas push cleanly via force-push (the agent
// branch is this machine's; no one else writes to it).
//
// We don't have a hook to fake a token here, so this test exercises the
// equivalent "agent advances offline then reconnects" code path via plain
// Sync + Push. The token-refresh-via-HAL semantics are covered by Task 12's
// repos-layer test on makeRemoteAuthFn; this test verifies the user-visible
// outcome (deltas land on origin).
func TestReconcile_G5_ResumeAfterTokenExpiry(t *testing.T) {
	t.Log("G5: local advanced while offline; reconnect; force-push replays cleanly")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")
	remote.WriteMain("kb/seed.md", testenv.Fact("seed"), "seed")

	a := sb.Repo("a").Connect(remote)
	agent := a.Branch("agent/test")
	agent.Write("kb/pushed.md", testenv.Fact("pushed"), "before token expired")
	pushResult := agent.Push()
	require.True(t, pushResult.Pushed, "initial push must report Pushed=true")

	// Simulate token expiry: local keeps writing while we "lose" auth.
	// (In production this is "sync loop ticks fail, but local writes still
	// land on the agent branch". The test models the same delta accumulation
	// without involving auth state.)
	agent.Write("kb/while-offline-1.md", testenv.Fact("o1"), "while offline 1")
	agent.Write("kb/while-offline-2.md", testenv.Fact("o2"), "while offline 2")

	// Auth restored — next sync+push should land the deltas.
	agent.Sync()
	res := agent.Push()
	require.True(t, res.Pushed, "deltas must push")

	// Re-clone via a second repo to verify the deltas landed on origin.
	verify := sb.Repo("verify").Connect(remote)
	verifyAgent := verify.Branch("agent/test")
	require.True(t, verifyAgent.HasFile("kb/pushed.md"), "initial push is visible")
	require.True(t, verifyAgent.HasFile("kb/while-offline-1.md"), "offline write 1 is visible")
	require.True(t, verifyAgent.HasFile("kb/while-offline-2.md"), "offline write 2 is visible")
}

// G6: origin/main is force-rewound by an admin (e.g. a destructive
// recovery). Local main is reset to the new origin/main, and the local
// agent re-migrates onto it so the agent ends up on top of the NEW
// consensus.
//
// With the watermark-based reconcile, the agent's walk runs from
// agent_tip back to the watermark (which equals the OLD main commit
// the agent last consumed). That walk collects ONLY the agent's
// local-only commits — the inherited v1 content is part of the
// watermark commit, so it is not "unpushed". Replaying only the
// local-only commits onto the new (disjoint) main means kb/v1.md is
// correctly DROPPED from the agent — the fix for the original G6
// design bug. kb/local.md replays cleanly onto the new root.
func TestReconcile_G6_OriginMainRewindReMigratesAgent(t *testing.T) {
	t.Log("G6: origin/main is force-rewound to a disjoint history; agent re-migrates onto new main")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")
	remote.WriteMain("kb/v1.md", testenv.Fact("v1"), "v1 of main")

	a := sb.Repo("a").Connect(remote)
	agent := a.Branch("agent/test")
	require.True(t, agent.HasFile("kb/v1.md"), "agent starts with v1")
	agent.Write("kb/local.md", testenv.Fact("local"), "local change")

	// Admin rewrites origin/main to a disjoint history. We pass a properly
	// formatted FactSpec body (via .Build()) so the integrity check on the
	// post-sync agent branch doesn't trip on a non-fact-shaped kb/*.md
	// file. WriteDisjointRootOnMain takes raw content because it models
	// arbitrary admin-supplied recovery payloads; in this test we keep
	// the payload fact-shaped to satisfy Verify.
	remote.WriteDisjointRootOnMain("kb/v2.md", testenv.Fact("v2").Build(), "force-pushed v2 root")

	agent.Sync()

	postAgent := a.Branch("agent/test")
	require.True(t, postAgent.HasFile("kb/v2.md"), "new main content is on the agent")
	require.True(t, postAgent.HasFile("kb/local.md"), "local change replayed onto new main")
	require.False(t, postAgent.HasFile("kb/v1.md"),
		"scrubbed-from-main content must drop off the agent (the watermark-based fix)")

	// Local main was force-updated (not replayed); the old chain is gone
	// from main.
	postMain := a.Branch("main")
	require.True(t, postMain.HasFile("kb/v2.md"), "main was force-updated to new origin/main")
	require.False(t, postMain.HasFile("kb/v1.md"), "old main content is gone from main")
}

// G7: Post-push agent picks up main advance.
// After the agent has pushed, subsequent main advances on origin should
// reach the agent's local branch (this was the design bug — main updates
// were silently ignored once origin/agent existed).
func TestReconcile_G7_PostPushPicksUpMainAdvance(t *testing.T) {
	t.Log("G7: agent pushes, then origin/main advances; agent must pick up the main delta")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")
	remote.WriteMain("kb/seed.md", testenv.Fact("seed"), "seed")

	a := sb.Repo("a").Connect(remote)
	agent := a.Branch("agent/test")
	require.True(t, agent.HasFile("kb/seed.md"))
	// Agent writes and pushes.
	agent.Write("kb/local.md", testenv.Fact("local"), "local change")
	agent.Push()

	// Remote main advances with a third-party fact.
	remote.WriteMain("kb/promoted.md", testenv.Fact("promoted"), "promoted by other")

	// Agent syncs — must see the new main content on its branch.
	// Post-push main advance reconciles via merge (divergent: agent has
	// kb/local.md, main has kb/promoted.md) or fast-forward; never via
	// the rebase fallback (which only fires on origin/main rewind).
	syncRes := agent.Sync()
	require.Contains(t, []store.Mode{store.ModeMerge, store.ModeFF}, syncRes.Agent.Mode,
		"post-push main advance must take the merge path, not rebase")

	postAgent := a.Branch("agent/test")
	require.True(t, postAgent.HasFile("kb/promoted.md"),
		"agent must pick up main advance even after its own push")
	require.True(t, postAgent.HasFile("kb/local.md"), "local change preserved")
	require.True(t, postAgent.HasFile("kb/seed.md"), "seed preserved")
}

// G8: Adopt origin/agent after main has advanced past the last push.
// When a new repo instance connects to origin where (a) origin/agent
// exists with the agent's pushed state and (b) origin/main has advanced
// (e.g., scrubbed a file) since that push, the watermark must be seeded
// to MergeBase(origin/agent, origin/main) so the next reconcile's walk
// stops there cleanly. If seeded to current origin/main, the walk would
// reach root and resurrect scrubbed files via re-replay.
func TestReconcile_G8_AdoptOriginAgentAfterMainAdvanced(t *testing.T) {
	t.Log("G8: adopt origin/agent at older main; subsequent main delete must propagate")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")
	remote.WriteMain("kb/keep.md", testenv.Fact("keep"), "seed")
	remote.WriteMain("kb/scrub-me.md", testenv.Fact("scrub-me"), "to be deleted")

	// First agent: writes and pushes. origin/agent now exists with both files.
	first := sb.Repo("first").Connect(remote)
	firstAgent := first.Branch("agent/test")
	require.True(t, firstAgent.HasFile("kb/scrub-me.md"))
	firstAgent.Write("kb/local.md", testenv.Fact("local"), "local before scrub")
	firstAgent.Push()

	// Main advances: admin deletes scrub-me via a forward delete commit.
	remote.DeleteMain("kb/scrub-me.md", "scrub")

	// Second instance connects (e.g., reinstall on a new machine with the
	// same hostname). It adopts origin/agent (which still has scrub-me)
	// but origin/main has advanced past the delete.
	second := sb.Repo("second").Connect(remote)
	secondAgent := second.Branch("agent/test")

	// Bug repro: WITHOUT the fix, secondAgent's branch would resurrect
	// scrub-me on the next reconcile because the watermark would point to
	// current origin/main (not on agent's chain) and unpushedCommits would
	// walk to root.
	secondAgent.Sync()
	postAgent := second.Branch("agent/test")

	require.True(t, postAgent.HasFile("kb/keep.md"), "kept file preserved")
	require.True(t, postAgent.HasFile("kb/local.md"), "local change preserved")
	require.False(t, postAgent.HasFile("kb/scrub-me.md"),
		"scrubbed file must drop from agent (was deleted on main after last push)")
}

// G10: merge commit + subsequent force-rewind. After the agent has done at
// least one steady-state reconcile (which puts a merge commit on its
// branch), origin/main is force-rewound to a disjoint history. The rebase
// fallback must NOT resurrect old-main content via the merge commit's
// tree — unpushedCommits skips merge commits so the walk collects only
// the agent's own commits.
func TestReconcile_G10_MergeThenRewindDoesNotResurrectOldMain(t *testing.T) {
	t.Log("G10: agent does a steady-state merge, then origin/main rewinds; old-main content must NOT survive")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")
	remote.WriteMain("kb/seed.md", testenv.Fact("seed"), "seed")

	a := sb.Repo("a").Connect(remote)
	agent := a.Branch("agent/test")
	require.True(t, agent.HasFile("kb/seed.md"))
	agent.Write("kb/local-1.md", testenv.Fact("local-1"), "local 1")

	// Remote main advances with an unrelated change.
	remote.WriteMain("kb/main-old.md", testenv.Fact("main-old"), "old main update")

	// Steady-state sync: produces a merge commit on the agent whose tree
	// includes both agent's local-1 and main's main-old.
	syncRes := agent.Sync()
	require.Equal(t, store.ModeMerge, syncRes.Agent.Mode,
		"setup: steady-state must produce a real merge commit")

	postMergeAgent := a.Branch("agent/test")
	require.True(t, postMergeAgent.HasFile("kb/main-old.md"),
		"setup: agent picked up old-main content via merge")

	// Agent makes one more local commit on top of the merge.
	agent.Write("kb/local-2.md", testenv.Fact("local-2"), "local 2")

	// Admin force-rewinds origin/main to a disjoint history.
	remote.WriteDisjointRootOnMain("kb/new-main.md",
		testenv.Fact("new-main").Build(), "force-pushed new disjoint root")

	// Rebase fallback runs.
	agent.Sync()

	postAgent := a.Branch("agent/test")
	require.True(t, postAgent.HasFile("kb/local-1.md"), "agent-local-1 survives the rebase")
	require.True(t, postAgent.HasFile("kb/local-2.md"), "agent-local-2 survives the rebase")
	require.True(t, postAgent.HasFile("kb/new-main.md"), "new main content is on the agent")
	require.False(t, postAgent.HasFile("kb/main-old.md"),
		"OLD-main content (baked into the prior merge commit's tree) must NOT survive the rewind — the walker fix skips merge commits during rebase")
}
