package store

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
)

// ownerFixture is the fleet repository judging itself (enforce, PKI) with two
// active, certified members, "boss" and "agent-x", on main. It returns the
// fixture, main and main's files.
func ownerFixture(t *testing.T) (*rangeFixture, *object.Commit, map[string]string, ssh.Signer, ssh.Signer) {
	t.Helper()
	p := newTestFleetPKI(t)
	boss, x := namedSigner(t, "boss"), namedSigner(t, "x")
	bl, _ := p.leaf("boss", boss)
	xl, _ := p.leaf("agent-x", x)
	arts := p.artifacts(p.crl(), map[string]string{"boss": bl, "agent-x": xl})
	arts["kb/members/agent-x/1.md"] = memberFile("agent-x", fact.MemberActive, x.PublicKey())
	f, main, base := selfRegFixture(t, arts)
	return f, main, base, boss, x
}

// without is m minus the given paths.
func without(m map[string]string, paths ...string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	for _, p := range paths {
		delete(out, p)
	}
	return out
}

// The record-owner rule: on the fleet repository a certified member's commit
// may change, outside artifacts/, only paths under members/<its own id>/, and
// may delete nothing there. Every row is signed by agent-x, an active member
// with a valid leaf, so the record rules and rule B admit every row: only the
// owner rule can refuse. The row count is asserted.
//
// SABOTAGE: make recordOwner return ok unconditionally → every refused row is
// admitted → red; drop the delete check → "deletes its record" admitted → red.
func TestRecordOwner_OnlyTheOwnFolderAndNoDelete(t *testing.T) {
	const own = "kb/members/agent-x/1.md"
	rows := []struct {
		name   string
		files  func(base map[string]string) map[string]string
		want   string // "" = admitted
		reason string
	}{
		{"edits its own record", func(b map[string]string) map[string]string { return with(b, own, b[own]+"\nv2\n") }, "", ""},
		{"adds a second file in its own folder", func(b map[string]string) map[string]string { return with(b, "kb/members/agent-x/notes.md", "n") }, "", ""},
		{"edits another member's record", func(b map[string]string) map[string]string {
			return with(b, "kb/members/boss/1.md", "x")
		}, RuleNotOwner, "kb/members/boss/1.md"},
		{"writes two folders", func(b map[string]string) map[string]string {
			return with(b, own, b[own]+"\nv2\n", "kb/members/boss/2.md", "x")
		}, RuleNotOwner, "kb/members/boss/2.md"},
		{"writes outside members", func(b map[string]string) map[string]string { return with(b, "kb/notes/1.md", "x") }, RuleNotOwner, "kb/notes/1.md"},
		{"writes the README", func(b map[string]string) map[string]string { return with(b, "README.md", "x") }, RuleNotOwner, "README.md"},
		{"deletes its record", func(b map[string]string) map[string]string { return without(b, own) }, RuleNotOwner, "deletes " + own},
	}
	ran := 0
	for _, row := range rows {
		f, main, base, _, x := ownerFixture(t)
		f.ref(f.fleet, "cand", f.commit(f.fleet, row.files(base), "agent-x", x, main))
		v := f.checkSelf("")
		require.Equal(t, 1, v.Checked, row.name)
		if row.want == "" {
			require.Empty(t, v.Refused, row.name)
		} else {
			r := onlyRefusal(t, v)
			require.Equal(t, row.want, r.Rule, row.name)
			require.Contains(t, r.Reason, row.reason, row.name)
		}
		ran++
	}
	require.Equal(t, 7, ran)
}

// Artifact paths are not the owner rule's: a member commit that adds a file
// under artifacts/ is judged by the artifact rules, so a leaf the root never
// signed is refused artifact-pki (the artifact rule ran), not not-owner. The
// master's own artifact commits (enroll, revoke) pass the owner rule the same
// way: they change no non-artifact path.
//
// SABOTAGE: drop the underArtifacts exclusion from recordOwner → not-owner →
// red.
func TestRecordOwner_ArtifactPathsAreTheArtifactRules(t *testing.T) {
	f, main, base, _, x := ownerFixture(t)
	f.ref(f.fleet, "cand", f.commit(f.fleet, with(base, "artifacts/fleet/certs/evil.pem", "x"), "agent-x", x, main))
	r := onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, RuleArtifactPKI, r.Rule)
}

