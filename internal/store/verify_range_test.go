package store

import (
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
)

// rangeFixture is a knowledge base and a fleet repository, both bare git
// repositories on disk, the way a CI job sees them.
type rangeFixture struct {
	t        *testing.T
	kbDir    string
	kb       *gogit.Repository
	fleetDir string
	fleet    *gogit.Repository
	n        int
}

func newRangeFixture(t *testing.T) *rangeFixture {
	t.Helper()
	f := &rangeFixture{t: t, kbDir: t.TempDir(), fleetDir: t.TempDir()}
	var err error
	f.kb, err = gogit.PlainInit(f.kbDir, true)
	require.NoError(t, err)
	f.fleet, err = gogit.PlainInit(f.fleetDir, true)
	require.NoError(t, err)
	return f
}

func kbOntology(mode string) string {
	s := "id: x\nname: X\n"
	if mode != "" {
		s += "attributes:\n  verify_signatures: " + mode + "\n"
	}
	return s + "topics:\n  notes:\n    description: d\n"
}

func fleetOntologyYAML(t *testing.T) string {
	t.Helper()
	out, err := fact.FleetOntology().Serialize()
	require.NoError(t, err)
	return string(out)
}

// commit writes a commit into repo: files, an agent author (id "" = a plain
// non-agent address), an optional signer, parents.
func (f *rangeFixture) commit(repo *gogit.Repository, files map[string]string, author string, signer ssh.Signer, parents ...*object.Commit) *object.Commit {
	f.t.Helper()
	r := repoOn(f.t, repo.Storer)
	f.n++
	when := time.Unix(1790000000+int64(f.n), 0).UTC()
	email := "t@t"
	if author != "" {
		email = author + "+learn@agents.knomit.io"
	}
	c := &object.Commit{
		Author:    object.Signature{Name: author, Email: email, When: when},
		Committer: object.Signature{Name: author, Email: email, When: when},
		Message:   "c",
		TreeHash:  r.tree(files),
	}
	for _, p := range parents {
		c.ParentHashes = append(c.ParentHashes, p.Hash)
	}
	if signer != nil {
		c = signed(f.t, c, signer)
	}
	o := repo.Storer.NewEncodedObject()
	require.NoError(f.t, c.Encode(o))
	h, err := repo.Storer.SetEncodedObject(o)
	require.NoError(f.t, err)
	got, err := object.GetCommit(repo.Storer, h)
	require.NoError(f.t, err)
	return got
}

func (f *rangeFixture) ref(repo *gogit.Repository, name string, c *object.Commit) {
	f.t.Helper()
	require.NoError(f.t, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), c.Hash)))
}

func memberFile(agent, state string, key ssh.PublicKey) string {
	return "---\nkind: pragmatic\ntype: policy\n---\n# member " + agent + "\n\n" +
		fact.RenderMember(fact.Member{Agent: agent, State: state, Key: key, Host: "h"})
}

// setFleet writes the fleet repository's main with the given member files
// (path -> content) under kb/members/.
func (f *rangeFixture) setFleet(files map[string]string) {
	f.t.Helper()
	all := map[string]string{fact.OntologyFile: fleetOntologyYAML(f.t)}
	for p, c := range files {
		all["kb/members/"+p] = c
	}
	// Chain onto the current main, so successive calls are the fleet's
	// HISTORY (the audit reads every version), not unrelated roots.
	var parents []*object.Commit
	if ref, err := f.fleet.Storer.Reference(plumbing.NewBranchReferenceName("main")); err == nil {
		prev, err := object.GetCommit(f.fleet.Storer, ref.Hash())
		require.NoError(f.t, err)
		parents = append(parents, prev)
	}
	f.ref(f.fleet, "main", f.commit(f.fleet, all, "", nil, parents...))
}

func (f *rangeFixture) check(mode string) (RangeVerdict, error) {
	return CheckRange(RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: "main"})
}

// kbWith builds main (mode set, one unsigned root commit) and a candidate of
// one commit on top, by author signed with signer.
func (f *rangeFixture) kbWith(mode, author string, signer ssh.Signer) *object.Commit {
	base := map[string]string{fact.OntologyFile: kbOntology(mode)}
	root := f.commit(f.kb, base, "", nil)
	f.ref(f.kb, "main", root)
	c := f.commit(f.kb, with(base, "kb/notes/a.md", "a"), author, signer, root)
	f.ref(f.kb, "cand", c)
	return c
}

