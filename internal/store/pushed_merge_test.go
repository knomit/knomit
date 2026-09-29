package store

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// pushedFixture is a host store with its agent branch and one pushed peer
// branch forked from main, as receive-pack leaves them (F11). Commits on the
// peer branch are signed with the peer's key; the merge runs as the host.
type pushedFixture struct {
	t    *testing.T
	svc  *Service
	host ssh.Signer
	peer ssh.Signer
}

const (
	pmAgent = "agent/host-aaaaaaaa"
	pmPeer  = "agent/peer-bbbbbbbb"
)

func newPushedFixture(t *testing.T) *pushedFixture {
	t.Helper()
	f := &pushedFixture{t: t, svc: newMergeTestStore(t), host: namedSigner(t, "host"), peer: namedSigner(t, "peer")}
	f.svc.SetSigner(f.host)
	writeMergeFact(t, f.svc, "main", "kb/base.md", "base", "base")
	ctx := context.Background()
	require.NoError(t, f.svc.Branches().CreateBranch(ctx, pmAgent, "main"))
	require.NoError(t, f.svc.Branches().CreateBranch(ctx, pmPeer, "main"))
	return f
}

// onPeer writes a fact to the peer branch signed with the peer's key.
func (f *pushedFixture) onPeer(path, body string) plumbing.Hash {
	f.t.Helper()
	f.svc.SetSigner(f.peer)
	defer f.svc.SetSigner(f.host)
	return plumbing.NewHash(writeMergeFact(f.t, f.svc, pmPeer, path, "t", body))
}

func (f *pushedFixture) onHost(path, body string) plumbing.Hash {
	f.t.Helper()
	return plumbing.NewHash(writeMergeFact(f.t, f.svc, pmAgent, path, "t", body))
}

func (f *pushedFixture) tip(branch string) plumbing.Hash {
	f.t.Helper()
	ref, err := f.svc.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	require.NoError(f.t, err)
	return ref.Hash()
}

func (f *pushedFixture) commit(h plumbing.Hash) *object.Commit {
	f.t.Helper()
	c, err := object.GetCommit(f.svc.rh.gits, h)
	require.NoError(f.t, err)
	return c
}

func (f *pushedFixture) merge(srcTip plumbing.Hash, side ResolutionSide) (AgentReconcileResult, error) {
	return f.svc.MergePushed(context.Background(), pmPeer, pmAgent, srcTip, side)
}

// requireHostMerge asserts the agent tip is a merge commit [hostBefore,
// peerTip] signed by the HOST's key.
func (f *pushedFixture) requireHostMerge(hostBefore, peerTip plumbing.Hash) *object.Commit {
	f.t.Helper()
	tip := f.commit(f.tip(pmAgent))
	require.Equal(f.t, []plumbing.Hash{hostBefore, peerTip}, tip.ParentHashes, "parents are [host tip, peer tip]")
	s, err := verifyCommitSignature(tip)
	require.NoError(f.t, err, "the merge commit is signed")
	hostFP, err := keyFingerprint(f.host.PublicKey())
	require.NoError(f.t, err)
	require.Equal(f.t, hostFP, s.Fingerprint, "the merge commit is signed by the HOST's key")
	return tip
}

func (f *pushedFixture) blob(commit plumbing.Hash, path string) string {
	f.t.Helper()
	file, err := f.commit(commit).File(path)
	require.NoError(f.t, err)
	s, err := file.Contents()
	require.NoError(f.t, err)
	return s
}

// Test 1: a three-way merge of a pushed branch writes a host-signed merge
// commit [host, peer] and leaves the peer's ref exactly where it was.
func TestMergePushed_ThreeWay(t *testing.T) {
	f := newPushedFixture(t)
	peerTip := f.onPeer("kb/p.md", "peer")
	hostBefore := f.onHost("kb/h.md", "host")

	res, err := f.merge(peerTip, "")
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode)
	f.requireHostMerge(hostBefore, peerTip)
	require.Equal(t, peerTip, f.tip(pmPeer), "the peer's ref is never written")
	require.Contains(t, f.blob(f.tip(pmAgent), "kb/p.md"), "peer")
	verifyMergeClean(t, f.svc, pmAgent)
}

