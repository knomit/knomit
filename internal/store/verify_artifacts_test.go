package store

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"knomit/internal/store/verify"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/domain/fact"
)

// The fleet ontology's per-operation record directories (fleet/issued/*.pem,
// fleet/revocations/*.txt). The judge never names them; the tests do.
const (
	issuedDir      = "artifacts/fleet/issued/"
	revocationsDir = "artifacts/fleet/revocations/"
)

// artFleet is a fleet repository judging itself, with a PKI on main: the
// root, CRL Number 1, leaves for the two active members boss and vic, their
// issuance records, and one revocation statement (for a serial nobody holds).
// Every candidate commit below is by boss, signed by boss, whom the signer
// rules and rule B admit, so a refusal is the artifact rules'.
type artFleet struct {
	t           *testing.T
	p           *testFleetPKI
	f           *rangeFixture
	main        *object.Commit
	base        map[string]string
	boss, vic   ssh.Signer
	l           fact.PKILayout
	issued, rev string // an issuance record and a revocation statement on main
}

func newArtFleet(t *testing.T) *artFleet {
	t.Helper()
	p := newTestFleetPKI(t)
	boss, vic := namedSigner(t, "boss"), namedSigner(t, "vic")
	bl, _ := p.leaf("boss", boss)
	vl, _ := p.leaf("vic", vic)
	a := &artFleet{t: t, p: p, boss: boss, vic: vic, l: fleetLayout(t),
		issued: issuedDir + "0001-boss.pem", rev: revocationsDir + "0001.txt"}
	arts := p.artifacts(p.crl(big.NewInt(0x5)), map[string]string{"boss": bl, "vic": vl})
	arts["kb/members/vic/1.md"] = memberFile("vic", fact.MemberActive, vic.PublicKey())
	arts[a.issued] = bl
	arts[issuedDir+"0002-vic.pem"] = vl
	arts[a.rev] = p.revocation(big.NewInt(0x5), "test")
	a.f, a.main, a.base = selfRegFixture(t, arts)
	return a
}

// cand puts one commit by author/signer on "cand", base + kv on top of main
// (a "" value deletes the path), and returns the verdict of the fleet's own
// gate at main.
func (a *artFleet) cand(author string, signer ssh.Signer, kv ...string) verify.RangeVerdict {
	a.t.Helper()
	c := a.f.commit(a.f.fleet, with(a.base, kv...), author, signer, a.main)
	a.f.ref(a.f.fleet, "cand", c)
	return a.f.checkSelf("")
}

func requireArtifactRefusal(t *testing.T, v verify.RangeVerdict, rule, reason, name string) {
	t.Helper()
	require.Equal(t, 1, v.Checked, name)
	r := onlyRefusal(t, v)
	require.Equal(t, rule, r.Rule, "%s: %s", name, r.Reason)
	require.Contains(t, r.Reason, reason, name)
}

