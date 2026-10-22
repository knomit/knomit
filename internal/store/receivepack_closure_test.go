package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"

	"knomit/internal/store/gitserve"

	"knomit/internal/store/fleetrec"
	"knomit/internal/store/verify"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/domain/fact"
	"knomit/internal/foundation/pki"
)

// pusherOf is the full certificate-key fingerprint an instance holding k
// pushes as (the principal ID the edge puts in PushPolicy.Pusher), and a
// branch to push to. The branch name carries no part of the fingerprint:
// receive-pack reads no ownership from a name (D18).
func pusherOf(t *testing.T, k ssh.Signer) (fp, branch string) {
	t.Helper()
	pub, ok := k.PublicKey().(ssh.CryptoPublicKey).CryptoPublicKey().(ed25519.PublicKey)
	require.True(t, ok)
	return pki.Fingerprint(pub), "agent/peer-opaque"
}

// branchFiles reads the files at a host branch's tip.
func (h *pushHost) branchFiles(branch string) map[string]string {
	h.t.Helper()
	c, err := object.GetCommit(h.svc.rh.Storer(), h.ref(branch))
	require.NoError(h.t, err)
	out := map[string]string{}
	it, err := c.Files()
	require.NoError(h.t, err)
	require.NoError(h.t, it.ForEach(func(f *object.File) error {
		s, err := f.Contents()
		out[f.Name] = s
		return err
	}))
	return out
}

// P1/D10: a new commit that changes .knomit/ontology.yaml refuses the whole
// push with the ERR naming the commit and the path; the branch is not
// created and no object reaches the host. The pushed ontology turns
// verification off under a host that enforces it: the closure, not the
// signature judge, refuses it (the fleet is never read).
func TestReceivePack_ClosureRefusesKnomitChange(t *testing.T) {
	h := newPushHost(t, verify.VerifyEnforce, 1)
	k := newTestSigner(t)
	fp, branch := pusherOf(t, k)
	m := fleetrec.Member{Member: fact.Member{Agent: "peer-abcd1234", State: fact.MemberActive, Key: k.PublicKey()}}
	h.setPolicy(gitserve.PushPolicy{Pusher: fp, OwnBranch: hostBranch, Fleet: h.members([]fleetrec.Member{m}, nil)})
	p := newPeer(t, h.srv.URL)
	c1 := p.commit(p.head(), k, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	c2 := p.commit(c1, k, fact.OntologyFile, kbOntology(verify.VerifyOff))
	c3 := p.commit(c2, k, "kb/notes/y.md", testFactBody("y", 0.9, nil))
	p.setBranch("work", c3)
	objs := h.objects()

	err := p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch)
	require.Error(t, err)
	// c2 makes the change and c3 still carries it; commits are judged tip
	// first, so the ERR names c3.
	require.Contains(t, err.Error(), "knomit: commit "+c3.Hash.String()+" changes .knomit/ontology.yaml relative to main")
	require.Equal(t, plumbing.ZeroHash, h.ref(branch), "the branch must not be created")
	require.Equal(t, objs, h.objects(), "no object of a refused push reaches the host")
	require.Equal(t, int32(0), h.membersCalls.Load(), "the closure refuses before the signature judge runs")
}

// P1/D10: an evil merge — its .knomit/ontology.yaml differs from BOTH parents,
// each of which changes only kb/ — is refused naming the merge commit.
func TestReceivePack_ClosureRefusesEvilMerge(t *testing.T) {
	h := newPushHost(t, verify.VerifyOff, 1)
	fp, branch := pusherOf(t, newTestSigner(t))
	h.setPolicy(gitserve.PushPolicy{Pusher: fp, OwnBranch: hostBranch})
	p := newPeer(t, h.srv.URL)
	base := p.head()
	a := p.commit(base, nil, "kb/notes/a.md", testFactBody("a", 0.9, nil))
	b := p.commit(base, nil, "kb/notes/b.md", testFactBody("b", 0.9, nil))
	files := with(p.files(a), "kb/notes/b.md", testFactBody("b", 0.9, nil),
		fact.OntologyFile, kbOntology(verify.VerifyOff)+"# evil\n")
	evil := p.commitAs("peer-abcd1234", []*object.Commit{a, b}, nil, files)
	p.setBranch("work", evil)
	objs := h.objects()

	err := p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch)
	require.Error(t, err)
	require.Contains(t, err.Error(), "knomit: commit "+evil.Hash.String()+" changes .knomit/ontology.yaml")
	require.Equal(t, plumbing.ZeroHash, h.ref(branch))
	require.Equal(t, objs, h.objects())

	// Control: the same merge without the .knomit edit is accepted, so the
	// refusal above is the closure and not the merge shape.
	clean := p.commitAs("peer-abcd1234", []*object.Commit{a, b}, nil,
		with(p.files(a), "kb/notes/b.md", testFactBody("b", 0.9, nil)))
	p.setBranch("work", clean)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch))
	require.Equal(t, clean.Hash, h.ref(branch))
}

