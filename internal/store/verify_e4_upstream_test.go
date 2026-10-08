package store

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// forgeMerge stores a commit signed the way a forge's web merge is: a GPG
// (not SSH) signature, by a key that is not this instance's.
func (o *e4Origin) forgeMerge(files map[string]string, parents ...*object.Commit) *object.Commit {
	o.t.Helper()
	c := o.commit(files, nil, parents...)
	c.PGPSignature = "-----BEGIN PGP SIGNATURE-----\n\nwsBcBAABCAAQBQJforge\n=abcd\n-----END PGP SIGNATURE-----\n"
	obj := o.repo.Storer.NewEncodedObject()
	require.NoError(o.t, c.Encode(obj))
	h, err := o.repo.Storer.SetEncodedObject(obj)
	require.NoError(o.t, err)
	got, err := object.GetCommit(o.repo.Storer, h)
	require.NoError(o.t, err)
	_, serr := verifyCommitSignature(got)
	require.ErrorIs(o.t, serr, ErrNotSSHSIG, "fixture: the forge merge must carry a non-SSH signature")
	return got
}

// TestE4_TipAlreadyOnUpstream: E4 judges only commits BEYOND origin's main.
// An agent tip that is itself reachable from origin's main (the instance's
// sync fast-forwarded its agent branch onto a forge merge) holds nothing
// beyond main, so the clone adopts it, whoever signed it. A commit genuinely
// beyond main and not own-signed is still refused.
func TestE4_TipAlreadyOnUpstream(t *testing.T) {
	branch := "agent/host-me"
	build := func(t *testing.T) (o *e4Origin, older, mainTip *object.Commit) {
		o = newE4Origin(t)
		me := namedSigner(t, "me")
		root := o.commit(o.files, nil)
		own := o.commit(with(o.files, "kb/mine.md", "mine"), me, root)
		older = o.forgeMerge(with(o.files, "kb/mine.md", "mine"), root, own)
		mainTip = o.forgeMerge(with(o.files, "kb/mine.md", "mine", "kb/b.md", "b"), older)
		o.setBranch("main", mainTip)
		return o, older, mainTip
	}
	adopted := func(t *testing.T, o *e4Origin, want plumbing.Hash) {
		t.Helper()
		svc, err := o.cloneErr(branch, namedSigner(t, "me"), nil)
		require.NoError(t, err, "an agent tip already on origin's main holds nothing beyond it")
		ref, rerr := svc.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
		require.NoError(t, rerr)
		require.Equal(t, want, ref.Hash(), "origin's agent branch is adopted as is")
	}

	t.Run("agent tip == main tip, forge-signed: adopted", func(t *testing.T) {
		o, _, mainTip := build(t)
		o.setBranch(branch, mainTip)
		adopted(t, o, mainTip.Hash)
	})
	t.Run("agent tip behind main's tip, forge-signed: adopted", func(t *testing.T) {
		o, older, _ := build(t)
		o.setBranch(branch, older)
		adopted(t, o, older.Hash)
	})
	t.Run("unsigned commit beyond a forge-merged main: refused, alone", func(t *testing.T) {
		o, _, mainTip := build(t)
		foreign := o.commit(with(o.files, "kb/x.md", "x"), nil, mainTip)
		o.setBranch(branch, foreign)
		svc, err := o.cloneErr(branch, namedSigner(t, "me"), nil)
		var fl *ForeignLineageError
		require.ErrorAs(t, err, &fl)
		require.Equal(t, []string{foreign.Hash.String()}, fl.Commits, "only the commit beyond main is judged")
		requireNoLocalBranches(t, svc, "main", branch)
	})
}

// TestWalkHistory_StartInStop: a start commit in the stop set is reachable
// from stop, so the walk visits nothing.
func TestWalkHistory_StartInStop(t *testing.T) {
	o := newE4Origin(t)
	root := o.commit(o.files, nil)
	tip := o.commit(with(o.files, "kb/a.md", "a"), nil, root)
	n := 0
	require.NoError(t, walkHistory(o.repo.Storer, tip.Hash, map[plumbing.Hash]bool{tip.Hash: true}, func(*object.Commit) { n++ }))
	require.Zero(t, n, "a start in the stop set is visited by nobody")
	require.NoError(t, walkHistory(o.repo.Storer, tip.Hash, map[plumbing.Hash]bool{root.Hash: true}, func(*object.Commit) { n++ }))
	require.Equal(t, 1, n, "a stop below start still lets start be visited")
}