// The review's probes P1–P3 and the deletions and corruptions of the trust
// files, each as boss — an active, certified member whose commit every other
// rule admits. Each must be refused by the named artifact rule. The positive
// controls (a new issuance record, a newer CRL, a large undeclared file) are
// admitted, so the refusals are not the fixture's doing.
//
// P1 is deterministic (review F1): the rogue boss leaf is issued an hour in
// the past, so its NotBefore is never later than the real leaf's and it can
// never pass as a renewal, whatever second the test runs in. The refusal is
// therefore always the FIRST path the sorted diff meets, certs/boss.pem.
//
// SABOTAGE: make artifactRules return true → every refusal row is admitted →
// red.
func TestArtifactRules_TrustFilesAreProtected(t *testing.T) {
	a := newArtFleet(t)
	rogue := newTestFleetPKI(t)
	rogueBoss, _ := rogue.leafAt("boss", a.boss, time.Now().Add(-time.Hour))
	stranger := namedSigner(t, "stranger")
	rogueStranger, _ := rogue.leaf("stranger", stranger)
	newer := a.p.crl(big.NewInt(0x5)) // Number 2, still listing 0x5

	type row struct {
		name   string
		kv     []string
		rule   string // "" = admitted
		reason string
	}
	rows := []row{
		// P1 as the review probed it: the first path the diff meets (sorted:
		// certs/ before crl.pem before root.crt) is boss's leaf, which no
		// rogue leaf renews.
		{"P1 rogue root, CRL and leaves", []string{
			a.l.Root, rogue.rootPEM(), a.l.CRL, rogue.crl(), a.l.LeafPath("boss"), rogueBoss, a.l.LeafPath("stranger"), rogueStranger,
		}, verify.RuleArtifactReplace, "certs/boss.pem is replaced but replace_when leaf-renewal does not hold"},
		{"P1 rogue root alone", []string{a.l.Root, rogue.rootPEM()}, verify.RuleArtifactWriteOnce, "write-once"},
		{"P2 root deleted", []string{a.l.Root, ""}, verify.RuleArtifactWriteOnce, "is deleted"},
		{"root corrupted", []string{a.l.Root, "junk"}, verify.RuleArtifactWriteOnce, "write-once"},
		{"CRL deleted", []string{a.l.CRL, ""}, verify.RuleArtifactReplace, "is deleted"},
		{"CRL corrupted", []string{a.l.CRL, "junk"}, verify.RuleArtifactReplace, "no X509 CRL PEM block"},
		{"CRL from another root", []string{a.l.CRL, rogue.crl()}, verify.RuleArtifactReplace, "not signed by the fleet root"},
		{"leaf deleted", []string{a.l.LeafPath("vic"), ""}, verify.RuleArtifactReplace, "is deleted"},
		{"leaf corrupted", []string{a.l.LeafPath("vic"), "junk"}, verify.RuleArtifactReplace, "replace_when leaf-renewal does not hold"},
		{"issuance record deleted", []string{a.issued, ""}, verify.RuleArtifactWriteOnce, "is deleted"},
		{"issuance record modified", []string{a.issued, a.base[issuedDir+"0002-vic.pem"]}, verify.RuleArtifactWriteOnce, "write-once"},
		{"revocation deleted", []string{a.rev, ""}, verify.RuleArtifactWriteOnce, "is deleted"},
		{"revocation modified", []string{a.rev, a.p.revocation(big.NewInt(0x5), "other")}, verify.RuleArtifactWriteOnce, "write-once"},
		{"size cap", []string{issuedDir + "big.pem", strings.Repeat("x", 16<<10+1)}, verify.RuleArtifactSize, "larger than its max_size of 16384"},
		{"control: a new issuance record", []string{issuedDir + "0003-boss.pem", a.base[a.l.LeafPath("boss")]}, "", ""},
		{"control: a newer CRL", []string{a.l.CRL, newer}, "", ""},
		// On the fleet repository an undeclared path has no artifact rule,
		// so it is the record-owner rule's (S4): boss may not write it.
		{"an undeclared file is the owner rule's", []string{"artifacts/fleet/notes.txt", strings.Repeat("x", 1<<20)}, verify.RuleNotOwner, "artifacts/fleet/notes.txt"},
	}
	ran := 0
	for _, r := range rows {
		v := a.cand("boss", a.boss, r.kv...)
		if r.rule == "" {
			require.Equal(t, 1, v.Checked, r.name)
			require.Empty(t, v.Refused, r.name)
		} else {
			requireArtifactRefusal(t, v, r.rule, r.reason, r.name)
		}
		ran++
	}
	require.Equal(t, len(rows), ran)
}