// P1/D10 and the signer rule together: the host's main gains a .knomit
// change after the peer branched; the peer merges main into its branch and
// pushes its own commit plus the merge, under enforce. The merge brings in a
// .knomit change already present in one parent (main), so the closure
// passes; main's commits are held by the host, so only the peer's two commits
// are judged, both signed by the pusher, and the push is accepted.
func TestReceivePack_MergeOfMainWithKnomitChangeAccepted(t *testing.T) {
	h := newPushHost(t, verify.VerifyEnforce, 1)
	k := newTestSigner(t)
	fp, branch := pusherOf(t, k)
	m := fleetrec.Member{Member: fact.Member{Agent: "peer-abcd1234", State: fact.MemberActive, Key: k.PublicKey()}}
	h.setPolicy(gitserve.PushPolicy{Pusher: fp, OwnBranch: hostBranch, Fleet: h.members([]fleetrec.Member{m}, nil)})
	p := newPeer(t, h.srv.URL)
	own := p.commit(p.head(), k, "kb/notes/x.md", testFactBody("x", 0.9, nil))

	// The host's main changes its ontology (still enforce) after the clone.
	changed := kbOntology(verify.VerifyEnforce) + "# host edit\n"
	_, _, err := h.svc.Facts().WriteFileExactForTest(context.Background(), "main", fact.OntologyFile, changed, "ontology", "edit", "")
	require.NoError(t, err)
	require.NoError(t, p.repo.Fetch(&gogit.FetchOptions{RemoteName: "origin"}))
	main := p.head()
	require.Equal(t, changed, p.files(main)[fact.OntologyFile], "main carries the .knomit change")

	merge := p.commitAs("peer-abcd1234", []*object.Commit{own, main}, k,
		with(p.files(main), "kb/notes/x.md", testFactBody("x", 0.9, nil)))
	p.setBranch("work", merge)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch))
	require.Equal(t, merge.Hash, h.ref(branch))
	require.Equal(t, changed, h.branchFiles(branch)[fact.OntologyFile])
	require.Equal(t, int32(1), h.membersCalls.Load(), "the signature judge ran")
}

// hostEditsOntology commits a .knomit/ontology.yaml change on the host's main
// and returns the new content.
func (h *pushHost) hostEditsOntology(mode string) string {
	h.t.Helper()
	changed := kbOntology(mode) + "# host edit\n"
	_, _, err := h.svc.Facts().WriteFileExactForTest(context.Background(), "main", fact.OntologyFile, changed, "ontology", "edit", "")
	require.NoError(h.t, err)
	return changed
}

// P1/D10 security review: a merge of the newer main that keeps the OLDER
// .knomit/ontology.yaml of its other parent (a commit the host already holds)
// equals one parent, so "differs from every parent" passed it — and a
// three-way merge into main would then apply it as a revert. Its merge base
// with main is main's tip, whose ontology it does not carry: refused.
func TestReceivePack_ClosureRefusesRevertMerge(t *testing.T) {
	h := newPushHost(t, verify.VerifyOff, 1)
	fp, branch := pusherOf(t, newTestSigner(t))
	h.setPolicy(gitserve.PushPolicy{Pusher: fp, OwnBranch: hostBranch})
	p := newPeer(t, h.srv.URL)
	old := p.head()
	own := p.commit(old, nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	changed := h.hostEditsOntology(verify.VerifyOff)
	require.NoError(t, p.repo.Fetch(&gogit.FetchOptions{RemoteName: "origin"}))
	main := p.head()
	require.Equal(t, changed, p.files(main)[fact.OntologyFile])

	files := with(p.files(main), "kb/notes/x.md", testFactBody("x", 0.9, nil),
		fact.OntologyFile, p.files(old)[fact.OntologyFile])
	revert := p.commitAs("peer-abcd1234", []*object.Commit{own, main}, nil, files)
	require.Equal(t, p.files(own)[fact.OntologyFile], files[fact.OntologyFile], "the merge equals one parent at the path")
	p.setBranch("work", revert)
	objs := h.objects()

	err := p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch)
	require.Error(t, err)
	require.Contains(t, err.Error(), "knomit: commit "+revert.Hash.String()+" changes .knomit/ontology.yaml relative to main")
	require.Equal(t, plumbing.ZeroHash, h.ref(branch))
	require.Equal(t, objs, h.objects())
}