func refusedRule(t *testing.T, v RangeVerdict) string {
	t.Helper()
	require.Len(t, v.Refused, 1, "%+v", v)
	return v.Refused[0].Rule
}

// G-ok: an active member's commit, signed with its current key, is admitted.
func TestCheckRange_ActiveMemberAdmitted(t *testing.T) {
	f := newRangeFixture(t)
	x := namedSigner(t, "x")
	f.setFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())})
	f.kbWith(VerifyEnforce, "agent-x", x)
	v, err := f.check(VerifyEnforce)
	require.NoError(t, err)
	require.Empty(t, v.Refused)
	require.Equal(t, 1, v.Checked)
	require.Equal(t, 0, v.ExitCode())
}

// G1: author X signed with ANOTHER ACTIVE member's key: refused. A key-only
// lookup (resolve by the signing key, ignore the author) would admit it.
func TestCheckRange_AuthorMustOwnTheKey(t *testing.T) {
	f := newRangeFixture(t)
	x, y := namedSigner(t, "x"), namedSigner(t, "y")
	f.setFleet(map[string]string{
		"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey()),
		"agent-y/1.md": memberFile("agent-y", fact.MemberActive, y.PublicKey()),
	})
	f.kbWith(VerifyEnforce, "agent-x", y)
	v, err := f.check(VerifyEnforce)
	require.NoError(t, err)
	require.Equal(t, RuleKeyMismatch, refusedRule(t, v))
	require.Equal(t, 1, v.ExitCode())
}

// G2: no record for the author: refused. An opaque id with no fingerprint
// pattern works like any other.
func TestCheckRange_NoRecord(t *testing.T) {
	f := newRangeFixture(t)
	x := namedSigner(t, "x")
	f.setFleet(map[string]string{"research-box/1.md": memberFile("research-box", fact.MemberActive, x.PublicKey())})
	f.kbWith(VerifyEnforce, "someone-else", x)
	v, err := f.check(VerifyEnforce)
	require.NoError(t, err)
	require.Equal(t, RuleNoRecord, refusedRule(t, v))

	f2 := newRangeFixture(t)
	f2.setFleet(map[string]string{"research-box/1.md": memberFile("research-box", fact.MemberActive, x.PublicKey())})
	f2.kbWith(VerifyEnforce, "research-box", x)
	v, err = f2.check(VerifyEnforce)
	require.NoError(t, err)
	require.Empty(t, v.Refused, "an id with no fp8 pattern must work")
}

// G3: two live records at one id: refused as ambiguous.
func TestCheckRange_TwoRecordsOneID(t *testing.T) {
	f := newRangeFixture(t)
	x := namedSigner(t, "x")
	f.setFleet(map[string]string{
		"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey()),
		"agent-x/2.md": memberFile("agent-x", fact.MemberActive, x.PublicKey()),
	})
	f.kbWith(VerifyEnforce, "agent-x", x)
	v, err := f.check(VerifyEnforce)
	require.NoError(t, err)
	require.Equal(t, RuleAmbiguous, refusedRule(t, v))
}

// G4: one key as the current key of two records: refused.
func TestCheckRange_DuplicateKey(t *testing.T) {
	f := newRangeFixture(t)
	x := namedSigner(t, "x")
	f.setFleet(map[string]string{
		"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey()),
		"agent-z/1.md": memberFile("agent-z", fact.MemberActive, x.PublicKey()),
	})
	f.kbWith(VerifyEnforce, "agent-x", x)
	v, err := f.check(VerifyEnforce)
	require.NoError(t, err)
	require.Equal(t, RuleDuplicateKey, refusedRule(t, v))
}

// G5: left and revoked agents get no new acceptance.
func TestCheckRange_InactiveRefused(t *testing.T) {
	for _, state := range []string{fact.MemberLeft, fact.MemberRevoked} {
		t.Run(state, func(t *testing.T) {
			f := newRangeFixture(t)
			x := namedSigner(t, "x")
			f.setFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", state, x.PublicKey())})
			f.kbWith(VerifyEnforce, "agent-x", x)
			v, err := f.check(VerifyEnforce)
			require.NoError(t, err)
			require.Equal(t, RuleInactive, refusedRule(t, v))
		})
	}
}

