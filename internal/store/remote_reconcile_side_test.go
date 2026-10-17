package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// The rewind replay keeps a committed experiment's work behind a merge only
// when it is this copy's own (ownSideCommits). These tests build the shapes
// around a CommitExperiment merge and a foreign merge and assert what a
// rewind leaves on the agent branch.

func obsFact(title string) string {
	return "---\ntype: observation\n---\n# " + title + "\n\n" + title + " body\n"
}

// sideStore opens a store with agent/test at c1 = main = watermark.
func sideStore(t *testing.T) (*Service, plumbing.Hash) {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	c1 := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1"))
	require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), c1)))
	require.NoError(t, svc.rh.writeAgentBase("agent/test", c1))
	return svc, c1
}

// commitExperimentAsMerge opens experiment name at agent/test's tip, writes
// path on it, moves agent/test with an own write (agentPath), and commits the
// experiment: a merge commit, the experiment deleted.
func commitExperimentAsMerge(t *testing.T, svc *Service, name, path, agentPath string) {
	t.Helper()
	ctx := context.Background()
	_, err := svc.Experiments().OpenExperiment(ctx, name, "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/"+name, path, name, "experiment knowledge")
	writeMergeFact(t, svc, "agent/test", agentPath, "agent-"+name, "agent moved")
	cr, err := svc.Experiments().CommitExperiment(ctx, name, nil)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, cr.Mode, "setup: the experiment commit is a merge")
	require.True(t, hasFileAt(t, svc, "agent/test", path), "setup: the experiment's content is on the agent branch")
}

// rewindTo force-moves main to a commit on parent that adds kb/u.md and runs
// the rewind reconcile.
func rewindTo(t *testing.T, svc *Service, parent plumbing.Hash) AgentReconcileResult {
	t.Helper()
	u := upstreamCommit(t, svc, parent, "kb/u.md", obsFact("U"))
	require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), u)))
	res, err := svc.rh.reconcileAgentRebase(context.Background(), "agent/test", "main", StrategyLocalWins)
	require.NoError(t, err)
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/u.md"), "the branch is on the new upstream")
	return res
}

// PR #531 review B1 left this open (gap531 probe B): the empty-walk no-op
// saved a committed experiment only while NOTHING was written after the
// commit. One ordinary write after CommitExperiment makes the walk non-empty;
// it replayed the write, skipped the experiment's merge and never looked
// behind it, so the experiment's committed knowledge was gone, silently (the
// experiment itself was deleted by the commit).
// SABOTAGE: remove the side collection in unpushedCommitsDropping -> exp.md
// is gone and NumReplayed is 1 -> red.
func TestReconcileAgentRebase_ExperimentMergeSurvivesALaterWriteAndARewind(t *testing.T) {
	ctx := context.Background()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(ctx, map[string]string{}, "agent/test"))
	c1 := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1"))
	require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), c1)))

	_, err = svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/exp.md", "Exp", "experiment knowledge")
	m1 := upstreamCommit(t, svc, c1, "kb/m1.md", obsFact("M1"))
	require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), m1)))
	r, err := svc.rh.reconcileAgentMerge(ctx, "agent/test", "main", StrategyLocalWins)
	require.NoError(t, err)
	require.Equal(t, ModeFF, r.Mode, "setup: the agent branch fast-forwards onto main")
	cr, err := svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, cr.Mode, "setup: the experiment commit is a merge")

	// The one ordinary write after the experiment commit.
	writeMergeFact(t, svc, "agent/test", "kb/later.md", "Later", "a write after the experiment commit")

	res := rewindTo(t, svc, c1)
	require.Equal(t, ModeRebase, res.Mode)
	require.Equal(t, 2, res.NumReplayed, "the experiment's commit and the later write")
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/exp.md"), "the experiment's committed knowledge survives the rewind")
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/later.md"))
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/m1.md"), "the rewound main's commit stays scrubbed")
}

// The side collection takes only THIS copy's commits. A hub master's
// consensus merge brings members' commits in through its second parent;
// replaying them would re-sign them as this copy (the new host refuses that
// for good, #528 N1). Here one merged-in commit is signed by another key and
// one is unsigned: neither is replayed, the copy's own write is.
// SABOTAGE: drop the own-key filter in ownSideCommits -> member.md and
// member2.md are replayed (NumReplayed 3) -> red.
func TestReconcileAgentRebase_ForeignMergedCommitsStayOffTheReplay(t *testing.T) {
	svc, c1 := sideStore(t)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	foreign, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	sig := object.Signature{Name: "member", Email: "m@m", When: time.Now()}
	f, _, err := writeFileToStore(svc.rh.gits, foreign, c1, "kb/member.md", obsFact("Member"), "member write", sig, sig)
	require.NoError(t, err)
	s, err := verifyCommitSignature(mustCommit(t, svc, f))
	require.NoError(t, err, "setup: the member commit is signed")
	g := upstreamCommit(t, svc, f, "kb/member2.md", obsFact("Member2")) // unsigned
	ownMerge(t, svc, "agent/test", g)
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/member.md"), "setup: the merge brought the member's content in")
	own, err := svc.rh.commitSigner()
	require.NoError(t, err)
	ownFP, err := keyFingerprint(own.PublicKey())
	require.NoError(t, err)
	require.NotEqual(t, ownFP, s.Fingerprint, "setup: the member commit is signed by another key")

	writeMergeFact(t, svc, "agent/test", "kb/own.md", "Own", "mine")

	res := rewindTo(t, svc, c1)
	require.Equal(t, 1, res.NumReplayed, "only the copy's own write")
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/own.md"))
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/member.md"), "a commit signed by another key is never replayed")
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/member2.md"), "an unsigned commit is never replayed")
}