// P1/D10: a branch that forked before main's .knomit change and never merged
// main adds a kb/ fact and is accepted: its merge base is its own fork point,
// whose .knomit/ it carries unchanged.
func TestReceivePack_ClosureAcceptsOldUnmergedBranch(t *testing.T) {
	h := newPushHost(t, verify.VerifyOff, 1)
	fp, branch := pusherOf(t, newTestSigner(t))
	h.setPolicy(gitserve.PushPolicy{Pusher: fp, OwnBranch: hostBranch})
	p := newPeer(t, h.srv.URL)
	old := p.head()
	c := p.commit(old, nil, "kb/notes/x.md", testFactBody("x", 0.9, nil))
	changed := h.hostEditsOntology(verify.VerifyOff)
	p.setBranch("work", c)

	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch))
	require.Equal(t, c.Hash, h.ref(branch))
	got := h.branchFiles(branch)
	require.Equal(t, p.files(old)[fact.OntologyFile], got[fact.OntologyFile])
	require.NotEqual(t, changed, got[fact.OntologyFile], "the branch carries its fork point's ontology, not main's")
}

// P1/D10 signer = pusher: commits authored and signed by ANOTHER active
// member, pushed on the pusher's own branch, pass every judgeCommit rule but
// are refused under enforce naming the commit and rule not-pusher; the same
// change signed by the pusher is accepted. Under log the refusal is logged
// and the push accepted (the rule rides the mode).
func TestReceivePack_SignerMustBePusher(t *testing.T) {
	kP, kO := newTestSigner(t), newTestSigner(t)
	members := []fleetrec.Member{
		{Member: fact.Member{Agent: "peer-abcd1234", State: fact.MemberActive, Key: kP.PublicKey()}},
		{Member: fact.Member{Agent: "other-0000", State: fact.MemberActive, Key: kO.PublicKey()}},
	}
	fp, branch := pusherOf(t, kP)

	t.Run("enforce", func(t *testing.T) {
		h := newPushHost(t, verify.VerifyEnforce, 1)
		h.setPolicy(gitserve.PushPolicy{Pusher: fp, OwnBranch: hostBranch, Fleet: h.members(members, nil)})
		p := newPeer(t, h.srv.URL)
		o1 := p.commitAs("other-0000", []*object.Commit{p.head()}, kO, with(p.files(p.head()), "kb/notes/x.md", testFactBody("x", 0.9, nil)))
		o2 := p.commitAs("other-0000", []*object.Commit{o1}, kO, with(p.files(o1), "kb/notes/y.md", testFactBody("y", 0.9, nil)))
		p.setBranch("work", o2)
		objs := h.objects()

		err := p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch)
		require.Error(t, err)
		require.Contains(t, err.Error(), "refused by verify_signatures=enforce: "+verify.RuleNotPusher)
		require.Contains(t, err.Error(), o2.Hash.String(), "the refusal names the commit (tip first)")
		require.Equal(t, plumbing.ZeroHash, h.ref(branch))
		require.Equal(t, objs, h.objects())

		// The same change, signed by the pusher.
		c1 := p.commit(p.head(), kP, "kb/notes/x.md", testFactBody("x", 0.9, nil))
		c2 := p.commit(c1, kP, "kb/notes/y.md", testFactBody("y", 0.9, nil))
		p.setBranch("work", c2)
		require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch))
		require.Equal(t, c2.Hash, h.ref(branch))
	})

	t.Run("log", func(t *testing.T) {
		h := newPushHost(t, verify.VerifyLog, 1)
		h.setPolicy(gitserve.PushPolicy{Pusher: fp, OwnBranch: hostBranch, Fleet: h.members(members, nil)})
		p := newPeer(t, h.srv.URL)
		o1 := p.commitAs("other-0000", []*object.Commit{p.head()}, kO, with(p.files(p.head()), "kb/notes/x.md", testFactBody("x", 0.9, nil)))
		p.setBranch("work", o1)
		logs := captureLog(t)
		require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+branch))
		require.Equal(t, o1.Hash, h.ref(branch))
		require.Contains(t, logs.String(), `"rule":"`+verify.RuleNotPusher+`"`)
		require.Contains(t, logs.String(), o1.Hash.String())
	})
}

// P1/D10: pushes that change only kb/ or only artifacts/ pass the closure
// and are registered with their content unchanged.
func TestReceivePack_ClosureAcceptsKBAndArtifacts(t *testing.T) {
	for _, path := range []string{"kb/notes/x.md", "artifacts/jobs/state.md"} {
		t.Run(path, func(t *testing.T) {
			h := newPushHost(t, verify.VerifyOff, 1)
			p := newPeer(t, h.srv.URL)
			body := testFactBody("x", 0.9, nil)
			c := p.commit(p.head(), nil, path, body)
			p.setBranch("work", c)
			require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch))
			require.Equal(t, c.Hash, h.ref(peerBranch))
			require.Equal(t, body, h.branchFiles(peerBranch)[path])
		})
	}
}