// P3: CRL rollback. With CRL Number 2 on main, re-committing the older,
// root-signed Number 1 — or Number 2 again with other content — is refused;
// Number 3 is admitted. A stale-branch MERGE that brings Number 1 back is
// judged against every parent and refused too.
//
// SABOTAGE: compare Numbers with < instead of <= in crlNewer → the same-number
// row is admitted → red; diff a merge against its first parent only → the
// merge row is admitted → red.
func TestArtifactRules_CRLRollbackRefused(t *testing.T) {
	a := newArtFleet(t)
	n1 := a.base[a.l.CRL]
	n2 := a.p.crl()
	stale := a.main // still holds Number 1
	a.base = with(a.base, a.l.CRL, n2)
	a.main = a.f.fleetMain(a.base)

	requireArtifactRefusal(t, a.cand("boss", a.boss, a.l.CRL, n1), verify.RuleArtifactReplace, "is not greater than the 2 it replaces", "older CRL")
	a.p.crlN-- // sign another Number 2, listing a serial so its bytes differ
	requireArtifactRefusal(t, a.cand("boss", a.boss, a.l.CRL, a.p.crl(big.NewInt(77))), verify.RuleArtifactReplace, "rollback", "same Number")
	v := a.cand("boss", a.boss, a.l.CRL, a.p.crl()) // Number 3
	require.Empty(t, v.Refused, "control: a newer CRL")

	// A merge of main with a stale side, resolved to the stale CRL. Against
	// the stale parent the CRL is unchanged; against main it is a rollback.
	m := a.f.commit(a.f.fleet, with(a.base, a.l.CRL, n1), "boss", a.boss, stale, a.main)
	a.f.ref(a.f.fleet, "cand", m)
	vm := a.f.checkSelf("")
	var rules []string
	for _, r := range vm.Refused {
		if r.Commit == m.Hash.String() {
			rules = append(rules, r.Rule)
		}
	}
	require.Equal(t, []string{verify.RuleArtifactReplace}, rules, "%+v", vm)
}

// P4: a leaf the root really issued, but to another id, written at vic's
// path — over vic's leaf (a replace) or where none was (an add) — is refused:
// the email SAN id must equal the file's id. A real renewal for vic, issued
// later, naming vic's record's key, is admitted; renewals that are older, for
// another key, or revoked are refused.
//
// SABOTAGE: drop the id comparison in validLeaf → both plant rows admitted →
// red; drop the NotBefore comparison → the older renewal row admitted → red.
func TestArtifactRules_LeafPlantAndRenewal(t *testing.T) {
	a := newArtFleet(t)
	bossLeaf := a.base[a.l.LeafPath("boss")]
	other := namedSigner(t, "other")
	renewal, _ := a.p.leafAt("vic", a.vic, time.Now().Add(time.Hour))
	older, _ := a.p.leafAt("vic", a.vic, time.Now().Add(-24*time.Hour))
	otherKey, _ := a.p.leafAt("vic", other, time.Now().Add(time.Hour))
	revoked, revSerial := a.p.leafAt("vic", a.vic, time.Now().Add(2*time.Hour))
	rows := []struct {
		name, rule, reason string
		kv                 []string
	}{
		{"plant over vic's leaf", verify.RuleArtifactReplace, `names agent "boss", but the file is for "vic"`, []string{a.l.LeafPath("vic"), bossLeaf}},
		{"plant where none was", verify.RuleArtifactPKI, `names agent "boss", but the file is for "newbie"`, []string{a.l.LeafPath("newbie"), bossLeaf}},
		{"older renewal", verify.RuleArtifactReplace, "is not later than", []string{a.l.LeafPath("vic"), older}},
		{"renewal for another key", verify.RuleArtifactReplace, "not the current key of agent \"vic\"", []string{a.l.LeafPath("vic"), otherKey}},
		{"revoked renewal", verify.RuleArtifactReplace, "revoked", []string{a.l.LeafPath("vic"), revoked, a.l.CRL, a.p.crl(revSerial, big.NewInt(0x5))}},
		// Agent ids are lower case and compared exactly (review F2 is
		// answered by never folding case): vic's real leaf at Vic.pem is for
		// "vic", not "Vic"; a leaf the root signed for the upper-case address
		// Vic@agents.knomit.io is no agent's at all.
		{"vic's leaf at an upper-case path", verify.RuleArtifactPKI, `names agent "vic", but the file is for "Vic"`, []string{a.l.LeafPath("Vic"), a.base[a.l.LeafPath("vic")]}},
		{"upper-case email SAN", verify.RuleArtifactPKI, `names agent "", but the file is for "Vic"`, []string{a.l.LeafPath("Vic"), a.p.craftedLeaf("Vic@agents.knomit.io", a.vic)}},
		{"control: renewal", "", "", []string{a.l.LeafPath("vic"), renewal}},
	}
	for _, r := range rows {
		v := a.cand("boss", a.boss, r.kv...)
		if r.rule == "" {
			require.Equal(t, 1, v.Checked, r.name)
			require.Empty(t, v.Refused, r.name)
			continue
		}
		requireArtifactRefusal(t, v, r.rule, r.reason, r.name)
	}
}

