package store

import (
	"testing"

	"knomit/internal/store/fleetrec"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/domain/fact"
	"knomit/internal/store/verify"
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
		}, verify.RuleNotOwner, "kb/members/boss/1.md"},
		{"writes two folders", func(b map[string]string) map[string]string {
			return with(b, own, b[own]+"\nv2\n", "kb/members/boss/2.md", "x")
		}, verify.RuleNotOwner, "kb/members/boss/2.md"},
		{"writes outside members", func(b map[string]string) map[string]string { return with(b, "kb/notes/1.md", "x") }, verify.RuleNotOwner, "kb/notes/1.md"},
		{"writes the README", func(b map[string]string) map[string]string { return with(b, "README.md", "x") }, verify.RuleNotOwner, "README.md"},
		{"deletes its record", func(b map[string]string) map[string]string { return without(b, own) }, verify.RuleNotOwner, "deletes " + own},
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

// Declared artifact paths are not the owner rule's: a member commit that adds
// a file at a declared path is judged by the artifact rules, so a leaf the
// root never signed is refused artifact-pki (the artifact rule ran), not
// not-owner. Undeclared artifacts/ paths are the owner rule's
// (TestRecordOwner_OnlyDeclaredArtifactPathsAreExempt, which also runs the
// master's enroll and revoke).
//
// SABOTAGE: drop the declaredArtifact exemption from recordOwner → not-owner
// → red.
func TestRecordOwner_ArtifactPathsAreTheArtifactRules(t *testing.T) {
	f, main, base, _, x := ownerFixture(t)
	f.ref(f.fleet, "cand", f.commit(f.fleet, with(base, "artifacts/fleet/certs/evil.pem", "x"), "agent-x", x, main))
	r := onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, verify.RuleArtifactPKI, r.Rule)
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
	base := map[string]string{fact.OntologyFile: kbOntology(verify.VerifyEnforce)}
	root := f.commit(f.kb, base, "", nil)
	f.ref(f.kb, "main", root)
	f.ref(f.kb, "cand", f.commit(f.kb, with(base, "kb/members/boss/1.md", "x"), "agent-x", x, root))
	v, err := f.check(verify.VerifyEnforce)
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
	require.Equal(t, verify.RuleNoRecord, r.Rule, "a merge beyond its parents is judged: rule A refuses another folder")
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
	require.Equal(t, verify.RuleNotOwner, r.Rule)
}

// brokenSig stores a copy of the signed commit c whose message is changed
// after signing: the SSHSIG is still there and still parses, but it no longer
// verifies (ErrBadSignature). Tree and parents are c's.
func brokenSig(t *testing.T, repo *gogit.Repository, c *object.Commit) *object.Commit {
	t.Helper()
	require.NotEmpty(t, c.PGPSignature, "fixture: c is signed")
	cp := *c
	cp.Message += "\ntampered after signing\n"
	o := repo.Storer.NewEncodedObject()
	require.NoError(t, cp.Encode(o))
	h, err := repo.Storer.SetEncodedObject(o)
	require.NoError(t, err)
	got, err := object.GetCommit(repo.Storer, h)
	require.NoError(t, err)
	_, serr := verify.VerifyCommitSignature(got)
	require.ErrorIs(t, serr, verify.ErrBadSignature, "fixture: the signature is present and does not verify")
	return got
}

