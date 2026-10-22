package store

import (
	"bytes"
	"testing"

	"knomit/internal/store/verify"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
)

// #533 re-review B1, probe made a test: a criss-cross candidate. PR 1 adds
// K0 (x.md INVALID), K2 and K1 (children of K0: one fixes x.md, the other
// keeps it invalid and adds y.md), merged as P with x.md valid: admitted and
// fast-forwarded onto main. PR 2 is T = merge(K2, K1) resolving x.md back to
// the INVALID version. T and main have TWO merge bases, K1 and K2; git's
// merge builds a virtual base from both, under which x.md lands INVALID on
// main. The net change must therefore skip a fact only when it equals its
// content at EVERY base: T's x.md differs from K2's, so it is judged and the
// candidate is refused.
//
// Two rows, one per committer-date order of K1 and K2 (go-git answers merge
// bases newest first, and the pusher sets the dates), so judging against
// any ONE base is red in one of them.
//
// SABOTAGE: compare with bases[0] only (the newest) -> the "invalid base
// newer" row is admitted -> red; compare with the oldest base only -> the
// "invalid base older" row is admitted -> red.
func TestValidation_ACrissCrossCannotSmuggleAnInvalidFact(t *testing.T) {
	for _, invalidNewer := range []bool{true, false} {
		name := map[bool]string{true: "invalid base newer", false: "invalid base older"}[invalidNewer]
		t.Run(name, func(t *testing.T) {
			f := newRangeFixture(t)
			base := map[string]string{fact.OntologyFile: validatedOntology("")}
			R := f.commit(f.kb, base, "", nil)
			f.ref(f.kb, "main", R)
			bad, good, y := note("x"), note("x", "e"), note("y", "e")
			K0 := f.commit(f.kb, with(base, "kb/notes/x.md", bad), "agent-x", nil, R)
			// The fixture dates commits in creation order.
			var K1, K2 *object.Commit
			mkK1 := func() *object.Commit {
				return f.commit(f.kb, with(base, "kb/notes/x.md", bad, "kb/notes/y.md", y), "agent-x", nil, K0)
			}
			mkK2 := func() *object.Commit { return f.commit(f.kb, with(base, "kb/notes/x.md", good), "agent-x", nil, K0) }
			if invalidNewer {
				K2 = mkK2()
				K1 = mkK1()
			} else {
				K1 = mkK1()
				K2 = mkK2()
			}
			P := f.commit(f.kb, with(base, "kb/notes/x.md", good, "kb/notes/y.md", y), "agent-x", nil, K1, K2)
			f.ref(f.kb, "cand", P)
			v, err := verify.CheckRange(verify.RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: "main"})
			require.NoError(t, err)
			require.Equal(t, 0, v.ExitCode(), "PR 1 is valid: %+v", v.Refused)
			f.ref(f.kb, "main", P)

			T := f.commit(f.kb, with(base, "kb/notes/x.md", bad, "kb/notes/y.md", y), "agent-x", nil, K2, K1)
			bases, err := T.MergeBase(P)
			require.NoError(t, err)
			require.Len(t, bases, 2, "fixture: a criss-cross")
			f.ref(f.kb, "cand", T)
			v, err = verify.CheckRange(verify.RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: "main"})
			require.NoError(t, err)
			require.Equal(t, 1, v.ExitCode(), "PR 2 must be refused: %+v", v.Refused)
			require.Equal(t, verify.RuleValidation, v.Refused[0].Rule)
			require.Contains(t, v.Refused[0].Reason, "kb/notes/x.md")
		})
	}
}

