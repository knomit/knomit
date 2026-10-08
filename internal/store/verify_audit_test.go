package store

import (
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// A1: the audit resolves signers against every VERSION of the member records.
// A commit signed with an agent's OLD key (since rotated away) is attributed
// to that agent, not flagged; a signer that never had a record is flagged;
// --key lists everything that key signed. The acceptance gate, in contrast,
// admits the current key only (TestCheckRange_RotationCurrentKeyOnly).
func TestAudit_ResolvesAgainstRecordHistory(t *testing.T) {
	f := newRangeFixture(t)
	old, cur, stranger := namedSigner(t, "old"), namedSigner(t, "new"), namedSigner(t, "stranger")
	f.setFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, old.PublicKey())})
	f.setFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, cur.PublicKey())})

	base := map[string]string{fact.OntologyFile: kbOntology("")}
	root := f.commit(f.kb, base, "agent-x", old)
	a := f.commit(f.kb, with(base, "kb/notes/a.md", "a"), "agent-x", old, root)
	b := f.commit(f.kb, with(base, "kb/notes/a.md", "a", "kb/notes/b.md", "b"), "agent-x", cur, a)
	s := f.commit(f.kb, with(base, "kb/notes/a.md", "a", "kb/notes/b.md", "b", "kb/notes/s.md", "s"), "agent-x", stranger, b)
	f.ref(f.kb, "main", s)

	oldFP, err := keyFingerprint(old.PublicKey())
	require.NoError(t, err)
	rep, err := Audit(AuditInput{KBDir: f.kbDir, Branch: "main", FleetDir: f.fleetDir, FleetRev: "main", Key: oldFP[:12]})
	require.NoError(t, err)
	require.Equal(t, 4, rep.Commits)
	require.Len(t, rep.Findings, 1, "%+v", rep.Findings)
	require.Equal(t, AuditNeverMember, rep.Findings[0].Rule)
	require.Equal(t, s.Hash.String(), rep.Findings[0].Commit)
	require.ElementsMatch(t, []string{root.Hash.String(), a.Hash.String()}, rep.KeySigned)
	require.Equal(t, 1, rep.ExitCode())
}

// A2: a key held by an agent whose record was revoked is flagged; one that
// belonged to another agent than the author claims is flagged.
func TestAudit_FlagsRevokedAndOtherAgent(t *testing.T) {
	f := newRangeFixture(t)
	x, y := namedSigner(t, "x"), namedSigner(t, "y")
	f.setFleet(map[string]string{
		"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey()),
		"agent-y/1.md": memberFile("agent-y", fact.MemberActive, y.PublicKey()),
	})
	f.setFleet(map[string]string{
		"agent-x/1.md": memberFile("agent-x", fact.MemberRevoked, x.PublicKey()),
		"agent-y/1.md": memberFile("agent-y", fact.MemberActive, y.PublicKey()),
	})
	base := map[string]string{fact.OntologyFile: kbOntology("")}
	c1 := f.commit(f.kb, base, "agent-x", x)
	c2 := f.commit(f.kb, with(base, "kb/notes/a.md", "a"), "agent-x", y, c1)
	f.ref(f.kb, "main", c2)
	rep, err := Audit(AuditInput{KBDir: f.kbDir, Branch: "main", FleetDir: f.fleetDir, FleetRev: "main"})
	require.NoError(t, err)
	rules := map[string]string{}
	for _, r := range rep.Findings {
		rules[r.Commit] = r.Rule
	}
	require.Equal(t, map[string]string{c1.Hash.String(): AuditRevoked, c2.Hash.String(): AuditOtherAgent}, rules)
}

// A symlinked fleet ontology is the named error (fact.ErrSymlinkNotFollowed),
// as at LoadFleet and FleetMembersAt, never "not a fleet": the symlink's link
// TEXT is the real fleet preset, so a reader that parsed it would audit
// cleanly, and one that folded the error into ErrNotFleet would lie about the
// data. Sabotage: fold the read error back into ErrNotFleet → red.
func TestAudit_SymlinkedFleetOntologyIsNamedNotNotAFleet(t *testing.T) {
	f := newRangeFixture(t)
	x := namedSigner(t, "x")
	r := repoOn(t, f.fleet.Storer)
	encode := func(entries []object.TreeEntry) plumbing.Hash {
		o := f.fleet.Storer.NewEncodedObject()
		require.NoError(t, (&object.Tree{Entries: entries}).Encode(o))
		h, err := f.fleet.Storer.SetEncodedObject(o)
		require.NoError(t, err)
		return h
	}
	knomit := encode([]object.TreeEntry{{Name: "ontology.yaml", Mode: filemode.Symlink, Hash: r.blob(fleetOntologyYAML(t))}})
	kb := r.tree(map[string]string{"members/agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())})
	root := encode([]object.TreeEntry{{Name: ".knomit", Mode: filemode.Dir, Hash: knomit}, {Name: "kb", Mode: filemode.Dir, Hash: kb}})
	when := time.Unix(1790000000, 0).UTC()
	c := &object.Commit{Author: object.Signature{Name: "t", Email: "t@t", When: when},
		Committer: object.Signature{Name: "t", Email: "t@t", When: when}, Message: "symlinked ontology", TreeHash: root}
	o := f.fleet.Storer.NewEncodedObject()
	require.NoError(t, c.Encode(o))
	h, err := f.fleet.Storer.SetEncodedObject(o)
	require.NoError(t, err)
	require.NoError(t, f.fleet.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), h)))

	kc := f.commit(f.kb, map[string]string{fact.OntologyFile: kbOntology("")}, "agent-x", x)
	f.ref(f.kb, "main", kc)
	_, err = Audit(AuditInput{KBDir: f.kbDir, Branch: "main", FleetDir: f.fleetDir, FleetRev: "main"})
	require.ErrorIs(t, err, fact.ErrSymlinkNotFollowed)
	require.NotErrorIs(t, err, ErrNotFleet)
	require.Contains(t, err.Error(), fact.OntologyFile+" is a symlink")
}

// A0: a branch whose every commit is signed by a member key is clean (exit 0).
func TestAudit_Clean(t *testing.T) {
	f := newRangeFixture(t)
	x := namedSigner(t, "x")
	f.setFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberLeft, x.PublicKey())})
	c := f.commit(f.kb, map[string]string{fact.OntologyFile: kbOntology("")}, "agent-x", x)
	f.ref(f.kb, "main", c)
	rep, err := Audit(AuditInput{KBDir: f.kbDir, Branch: "main", FleetDir: f.fleetDir, FleetRev: "main"})
	require.NoError(t, err)
	require.Empty(t, rep.Findings, "a LEFT agent's past commit is attributed, not flagged")
	require.Equal(t, 0, rep.ExitCode())
}