// G6: after a rotation (the record updated to the new key) the old key is
// refused and the new one admitted: only the CURRENT key counts.
func TestCheckRange_RotationCurrentKeyOnly(t *testing.T) {
	f := newRangeFixture(t)
	old, cur := namedSigner(t, "old"), namedSigner(t, "new")
	f.setFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, old.PublicKey())})
	f.setFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, cur.PublicKey())})
	f.kbWith(VerifyEnforce, "agent-x", old)
	v, err := f.check(VerifyEnforce)
	require.NoError(t, err)
	require.Equal(t, RuleKeyMismatch, refusedRule(t, v))

	f.kbWith(VerifyEnforce, "agent-x", cur)
	v, err = f.check(VerifyEnforce)
	require.NoError(t, err)
	require.Empty(t, v.Refused)
}

// G7: an unsigned non-merge is refused; a clean unsigned merge (M3) passes.
func TestCheckRange_UnsignedAndM3(t *testing.T) {
	f := newRangeFixture(t)
	x := namedSigner(t, "x")
	f.setFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())})
	f.kbWith(VerifyEnforce, "agent-x", nil)
	v, err := f.check(VerifyEnforce)
	require.NoError(t, err)
	require.Equal(t, RuleSignature, refusedRule(t, v))

	base := map[string]string{fact.OntologyFile: kbOntology(VerifyEnforce)}
	root := f.commit(f.kb, base, "", nil)
	f.ref(f.kb, "main", root)
	a := f.commit(f.kb, with(base, "kb/notes/a.md", "a"), "agent-x", x, root)
	b := f.commit(f.kb, with(base, "kb/notes/b.md", "b"), "agent-x", x, root)
	m := f.commit(f.kb, with(base, "kb/notes/a.md", "a", "kb/notes/b.md", "b"), "", nil, a, b)
	f.ref(f.kb, "cand", m)
	v, err = f.check(VerifyEnforce)
	require.NoError(t, err)
	require.Empty(t, v.Refused, "a clean unsigned merge of admitted parents passes M3")
	require.Equal(t, 3, v.Checked)
}

// G8: verification off (absent) at main: exit 0 and the fleet is never
// touched, so no fleet needs to be given at all.
func TestCheckRange_OffTouchesNothing(t *testing.T) {
	f := newRangeFixture(t)
	f.kbWith("", "agent-x", nil)
	v, err := CheckRange(RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand"})
	require.NoError(t, err)
	require.Equal(t, VerifyOff, v.Mode)
	require.Equal(t, 0, v.ExitCode())
	require.Zero(t, v.Checked)
}

// G9: verification on with no fleet, an unreadable fleet, or a fleet whose
// ontology is not the fleet preset: the gate could not run (an error, exit 2
// in the command), never an empty member set.
func TestCheckRange_FleetMissingIsCouldNotRun(t *testing.T) {
	f := newRangeFixture(t)
	x := namedSigner(t, "x")
	f.kbWith(VerifyEnforce, "agent-x", x)
	_, err := CheckRange(RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand"})
	require.ErrorIs(t, err, ErrNoFleet)

	_, err = CheckRange(RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand", FleetDir: t.TempDir(), FleetRev: "main"})
	require.Error(t, err, "an unreadable fleet must not pass")

	f.ref(f.fleet, "main", f.commit(f.fleet, map[string]string{fact.OntologyFile: kbOntology("")}, "", nil))
	_, err = f.check(VerifyEnforce)
	require.ErrorIs(t, err, ErrNotFleet)
}

// G10: a malformed record is skipped (the gate still runs) and logged ONCE
// per blob, not once per run.
func TestCheckRange_MalformedRecordSkipped(t *testing.T) {
	f := newRangeFixture(t)
	x := namedSigner(t, "x")
	f.setFleet(map[string]string{
		"agent-x/1.md":  memberFile("agent-x", fact.MemberActive, x.PublicKey()),
		"broken/1.md":   "---\nkind: pragmatic\ntype: policy\n---\n# broken\n\nagent: broken\nstate: paused\n",
		"agent-x2/1.md": memberFile("agent-x2", fact.MemberActive, namedSigner(t, "x2").PublicKey()),
	})
	f.kbWith(VerifyEnforce, "agent-x", x)
	before := countMalformedLogged()
	for range 2 {
		v, err := f.check(VerifyEnforce)
		require.NoError(t, err)
		require.Empty(t, v.Refused)
	}
	require.Equal(t, before+1, countMalformedLogged(), "one ERROR per malformed blob, over two runs")
}

func countMalformedLogged() int {
	n := 0
	malformedLogged.Range(func(_, _ any) bool { n++; return true })
	return n
}

// G11: log reports and exits 0; enforce blocks with 1. The literals are the
// contract a CI job reads.
func TestCheckRange_ExitCodes(t *testing.T) {
	for mode, want := range map[string]int{VerifyLog: 0, VerifyEnforce: 1} {
		t.Run(mode, func(t *testing.T) {
			f := newRangeFixture(t)
			x := namedSigner(t, "x")
			f.setFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())})
			f.kbWith(mode, "agent-x", nil)
			v, err := f.check(mode)
			require.NoError(t, err)
			require.Len(t, v.Refused, 1, "log still REPORTS the refusal")
			require.Equal(t, want, v.ExitCode())
		})
	}
}