// The bootstrap: on a fleet with NO PKI, one commit adding the root, the CRL
// and every active member's leaf is admitted (first adds; the adds hold as
// their roles). Adding a CRL another root signed, or a leaf that does not
// chain, in the same bootstrap is refused artifact-pki.
//
// The other order: a commit adding only the root is admitted, and once it is
// main every member's next commit is refused no-cert (rule B fails closed:
// no CRL, no leaf) — P4/P5 must land the root with the leaves.
func TestArtifactRules_Bootstrap(t *testing.T) {
	p, rogue := newTestFleetPKI(t), newTestFleetPKI(t)
	boss := namedSigner(t, "boss")
	l := fleetLayout(t)
	bl, _ := p.leaf("boss", boss)
	rbl, _ := rogue.leaf("boss", boss)

	f, main, base := selfRegFixture(t, nil)
	c := f.commit(f.fleet, with(base, l.Root, p.rootPEM(), l.CRL, p.crl(), l.LeafPath("boss"), bl), "boss", boss, main)
	f.ref(f.fleet, "cand", c)
	v := f.checkSelf("")
	require.Equal(t, 1, v.Checked)
	require.Empty(t, v.Refused, "the single-commit bootstrap is admitted")

	for name, kv := range map[string][]string{
		"CRL from another root":  {l.Root, p.rootPEM(), l.CRL, rogue.crl(), l.LeafPath("boss"), bl},
		"leaf from another root": {l.Root, p.rootPEM(), l.CRL, p.crl(), l.LeafPath("boss"), rbl},
		"root that is no CA":     {l.Root, bl},
	} {
		g, gmain, gbase := selfRegFixture(t, nil)
		g.ref(g.fleet, "cand", g.commit(g.fleet, with(gbase, kv...), "boss", boss, gmain))
		requireArtifactRefusal(t, g.checkSelf(""), verify.RuleArtifactPKI, "does not hold", name)
	}

	h, hmain, hbase := selfRegFixture(t, nil)
	rootOnly := with(hbase, l.Root, p.rootPEM())
	h.ref(h.fleet, "cand", h.commit(h.fleet, rootOnly, "boss", boss, hmain))
	require.Empty(t, h.checkSelf("").Refused, "the root alone is admitted (a first add)")
	hmain = h.fleetMain(rootOnly)
	h.ref(h.fleet, "cand", h.commit(h.fleet, with(rootOnly, "kb/members/boss/2.md", "note"), "boss", boss, hmain))
	r := onlyRefusal(t, h.checkSelf(""))
	require.Equal(t, verify.RuleNoCert, r.Rule, "with the root on main, a member with no CRL and no leaf is refused")
}

// An oversized trust file at the fleet revision is never read whole: the
// reader treats it as unusable and rule B fails closed.
func TestFleetPKI_ReadsAreBounded(t *testing.T) {
	p := newTestFleetPKI(t)
	x := namedSigner(t, "x")
	l := fleetLayout(t)
	leaf, _ := p.leaf("agent-x", x)
	f := newRangeFixture(t)
	arts := p.artifacts(p.crl(), map[string]string{"agent-x": leaf + strings.Repeat("\n", 16<<10)})
	f.abFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())}, arts)
	f.kbWith(verify.VerifyEnforce, "agent-x", x)
	v, err := f.check(verify.VerifyEnforce)
	require.NoError(t, err)
	r := onlyRefusal(t, v)
	require.Equal(t, verify.RuleNoCert, r.Rule)
	require.Contains(t, r.Reason, l.LeafPath("agent-x")+": larger than its max_size")
}

