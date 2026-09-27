package store

import (
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/platform/fileuri"
)

// e4Origin is a bare git repository on disk that a fresh Service clones from
// over file://, filled with (optionally signed) commits.
type e4Origin struct {
	t     *testing.T
	bare  string
	repo  *gogit.Repository
	n     int
	files map[string]string
}

func newE4Origin(t *testing.T) *e4Origin {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "origin.git")
	repo, err := gogit.PlainInit(bare, true)
	require.NoError(t, err)
	ont, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	return &e4Origin{t: t, bare: bare, repo: repo, files: map[string]string{fact.OntologyFile: string(ont)}}
}

func (o *e4Origin) commit(files map[string]string, signer ssh.Signer, parents ...*object.Commit) *object.Commit {
	o.t.Helper()
	r := repoOn(o.t, o.repo.Storer)
	o.n++
	when := time.Unix(1790000000+int64(o.n), 0).UTC()
	c := &object.Commit{
		Author:    object.Signature{Name: "a", Email: "a@agents.knomit.io", When: when},
		Committer: object.Signature{Name: "a", Email: "a@agents.knomit.io", When: when},
		Message:   "c",
		TreeHash:  r.tree(files),
	}
	for _, p := range parents {
		c.ParentHashes = append(c.ParentHashes, p.Hash)
	}
	if signer != nil {
		c = signed(o.t, c, signer)
	}
	obj := o.repo.Storer.NewEncodedObject()
	require.NoError(o.t, c.Encode(obj))
	h, err := o.repo.Storer.SetEncodedObject(obj)
	require.NoError(o.t, err)
	got, err := object.GetCommit(o.repo.Storer, h)
	require.NoError(o.t, err)
	return got
}

func (o *e4Origin) setBranch(name string, c *object.Commit) {
	o.t.Helper()
	require.NoError(o.t, o.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), c.Hash)))
}

// cloneErr creates a fresh Service and runs InitFromRemote against the
// origin. signer is this instance's key (nil = no signer); ownKeys, when set,
// are its fleet record's historical keys; accept lists commits on this
// instance's accept list.
func (o *e4Origin) cloneErr(agentBranch string, signer ssh.Signer, ownKeys []ssh.PublicKey, accept ...plumbing.Hash) (*Service, error) {
	o.t.Helper()
	svc, err := Open(filepath.Join(o.t.TempDir(), "clone.db"))
	require.NoError(o.t, err)
	o.t.Cleanup(func() { _ = svc.Close() })
	if signer != nil {
		svc.SetSigner(signer)
	}
	if ownKeys != nil {
		svc.SetOwnKeys(func() []ssh.PublicKey { return ownKeys })
	}
	if len(accept) > 0 {
		svc.SetAcceptList(fixedAccepts(accept))
	}
	_, _, err = svc.InitFromRemote(fileuri.New(o.bare), nil, "main", agentBranch, nil, nil)
	return svc, err
}

func requireNoLocalBranches(t *testing.T, svc *Service, names ...string) {
	t.Helper()
	for _, n := range names {
		_, err := svc.rh.gits.Reference(plumbing.NewBranchReferenceName(n))
		require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "a refused create must not create %s", n)
	}
}

