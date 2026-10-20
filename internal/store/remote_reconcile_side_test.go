package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"strings"
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
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), c1)))
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
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), u)))
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
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), c1)))

	_, err = svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/exp.md", "Exp", "experiment knowledge")
	m1 := upstreamCommit(t, svc, c1, "kb/m1.md", obsFact("M1"))
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), m1)))
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
	f, _, err := WriteFileToStore(svc.rh.Storer(), foreign, c1, "kb/member.md", obsFact("Member"), "member write", sig, sig)
	require.NoError(t, err)
	s, err := verifyCommitSignature(mustCommit(t, svc, f))
	require.NoError(t, err, "setup: the member commit is signed")
	g := upstreamCommit(t, svc, f, "kb/member2.md", obsFact("Member2")) // unsigned
	ownMerge(t, svc, "agent/test", g)
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/member.md"), "setup: the merge brought the member's content in")
	own, err := svc.rh.CommitSigner()
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

// An experiment whose first commit the new upstream already holds, and
// whose file the upstream then edited: the merge is NOT replayed as one (its
// change against its first parent would re-add e1's version over the
// upstream's edit); only the commit upstream lacks is replayed from it (a
// commit upstream holds is never re-submitted).
// SABOTAGE: never set heldOffLine in sideOf -> the merge is replayed whole,
// kb/e1.md is back at the experiment's version -> red.
func TestReconcileAgentRebase_ExperimentPartlyUpstreamReplaysOnlyTheRest(t *testing.T) {
	ctx := context.Background()
	svc, _ := sideStore(t)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	e1 := plumbing.NewHash(writeMergeFact(t, svc, "exp/e", "kb/e1.md", "E1", "EXPV upstream will hold this"))
	writeMergeFact(t, svc, "exp/e", "kb/e2.md", "E2", "upstream lacks this")
	writeMergeFact(t, svc, "agent/test", "kb/agent.md", "Agent", "agent moved")
	cr, err := svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, cr.Mode)
	writeMergeFact(t, svc, "agent/test", "kb/later.md", "Later", "after the commit")

	edited := upstreamCommit(t, svc, e1, "kb/e1.md", "---\ntype: observation\n---\n# E1\n\nMAINV upstream edited it\n")
	res := rewindTo(t, svc, edited)
	require.Equal(t, 3, res.NumReplayed, "e2, agent, later: e1 is upstream")
	for _, p := range []string{"kb/e1.md", "kb/e2.md", "kb/agent.md", "kb/later.md"} {
		require.True(t, hasFileAt(t, svc, "agent/test", p), "%s on the branch", p)
	}
	require.Equal(t, "MAINV", bodyMark(t, svc, "kb/e1.md"), "the upstream's edit of a commit it holds is never reverted")
}

// bodyMark returns which marker (EXPV, AGENTV, RESOLVEDV, MAINV) the file at
// path on agent/test's tip holds, "<absent>" when there is none.
func bodyMark(t *testing.T, svc *Service, path string) string {
	t.Helper()
	tree, err := mustCommit(t, svc, mustHeadHash(t, svc, "agent/test")).Tree()
	require.NoError(t, err)
	f, err := tree.File(path)
	if err != nil {
		return "<absent>"
	}
	s, err := f.Contents()
	require.NoError(t, err)
	for _, m := range []string{"EXPV", "AGENTV", "RESOLVEDV", "MAINV"} {
		if strings.Contains(s, m) {
			return m
		}
	}
	return s
}

// conflictedCommit opens experiment e at agent/test's tip, writes kb/p.md
// (EXPV) and kb/other.md on it, writes kb/p.md (AGENTV) on agent/test, and
// commits the experiment: refused, then retried with res for kb/p.md.
func conflictedCommit(t *testing.T, svc *Service, res Resolution) {
	t.Helper()
	ctx := context.Background()
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/p.md", "P", "EXPV")
	writeMergeFact(t, svc, "exp/e", "kb/other.md", "O", "exp other")
	writeMergeFact(t, svc, "agent/test", "kb/p.md", "P", "AGENTV")
	_, err = svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.Error(t, err, "setup: the conflict refuses")
	cr, err := svc.Experiments().CommitExperiment(ctx, "e", map[string]Resolution{"kb/p.md": res})
	require.NoError(t, err)
	require.Equal(t, ModeMerge, cr.Mode)
}