// Log mode reports an artifact refusal and blocks nothing; off judges
// nothing. Under enforce the same commit is exit 1.
func TestArtifactRules_FollowTheMode(t *testing.T) {
	for _, mode := range []string{verify.VerifyEnforce, verify.VerifyLog, verify.VerifyOff} {
		a := newArtFleet(t)
		ont := strings.Replace(a.base[fact.OntologyFile], "verify_signatures: enforce", "verify_signatures: "+mode, 1)
		require.Contains(t, ont, "verify_signatures: "+mode, "fixture: the mode line is in the preset")
		a.base = with(a.base, fact.OntologyFile, ont)
		a.main = a.f.fleetMain(a.base)
		v := a.cand("boss", a.boss, a.l.Root, "")
		switch mode {
		case verify.VerifyEnforce:
			require.Equal(t, verify.RuleArtifactWriteOnce, onlyRefusal(t, v).Rule)
			require.Equal(t, 1, v.ExitCode())
		case verify.VerifyLog:
			require.Equal(t, verify.RuleArtifactWriteOnce, onlyRefusal(t, v).Rule)
			require.Equal(t, 0, v.ExitCode())
		case verify.VerifyOff:
			require.Equal(t, 0, v.Checked)
			require.Empty(t, v.Refused)
		}
	}
}

// Ruling 5: each issuance and each revocation is a file of its own, judged
// when ADDED. An issuance record is a leaf the fleet root issued (chains,
// IssueLeaf's email rules); a revocation is a statement the fleet root
// signed, verified over bytes rebuilt from its validated fields. A forged
// statement (another root signed it), a tampered field, a leaf that does
// not chain, junk, and an upper-case agent address are refused artifact-pki.
// A file NAME means nothing: a record named for vic that holds boss's leaf,
// and a statement named "first", are admitted on their content alone.
//
// SABOTAGE: return nil from pkiAdd for pki: revocation → the forged and
// tampered rows are admitted → red; for pki: issuance → the rogue and junk
// rows are admitted → red.
func TestArtifactRules_PerOperationRecords(t *testing.T) {
	a := newArtFleet(t)
	rogue := newTestFleetPKI(t)
	rogueLeaf, _ := rogue.leaf("boss", a.boss)
	good := a.p.revocation(big.NewInt(0x77), "key lost")
	lines := strings.Split(good, "\n")
	lines[2] = "78" // the serial, after signing
	tampered := strings.Join(lines, "\n")
	operator := a.p.craftedLeaf("ops@example.com", a.boss)
	for _, r := range []struct {
		name, rule, reason string
		kv                 []string
	}{
		{"forged revocation (another root)", verify.RuleArtifactPKI, "not this fleet's root", []string{revocationsDir + "0002.txt", rogue.revocation(big.NewInt(0x77), "x")}},
		{"revocation with a tampered serial", verify.RuleArtifactPKI, "signature does not verify", []string{revocationsDir + "0002.txt", tampered}},
		{"revocation that is a CRL", verify.RuleArtifactPKI, "statement lines", []string{revocationsDir + "0002.txt", a.p.crl(big.NewInt(0x5))}},
		{"issuance that does not chain", verify.RuleArtifactPKI, "does not chain", []string{issuedDir + "0003.pem", rogueLeaf}},
		{"issuance junk", verify.RuleArtifactPKI, "no CERTIFICATE PEM block", []string{issuedDir + "0003.pem", "junk"}},
		{"issuance of the root itself", verify.RuleArtifactPKI, "CA certificate", []string{issuedDir + "0003.pem", a.p.rootPEM()}},
		{"issuance for an upper-case agent address", verify.RuleArtifactPKI, "upper-case", []string{issuedDir + "0003.pem", a.p.craftedLeaf("Vic@agents.knomit.io", a.vic)}},
		{"control: a revocation statement", "", "", []string{revocationsDir + "first.txt", good}},
		{"control: an operator issuance", "", "", []string{issuedDir + "0003-ops.pem", operator}},
		{"control: a name means nothing", "", "", []string{issuedDir + "9999-vic.pem", a.base[a.l.LeafPath("boss")]}},
	} {
		v := a.cand("boss", a.boss, r.kv...)
		if r.rule == "" {
			require.Equal(t, 1, v.Checked, r.name)
			require.Empty(t, v.Refused, r.name)
			continue
		}
		requireArtifactRefusal(t, v, r.rule, r.reason, r.name)
	}
}

