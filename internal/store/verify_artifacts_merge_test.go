package store

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// A merge that brings a pki leaf ANOTHER parent already holds adds nothing:
// main issued gamma's leaf and then revoked it, and agent-x's branch forked
// before the leaf was issued. agent-x's clean merge of that main (and the
// master's merge of agent-x's branch, the same shape) is admitted: against
// its own side the leaf is "added", but it is the very blob main holds, and
// it was judged when main took it. A merge that adds a revoked leaf neither
// parent holds is still artifact-pki. (The master's automatic revoke of a
// member that left, #532, met this: its merge of the leaver's branch was
// refused by the judge of its own commits.)
//
// SABOTAGE: drop the inAnotherParent skip from artifactCheck.change -> the
// clean merge of main is artifact-pki (gamma's leaf is revoked in the
// merge's CRL) -> red; skip on a blob found in ANY tree including the merge's
// own (compare only to the commit tree) -> the forged-leaf merge is admitted
// -> red.
func TestArtifacts_AMergeBringingALeafAParentHoldsAddsNothing(t *testing.T) {
	p := newTestFleetPKI(t)
	boss, x, g := namedSigner(t, "boss"), namedSigner(t, "x"), namedSigner(t, "gamma")
	bl, _ := p.leaf("boss", boss)
	xl, _ := p.leaf("agent-x", x)
	arts := p.artifacts(p.crl(), map[string]string{"boss": bl, "agent-x": xl})
	arts["kb/members/agent-x/1.md"] = memberFile("agent-x", fact.MemberActive, x.PublicKey())
	f, main, base := selfRegFixture(t, arts)
	l := fleetLayout(t)
	const own = "kb/members/agent-x/1.md"

	// agent-x's branch, forked at main.
	side := f.commit(f.fleet, with(base, own, base[own]+"\nv2\n"), "agent-x", x, main)

	// main: boss issues gamma's leaf, then revokes it.
	gl, gs := p.leaf("gamma", g)
	issued := with(base, l.LeafPath("gamma"), gl)
	main2 := f.commit(f.fleet, issued, "boss", boss, main)
	revokedCRL := p.crl(gs)
	revoked := with(issued, l.CRL, revokedCRL)
	main3 := f.commit(f.fleet, revoked, "boss", boss, main2)
	f.ref(f.fleet, "main", main3)

	// The clean merge of that main.
	merged := with(revoked, own, base[own]+"\nv2\n")
	r, ok := f.judgeEntry(f.commit(f.fleet, merged, "agent-x", x, side, main3))
	require.True(t, ok, "the leaf main holds is not added by the merge: %+v", r)

	// A merge that puts a revoked leaf neither parent holds at gamma's path.
	other, _ := p.leaf("gamma", g)
	forged := with(merged, l.LeafPath("gamma"), other)
	r, ok = f.judgeEntry(f.commit(f.fleet, forged, "agent-x", x, side, main3))
	require.False(t, ok, "a leaf no parent holds is judged")
	require.Contains(t, []string{RuleArtifactPKI, RuleArtifactReplace}, r.Rule, r.Reason)
}
