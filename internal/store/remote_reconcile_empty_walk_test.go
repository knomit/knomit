package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/merge"
)

// A rewind whose replay walk is EMPTY (nothing of this instance's own beyond
// the watermark) used to keep the agent branch as it stood (ModeNoop). The
// branch then still held what the rewind removed upstream, never took the new
// upstream, and the caller's force-push sent the removed commits back to the
// origin. These tests pin that an empty walk rebuilds the branch on the new
// upstream instead, and that nothing of the instance's own is lost doing so.

// reachableFrom reports whether h is the tip of branch or one of its
// ancestors.
func reachableFrom(t *testing.T, svc *Service, branch string, h plumbing.Hash) bool {
	t.Helper()
	tip := mustHeadHash(t, svc, branch)
	if tip == h {
		return true
	}
	ok, err := mustCommit(t, svc, h).IsAncestor(mustCommit(t, svc, tip))
	require.NoError(t, err)
	return ok
}

// rewindToCommit force-moves main to target (an existing commit) and runs the
// rewind reconcile.
func rewindToCommit(t *testing.T, svc *Service, target plumbing.Hash) merge.AgentReconcileResult {
	t.Helper()
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), target)))
	res, err := svc.rh.ReconcileAgentRebaseForTest(context.Background(), "agent/test", "main", merge.StrategyLocalWins)
	require.NoError(t, err)
	return res
}

// The leak probe (#538 report, "finding beyond the brief"). The agent writes
// a secret; it is pushed and consumed (main and the watermark move to it);
// the origin then scrubs it by force-moving main to u, a child of c1. The
// walk stops at the watermark at once, so it is empty.
// SABOTAGE: keep the branch as it stands when the walk is empty and the two
// diverged (the old no-op) -> secret.md is on the branch, u.md is not -> red.
func TestReconcileAgentRebase_EmptyWalkDoesNotKeepAScrubbedCommit(t *testing.T) {
	svc, c1 := sideStore(t)
	s := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/secret.md", "Secret", "scrub me"))
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), s)))
	require.NoError(t, svc.rh.WriteAgentBase("agent/test", s))

	res := rewindTo(t, svc, c1)
	require.Equal(t, merge.ModeRebase, res.Mode)
	require.Equal(t, 0, res.NumReplayed)
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/secret.md"), "the scrubbed file stays scrubbed")
	require.False(t, reachableFrom(t, svc, "agent/test", s), "the scrubbed commit is not on the branch, so a push cannot send it back")
	require.Equal(t, mustHeadHash(t, svc, "main"), mustHeadHash(t, svc, "agent/test"), "the branch is the new upstream")
}

// The same scrub by the plainest rewind: main is force-moved BACK to an
// ancestor (c1). The new upstream is then an ancestor of the agent branch, so
// the walk was never run: "strictly ahead" kept the branch, the secret
// included. A watermark beyond the upstream means the branch is not strictly
// ahead: what lies between them is what the rewind removed. An own write on
// top of the secret is still replayed.
// SABOTAGE: return early whenever the upstream is an ancestor of the branch,
// whatever the watermark (the old rule) -> secret.md is kept -> red.
func TestReconcileAgentRebase_RewindToAnAncestorDoesNotKeepAScrubbedCommit(t *testing.T) {
	t.Run("nothing else", func(t *testing.T) {
		svc, c1 := sideStore(t)
		s := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/secret.md", "Secret", "scrub me"))
		require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), s)))
		require.NoError(t, svc.rh.WriteAgentBase("agent/test", s))

		res := rewindToCommit(t, svc, c1)
		require.Equal(t, merge.ModeRebase, res.Mode)
		require.False(t, hasFileAt(t, svc, "agent/test", "kb/secret.md"))
		require.False(t, reachableFrom(t, svc, "agent/test", s))
		require.Equal(t, c1, mustHeadHash(t, svc, "agent/test"))
	})
	t.Run("an own write on top", func(t *testing.T) {
		svc, c1 := sideStore(t)
		s := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/secret.md", "Secret", "scrub me"))
		require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), s)))
		require.NoError(t, svc.rh.WriteAgentBase("agent/test", s))
		writeMergeFact(t, svc, "agent/test", "kb/later.md", "Later", "after the push")

		res := rewindToCommit(t, svc, c1)
		require.Equal(t, merge.ModeRebase, res.Mode)
		require.Equal(t, 1, res.NumReplayed)
		require.True(t, hasFileAt(t, svc, "agent/test", "kb/later.md"), "the own write is replayed")
		require.False(t, hasFileAt(t, svc, "agent/test", "kb/secret.md"))
		require.False(t, reachableFrom(t, svc, "agent/test", s))
	})
	// Regression guard: an upstream the branch is truly ahead of (the
	// watermark is at or below it) keeps the branch, hashes unchanged.
	t.Run("truly ahead stays as it is", func(t *testing.T) {
		svc, c1 := sideStore(t)
		w := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "not pushed yet"))
		res := rewindToCommit(t, svc, c1)
		require.Equal(t, merge.ModeNoop, res.Mode)
		require.Equal(t, w, mustHeadHash(t, svc, "agent/test"))
	})
	// The watermark STRICTLY below an upstream that is an ancestor of the
	// branch (#539 review N1): the upstream is w1, the branch w2 on it, the
	// watermark still c1. Nothing beyond the watermark was removed, so the
	// branch is ahead and keeps its hashes.
	// SABOTAGE: swap the watermark/upstream ancestry in strictlyAhead
	// (upstream.IsAncestor(base)) -> w2 is replayed onto w1 under a new hash
	// -> red.
	t.Run("watermark below the upstream stays as it is", func(t *testing.T) {
		svc, c1 := sideStore(t)
		w1 := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/w1.md", "W1", "first"))
		w2 := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/w2.md", "W2", "second"))
		base, err := svc.rh.ReadAgentBaseForTest("agent/test")
		require.NoError(t, err)
		require.Equal(t, c1, base, "setup: the watermark is c1, below w1")
		res := rewindToCommit(t, svc, w1)
		require.Equal(t, merge.ModeNoop, res.Mode)
		require.Equal(t, w2, mustHeadHash(t, svc, "agent/test"), "hashes unchanged")
	})
}