// Ruling 5, CRL consistency: a replacing crl.pem must list a SUPERSET of the
// serials the CRL it replaces lists (revoked_set: superset), besides being
// newer. Dropping one serial is refused even when the Number rises; a
// superset is admitted.
//
// SABOTAGE: skip the superset loop in crlNewer → the "drops a serial" row is
// admitted → red.
func TestArtifactRules_ReplacementCRLIsASuperset(t *testing.T) {
	a := newArtFleet(t) // main: CRL #1 listing 0x5
	a.base = with(a.base, a.l.CRL, a.p.crl(big.NewInt(0x5), big.NewInt(0x6)))
	a.main = a.f.fleetMain(a.base) // main: CRL #2 listing 0x5, 0x6

	requireArtifactRefusal(t, a.cand("boss", a.boss, a.l.CRL, a.p.crl(big.NewInt(0x6), big.NewInt(0x7))),
		verify.RuleArtifactReplace, "drops serial 5", "drops a serial")
	requireArtifactRefusal(t, a.cand("boss", a.boss, a.l.CRL, a.p.crl()),
		verify.RuleArtifactReplace, "drops serial", "empty")
	v := a.cand("boss", a.boss, a.l.CRL, a.p.crl(big.NewInt(0x5), big.NewInt(0x6), big.NewInt(0x7)))
	require.Equal(t, 1, v.Checked)
	require.Empty(t, v.Refused, "control: a superset")
}

// Review F6: a non-file entry AT a declared path, or anything UNDER one, is
// refused — a directory squatting a leaf's name (certs/<id>.pem/x) or an
// issuance record's, a symlink, a submodule. A symlink or submodule AT a
// declared path is the artifact rules' (artifact-not-a-file). A path UNDER a
// declared file matches no pattern, so on the fleet repository it is the
// record-owner rule's first (S4: not-owner); the squat check for a path a
// family pattern declares is pinned by TestArtifactRules_FamilyPatternIsNotASquat.
//
// SABOTAGE: skip squat() in change → the symlink and submodule rows stay
// refused (mode check), the squat rows of FamilyPatternIsNotASquat are
// admitted → red there.
func TestArtifactRules_DirectorySquatRefused(t *testing.T) {
	a := newArtFleet(t)
	gitlink := gitlinkEntry + a.main.Hash.String()
	for _, r := range []struct {
		name, rule, reason string
		kv                 []string
	}{
		{"directory at a leaf's path", verify.RuleNotOwner, "changes artifacts/fleet/certs/newbie.pem/x", []string{a.l.LeafPath("newbie") + "/x", "junk"}},
		{"deep directory at a leaf's path", verify.RuleNotOwner, "changes artifacts/fleet/certs/newbie.pem/a/b", []string{a.l.LeafPath("newbie") + "/a/b", "junk"}},
		{"directory at an issuance path", verify.RuleNotOwner, "changes artifacts/fleet/issued/x.pem/y", []string{issuedDir + "x.pem/y", "junk"}},
		{"directory at a revocation path", verify.RuleNotOwner, "changes artifacts/fleet/revocations/x.txt/y", []string{revocationsDir + "x.txt/y", "junk"}},
		{"symlink at a leaf's path", verify.RuleArtifactNotAFile, "must be a regular file", []string{a.l.LeafPath("newbie"), symlinkEntry + "../root.crt"}},
		{"submodule at an issuance path", verify.RuleArtifactNotAFile, "must be a regular file", []string{issuedDir + "x.pem", gitlink}},
		{"control: a declared path", "", "", []string{issuedDir + "0003-boss.pem", a.base[a.l.LeafPath("boss")]}},
	} {
		v := a.cand("boss", a.boss, r.kv...)
		if r.rule == "" {
			require.Equal(t, 1, v.Checked, r.name)
			require.Empty(t, v.Refused, r.name)
			continue
		}
		requireArtifactRefusal(t, v, r.rule, r.reason, r.name)
	}
}