// Test 1c (M1): in the fast-forward shape — the host's agent branch is an
// ancestor of the peer tip — the merge still writes a host-signed merge
// commit whose tree is the peer's. Other callers are unchanged: an experiment
// commit in the same shape still fast-forwards.
func TestMergePushed_NeverFastForwards(t *testing.T) {
	f := newPushedFixture(t)
	hostBefore := f.tip(pmAgent)
	peerTip := f.onPeer("kb/p.md", "peer")

	res, err := f.merge(peerTip, "")
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode, "never a fast-forward for this caller")
	tip := f.requireHostMerge(hostBefore, peerTip)
	require.Equal(t, f.commit(peerTip).TreeHash, tip.TreeHash, "the tree is the peer's")
	verifyMergeClean(t, f.svc, pmAgent)

	// The other caller: an experiment committed in an FF shape still
	// fast-forwards its parent onto the experiment's own commit.
	ctx := context.Background()
	_, err = f.svc.Experiments().OpenExperiment(ctx, "ff", "", pmAgent)
	require.NoError(t, err)
	expTip := plumbing.NewHash(writeMergeFact(t, f.svc, "exp/ff", "kb/e.md", "e", "e"))
	eres, err := f.svc.Experiments().CommitExperiment(ctx, "ff", nil)
	require.NoError(t, err)
	require.Equal(t, ModeFF, eres.Mode, "experiment commit keeps its fast-forward")
	require.Equal(t, expTip, f.tip(pmAgent))
}

// Test 2: a conflict is refused with the paths and the three commits, and
// nothing moves.
func TestMergePushed_ConflictRefused(t *testing.T) {
	f := newPushedFixture(t)
	peerTip := f.onPeer("kb/base.md", "peer version")
	hostBefore := f.onHost("kb/base.md", "host version")

	_, err := f.merge(peerTip, "")
	var conflict *MergeConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, []string{"kb/base.md"}, conflict.Paths)
	require.Equal(t, peerTip.String(), conflict.SrcCommit)
	require.Equal(t, hostBefore.String(), conflict.DstCommit)
	require.NotEmpty(t, conflict.BaseCommit)
	require.Equal(t, hostBefore, f.tip(pmAgent), "a refused merge moves nothing")
}

// Test 3 (store half): a whole-set side settles every conflicting path, and
// the survivor is WHICH body the side names.
func TestMergePushed_WholeSetResolution(t *testing.T) {
	for _, c := range []struct {
		side ResolutionSide
		want string
	}{{ResolveDst, "host version"}, {ResolveSrc, "peer version"}} {
		t.Run(string(c.side), func(t *testing.T) {
			f := newPushedFixture(t)
			peerTip := f.onPeer("kb/base.md", "peer version")
			hostBefore := f.onHost("kb/base.md", "host version")

			res, err := f.merge(peerTip, c.side)
			require.NoError(t, err)
			require.Equal(t, ModeMerge, res.Mode)
			f.requireHostMerge(hostBefore, peerTip)
			require.Contains(t, f.blob(f.tip(pmAgent), "kb/base.md"), c.want)
		})
	}
}

// Test 5 (M2): the merge takes exactly the peer commit the operator saw. If
// the branch moved, it refuses before writing anything.
func TestMergePushed_BranchMoved(t *testing.T) {
	f := newPushedFixture(t)
	seen := f.onPeer("kb/p.md", "v1")
	moved := f.onPeer("kb/p2.md", "unseen")
	hostBefore := f.tip(pmAgent)

	_, err := f.merge(seen, "")
	var bm *BranchMovedError
	require.ErrorAs(t, err, &bm)
	require.True(t, errors.Is(err, ErrBranchMoved))
	require.Equal(t, seen.String(), bm.Expected)
	require.Equal(t, moved.String(), bm.Actual)
	require.Equal(t, hostBefore, f.tip(pmAgent), "nothing moves")
}

