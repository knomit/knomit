package store

import (
	"bytes"
	"context"
	"testing"

	"knomit/internal/store/gitserve"

	"knomit/internal/store/verify"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store/merge"
)

// Review tests for PR #511 (written by the reviewer; asserts adjusted to the
// ruling: push time is a fast fail matching ANY best base, the merge-time
// guard is the authority).
//
// R1: a criss-cross between the host merging the peer's branch into main and
// the peer merging the previous main into its branch, around a host .knomit
// change, gives the peer's merge two best merge bases with DIFFERENT .knomit.
// "Must match each base" refused the benign merge for good (it stays new on
// every later push); "matches any base" accepts it.
func TestReceivePack_CrissCrossMergeOfMainAccepted(t *testing.T) {
	ctx := context.Background()
	h := newPushHost(t, verify.VerifyOff, 1)
	fp, branch := pusherOf(t, newTestSigner(t))
	h.setPolicy(gitserve.PushPolicy{Pusher: fp, OwnBranch: hostBranch})
	h.svc.SetSigner(newTestSigner(t))
	p := newPeer(t, h.srv.URL)

	b := p.commit(p.head(), nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	p.setBranch("work", b)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch))

	changed := h.hostEditsOntology(verify.VerifyOff) // M1 on main
	require.NoError(t, p.repo.Fetch(&gogit.FetchOptions{RemoteName: "origin"}))
	m1 := p.head()
	require.Equal(t, changed, p.files(m1)[fact.OntologyFile])

	// The peer merges M1 (takes main's .knomit — the legitimate shape).
	c1 := p.commitAs("peer-abcd1234", []*object.Commit{b, m1}, nil,
		with(p.files(m1), "kb/notes/x.md", testFactBody("x", 0.9, nil)))

	// Meanwhile the host merges b into main (M2 = [M1, b]).
	_, err := h.svc.MergePushed(ctx, branch, "main", b.Hash, "")
	require.NoError(t, err)

	p.setBranch("work", c1)
	err = p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch)
	require.NoError(t, err, "R1a: a benign merge of main is refused (criss-cross, bases {M1, b} disagree on .knomit)")
}

// R1b: after the refusal the peer does what its sync does next — merges the
// newer main. c1 is still new on this push, so the branch stays wedged.
func TestReceivePack_CrissCrossThenNewerMainAccepted(t *testing.T) {
	ctx := context.Background()
	h := newPushHost(t, verify.VerifyOff, 1)
	fp, branch := pusherOf(t, newTestSigner(t))
	h.setPolicy(gitserve.PushPolicy{Pusher: fp, OwnBranch: hostBranch})
	h.svc.SetSigner(newTestSigner(t))
	p := newPeer(t, h.srv.URL)

	b := p.commit(p.head(), nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	p.setBranch("work", b)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch))
	h.hostEditsOntology(verify.VerifyOff)
	require.NoError(t, p.repo.Fetch(&gogit.FetchOptions{RemoteName: "origin"}))
	m1 := p.head()
	c1 := p.commitAs("peer-abcd1234", []*object.Commit{b, m1}, nil,
		with(p.files(m1), "kb/notes/x.md", testFactBody("x", 0.9, nil)))
	_, err := h.svc.MergePushed(ctx, branch, "main", b.Hash, "")
	require.NoError(t, err)

	require.NoError(t, p.repo.Fetch(&gogit.FetchOptions{RemoteName: "origin"}))
	m2 := p.head()
	require.NotEqual(t, m1.Hash, m2.Hash)
	c2 := p.commitAs("peer-abcd1234", []*object.Commit{c1, m2}, nil,
		with(p.files(m2), "kb/notes/y.md", testFactBody("y", 0.9, nil)))
	t.Logf("c1=%s c2=%s", c1.Hash, c2.Hash)
	p.setBranch("work", c2)
	err = p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch)
	require.NoError(t, err, "R1b: merging the newer main does not unstick the branch")

	// And the merge-time guard lets it through: c2 carries main's .knomit/.
	ontBefore := h.branchFiles("main")[fact.OntologyFile]
	res, err := h.svc.MergePushed(ctx, branch, "main", c2.Hash, "")
	require.NoError(t, err)
	require.Equal(t, merge.ModeMerge, res.Mode)
	require.Equal(t, ontBefore, h.branchFiles("main")[fact.OntologyFile])
	require.Contains(t, h.branchFiles("main"), "kb/notes/y.md")
}

// R2: the closure judges only commits the push SENDS. A pushed tip the host
// already holds (here: another agent/* branch on the host whose tip changed
// .knomit — e.g. a branch pushed before this PR) yields zero new commits, so
// the push is accepted; the merge-time guard refuses to apply its .knomit
// change to the host's agent branch.
func TestReceivePack_AdoptedHostHeldTipMergeRefused(t *testing.T) {
	ctx := context.Background()
	h := newPushHost(t, verify.VerifyOff, 1)
	fp, branch := pusherOf(t, newTestSigner(t))
	h.setPolicy(gitserve.PushPolicy{Pusher: fp, OwnBranch: hostBranch})
	h.svc.SetSigner(newTestSigner(t))

	const legacy = "agent/legacy-11111111"
	require.NoError(t, h.svc.Branches().CreateBranch(ctx, legacy, "main"))
	evil := kbOntology(verify.VerifyOff) + "# legacy edit\n"
	_, _, err := h.svc.Facts().WriteFileExactForTest(ctx, legacy, fact.OntologyFile, evil, "ontology", "edit", "")
	require.NoError(t, err)

	p := newPeer(t, h.srv.URL) // clones every advertised agent/* ref
	ref, err := p.repo.Reference(plumbing.NewRemoteReferenceName("origin", legacy), true)
	require.NoError(t, err)
	lc, err := p.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	p.setBranch("work", lc)

	// The push sends no new commit, so push time cannot judge it: accepted.
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch))
	require.Equal(t, lc.Hash, h.ref(branch))

	// The merge is the authority: refused, and the host's agent branch is
	// unchanged under .knomit/ (same subtree hash) and at its tip.
	agentTip := h.ref(hostBranch)
	knomitBefore := knomitTreeHash(t, h, hostBranch)
	_, err = h.svc.MergePushed(ctx, branch, hostBranch, lc.Hash, "")
	var pe *merge.PrivatePathMergeError
	require.ErrorAs(t, err, &pe)
	require.ErrorIs(t, err, merge.ErrPrivatePathMerge)
	require.Equal(t, []string{fact.OntologyFile}, pe.Paths)
	require.Equal(t, agentTip, h.ref(hostBranch))
	require.Equal(t, knomitBefore, knomitTreeHash(t, h, hostBranch))
	require.NotEqual(t, evil, h.branchFiles(hostBranch)[fact.OntologyFile])
}

// knomitTreeHash is the hash of the .knomit/ subtree at branch's tip.
func knomitTreeHash(t *testing.T, h *pushHost, branch string) plumbing.Hash {
	t.Helper()
	c, err := object.GetCommit(h.svc.rh.Storer(), h.ref(branch))
	require.NoError(t, err)
	tree, err := c.Tree()
	require.NoError(t, err)
	e, err := tree.FindEntry(fact.PrivateRoot)
	require.NoError(t, err)
	return e.Hash
}