// #533 re-review S3, probe made a test: receive-pack judges a tip the host
// already holds. A fixed branch is admitted; the host then holds its invalid
// intermediate. A force-push moving the ref back onto that held commit is
// refused, and the ref does not move.
//
// SABOTAGE: return "" from incomingValid for a held tip (the previous
// behaviour) -> the ref moves onto the invalid commit -> red.
func TestValidation_APushOntoAHeldInvalidIntermediateIsRefused(t *testing.T) {
	h := newPushHostWith(t, map[string]string{fact.OntologyFile: validatedOntology("")}, 0)
	p := newPeer(t, h.srv.URL)
	bad := p.commit(p.head(), nil, "kb/notes/n.md", note("n"))
	fixed := p.commit(bad, nil, "kb/notes/n.md", note("n", "e"))
	p.setBranch("work", fixed)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch))
	p.setBranch("work", bad)
	err := p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch)
	require.Equal(t, verify.RuleValidation, pushRefusal(t, err))
	require.Equal(t, fixed.Hash, h.ref(peerBranch), "the ref stays at the admitted tip")
}

// #533 re-review S2: introducedBy names a NON-MERGE commit that wrote the
// content when there is one, never the merge (a merge cannot be dropped). A
// writes x.md invalid, B fixes it, C (a side of B) changes it again, and
// the merge M of B and C resolves x.md back to A's invalid content, which
// neither of its parents holds: both A and M "wrote" it. The refusal names A.
//
// SABOTAGE: drop the non-merge preference -> M (the latest) is named -> red.
func TestValidation_TheRefusalNamesANonMergeWriter(t *testing.T) {
	f := newRangeFixture(t)
	base := map[string]string{fact.OntologyFile: validatedOntology("")}
	R := f.commit(f.kb, base, "", nil)
	f.ref(f.kb, "main", R)
	bad := note("x")
	A := f.commit(f.kb, with(base, "kb/notes/x.md", bad), "agent-x", nil, R)
	B := f.commit(f.kb, with(base, "kb/notes/x.md", note("x", "e")), "agent-x", nil, A)
	C := f.commit(f.kb, with(base, "kb/notes/x.md", note("x", "c")), "agent-x", nil, B)
	M := f.commit(f.kb, with(base, "kb/notes/x.md", bad), "agent-x", nil, B, C)
	f.ref(f.kb, "cand", M)
	v, err := verify.CheckRange(verify.RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: "main"})
	require.NoError(t, err)
	require.Equal(t, verify.RuleValidation, v.Refused[0].Rule)
	require.Equal(t, A.Hash.String(), v.Refused[0].Commit)
}

// #533 re-review N1: introducedBy names the LATEST writer by topology, not
// by committer date (which the author sets). x.md is written invalid by A,
// fixed by B, and written with the same invalid blob again by C; A is dated
// AFTER C. The refusal names C, the commit a Drop must remove.
//
// SABOTAGE: pick the writer with the newest committer date -> A is named
// -> red; pick the earliest by topology -> A is named -> red.
func TestValidation_TheRefusalNamesTheLatestWriterByTopology(t *testing.T) {
	f := newRangeFixture(t)
	base := map[string]string{fact.OntologyFile: validatedOntology("")}
	R := f.commit(f.kb, base, "", nil)
	f.ref(f.kb, "main", R)
	bad, good := note("x"), note("x", "e")
	// The fixture dates commits in creation order (by f.n); A is C's
	// ancestor, so it is created first and dated far ahead instead.
	f.n += 1000
	A := f.commit(f.kb, with(base, "kb/notes/x.md", bad), "agent-x", nil, R)
	f.n -= 2000
	B := f.commit(f.kb, with(base, "kb/notes/x.md", good), "agent-x", nil, A)
	C := f.commit(f.kb, with(base, "kb/notes/x.md", bad), "agent-x", nil, B)
	require.True(t, A.Committer.When.After(C.Committer.When), "fixture: A is dated after C")
	f.ref(f.kb, "cand", C)
	v, err := verify.CheckRange(verify.RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: "main"})
	require.NoError(t, err)
	require.Equal(t, verify.RuleValidation, v.Refused[0].Rule)
	require.Equal(t, C.Hash.String(), v.Refused[0].Commit)
}