func TestAgentIDOfAuthor(t *testing.T) {
	require.Equal(t, "research-box", agentIDOfAuthor("research-box+learn@agents.knomit.io"))
	require.Equal(t, "mindev-local-8ef0cd32", agentIDOfAuthor("mindev-local-8ef0cd32@agents.knomit.io"))
	require.Equal(t, "", agentIDOfAuthor("someone@example.com"))
	require.False(t, strings.Contains(agentIDOfAuthor("a+b+c@agents.knomit.io"), "+"))
}

// R5-G1: an unsigned merge that adds content beyond its parents fails M3 and
// is refused as a merge (not waived as "a merge").
func TestCheckRange_UnsignedMergeFailingM3Refused(t *testing.T) {
	f := newRangeFixture(t)
	x := namedSigner(t, "x")
	f.setFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())})
	base := map[string]string{fact.OntologyFile: kbOntology(VerifyEnforce)}
	root := f.commit(f.kb, base, "", nil)
	f.ref(f.kb, "main", root)
	a := f.commit(f.kb, with(base, "kb/notes/a.md", "a"), "agent-x", x, root)
	b := f.commit(f.kb, with(base, "kb/notes/b.md", "b"), "agent-x", x, root)
	m := f.commit(f.kb, with(base, "kb/notes/a.md", "a", "kb/notes/b.md", "b", "kb/notes/smuggled.md", "s"), "", nil, a, b)
	f.ref(f.kb, "cand", m)
	v, err := f.check(VerifyEnforce)
	require.NoError(t, err)
	require.Equal(t, RuleMerge, refusedRule(t, v))
	require.Equal(t, m.Hash.String(), v.Refused[0].Commit)
	require.Equal(t, 1, v.ExitCode())
}

// R5-G2: the mode is read at MAIN's tip, never from the candidate. A
// candidate that switches verification off in its own ontology is still
// judged under main's enforce.
func TestCheckRange_ModeFromMainNotCandidate(t *testing.T) {
	f := newRangeFixture(t)
	x, stranger := namedSigner(t, "x"), namedSigner(t, "stranger")
	f.setFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())})
	base := map[string]string{fact.OntologyFile: kbOntology(VerifyEnforce)}
	root := f.commit(f.kb, base, "", nil)
	f.ref(f.kb, "main", root)
	off := with(base, fact.OntologyFile, kbOntology(VerifyOff))
	c1 := f.commit(f.kb, off, "agent-x", x, root)
	c2 := f.commit(f.kb, with(off, "kb/notes/s.md", "s"), "agent-stranger", stranger, c1)
	f.ref(f.kb, "cand", c2)
	v, err := f.check(VerifyEnforce)
	require.NoError(t, err)
	require.Equal(t, VerifyEnforce, v.Mode, "main's mode, not the candidate's")
	require.Equal(t, RuleNoRecord, refusedRule(t, v))
	require.Equal(t, c2.Hash.String(), v.Refused[0].Commit)
	require.Equal(t, 1, v.ExitCode())
}

// R5-G2 (reverse): main off, the candidate switching verification ON: the
// gate reads main's tip, sees off, and checks nothing (exit 0), even though
// the candidate's commit is by a stranger.
func TestCheckRange_MainOffCandidateOnChecksNothing(t *testing.T) {
	f := newRangeFixture(t)
	stranger := namedSigner(t, "stranger")
	base := map[string]string{fact.OntologyFile: kbOntology("")}
	root := f.commit(f.kb, base, "", nil)
	f.ref(f.kb, "main", root)
	c := f.commit(f.kb, with(base, fact.OntologyFile, kbOntology(VerifyEnforce)), "agent-stranger", stranger, root)
	f.ref(f.kb, "cand", c)
	v, err := CheckRange(RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand"})
	require.NoError(t, err, "no fleet needed: main is off")
	require.Equal(t, VerifyOff, v.Mode)
	require.Zero(t, v.Checked)
	require.Equal(t, 0, v.ExitCode())
}
