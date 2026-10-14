package store

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
)

// artFleet is a fleet repository judging itself, with a PKI on main: the
// root, CRL Number 1, leaves for the two active members boss and vic, and the
// two ledgers. Every candidate commit below is by boss, signed by boss, whom
// the signer rules and rule B admit, so a refusal is the artifact rules'.
type artFleet struct {
	t         *testing.T
	p         *testFleetPKI
	f         *rangeFixture
	main      *object.Commit
	base      map[string]string
	boss, vic ssh.Signer
	l         fact.PKILayout
	ledger    string
}

func newArtFleet(t *testing.T) *artFleet {
	t.Helper()
	p := newTestFleetPKI(t)
	boss, vic := namedSigner(t, "boss"), namedSigner(t, "vic")
	bl, _ := p.leaf("boss", boss)
	vl, _ := p.leaf("vic", vic)
	a := &artFleet{t: t, p: p, boss: boss, vic: vic, l: presetLayout(t), ledger: "artifacts/fleet/issued.jsonl"}
	arts := p.artifacts(p.crl(), map[string]string{"boss": bl, "vic": vl})
	arts["kb/members/vic/1.md"] = memberFile("vic", fact.MemberActive, vic.PublicKey())
	arts[a.ledger] = "{\"serial\":\"1\"}\n{\"serial\":\"2\"}\n"
	arts["artifacts/fleet/revoked.jsonl"] = "{\"serial\":\"0\"}\n"
	a.f, a.main, a.base = selfRegFixture(t, arts)
	return a
}

// cand puts one commit by author/signer on "cand", base + kv on top of main
// (a "" value deletes the path), and returns the verdict of the fleet's own
// gate at main.
func (a *artFleet) cand(author string, signer ssh.Signer, kv ...string) RangeVerdict {
	a.t.Helper()
	c := a.f.commit(a.f.fleet, with(a.base, kv...), author, signer, a.main)
	a.f.ref(a.f.fleet, "cand", c)
	return a.f.checkSelf("")
}

func requireArtifactRefusal(t *testing.T, v RangeVerdict, rule, reason, name string) {
	t.Helper()
	require.Equal(t, 1, v.Checked, name)
	r := onlyRefusal(t, v)
	require.Equal(t, rule, r.Rule, "%s: %s", name, r.Reason)
	require.Contains(t, r.Reason, reason, name)
}