// #535 review B1 (probes P1, P2): a conflicted experiment commit, resolved by
// the caller, then a later write and a rewind. The merge's own decision
// comes back as decided: the agent's version kept on "dst", the written
// body. Replaying the experiment's commits one by one instead brought back
// the experiment's version, the one the merge dropped.
// SABOTAGE: replay a clean own side commit by commit (never the merge as
// one) -> p.md is EXPV -> red.
func TestReconcileAgentRebase_AResolvedExperimentMergeComesBackAsDecided(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  Resolution
		want string
	}{
		{"dst", Resolution{Side: ResolveDst}, "AGENTV"},
		{"body", Resolution{Body: []byte("---\ntype: observation\n---\n# P\n\nRESOLVEDV\n")}, "RESOLVEDV"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, c1 := sideStore(t)
			conflictedCommit(t, svc, tc.res)
			require.Equal(t, tc.want, bodyMark(t, svc, "kb/p.md"), "setup: the merge decided")
			writeMergeFact(t, svc, "agent/test", "kb/later.md", "L", "later")

			res := rewindTo(t, svc, c1)
			require.Equal(t, ModeRebase, res.Mode)
			require.Equal(t, 3, res.NumReplayed, "the agent's write, the merge as one commit, the later write")
			require.Equal(t, tc.want, bodyMark(t, svc, "kb/p.md"), "the merge's resolution survives the rewind")
			require.True(t, hasFileAt(t, svc, "agent/test", "kb/other.md"), "the experiment's other work survives")
			require.True(t, hasFileAt(t, svc, "agent/test", "kb/later.md"))
		})
	}
}

// #535 review B1 (probe P3), the #531 B1 shape plus a conflict: the agent
// fast-forwarded onto main m1 (which wrote p.md), the experiment commit kept
// main's p.md ("dst"), nothing else beyond the watermark; main is rewound to
// a child of m1. dev kept the branch as it stood (correct); the per-commit
// side replay rebuilt it with the experiment's p.md (a regression).
// SABOTAGE: as above -> p.md is EXPV -> red.
func TestReconcileAgentRebase_AResolvedMergeOnlyDivergenceKeepsTheResolution(t *testing.T) {
	ctx := context.Background()
	svc, c1 := sideStore(t)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/p.md", "P", "EXPV")
	writeMergeFact(t, svc, "exp/e", "kb/other.md", "O", "exp other")
	m1 := upstreamCommit(t, svc, c1, "kb/p.md", "---\ntype: observation\n---\n# P\n\nMAINV\n")
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), m1)))
	r0, err := svc.rh.reconcileAgentMerge(ctx, "agent/test", "main", StrategyLocalWins)
	require.NoError(t, err)
	require.Equal(t, ModeFF, r0.Mode, "setup: the agent fast-forwards onto m1")
	_, err = svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.Error(t, err, "setup: the conflict refuses")
	_, err = svc.Experiments().CommitExperiment(ctx, "e", map[string]Resolution{"kb/p.md": {Side: ResolveDst}})
	require.NoError(t, err)
	require.Equal(t, "MAINV", bodyMark(t, svc, "kb/p.md"), "setup: the merge kept main's version")

	rewindTo(t, svc, m1)
	require.Equal(t, "MAINV", bodyMark(t, svc, "kb/p.md"), "the merge's resolution survives the rewind")
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/other.md"))
}

// #535 review probe P4: experiment content deleted later on the first-parent
// line stays deleted (what the merge brought in is replayed before the
// commits above it).
// SABOTAGE: replay the merge after the commits above it (prepend it to the
// newest-first list) -> x.md is back -> red.
func TestReconcileAgentRebase_ExperimentContentDeletedLaterStaysDeleted(t *testing.T) {
	ctx := context.Background()
	svc, c1 := sideStore(t)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/x.md", "X", "EXPV")
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")
	_, err = svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	_, _, err = svc.Facts().BatchWriteFacts(ctx, "agent/test", nil, []string{"kb/x.md"}, "revert x", "deleted")
	require.NoError(t, err)
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/x.md"))

	res := rewindTo(t, svc, c1)
	require.Equal(t, 3, res.NumReplayed)
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/x.md"), "the later delete wins")
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/w.md"))
}

// #535 review S1 (probe P5): an own commit below the watermark (pushed,
// consumed, then scrubbed by the rewind) that the experiment's merge reaches
// through its second parent stays scrubbed: the stop point's history is held.
// SABOTAGE: leave the stop point's history out of sideWalk.held -> the side
// walk collects secret.md's commit -> red.
func TestReconcileAgentRebase_ScrubbedOwnCommitBehindAnExperimentStaysScrubbed(t *testing.T) {
	ctx := context.Background()
	svc, c1 := sideStore(t)
	writeMergeFact(t, svc, "agent/test", "kb/secret.md", "S", "scrub me")
	// The experiment opens at the secret commit; one more own commit y is
	// consumed with it, so the secret commit is BELOW the watermark, not the
	// watermark itself.
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	y := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/y.md", "Y", "consumed with it"))
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), y)))
	require.NoError(t, svc.rh.writeAgentBase("agent/test", y))
	writeMergeFact(t, svc, "exp/e", "kb/e.md", "E", "exp")
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")
	cr, err := svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, cr.Mode)

	rewindTo(t, svc, c1)
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/e.md"))
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/w.md"))
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/secret.md"), "a commit below the watermark stays scrubbed")
	t.Run("behind a mixed merge, replayed one by one", scrubbedOwnCommitBehindAMixedMerge)
}