// B1, the judge's half: a record in a member's own folder must be a record FOR
// that member. agent-x adds members/agent-x/evil.md whose body says
// agent: boss (the review's attack: once on main it made boss "ambiguous"
// and locked boss out), or rewrites its own record to name boss: both
// not-owner. Controls: its own record edited, and a note in its folder that
// is no record, are admitted.
//
// SABOTAGE: drop the record-agent check in recordOwner → the attack rows are
// admitted → red.
func TestRecordOwner_ARecordInTheOwnFolderIsForTheOwner(t *testing.T) {
	const own = "kb/members/agent-x/1.md"
	other := namedSigner(t, "other-key")
	rows := []struct {
		name   string
		files  func(base map[string]string) map[string]string
		want   string // "" = admitted
		reason string
	}{
		{"adds a record for boss in its own folder", func(b map[string]string) map[string]string {
			return with(b, "kb/members/agent-x/evil.md", memberFile("boss", fact.MemberActive, other.PublicKey()))
		}, verify.RuleNotOwner, `kb/members/agent-x/evil.md, a record for agent "boss"`},
		{"rewrites its own record to name boss", func(b map[string]string) map[string]string {
			return with(b, own, memberFile("boss", fact.MemberActive, other.PublicKey()))
		}, verify.RuleNotOwner, `kb/members/agent-x/1.md, a record for agent "boss"`},
		{"control: edits its own record", func(b map[string]string) map[string]string { return with(b, own, b[own]+"\nv2\n") }, "", ""},
		{"control: a note that is no record", func(b map[string]string) map[string]string {
			return with(b, "kb/members/agent-x/notes.md", "n")
		}, "", ""},
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
	require.Equal(t, 4, ran)
}

// B1, the loader's half: records already on main in the WRONG folder
// (members/agent-x/evil.md saying agent: boss, members/agent-x/pre.md saying
// agent: joiner) are not loaded, so they lock nobody out. Boss's ordinary
// edit of its own record is admitted (it was "ambiguous: 2 member records"),
// and the joiner's first registration is rule A's as usual (the stray record
// made mayRegister false). The loader returns one record per own folder.
//
// SABOTAGE: drop the folder check in fleetrec.LoadMembersOfTree → boss is ambiguous
// and the joiner is refused → red.
func TestFleetLoader_IgnoresARecordInAnotherAgentsFolder(t *testing.T) {
	p := newTestFleetPKI(t)
	boss, x, j := namedSigner(t, "boss"), namedSigner(t, "x"), namedSigner(t, "joiner")
	bl, _ := p.leaf("boss", boss)
	xl, _ := p.leaf("agent-x", x)
	jl, _ := p.leaf("joiner", j)
	arts := p.artifacts(p.crl(), map[string]string{"boss": bl, "agent-x": xl, "joiner": jl})
	arts["kb/members/agent-x/1.md"] = memberFile("agent-x", fact.MemberActive, x.PublicKey())
	// Already on main (main itself is not judged): the stray records.
	arts["kb/members/agent-x/evil.md"] = memberFile("boss", fact.MemberActive, x.PublicKey())
	arts["kb/members/agent-x/pre.md"] = memberFile("joiner", fact.MemberActive, j.PublicKey())
	f, main, base := selfRegFixture(t, arts)

	ms, err := fleetrec.LoadFleet(f.fleetDir, "main", "")
	require.NoError(t, err)
	byPath := map[string]string{}
	for _, m := range ms {
		byPath[m.Path] = m.Agent
	}
	require.Equal(t, map[string]string{"kb/members/boss/1.md": "boss", "kb/members/agent-x/1.md": "agent-x"}, byPath,
		"only the records in their own folder are loaded")

	f.ref(f.fleet, "cand", f.commit(f.fleet, with(base, "kb/members/boss/1.md", base["kb/members/boss/1.md"]+"\nv2\n"), "boss", boss, main))
	v := f.checkSelf("")
	require.Equal(t, 1, v.Checked)
	require.Empty(t, v.Refused, "boss is not locked out by a record in agent-x's folder")

	f.selfReg(main, base, "joiner", j)
	v = f.checkSelf("")
	require.Equal(t, 1, v.Checked)
	require.Empty(t, v.Refused, "a joiner is not pre-empted by a record for it in another folder")
}

// S4: only a DECLARED artifact path (one an artifacts: pattern matches) is
// the artifact rules' instead of the owner rule's. A member adding,
// modifying or deleting an undeclared path under artifacts/ is not-owner.
// The master's enroll (a new leaf and its issuance record) and revoke (a
// newer CRL and a revocation statement) touch declared paths only and pass.
//
// SABOTAGE: exempt every artifacts/ path from recordOwner again → the three
// undeclared rows are admitted → red.
func TestRecordOwner_OnlyDeclaredArtifactPathsAreExempt(t *testing.T) {
	const stray = "artifacts/x/y.bin"
	rows := []struct {
		name  string
		files func(base map[string]string) map[string]string
		path  string
	}{
		{"adds an undeclared artifact", func(b map[string]string) map[string]string { return with(b, "artifacts/fleet/notes.txt", "n") }, "artifacts/fleet/notes.txt"},
		{"modifies an undeclared artifact", func(b map[string]string) map[string]string { return with(b, stray, "v2") }, stray},
		{"deletes an undeclared artifact", func(b map[string]string) map[string]string { return without(b, stray) }, stray},
	}
	ran := 0
	for _, row := range rows {
		f, _, base, _, x := ownerFixture(t)
		base = with(base, stray, "v1")
		main := f.fleetMain(base)
		f.ref(f.fleet, "cand", f.commit(f.fleet, row.files(base), "agent-x", x, main))
		v := f.checkSelf("")
		require.Equal(t, 1, v.Checked, row.name)
		r := onlyRefusal(t, v)
		require.Equal(t, verify.RuleNotOwner, r.Rule, row.name)
		require.Contains(t, r.Reason, row.path, row.name)
		ran++
	}
	require.Equal(t, 3, ran)

	// The master (boss) enrolls newbie, then revokes agent-x.
	p := newTestFleetPKI(t)
	boss, x, nb := namedSigner(t, "boss"), namedSigner(t, "x"), namedSigner(t, "newbie")
	bl, _ := p.leaf("boss", boss)
	xl, xs := p.leaf("agent-x", x)
	arts := p.artifacts(p.crl(), map[string]string{"boss": bl, "agent-x": xl})
	arts["kb/members/agent-x/1.md"] = memberFile("agent-x", fact.MemberActive, x.PublicKey())
	f, main, base := selfRegFixture(t, arts)
	l := fleetLayout(t)
	nl, _ := p.leaf("newbie", nb)
	enrolled := with(base, l.LeafPath("newbie"), nl, issuedDir+"0003-newbie.pem", nl)
	enroll := f.commit(f.fleet, enrolled, "boss", boss, main)
	revoke := f.commit(f.fleet, with(enrolled, l.CRL, p.crl(xs), revocationsDir+"0001.txt", p.revocation(xs, "test")), "boss", boss, enroll)
	f.ref(f.fleet, "cand", revoke)
	v := f.checkSelf("")
	require.Equal(t, 2, v.Checked, "enroll and revoke")
	require.Empty(t, v.Refused, "the master's enroll and revoke commits keep the owner rule")
}

// S2, the user's ruling, pinned so it is not lost: a two-parent merge that
// passes M3 is ADMITTED even when its signature is present but does not
// verify, exactly like an unsigned clean merge. It carries no content beyond
// its parents, and stripping the signature would give the same verdict. The
// merge reaches the clean-merge branch (verify.CheckM3 is OK; the signature is
// ErrBadSignature, not unsigned). Control: the same broken signature on a
// merge that changes a file beyond its parents is refused signature.
//
// SABOTAGE: admit a clean merge only when its signature verifies or is
// absent (`serr == nil || unsigned`) → the clean row is refused signature →
// red.
func TestM3_CleanMergeWithABadSignatureIsAdmitted(t *testing.T) {
	f, main, base, boss, x := ownerFixture(t)
	const own = "kb/members/agent-x/1.md"
	side := f.commit(f.fleet, with(base, own, base[own]+"\nv2\n"), "agent-x", x, main)
	bossV2 := with(base, "kb/members/boss/1.md", base["kb/members/boss/1.md"]+"\nv2\n")
	main2 := f.commit(f.fleet, bossV2, "boss", boss, main)
	f.ref(f.fleet, "main", main2)
	merged := with(bossV2, own, base[own]+"\nv2\n")

	clean := brokenSig(t, f.fleet, f.commit(f.fleet, merged, "agent-x", x, side, main2))
	m3, err := verify.CheckM3(clean)
	require.NoError(t, err)
	require.True(t, m3.OK, "fixture: the merge is M3-clean")
	f.ref(f.fleet, "cand", clean)
	v := f.checkSelf("")
	require.Equal(t, 2, v.Checked, "the side commit and the merge")
	require.Empty(t, v.Refused, "a clean merge whose signature does not verify is admitted")

	dirty := brokenSig(t, f.fleet, f.commit(f.fleet, with(merged, own, base[own]+"\nv3\n"), "agent-x", x, side, main2))
	f.ref(f.fleet, "cand", dirty)
	r := onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, dirty.Hash.String(), r.Commit)
	require.Equal(t, verify.RuleSignature, r.Rule, "a merge beyond its parents needs a signature that verifies")
}