// The review's probes P1–P3 and the deletions and corruptions of the trust
// files, each as boss — an active, certified member whose commit every other
// rule admits. Each must be refused by the named artifact rule. The positive
// controls (an append to a ledger, a write under fleet/ within the cap) are
// admitted, so the refusals are not the fixture's doing.
//
// SABOTAGE: make artifactRules return true → every refusal row is admitted →
// red.
func TestArtifactRules_TrustFilesAreProtected(t *testing.T) {
	a := newArtFleet(t)
	rogue := newTestFleetPKI(t)
	rogueBoss, _ := rogue.leaf("boss", a.boss)
	stranger := namedSigner(t, "stranger")
	rogueStranger, _ := rogue.leaf("stranger", stranger)
	newer := a.p.crl() // Number 2

	type row struct {
		name   string
		kv     []string
		rule   string // "" = admitted
		reason string
	}
	rows := []row{
		// P1 as the review probed it: the first path the diff meets (sorted:
		// certs/ before root.crt) is boss's leaf, which no rogue leaf renews.
		{"P1 rogue root, CRL and leaves", []string{
			a.l.Root, rogue.rootPEM(), a.l.CRL, rogue.crl(), a.l.LeafPath("boss"), rogueBoss, a.l.LeafPath("stranger"), rogueStranger,
		}, RuleArtifactReplace, "certs/boss.pem is replaced but replace_when leaf-renewal does not hold"},
		{"P1 rogue root alone", []string{a.l.Root, rogue.rootPEM()}, RuleArtifactWriteOnce, "write-once"},
		{"P2 root deleted", []string{a.l.Root, ""}, RuleArtifactWriteOnce, "is deleted"},
		{"root corrupted", []string{a.l.Root, "junk"}, RuleArtifactWriteOnce, "write-once"},
		{"CRL deleted", []string{a.l.CRL, ""}, RuleArtifactReplace, "is deleted"},
		{"CRL corrupted", []string{a.l.CRL, "junk"}, RuleArtifactReplace, "no X509 CRL PEM block"},
		{"CRL from another root", []string{a.l.CRL, rogue.crl()}, RuleArtifactReplace, "not signed by the fleet root"},
		{"leaf deleted", []string{a.l.LeafPath("vic"), ""}, RuleArtifactReplace, "is deleted"},
		{"leaf corrupted", []string{a.l.LeafPath("vic"), "junk"}, RuleArtifactReplace, "replace_when leaf-renewal does not hold"},
		{"ledger truncated", []string{a.ledger, "{\"serial\":\"1\"}\n"}, RuleArtifactAppendOnly, "does not start with the old"},
		{"ledger rewritten", []string{a.ledger, "{\"serial\":\"9\"}\n{\"serial\":\"2\"}\n{\"serial\":\"3\"}\n"}, RuleArtifactAppendOnly, "does not start with the old"},
		{"ledger deleted", []string{a.ledger, ""}, RuleArtifactAppendOnly, "is deleted"},
		{"size cap", []string{"artifacts/fleet/notes.txt", strings.Repeat("x", 16<<10+1)}, RuleArtifactSize, "larger than its max_size of 16384"},
		{"control: ledger appended", []string{a.ledger, a.base[a.ledger] + "{\"serial\":\"3\"}\n"}, "", ""},
		{"control: a file within the cap", []string{"artifacts/fleet/notes.txt", strings.Repeat("x", 16<<10)}, "", ""},
		{"control: a newer CRL", []string{a.l.CRL, newer}, "", ""},
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

	requireArtifactRefusal(t, a.cand("boss", a.boss, a.l.CRL, n1), RuleArtifactReplace, "is not greater than the 2 it replaces", "older CRL")
	a.p.crlN-- // sign another Number 2, listing a serial so its bytes differ
	requireArtifactRefusal(t, a.cand("boss", a.boss, a.l.CRL, a.p.crl(big.NewInt(77))), RuleArtifactReplace, "rollback", "same Number")
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
	require.Equal(t, []string{RuleArtifactReplace}, rules, "%+v", vm)
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
		{"plant over vic's leaf", RuleArtifactReplace, `names agent "boss", but the file is for "vic"`, []string{a.l.LeafPath("vic"), bossLeaf}},
		{"plant where none was", RuleArtifactPKI, `names agent "boss", but the file is for "newbie"`, []string{a.l.LeafPath("newbie"), bossLeaf}},
		{"older renewal", RuleArtifactReplace, "is not later than", []string{a.l.LeafPath("vic"), older}},
		{"renewal for another key", RuleArtifactReplace, "not the current key of agent \"vic\"", []string{a.l.LeafPath("vic"), otherKey}},
		{"revoked renewal", RuleArtifactReplace, "revoked", []string{a.l.LeafPath("vic"), revoked, a.l.CRL, a.p.crl(revSerial)}},
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
	l := presetLayout(t)
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
		requireArtifactRefusal(t, g.checkSelf(""), RuleArtifactPKI, "does not hold", name)
	}

	h, hmain, hbase := selfRegFixture(t, nil)
	rootOnly := with(hbase, l.Root, p.rootPEM())
	h.ref(h.fleet, "cand", h.commit(h.fleet, rootOnly, "boss", boss, hmain))
	require.Empty(t, h.checkSelf("").Refused, "the root alone is admitted (a first add)")
	hmain = h.fleetMain(rootOnly)
	h.ref(h.fleet, "cand", h.commit(h.fleet, with(rootOnly, "kb/members/boss/2.md", "note"), "boss", boss, hmain))
	r := onlyRefusal(t, h.checkSelf(""))
	require.Equal(t, RuleNoCert, r.Rule, "with the root on main, a member with no CRL and no leaf is refused")
}

// An oversized trust file at the fleet revision is never read whole: the
// reader treats it as unusable and rule B fails closed.
func TestFleetPKI_ReadsAreBounded(t *testing.T) {
	p := newTestFleetPKI(t)
	x := namedSigner(t, "x")
	l := presetLayout(t)
	leaf, _ := p.leaf("agent-x", x)
	f := newRangeFixture(t)
	arts := p.artifacts(p.crl(), map[string]string{"agent-x": leaf + strings.Repeat("\n", 16<<10)})
	f.abFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())}, arts)
	f.kbWith(VerifyEnforce, "agent-x", x)
	v, err := f.check(VerifyEnforce)
	require.NoError(t, err)
	r := onlyRefusal(t, v)
	require.Equal(t, RuleNoCert, r.Rule)
	require.Contains(t, r.Reason, l.LeafPath("agent-x")+": larger than its max_size")
}

// Log mode reports an artifact refusal and blocks nothing; off judges
// nothing. Under enforce the same commit is exit 1.
func TestArtifactRules_FollowTheMode(t *testing.T) {
	for _, mode := range []string{VerifyEnforce, VerifyLog, VerifyOff} {
		a := newArtFleet(t)
		ont := strings.Replace(a.base[fact.OntologyFile], "verify_signatures: enforce", "verify_signatures: "+mode, 1)
		require.Contains(t, ont, "verify_signatures: "+mode, "fixture: the mode line is in the preset")
		a.base = with(a.base, fact.OntologyFile, ont)
		a.main = a.f.fleetMain(a.base)
		v := a.cand("boss", a.boss, a.l.Root, "")
		switch mode {
		case VerifyEnforce:
			require.Equal(t, RuleArtifactWriteOnce, onlyRefusal(t, v).Rule)
			require.Equal(t, 1, v.ExitCode())
		case VerifyLog:
			require.Equal(t, RuleArtifactWriteOnce, onlyRefusal(t, v).Rule)
			require.Equal(t, 0, v.ExitCode())
		case VerifyOff:
			require.Equal(t, 0, v.Checked)
			require.Empty(t, v.Refused)
		}
	}
}
