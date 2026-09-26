package store

import (
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/platform/fileuri"
)

// originFixture is a bare git repository on disk, filled with signed commits
// by a foldFixture, that a fresh Service clones from over file://.
type originFixture struct {
	*foldFixture
	bare string
	repo *gogit.Repository
}

func newOriginFixture(t *testing.T) *originFixture {
	t.Helper()
	f := newFoldFixture(t)
	bare := filepath.Join(t.TempDir(), "origin.git")
	repo, err := gogit.PlainInit(bare, true)
	require.NoError(t, err)
	f.r = repoOn(t, repo.Storer)
	return &originFixture{foldFixture: f, bare: bare, repo: repo}
}

func (o *originFixture) setBranch(name string, c *object.Commit) {
	o.t.Helper()
	require.NoError(o.t, o.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), c.Hash)))
}

// clone creates a fresh Service and runs InitFromRemote against the origin.
// signer is this instance's own key (nil = a store with no signer); root is
// its root of trust.
func (o *originFixture) clone(agentBranch string, signer ssh.Signer, root RootOfTrust) *Service {
	o.t.Helper()
	svc, err := Open(filepath.Join(o.t.TempDir(), "clone.db"))
	require.NoError(o.t, err)
	o.t.Cleanup(func() { _ = svc.Close() })
	if signer != nil {
		svc.SetSigner(signer)
	}
	if root != nil {
		svc.SetRootOfTrust(root)
	}
	_, empty, err := svc.InitFromRemote(fileuri.New(o.bare), nil, "main", agentBranch, nil, nil)
	require.NoError(o.t, err)
	require.False(o.t, empty)
	return svc
}

// stalledHistory is: root → operator enable(enforce, a, b) → good(a) →
// unsigned "off" relaxation → garbage(stranger). Every enforcing instance holds
// at good.
func (o *originFixture) stalledHistory() (enable, good, relax, garbage *object.Commit) {
	root := o.commit(o.baseFiles, nil)
	files := with(o.baseFiles, o.ontPath, o.ont(VerifyEnforce, o.a, o.b))
	enable = o.commit(files, o.op, root)
	good = o.commit(with(files, "kb/good.md", "g"), o.a, enable)
	relaxed := with(files, "kb/good.md", "g", o.ontPath, o.ont(VerifyOff))
	relax = o.commit(relaxed, nil, good)
	garbage = o.commit(with(relaxed, "kb/garbage.md", "x"), o.stranger, relax)
	o.setBranch("main", garbage)
	return
}

// TestFirstContact_CloneDuringAStall: proposal test 21 (V3-2, V4-3). A fresh
// clone lands its upstream AND its agent branch on the same anchor a
// long-running instance holds, and never reaches the garbage.
func TestFirstContact_CloneDuringAStall(t *testing.T) {
	o := newOriginFixture(t)
	_, good, _, garbage := o.stalledHistory()
	me := namedSigner(t, "me")

	svc := o.clone("agent/me-00000000", me, o.root)
	require.Equal(t, good.Hash, mustHeadHash(t, svc, "main"), "the upstream lands at the anchor, not origin's tip")
	require.Equal(t, good.Hash, mustHeadHash(t, svc, "agent/me-00000000"), "the agent branch bootstraps from the anchor")
	ref, err := svc.rh.gits.Reference(verifiedRefName("main"))
	require.NoError(t, err)
	require.Equal(t, good.Hash, ref.Hash())

	// The same anchor a long-running instance computes by folding from the root.
	long, err := (&verifier{st: o.repo.Storer, root: o.root}).fold(plumbing.ZeroHash, verifyContext{Mode: VerifyOff}, garbage.Hash)
	require.NoError(t, err)
	require.Equal(t, long.NewAnchor, ref.Hash(), "every instance of a repository reaches the same anchor")
	ok, err := isAncestorCommit(svc.rh, garbage.Hash, mustHeadHash(t, svc, "agent/me-00000000"))
	require.NoError(t, err)
	require.False(t, ok)
}

// TestFirstContact_UnrootedCloneIsClosed: proposal test 28 (V4-2). With no
// operator key, a fresh clone stops at the first-parent predecessor of the
// first enable.
func TestFirstContact_UnrootedCloneIsClosed(t *testing.T) {
	o := newOriginFixture(t)
	enable, _, _, _ := o.stalledHistory()
	svc := o.clone("agent/me-00000000", namedSigner(t, "me"), nil)
	require.Equal(t, enable.ParentHashes[0], mustHeadHash(t, svc, "main"),
		"unrooted: the upstream stops before the enable it cannot judge")
}

// TestFirstContact_OwnKeyIsNeverTheRoot: plan delta 2. An instance whose own
// key IS the operator's, but with no [verify].operator_key configured, is
// unrooted: the own key never stands in for the root.
func TestFirstContact_OwnKeyIsNeverTheRoot(t *testing.T) {
	o := newOriginFixture(t)
	enable, _, _, _ := o.stalledHistory()
	svc := o.clone("agent/me-00000000", o.op, nil)
	require.Equal(t, enable.ParentHashes[0], mustHeadHash(t, svc, "main"))
}