// TestE4: origin's copy of this instance's agent branch is adopted only when
// its own commits are signed by THIS instance's key, in every mode (the repo
// here is off). Otherwise the create FAILS with a ForeignLineageError, creates
// no local branch, and leaves the remote alone.
func TestE4(t *testing.T) {
	other := namedSigner(t, "other")
	cases := map[string]struct {
		signer   func(me ssh.Signer) ssh.Signer
		noOwnKey bool
		adopt    bool
	}{
		"own key: adopted":                {signer: func(me ssh.Signer) ssh.Signer { return me }, adopt: true},
		"unsigned forgery: refused":       {signer: func(ssh.Signer) ssh.Signer { return nil }},
		"another key: refused":            {signer: func(ssh.Signer) ssh.Signer { return other }},
		"no signer on the store: refused": {signer: func(me ssh.Signer) ssh.Signer { return me }, noOwnKey: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := newE4Origin(t)
			me := namedSigner(t, "me")
			branch := "agent/host-me"
			root := o.commit(o.files, nil)
			mainTip := o.commit(with(o.files, "kb/m.md", "m"), other, root)
			o.setBranch("main", mainTip)
			own := o.commit(with(o.files, "kb/m.md", "m", "kb/mine.md", "mine"), c.signer(me), mainTip)
			o.setBranch(branch, own)

			var s ssh.Signer = me
			if c.noOwnKey {
				s = nil
			}
			svc, err := o.cloneErr(branch, s, nil)
			if c.adopt {
				require.NoError(t, err)
				ref, rerr := svc.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
				require.NoError(t, rerr)
				require.Equal(t, own.Hash, ref.Hash(), "the own-signed branch must be adopted")
				return
			}
			var fl *ForeignLineageError
			require.ErrorAs(t, err, &fl, "a foreign lineage must FAIL the create, loudly")
			require.ErrorIs(t, err, ErrForeignLineage)
			require.Len(t, fl.Refused, 1)
			require.Contains(t, err.Error(), own.Hash.String()[:8], "the error names the refused commit")
			requireNoLocalBranches(t, svc, "main", branch)
		})
	}
}

// TestE4_PreSigningCommitsAreNotDiscarded: a re-clone must refuse unsigned
// commits ahead of main, keep them, and succeed once they are accepted on
// this instance.
func TestE4_PreSigningCommitsAreNotDiscarded(t *testing.T) {
	o := newE4Origin(t)
	me := namedSigner(t, "me")
	branch := "agent/host-me"
	root := o.commit(o.files, nil)
	o.setBranch("main", root)
	files := o.files
	tip := root
	var unsigned []plumbing.Hash
	for i := 0; i < 4; i++ {
		files = with(files, "kb/pre"+string(rune('0'+i))+".md", "pre-signing")
		tip = o.commit(files, nil, tip)
		unsigned = append(unsigned, tip.Hash)
	}
	o.setBranch(branch, tip)

	svc, err := o.cloneErr(branch, me, nil)
	var fl *ForeignLineageError
	require.ErrorAs(t, err, &fl)
	require.Len(t, fl.Refused, 4, "every refused commit is listed")
	requireNoLocalBranches(t, svc, "main", branch)

	_, err = o.cloneErr(branch, me, nil, unsigned...)
	require.NoError(t, err, "accepted on this instance, the lineage is adopted")
}

// TestE4_OwnRecordKeysAreOwn (F09 PR 5): registered in a fleet, every key this
// instance's member record has held is its own, so a commit signed before a
// key rotation is not foreign; without the record it is.
func TestE4_OwnRecordKeysAreOwn(t *testing.T) {
	o := newE4Origin(t)
	old, cur := namedSigner(t, "old"), namedSigner(t, "current")
	branch := "agent/host-me"
	root := o.commit(o.files, nil)
	o.setBranch("main", root)
	mine := o.commit(with(o.files, "kb/mine.md", "mine"), old, root)
	o.setBranch(branch, mine)

	_, err := o.cloneErr(branch, cur, nil)
	require.ErrorIs(t, err, ErrForeignLineage, "standalone: only the current key is own")

	_, err = o.cloneErr(branch, cur, []ssh.PublicKey{old.PublicKey(), cur.PublicKey()})
	require.NoError(t, err, "the record's earlier key is this instance's own")
}

// fixedAccepts is an accept list holding exactly these commits (control.db's
// list is repos.VerifyAccepts; store sees only the interface).
type fixedAccepts []plumbing.Hash

func (f fixedAccepts) Lookup(h plumbing.Hash) (Accept, bool) {
	for _, x := range f {
		if x == h {
			return Accept{Commit: h.String()}, true
		}
	}
	return Accept{}, false
}