// An agent branch with nothing of its own at all: it fast-forwarded onto
// main m1, and the origin then removed m1. The branch is the new upstream;
// m1's content is not kept (it is the origin's to remove).
// SABOTAGE: the old no-op -> m1.md stays and u.md is missing -> red.
func TestReconcileAgentRebase_EmptyBranchTakesTheNewUpstream(t *testing.T) {
	ctx := context.Background()
	svc, c1 := sideStore(t)
	m1 := upstreamCommit(t, svc, c1, "kb/m1.md", obsFact("M1"))
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), m1)))
	r, err := svc.rh.ReconcileAgentMergeForTest(ctx, "agent/test", "main", merge.StrategyLocalWins)
	require.NoError(t, err)
	require.Equal(t, merge.ModeFF, r.Mode, "setup: the agent branch fast-forwards onto m1")

	rewindTo(t, svc, c1)
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/m1.md"))
	require.False(t, reachableFrom(t, svc, "agent/test", m1))
	require.Equal(t, mustHeadHash(t, svc, "main"), mustHeadHash(t, svc, "agent/test"))
}

// A merges-only divergence (kb gap 75e0108b): beyond the watermark the branch
// holds only an own-signed merge of a commit signed by another key (a hub
// master's consensus merge). Nothing of the instance's own is behind it, so
// the walk is empty; the branch is rebuilt on the new upstream and neither
// the merge nor the foreign commit is pushed.
// SABOTAGE: the old no-op -> member.md and the merge are kept -> red.
func TestReconcileAgentRebase_MergesOnlyDivergenceIsNotPushedAsItStands(t *testing.T) {
	svc, c1 := sideStore(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	foreign, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	sig := object.Signature{Name: "member", Email: "m@m", When: time.Now()}
	f, _, err := vcs.WriteFileToStore(svc.rh.Storer(), foreign, c1, "kb/member.md", obsFact("Member"), "member write", sig, sig)
	require.NoError(t, err)
	m := ownMerge(t, svc, "agent/test", f)

	res := rewindTo(t, svc, c1)
	require.Equal(t, 0, res.NumReplayed)
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/member.md"), "a foreign commit is never pushed as this copy's")
	require.False(t, reachableFrom(t, svc, "agent/test", m))
	require.False(t, reachableFrom(t, svc, "agent/test", f))
}

// The no-op's reason to exist (#531 B1): a branch whose only divergence is
// an own experiment merge. The side collection (#535) replays the merge as
// one commit, so the walk is not empty and the experiment survives; what the
// rewind removed (m1) does not come back.
// SABOTAGE: never collect a clean side (skip the merge) -> exp.md is lost
// with the rebuild -> red.
func TestReconcileAgentRebase_OwnExperimentMergeOnlySurvivesTheRebuild(t *testing.T) {
	ctx := context.Background()
	svc, c1 := sideStore(t)
	_, err := svc.Merge().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/exp.md", "Exp", "experiment knowledge")
	m1 := upstreamCommit(t, svc, c1, "kb/m1.md", obsFact("M1"))
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), m1)))
	r, err := svc.rh.ReconcileAgentMergeForTest(ctx, "agent/test", "main", merge.StrategyLocalWins)
	require.NoError(t, err)
	require.Equal(t, merge.ModeFF, r.Mode)
	cr, err := svc.Merge().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	require.Equal(t, merge.ModeMerge, cr.Mode, "setup: the experiment commit is a merge")

	res := rewindTo(t, svc, c1)
	require.Equal(t, merge.ModeRebase, res.Mode)
	require.Equal(t, 1, res.NumReplayed, "the experiment's merge, as one commit")
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/exp.md"), "the experiment's committed knowledge survives")
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/m1.md"), "the rewound main's commit stays scrubbed")
	require.False(t, reachableFrom(t, svc, "agent/test", m1))
}