// An experiment with its own internal merge: SyncExperiment merged the
// agent branch (holding an own write made after the experiment opened) into
// the experiment, then CommitExperiment fast-forwarded the agent branch onto
// it. The first-parent chain now runs through the experiment, and the
// agent's own write is behind the sync merge's second parent. Every own
// commit survives: the experiment's two, that write, a later write.
// SABOTAGE: remove the side collection -> kb/agent.md is gone -> red.
func TestReconcileAgentRebase_ExperimentWithASyncMergeSurvivesARewind(t *testing.T) {
	ctx := context.Background()
	svc, c1 := sideStore(t)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/e1.md", "E1", "before the sync")
	writeMergeFact(t, svc, "agent/test", "kb/agent.md", "Agent", "agent moved")
	sr, err := svc.Experiments().SyncExperiment(ctx, "e")
	require.NoError(t, err)
	require.Equal(t, ModeMerge, sr.Mode, "setup: the sync is a merge")
	writeMergeFact(t, svc, "exp/e", "kb/e2.md", "E2", "after the sync")
	cr, err := svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	// A fast-forward: the agent's own write now sits behind the sync merge's
	// SECOND parent, off the first-parent chain.
	require.Equal(t, ModeFF, cr.Mode, "setup: the experiment commit fast-forwards onto the synced experiment")
	writeMergeFact(t, svc, "agent/test", "kb/later.md", "Later", "after the commit")

	res := rewindTo(t, svc, c1)
	require.Equal(t, 4, res.NumReplayed, "e1, agent, e2, later")
	for _, p := range []string{"kb/e1.md", "kb/e2.md", "kb/agent.md", "kb/later.md"} {
		require.True(t, hasFileAt(t, svc, "agent/test", p), "%s survives the rewind", p)
	}
}

// Two experiments committed one after the other, a write after each: both
// experiments' work survives, each commit replayed once.
func TestReconcileAgentRebase_TwoExperimentsSurviveARewind(t *testing.T) {
	svc, c1 := sideStore(t)
	commitExperimentAsMerge(t, svc, "one", "kb/one.md", "kb/a1.md")
	writeMergeFact(t, svc, "agent/test", "kb/l1.md", "L1", "after one")
	commitExperimentAsMerge(t, svc, "two", "kb/two.md", "kb/a2.md")
	writeMergeFact(t, svc, "agent/test", "kb/l2.md", "L2", "after two")

	res := rewindTo(t, svc, c1)
	require.Equal(t, 6, res.NumReplayed, "one, a1, l1, two, a2, l2 once each")
	for _, p := range []string{"kb/one.md", "kb/two.md", "kb/a1.md", "kb/a2.md", "kb/l1.md", "kb/l2.md"} {
		require.True(t, hasFileAt(t, svc, "agent/test", p), "%s survives the rewind", p)
	}
}

// An experiment whose first commit the new upstream already holds: only the
// commit upstream lacks is replayed from it (a commit upstream holds is
// never re-submitted).
func TestReconcileAgentRebase_ExperimentPartlyUpstreamReplaysOnlyTheRest(t *testing.T) {
	ctx := context.Background()
	svc, _ := sideStore(t)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	e1 := plumbing.NewHash(writeMergeFact(t, svc, "exp/e", "kb/e1.md", "E1", "upstream will hold this"))
	writeMergeFact(t, svc, "exp/e", "kb/e2.md", "E2", "upstream lacks this")
	writeMergeFact(t, svc, "agent/test", "kb/agent.md", "Agent", "agent moved")
	cr, err := svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, cr.Mode)
	writeMergeFact(t, svc, "agent/test", "kb/later.md", "Later", "after the commit")

	res := rewindTo(t, svc, e1)
	require.Equal(t, 3, res.NumReplayed, "e2, agent, later: e1 is upstream")
	for _, p := range []string{"kb/e1.md", "kb/e2.md", "kb/agent.md", "kb/later.md"} {
		require.True(t, hasFileAt(t, svc, "agent/test", p), "%s on the branch", p)
	}
}

func mustCommit(t *testing.T, svc *Service, h plumbing.Hash) *object.Commit {
	t.Helper()
	c, err := svc.rh.repo.CommitObject(h)
	require.NoError(t, err)
	return c
}

// ownMerge writes an own-signed merge onto branch: first parent the branch
// tip, second parent other, tree = other's tree (other is based on the tip's
// history and only adds files), as a hub master's consensus merge.
func ownMerge(t *testing.T, svc *Service, branch string, other plumbing.Hash) plumbing.Hash {
	t.Helper()
	tip := mustHeadHash(t, svc, branch)
	signer, err := svc.rh.commitSigner()
	require.NoError(t, err)
	sig := object.Signature{Name: "master", Email: "m@m", When: time.Now()}
	mc := &object.Commit{Author: sig, Committer: sig, Message: "consensus merge of member",
		TreeHash: mustCommit(t, svc, other).TreeHash, ParentHashes: []plumbing.Hash{tip, other}}
	h, err := storeCommit(svc.rh.gits, signer, mc)
	require.NoError(t, err)
	require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), h)))
	return h
}