// scrubbedOwnCommitBehindAMixedMerge: the same exclusion on the one-by-one
// path. An own commit s below the watermark (consumed from main), then a
// foreign commit on it merged into the agent branch: s is held, so the side
// holds nothing of the instance's own, and nothing is replayed from it. Left
// out of the held set, s would be collected as an own commit of a mixed side
// and replayed.
func scrubbedOwnCommitBehindAMixedMerge(t *testing.T) {
	svc, c1 := sideStore(t)
	own, err := svc.rh.CommitSigner()
	require.NoError(t, err)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	foreign, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	sig := object.Signature{Name: "x", Email: "x@x", When: time.Now()}
	s, _, err := WriteFileToStore(svc.rh.Storer(), own, c1, "kb/secret.md", obsFact("Secret"), "own, consumed", sig, sig)
	require.NoError(t, err)
	// main moved on past s (s2), so s is BELOW the watermark, not at it.
	s2, _, err := WriteFileToStore(svc.rh.Storer(), own, s, "kb/s2.md", obsFact("S2"), "own, consumed", sig, sig)
	require.NoError(t, err)
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), s2)))
	require.NoError(t, svc.rh.writeAgentBase("agent/test", s2))
	f, _, err := WriteFileToStore(svc.rh.Storer(), foreign, s, "kb/member.md", obsFact("Member"), "member write", sig, sig)
	require.NoError(t, err)
	ownMerge(t, svc, "agent/test", f)
	writeMergeFact(t, svc, "agent/test", "kb/later.md", "Later", "after the merge")

	res := rewindTo(t, svc, c1)
	require.Equal(t, 1, res.NumReplayed, "only the later write")
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/secret.md"), "a commit below the watermark stays scrubbed")
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/member.md"))
}

// A merge whose side holds this instance's own commit AND another key's: the
// merge is not replayed as one (its change would carry the foreign content);
// the own commit is replayed on its own, the foreign one never.
// SABOTAGE: treat a mixed side as clean (ignore foreign in the switch) ->
// the merge is replayed whole and member.md comes back -> red; drop the own
// commits of a mixed side -> own-side.md is lost -> red.
func TestReconcileAgentRebase_AMixedMergeReplaysOnlyItsOwnCommits(t *testing.T) {
	svc, c1 := sideStore(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	foreign, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	own, err := svc.rh.CommitSigner()
	require.NoError(t, err)
	sig := object.Signature{Name: "x", Email: "x@x", When: time.Now()}
	f, _, err := WriteFileToStore(svc.rh.Storer(), foreign, c1, "kb/member.md", obsFact("Member"), "member write", sig, sig)
	require.NoError(t, err)
	o, _, err := WriteFileToStore(svc.rh.Storer(), own, f, "kb/own-side.md", obsFact("OwnSide"), "own write on the side", sig, sig)
	require.NoError(t, err)
	ownMerge(t, svc, "agent/test", o)
	writeMergeFact(t, svc, "agent/test", "kb/later.md", "Later", "after the merge")

	res := rewindTo(t, svc, c1)
	require.Equal(t, 2, res.NumReplayed, "own-side and later")
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/own-side.md"), "the own commit is replayed")
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/later.md"))
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/member.md"), "foreign content is never replayed")
}

func mustCommit(t *testing.T, svc *Service, h plumbing.Hash) *object.Commit {
	t.Helper()
	c, err := svc.rh.Repository().CommitObject(h)
	require.NoError(t, err)
	return c
}

// ownMerge writes an own-signed merge onto branch: first parent the branch
// tip, second parent other, tree = other's tree (other is based on the tip's
// history and only adds files), as a hub master's consensus merge.
func ownMerge(t *testing.T, svc *Service, branch string, other plumbing.Hash) plumbing.Hash {
	t.Helper()
	tip := mustHeadHash(t, svc, branch)
	signer, err := svc.rh.CommitSigner()
	require.NoError(t, err)
	sig := object.Signature{Name: "master", Email: "m@m", When: time.Now()}
	mc := &object.Commit{Author: sig, Committer: sig, Message: "consensus merge of member",
		TreeHash: mustCommit(t, svc, other).TreeHash, ParentHashes: []plumbing.Hash{tip, other}}
	h, err := StoreCommit(svc.rh.Storer(), signer, mc)
	require.NoError(t, err)
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), h)))
	return h
}