// TestFirstContact_OffRepoClonesAsBefore: default off. The clone lands on
// origin's tip and no anchor ref is written.
func TestFirstContact_OffRepoClonesAsBefore(t *testing.T) {
	o := newOriginFixture(t)
	root := o.commit(o.baseFiles, nil)
	tip := o.commit(with(o.baseFiles, "kb/x.md", "x"), o.stranger, root)
	o.setBranch("main", tip)
	svc := o.clone("agent/me-00000000", namedSigner(t, "me"), o.root)
	require.Equal(t, tip.Hash, mustHeadHash(t, svc, "main"))
	_, err := svc.rh.gits.Reference(verifiedRefName("main"))
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
}

// agentBranchFor names an agent branch after a key, the way identity.go does.
func agentBranchFor(t *testing.T, s ssh.Signer) string {
	t.Helper()
	fp, err := keyFingerprint(s.PublicKey())
	require.NoError(t, err)
	return "agent/host-" + fp[:8]
}

// TestE4 covers proposal tests 3, 6b and 13: origin's copy of this
// instance's agent branch is adopted only when its own commits are signed by
// THIS instance's key, in every mode (the repo here is off).
func TestE4(t *testing.T) {
	cases := map[string]struct {
		signer   func(o *originFixture, me ssh.Signer) ssh.Signer // who signed the agent branch's own commit
		noOwnKey bool
		adopt    bool
	}{
		"own key: adopted":                         {signer: func(o *originFixture, me ssh.Signer) ssh.Signer { return me }, adopt: true},
		"unsigned forgery: refused (test 13, off)": {signer: func(*originFixture, ssh.Signer) ssh.Signer { return nil }},
		"another admitted key: refused (test 3)":   {signer: func(o *originFixture, _ ssh.Signer) ssh.Signer { return o.a }},
		"no signer on the store: refused":          {signer: func(o *originFixture, me ssh.Signer) ssh.Signer { return me }, noOwnKey: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := newOriginFixture(t)
			me := namedSigner(t, "me")
			branch := agentBranchFor(t, me)
			root := o.commit(o.baseFiles, nil)
			mainTip := o.commit(with(o.baseFiles, "kb/m.md", "m"), o.a, root)
			o.setBranch("main", mainTip)
			own := o.commit(with(o.baseFiles, "kb/m.md", "m", "kb/mine.md", "mine"), c.signer(o, me), mainTip)
			o.setBranch(branch, own)

			var s ssh.Signer = me
			if c.noOwnKey {
				s = nil
			}
			svc := o.clone(branch, s, o.root)
			got := mustHeadHash(t, svc, branch)
			if c.adopt {
				require.Equal(t, own.Hash, got, "the own-signed branch must be adopted")
			} else {
				require.Equal(t, mainTip.Hash, got, "a foreign lineage must not be adopted; bootstrap from the verified upstream")
			}
		})
	}
}

// TestE4_OldCommitReplayedUnderMyName: proposal test 6b. An old commit of
// agent a, presented as origin's copy of MY branch, is refused (not signed by
// me), even though it once verified.
func TestE4_OldCommitReplayedUnderMyName(t *testing.T) {
	o := newOriginFixture(t)
	me := namedSigner(t, "me")
	root := o.commit(o.baseFiles, nil)
	old := o.commit(with(o.baseFiles, "kb/old.md", "o"), o.a, root)
	mainTip := o.commit(with(o.baseFiles, "kb/m.md", "m"), o.a, root)
	o.setBranch("main", mainTip)
	o.setBranch(agentBranchFor(t, me), old)
	svc := o.clone(agentBranchFor(t, me), me, o.root)
	require.Equal(t, mainTip.Hash, mustHeadHash(t, svc, agentBranchFor(t, me)))
}

// TestFirstContact_Subscription: InitSubscription places the upstream at the
// verified point too.
func TestFirstContact_Subscription(t *testing.T) {
	o := newOriginFixture(t)
	_, good, _, _ := o.stalledHistory()
	svc, err := Open(filepath.Join(t.TempDir(), "sub.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetRootOfTrust(o.root)
	_, err = svc.InitSubscription(fileuri.New(o.bare), nil, "main", nil)
	require.NoError(t, err)
	require.Equal(t, good.Hash, mustHeadHash(t, svc, "main"))
}

// TestNewStaticRoot: the operator key parses as an ssh-ed25519
// authorized-key line; empty is unrooted; anything else is an error (boot
// refuses).
func TestNewStaticRoot(t *testing.T) {
	s := namedSigner(t, "op")
	r, err := NewStaticRoot(keyLine(s))
	require.NoError(t, err)
	require.True(t, r.Configured())
	fp, _ := keyFingerprint(s.PublicKey())
	require.Equal(t, fp, r.Fingerprint)

	empty, err := NewStaticRoot("  ")
	require.NoError(t, err)
	require.False(t, empty.Configured())

	_, err = NewStaticRoot("not a key")
	require.Error(t, err)
	_, err = NewStaticRoot("ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQC7 x")
	require.Error(t, err)
	require.False(t, strings.Contains(r.Fingerprint, " "))
}