// A pattern that matches a directory AND the paths beneath it (fleet/**) is a
// family pattern, not a file declaration: with it declared, the issuance
// record, the newer CRL and an undeclared note beneath are admitted (review
// M2: every add under fleet/ was refused artifact-not-a-file). A pattern that
// matches the directory but NOT the path beneath it still declares a file
// there: fleet/* declares fleet/issued a file, so fleet/issued/x squats it,
// and certs/<id>.pem/x still squats a leaf with fleet/** declared too.
//
// SABOTAGE: let squat count any matching pattern again (drop the
// beneath-check) → the fleet/** admitted rows are refused → red.
func TestArtifactRules_FamilyPatternIsNotASquat(t *testing.T) {
	withCap := func(t *testing.T, line string) *artFleet {
		a := newArtFleet(t)
		ont := strings.Replace(a.base[fact.OntologyFile], "artifacts:\n", "artifacts:\n  "+line+"\n", 1)
		require.Contains(t, ont, "artifacts:\n  "+line+"\n", "fixture: the line is in the artifacts section")
		a.base = with(a.base, fact.OntologyFile, ont)
		a.main = a.f.fleetMain(a.base)
		return a
	}
	a := withCap(t, "fleet/**: { max_size: 1MiB }")
	newer := a.p.crl(big.NewInt(0x5))
	for _, r := range []struct {
		name, rule, reason string
		kv                 []string
	}{
		{"fleet/**: a new issuance record", "", "", []string{issuedDir + "0003-boss.pem", a.base[a.l.LeafPath("boss")]}},
		{"fleet/**: a newer CRL", "", "", []string{a.l.CRL, newer}},
		{"fleet/**: an undeclared note", "", "", []string{"artifacts/fleet/notes/x.md", "ok"}},
		{"fleet/**: its cap still applies", verify.RuleArtifactSize, "larger than its max_size of 1048576", []string{"artifacts/fleet/notes/x.md", strings.Repeat("x", 1<<20+1)}},
		{"fleet/**: a directory at a leaf's path", verify.RuleArtifactNotAFile, "certs/newbie.pem, which is a declared artifact file", []string{a.l.LeafPath("newbie") + "/x", "junk"}},
	} {
		v := a.cand("boss", a.boss, r.kv...)
		if r.rule == "" {
			require.Equal(t, 1, v.Checked, r.name)
			require.Empty(t, v.Refused, r.name)
			continue
		}
		requireArtifactRefusal(t, v, r.rule, r.reason, r.name)
	}

	b := withCap(t, "fleet/*: { max_size: 1MiB }")
	requireArtifactRefusal(t, b.cand("boss", b.boss, issuedDir+"0003-boss.pem", b.base[b.l.LeafPath("boss")]),
		verify.RuleArtifactNotAFile, "under artifacts/fleet/issued, which is a declared artifact file", "fleet/*: declares fleet/issued a file")
}

// Agent ids are lower case everywhere and compared exactly: an author
// address with an upper-case id (Vic+learn@agents.knomit.io) is no agent
// address, so vic's own key does not make it vic's commit.
func TestRulesAB_UpperCaseAuthorIsNoAgent(t *testing.T) {
	a := newArtFleet(t)
	r := onlyRefusal(t, a.cand("Vic", a.vic, "kb/members/vic/2.md", "note"))
	require.Equal(t, verify.RuleNoRecord, r.Rule)
	require.Contains(t, r.Reason, "is not an agent address")
	v := a.cand("vic", a.vic, "kb/members/vic/2.md", "note")
	require.Empty(t, v.Refused, "control: the same commit as vic")
}