// Tests 4 and 4b (M3): to_merge counts commits the host does not have, and a
// merge whose result tree equals the host's still records the merge, so the
// count reaches 0 instead of staying at 1 forever. The commit-log parity
// check stays clean with that zero-diff merge commit on the branch.
func TestMergePushed_ToMergeAndTreeIdentical(t *testing.T) {
	f := newPushedFixture(t)
	ctx := context.Background()
	info := func() PushedBranchInfo {
		t.Helper()
		got, err := f.svc.PushedBranches(ctx, []string{pmPeer}, pmAgent, "kb")
		require.NoError(t, err)
		require.Len(t, got, 1)
		return got[0]
	}

	require.Equal(t, 0, info().ToMerge, "a fresh peer branch at main brings nothing")
	p1 := f.onPeer("kb/p1.md", "p1")
	p2 := f.onPeer("kb/p2.md", "p2")
	got := info()
	require.Equal(t, 2, got.ToMerge)
	require.Equal(t, p2.String(), got.Tip)

	_, err := f.merge(p2, "")
	require.NoError(t, err)
	require.Equal(t, 0, info().ToMerge, "merged: nothing left")
	_ = p1

	// The steady state: the host writes a fact, and the peer independently
	// lands the SAME content (as a reconcile merge of main would), so the
	// peer's new tip is not in the host's history but its tree equals it.
	f.onHost("kb/same.md", "same")
	peerSame := f.onPeer("kb/same.md", "same")
	require.Equal(t, f.commit(f.tip(pmAgent)).TreeHash, f.commit(peerSame).TreeHash, "fixture: trees are identical")
	require.Equal(t, 1, info().ToMerge)

	hostBefore := f.tip(pmAgent)
	res, err := f.merge(peerSame, "")
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode, "a tree-identical merge is still recorded")
	tip := f.requireHostMerge(hostBefore, peerSame)
	require.Equal(t, f.commit(hostBefore).TreeHash, tip.TreeHash)
	require.Equal(t, 0, info().ToMerge, "the count clears")
	verifyMergeClean(t, f.svc, pmAgent)

	// Nothing new: a true no-op.
	res, err = f.merge(peerSame, "")
	require.NoError(t, err)
	require.Equal(t, ModeNoop, res.Mode)
}

// PushedBranches reports files outside the ontology root separately, since
// the merge dialog lists only facts.
func TestPushedBranches_OtherFilesChanged(t *testing.T) {
	f := newPushedFixture(t)
	ctx := context.Background()
	f.onPeer("kb/p.md", "p")
	f.svc.SetSigner(f.peer)
	_, err := f.svc.Facts().WriteFact(ctx, pmPeer, "notes/readme.txt", "hello", "test", "test")
	f.svc.SetSigner(f.host)
	require.NoError(t, err)

	got, err := f.svc.PushedBranches(ctx, []string{pmPeer}, pmAgent, "kb")
	require.NoError(t, err)
	require.Equal(t, 1, got[0].OtherFilesChanged)
	require.NotEmpty(t, got[0].MergeBase)
}

// noFFMerge writes, on the PEER branch, a merge commit [peer tip, other]
// whose tree is tree, signed by the peer: what a peer whose sync never
// fast-forwards leaves after merging the host back in. The ref moves
// directly, as receive-pack's register would leave a pushed tip.
func (f *pushedFixture) noFFMerge(other plumbing.Hash, tree plumbing.Hash) plumbing.Hash {
	f.t.Helper()
	parent := f.tip(pmPeer)
	c := &object.Commit{
		Author:    object.Signature{Name: "peer", Email: "peer-bbbbbbbb+merge@agents.knomit.io"},
		Committer: object.Signature{Name: "peer", Email: "peer-bbbbbbbb@agents.knomit.io"},
		Message:   "merge: main into " + pmPeer, TreeHash: tree,
		ParentHashes: []plumbing.Hash{parent, other},
	}
	h, err := storeCommit(f.svc.rh.gits, f.peer, c)
	require.NoError(f.t, err)
	require.NoError(f.t, f.svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(pmPeer), h)))
	require.NoError(f.t, f.svc.rh.populateCommitLog(context.Background(), pmPeer))
	return h
}