// The owner rule is the fleet repository's only: in an ordinary knowledge
// base judged against the fleet, the same member writing another folder is
// admitted (the rule has no members there).
//
// SABOTAGE: apply recordOwner when membersDir is "" → red.
func TestRecordOwner_OnlyInTheFleetRepository(t *testing.T) {
	p := newTestFleetPKI(t)
	x := namedSigner(t, "x")
	xl, _ := p.leaf("agent-x", x)
	f := newRangeFixture(t)
	f.abFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())},
		p.artifacts(p.crl(), map[string]string{"agent-x": xl}))
	base := map[string]string{fact.OntologyFile: kbOntology(VerifyEnforce)}
	root := f.commit(f.kb, base, "", nil)
	f.ref(f.kb, "main", root)
	f.ref(f.kb, "cand", f.commit(f.kb, with(base, "kb/members/boss/1.md", "x"), "agent-x", x, root))
	v, err := f.check(VerifyEnforce)
	require.NoError(t, err)
	require.Equal(t, 1, v.Checked)
	require.Empty(t, v.Refused)
}

// H4: M3 runs for EVERY two-parent merge, signed or not, before the record
// rules. A joiner whose record is not on main yet merges main (which moved:
// boss edited its record) into its branch, signs the merge, and pushes: the
// merge adds nothing beyond its parents, so it is admitted, never no-record.
// Its own registration commit is rule A's. A signed merge that adds a file
// beyond its parents is judged by the rules: by a joiner it is no-record
// (rule A does not admit another folder), by an active member not-owner.
//
// SABOTAGE: run M3 only for unsigned merges (the pre-PR4 judge) → the clean
// signed merge is no-record → red.
func TestM3_EverySignedMergeBeforeTheRecordRules(t *testing.T) {
	p := newTestFleetPKI(t)
	j, boss := namedSigner(t, "joiner"), namedSigner(t, "boss")
	jl, _ := p.leaf("joiner", j)
	bl, _ := p.leaf("boss", boss)
	f, main, base := selfRegFixture(t, p.artifacts(p.crl(), map[string]string{"joiner": jl, "boss": bl}))
	reg := f.commit(f.fleet, with(base, "kb/members/joiner/1.md", memberFile("joiner", fact.MemberActive, j.PublicKey())), "joiner", j, main)
	// main moves: boss edits its own record (unjudged here: it is main).
	bossV2 := with(base, "kb/members/boss/1.md", base["kb/members/boss/1.md"]+"\nv2\n")
	main2 := f.commit(f.fleet, bossV2, "boss", boss, main)
	f.ref(f.fleet, "main", main2)
	merged := with(bossV2, "kb/members/joiner/1.md", memberFile("joiner", fact.MemberActive, j.PublicKey()))

	clean := f.commit(f.fleet, merged, "joiner", j, reg, main2)
	f.ref(f.fleet, "cand", clean)
	v := f.checkSelf("")
	require.Equal(t, 2, v.Checked, "the registration and the merge")
	require.Empty(t, v.Refused, "a signed clean merge by a no-record id is admitted")

	dirty := f.commit(f.fleet, with(merged, "kb/members/boss/9.md", "x"), "joiner", j, reg, main2)
	f.ref(f.fleet, "cand", dirty)
	r := onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, dirty.Hash.String(), r.Commit)
	require.Equal(t, RuleNoRecord, r.Rule, "a merge beyond its parents is judged: rule A refuses another folder")
}

// The same for an active member: its signed clean merge of main is admitted
// even though, against main's side, it brings in another member's change; a
// signed merge adding another member's file is not-owner.
//
// SABOTAGE: run M3 only for unsigned merges → the clean merge is not-owner →
// red.
func TestM3_ActiveMemberMergeOfMain(t *testing.T) {
	f, main, base, boss, x := ownerFixture(t)
	const own = "kb/members/agent-x/1.md"
	side := f.commit(f.fleet, with(base, own, base[own]+"\nv2\n"), "agent-x", x, main)
	bossV2 := with(base, "kb/members/boss/1.md", base["kb/members/boss/1.md"]+"\nv2\n")
	main2 := f.commit(f.fleet, bossV2, "boss", boss, main)
	f.ref(f.fleet, "main", main2)
	merged := with(bossV2, own, base[own]+"\nv2\n")

	f.ref(f.fleet, "cand", f.commit(f.fleet, merged, "agent-x", x, side, main2))
	v := f.checkSelf("")
	require.Equal(t, 2, v.Checked)
	require.Empty(t, v.Refused)

	dirty := f.commit(f.fleet, with(merged, "kb/members/boss/9.md", "x"), "agent-x", x, side, main2)
	f.ref(f.fleet, "cand", dirty)
	r := onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, dirty.Hash.String(), r.Commit)
	require.Equal(t, RuleNotOwner, r.Rule)
}