// A fleet MEMBER after a dead-master fallback (75e0108b's fleet case). The
// member's commit x was merged by the old master (merge signed by the
// master, on top of the master's own host-record commit h); the member
// fast-forwarded onto that merge, so the watermark is the merge. The new
// host's main lacks h and the merge. Kept as it stood, the member's branch
// carried the master-signed commits, which the new host refuses for good
// (signer-is-pusher, not-pusher). Rebuilt on the new upstream it carries
// none. x lies below the watermark, so, as for any consumed commit (and as
// already happened when the member had written anything after the
// fast-forward), it is not replayed.
// SABOTAGE: the old no-op -> h and the merge are on the branch -> red.
func TestReconcileAgentRebase_FleetMemberAfterAFallbackPushesNoMasterCommit(t *testing.T) {
	ctx := context.Background()
	svc, c1 := sideStore(t)
	own, err := svc.rh.CommitSigner()
	require.NoError(t, err)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	master, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	sig := object.Signature{Name: "x", Email: "x@x", When: time.Now()}

	x, _, err := vcs.WriteFileToStore(svc.rh.Storer(), own, c1, "kb/x.md", obsFact("X"), "member write", sig, sig)
	require.NoError(t, err)
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("agent/test"), x)))
	h, _, err := vcs.WriteFileToStore(svc.rh.Storer(), master, c1, "kb/host.md", obsFact("Host"), "host record", sig, sig)
	require.NoError(t, err)
	both, _, err := vcs.WriteFileToStore(svc.rh.Storer(), master, h, "kb/x.md", obsFact("X"), "tree of the merge", sig, sig)
	require.NoError(t, err)
	m, err := vcs.StoreCommit(svc.rh.Storer(), master, &object.Commit{Author: sig, Committer: sig, Message: "consensus merge",
		TreeHash: mustCommit(t, svc, both).TreeHash, ParentHashes: []plumbing.Hash{h, x}})
	require.NoError(t, err)
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), m)))
	r, err := svc.rh.ReconcileAgentMergeForTest(ctx, "agent/test", "main", merge.StrategyLocalWins)
	require.NoError(t, err)
	require.Equal(t, merge.ModeFF, r.Mode, "setup: the member fast-forwards onto the master's merge")

	rewindTo(t, svc, c1)
	require.False(t, reachableFrom(t, svc, "agent/test", m), "the master's merge is not pushed as the member's")
	require.False(t, reachableFrom(t, svc, "agent/test", h), "the master's commit is not pushed as the member's")
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/host.md"))
}

// Disjoint histories keep the no-op. A fresh local repository attached to a
// remote of the same knowledge base has an empty walk onto a DISJOINT upstream
// (its watermark is its own InitRepo seed); taking the remote's main there
// would change the repository's identity, its root commit (pinned at the repo
// level by repos TestSwap_CopyFailureRestoresStoreAndOrigin and web
// TestHandleCommit_Disjoint_FailedSwapRestartsThePreviousSyncMode).
// SABOTAGE: drop the disjoint check in replayOntoUpstream -> the branch is the
// remote's root -> red.
func TestReconcileAgentRebase_EmptyWalkOntoADisjointUpstreamKeepsTheBranch(t *testing.T) {
	svc, c1 := sideStore(t)
	d := plumbing.NewHash(writeRawCommit(t, svc, nil, map[string]string{"kb/remote.md": obsFact("Remote")}, time.Now(), "another root"))
	res := rewindToCommit(t, svc, d)
	require.Equal(t, merge.ModeNoop, res.Mode)
	require.Equal(t, c1, mustHeadHash(t, svc, "agent/test"), "the branch keeps its own root")
}