// F08 PR B, the ping-pong fix: MergeConsensus no-ops on a peer tip that only
// merged the host back in (every new commit a merge, the merge changing
// nothing), and records everything else as MergePushed does — a peer's
// authored commit even when its content is already here, and a merge-only
// tip that DOES change something.
//
// SABOTAGE: dropping `skipMergeOnly: true` from MergeConsensus turns the
// first case red (a host merge commit for a merge that brings nothing).
func TestMergeConsensus_SkipsMergeOnlyNoChange(t *testing.T) {
	f := newPushedFixture(t)
	ctx := context.Background()
	mergeC := func(tip plumbing.Hash) AgentReconcileResult {
		t.Helper()
		res, err := f.svc.MergeConsensus(ctx, pmPeer, pmAgent, tip)
		require.NoError(t, err)
		return res
	}

	// The first exchange: the peer's authored fact is merged and recorded.
	p1 := f.onPeer("kb/p1.md", "p1")
	require.Equal(t, ModeMerge, mergeC(p1).Mode)
	h1 := f.tip(pmAgent)

	// The peer merges the host's merge back in with a merge commit of its
	// own (no fast-forward), tree = the host's: nothing new.
	pm := f.noFFMerge(h1, f.commit(h1).TreeHash)
	require.Equal(t, ModeNoop, mergeC(pm).Mode, "a merge-only, change-free peer tip is not recorded")
	require.Equal(t, h1, f.tip(pmAgent), "the host's branch did not move")

	// Again, with the host moving on in between: still nothing to record.
	f.onHost("kb/h2.md", "h2")
	h2 := f.tip(pmAgent)
	pm2 := f.noFFMerge(h2, f.commit(h2).TreeHash)
	require.Equal(t, ModeNoop, mergeC(pm2).Mode)
	require.Equal(t, h2, f.tip(pmAgent))

	// A merge-only tip that CHANGES something (an edited merge) is recorded.
	require.NoError(t, f.svc.Branches().CreateBranch(ctx, "scratch", pmAgent))
	scratch := plumbing.NewHash(writeMergeFact(t, f.svc, "scratch", "kb/evil.md", "evil", "evil"))
	pm3 := f.noFFMerge(h2, f.commit(scratch).TreeHash)
	require.Equal(t, ModeMerge, mergeC(pm3).Mode, "a merge commit that brings a change is merged")
	f.requireHostMerge(h2, pm3)

	// A peer's AUTHORED commit whose content the host already has is still
	// recorded (MergePushed's M3 behaviour): only merge-only tips are skipped.
	f.onHost("kb/same.md", "same")
	peerSame := f.onPeer("kb/same.md", "same")
	hostBefore := f.tip(pmAgent)
	require.Equal(t, f.commit(hostBefore).TreeHash, f.commit(peerSame).TreeHash, "fixture: trees are identical")
	require.Equal(t, ModeMerge, mergeC(peerSame).Mode, "an authored commit is recorded")
	f.requireHostMerge(hostBefore, peerSame)
	verifyMergeClean(t, f.svc, pmAgent)
}

// MergeConsensus refuses a conflict (no side, nothing moves) and a moved tip,
// like MergePushed.
func TestMergeConsensus_RefusesConflictAndMovedTip(t *testing.T) {
	f := newPushedFixture(t)
	ctx := context.Background()
	peerTip := f.onPeer("kb/base.md", "peer's")
	hostBefore := f.onHost("kb/base.md", "host's")

	_, err := f.svc.MergeConsensus(ctx, pmPeer, pmAgent, peerTip)
	var conflict *MergeConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, []string{"kb/base.md"}, conflict.Paths)
	require.Equal(t, hostBefore, f.tip(pmAgent), "a refused merge moves nothing")

	_, err = f.svc.MergeConsensus(ctx, pmPeer, pmAgent, hostBefore)
	require.ErrorIs(t, err, ErrBranchMoved)
}

// OntologyAt reads the ontology at a branch's tip, nil when there is none.
func TestOntologyAt(t *testing.T) {
	f := newPushedFixture(t)
	ctx := context.Background()
	got, err := f.svc.OntologyAt(ctx, pmAgent)
	require.NoError(t, err)
	require.Nil(t, got, "no ontology in this tree")
	_, err = f.svc.Facts().WriteFact(ctx, pmAgent, ".knomit/ontology.yaml", "id: x\nattributes:\n  consensus: auto\n", "ont", "updated")
	require.NoError(t, err)
	got, err = f.svc.OntologyAt(ctx, pmAgent)
	require.NoError(t, err)
	require.Contains(t, string(got), "consensus: auto")
	got, err = f.svc.OntologyAt(ctx, "main")
	require.NoError(t, err)
	require.Nil(t, got, "read at THAT branch's tip, not another's")
	_, err = f.svc.OntologyAt(ctx, "no-such-branch")
	require.ErrorIs(t, err, ErrBranchNotFound)
}