// S1 at the gate: a merge whose tree adds a gitlink (a submodule entry)
// beyond its parents is not M3-clean. Signed, it is judged by the rules and
// the owner rule refuses the gitlink's path; unsigned, it is refused merge.
//
// SABOTAGE: flatten M3's trees with tree.Files() again (it skips gitlinks) →
// both merges are admitted → red.
func TestM3_AGitlinkBeyondTheParentsIsJudged(t *testing.T) {
	f, main, base, boss, x := ownerFixture(t)
	const own = "kb/members/agent-x/1.md"
	side := f.commit(f.fleet, with(base, own, base[own]+"\nv2\n"), "agent-x", x, main)
	bossV2 := with(base, "kb/members/boss/1.md", base["kb/members/boss/1.md"]+"\nv2\n")
	main2 := f.commit(f.fleet, bossV2, "boss", boss, main)
	f.ref(f.fleet, "main", main2)
	// "a-gitlink" sorts before kb/, so it is the first path the owner
	// rule's diff meets (against main's side the merge also changes boss's
	// record, which the owner rule would name otherwise).
	merged := with(bossV2, own, base[own]+"\nv2\n", "a-gitlink", gitlinkEntry+main.Hash.String())

	signed := f.commit(f.fleet, merged, "agent-x", x, side, main2)
	f.ref(f.fleet, "cand", signed)
	r := onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, signed.Hash.String(), r.Commit)
	require.Equal(t, verify.RuleNotOwner, r.Rule, r.Reason)
	require.Contains(t, r.Reason, "changes a-gitlink")

	unsigned := f.commit(f.fleet, merged, "", nil, side, main2)
	f.ref(f.fleet, "cand", unsigned)
	r = onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, unsigned.Hash.String(), r.Commit)
	require.Equal(t, verify.RuleMerge, r.Rule)
	require.Contains(t, r.Reason, "a-gitlink")
}
