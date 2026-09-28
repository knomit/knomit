package store

import (
	"sort"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// refusedSet is the full hashes a ForeignLineageError names, sorted, so a test
// asserts WHICH commits E4 refused, not merely that it refused something.
func refusedSet(t *testing.T, err error) []string {
	t.Helper()
	var fl *ForeignLineageError
	require.ErrorAs(t, err, &fl, "a foreign lineage must FAIL the create")
	out := append([]string(nil), fl.Commits...)
	sort.Strings(out)
	return out
}

func hashes(cs ...*object.Commit) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Hash.String())
	}
	sort.Strings(out)
	return out
}

// TestE4_TransitiveTrust (F11 UI merge, user ruling D4 "chain of trust"): a
// commit this instance did not sign is adopted when an OWN-signed merge
// commit brought it in — it is reachable from that merge's NON-FIRST parent.
// Nothing else is vouched: not the first-parent chain of an own merge, not the
// ancestors of an own non-merge commit, not what a foreign or unsigned merge
// brought in.
func TestE4_TransitiveTrust(t *testing.T) {
	branch := "agent/host-me"

	// setup builds root ← main on origin, signed by the fleet ("other").
	// base is main's file set; every other commit's files are spelled out
	// from it, so each tree says exactly what that commit holds.
	setup := func(t *testing.T) (*e4Origin, *object.Commit, map[string]string) {
		o := newE4Origin(t)
		root := o.commit(o.files, nil)
		base := with(o.files, "kb/m.md", "m")
		mainTip := o.commit(base, namedSigner(t, "fleet"), root)
		o.setBranch("main", mainTip)
		return o, mainTip, base
	}

	t.Run("peer history brought in by one own merge: adopted", func(t *testing.T) {
		o, mainTip, base := setup(t)
		me, peer := namedSigner(t, "me"), namedSigner(t, "peer")
		// The peer's history: two commits, a side commit, and the peer's own
		// merge of the two — at least two peer commits plus a peer merge.
		p1 := o.commit(with(base, "kb/p1.md", "p1"), peer, mainTip)
		p2 := o.commit(with(base, "kb/p1.md", "p1", "kb/p2.md", "p2"), peer, p1)
		p3 := o.commit(with(base, "kb/p3.md", "p3"), peer, mainTip)
		pm := o.commit(with(base, "kb/p1.md", "p1", "kb/p2.md", "p2", "kb/p3.md", "p3"), peer, p2, p3)
		// This instance's own work, then its merge of the peer's head.
		own := o.commit(with(base, "kb/mine.md", "mine"), me, mainTip)
		merged := o.commit(with(base, "kb/p1.md", "p1", "kb/p2.md", "p2", "kb/p3.md", "p3", "kb/mine.md", "mine"), me, own, pm)
		o.setBranch(branch, merged)

		svc, err := o.cloneErr(branch, me, nil)
		require.NoError(t, err, "every peer commit is reachable from the own merge's second parent")
		ref, rerr := svc.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
		require.NoError(t, rerr)
		require.Equal(t, merged.Hash, ref.Hash())
	})

	t.Run("peer commit set directly as the tip: refused", func(t *testing.T) {
		o, mainTip, base := setup(t)
		me, peer := namedSigner(t, "me"), namedSigner(t, "peer")
		p := o.commit(with(base, "kb/p.md", "p"), peer, mainTip)
		o.setBranch(branch, p)

		_, err := o.cloneErr(branch, me, nil)
		require.Equal(t, hashes(p), refusedSet(t, err))
	})

	t.Run("foreign commit under an own NON-merge commit: refused", func(t *testing.T) {
		o, mainTip, base := setup(t)
		me, peer := namedSigner(t, "me"), namedSigner(t, "peer")
		p := o.commit(with(base, "kb/p.md", "p"), peer, mainTip)
		own := o.commit(with(base, "kb/p.md", "p", "kb/mine.md", "mine"), me, p)
		o.setBranch(branch, own)

		_, err := o.cloneErr(branch, me, nil)
		require.Equal(t, hashes(p), refusedSet(t, err),
			"an own-signed commit vouches for nothing below it unless it is a merge and the commit came in through a non-first parent")
	})

	t.Run("foreign commit under the FIRST parent of an own merge: refused", func(t *testing.T) {
		o, mainTip, base := setup(t)
		me, peer, stranger := namedSigner(t, "me"), namedSigner(t, "peer"), namedSigner(t, "stranger")
		// First-parent side: a stranger's commit, then own work on top of it.
		x := o.commit(with(base, "kb/x.md", "x"), stranger, mainTip)
		own := o.commit(with(base, "kb/x.md", "x", "kb/mine.md", "mine"), me, x)
		// Second-parent side: a legitimate peer chain.
		p := o.commit(with(base, "kb/p.md", "p"), peer, mainTip)
		merged := o.commit(with(base, "kb/mine.md", "mine", "kb/p.md", "p"), me, own, p)
		o.setBranch(branch, merged)

		_, err := o.cloneErr(branch, me, nil)
		require.Equal(t, hashes(x), refusedSet(t, err),
			"only the first-parent foreigner is refused; the merged-in peer commit is vouched")
	})

	t.Run("merge signed by another key vouches for nothing: refused", func(t *testing.T) {
		o, mainTip, base := setup(t)
		me, peer, other := namedSigner(t, "me"), namedSigner(t, "peer"), namedSigner(t, "other")
		own := o.commit(with(base, "kb/mine.md", "mine"), me, mainTip)
		p := o.commit(with(base, "kb/p.md", "p"), peer, mainTip)
		m := o.commit(with(base, "kb/mine.md", "mine", "kb/p.md", "p"), other, own, p)
		o.setBranch(branch, m)

		_, err := o.cloneErr(branch, me, nil)
		require.Equal(t, hashes(p, m), refusedSet(t, err))
	})

	t.Run("unsigned merge vouches for nothing: refused", func(t *testing.T) {
		o, mainTip, base := setup(t)
		me, peer := namedSigner(t, "me"), namedSigner(t, "peer")
		own := o.commit(with(base, "kb/mine.md", "mine"), me, mainTip)
		p := o.commit(with(base, "kb/p.md", "p"), peer, mainTip)
		m := o.commit(with(base, "kb/mine.md", "mine", "kb/p.md", "p"), nil, own, p)
		o.setBranch(branch, m)

		_, err := o.cloneErr(branch, me, nil)
		require.Equal(t, hashes(p, m), refusedSet(t, err))
	})
}
