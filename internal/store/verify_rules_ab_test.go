package store

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"knomit/internal/store/verify"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/pki"
)

// testFleetPKI is a fleet root that issues leaves and signs CRLs in memory,
// producing the artifact files rules A and B read (D7).
type testFleetPKI struct {
	t    *testing.T
	root pki.Root
	crlN int64
}

func newTestFleetPKI(t *testing.T) *testFleetPKI {
	t.Helper()
	root, err := pki.NewRoot(t.TempDir(), "fleet-test", 10*365*24*time.Hour)
	require.NoError(t, err)
	return &testFleetPKI{t: t, root: root}
}

func edPubOf(t *testing.T, k ssh.Signer) ed25519.PublicKey {
	t.Helper()
	pub, ok := k.PublicKey().(ssh.CryptoPublicKey).CryptoPublicKey().(ed25519.PublicKey)
	require.True(t, ok)
	return pub
}

// leaf issues an instance leaf for k's key, to agent id (its email SAN is
// <id>@agents.knomit.io, R1).
func (p *testFleetPKI) leaf(id string, k ssh.Signer) (string, *big.Int) {
	p.t.Helper()
	return p.leafAt(id, k, time.Now())
}

// leafAt is leaf issued as of now (its NotBefore is now minus the skew).
func (p *testFleetPKI) leafAt(id string, k ssh.Signer, now time.Time) (string, *big.Int) {
	p.t.Helper()
	email, err := pki.AgentEmail(id)
	require.NoError(p.t, err)
	is, err := pki.IssueLeaf(p.root, edPubOf(p.t, k), email, 365*24*time.Hour, now)
	require.NoError(p.t, err)
	serial, ok := new(big.Int).SetString(is.Record.Serial, 16)
	require.True(p.t, ok)
	return string(is.CertPEM), serial
}

// crl signs a CRL listing serials, with a fresh, higher Number.
func (p *testFleetPKI) crl(serials ...*big.Int) string {
	p.t.Helper()
	p.crlN++
	var rs []pki.Revoked
	for _, s := range serials {
		rs = append(rs, pki.Revoked{Serial: s, At: time.Now()})
	}
	raw, err := pki.SignCRL(p.root, rs, big.NewInt(p.crlN), time.Now(), time.Now().Add(30*24*time.Hour))
	require.NoError(p.t, err)
	return string(raw)
}

func (p *testFleetPKI) rootPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.root.Cert.Raw}))
}

// revocation is a root-signed revocation statement file for serial.
func (p *testFleetPKI) revocation(serial *big.Int, reason string) string {
	p.t.Helper()
	id, err := pki.RootID(p.root.Cert)
	require.NoError(p.t, err)
	raw, err := pki.SignRevocation(p.root, pki.RevocationStatement{Root: id, Serial: serial, At: time.Now().UTC().Truncate(time.Second), Reason: reason})
	require.NoError(p.t, err)
	return string(raw)
}

// craftedLeaf is a leaf the root signs for k's key carrying exactly this
// email SAN, so a test can express an address IssueLeaf refuses to write
// (an upper-case agent id).
func (p *testFleetPKI) craftedLeaf(email string, k ssh.Signer) string {
	p.t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: email},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		EmailAddresses:        []string{email},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.root.Cert, edPubOf(p.t, k), p.root.Signer)
	require.NoError(p.t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// fleetLayout is where the fleet ontology declares the certificate artifacts
// (pki: root, crl, leaf): the tests take the paths from the declaration, as
// the reader does.
func fleetLayout(t *testing.T) fact.PKILayout {
	t.Helper()
	l, ok := fact.FleetOntology().PKILayout()
	require.True(t, ok)
	return l
}

// artifacts is the artifact files at the preset's declared paths: the root,
// the CRL (crl "" = absent) and one leaf per id.
func (p *testFleetPKI) artifacts(crl string, leaves map[string]string) map[string]string {
	l := fleetLayout(p.t)
	out := map[string]string{l.Root: p.rootPEM()}
	if crl != "" {
		out[l.CRL] = crl
	}
	for id, leaf := range leaves {
		out[l.LeafPath(id)] = leaf
	}
	return out
}

// fleetEnforceOntology is the fleet ontology as it ships (R7): verify_signatures
// enforce, and the artifacts section declaring the PKI and its rules.
var fleetEnforceOntology = func() string {
	out, err := fact.FleetOntology().Serialize()
	if err != nil {
		panic(err)
	}
	return string(out)
}()

// fleetMain commits files (complete tree) onto the fleet repository's main,
// chaining onto the previous main, and returns the commit.
func (f *rangeFixture) fleetMain(files map[string]string) *object.Commit {
	f.t.Helper()
	var parents []*object.Commit
	if ref, err := f.fleet.Storer.Reference(plumbing.NewBranchReferenceName("main")); err == nil {
		prev, err := object.GetCommit(f.fleet.Storer, ref.Hash())
		require.NoError(f.t, err)
		parents = append(parents, prev)
	}
	c := f.commit(f.fleet, files, "", nil, parents...)
	f.ref(f.fleet, "main", c)
	return c
}

// checkSelf runs CheckRange with the fleet repository as BOTH the knowledge
// base and the fleet (a fleet's own merge job: --dir . --fleet .), candidate
// "cand", fleet revision fleetRev ("" = main).
func (f *rangeFixture) checkSelf(fleetRev string) verify.RangeVerdict {
	f.t.Helper()
	if fleetRev == "" {
		fleetRev = "main"
	}
	v, err := verify.CheckRange(verify.RangeInput{KBDir: f.fleetDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: fleetRev})
	require.NoError(f.t, err)
	return v
}

// selfRegFixture: a fleet with enforce, one active member "boss", and the
// given artifacts on main; returns the fixture and main.
func selfRegFixture(t *testing.T, artifacts map[string]string) (*rangeFixture, *object.Commit, map[string]string) {
	t.Helper()
	f := newRangeFixture(t)
	boss := namedSigner(t, "boss")
	files := map[string]string{
		fact.OntologyFile:      fleetEnforceOntology,
		"kb/members/boss/1.md": memberFile("boss", fact.MemberActive, boss.PublicKey()),
	}
	for p, c := range artifacts {
		files[p] = c
	}
	return f, f.fleetMain(files), files
}

// selfReg puts on "cand" one commit by id, signed by k, that adds id's own
// record (and the extra kv files) on top of main.
func (f *rangeFixture) selfReg(main *object.Commit, base map[string]string, id string, k ssh.Signer, extra ...string) *object.Commit {
	f.t.Helper()
	kv := append([]string{"kb/members/" + id + "/1.md", memberFile(id, fact.MemberActive, k.PublicKey())}, extra...)
	c := f.commit(f.fleet, with(base, kv...), id, k, main)
	f.ref(f.fleet, "cand", c)
	return c
}

func onlyRefusal(t *testing.T, v verify.RangeVerdict) verify.Refusal {
	t.Helper()
	require.Len(t, v.Refused, 1, "%+v", v)
	return v.Refused[0]
}

// Rule A admits a self-registration backed by a matching, unrevoked leaf that
// chains to the root — the bootstrap under enforce. Without root.crt the SAME
// commit is no-record: the admission is rule A's, nothing else's.
//
// SABOTAGE: make selfRegistration return refusal unconditionally → red.
func TestRuleA_AdmittedWithMatchingCert(t *testing.T) {
	p := newTestFleetPKI(t)
	j := namedSigner(t, "joiner")
	leaf, _ := p.leaf("joiner", j)
	f, main, base := selfRegFixture(t, p.artifacts(p.crl(), map[string]string{"joiner": leaf}))
	f.selfReg(main, base, "joiner", j)
	v := f.checkSelf("")
	require.Equal(t, verify.VerifyEnforce, v.Mode)
	require.Equal(t, 1, v.Checked)
	require.Empty(t, v.Refused, "a self-registration with a valid leaf artifact is admitted")
	require.Equal(t, 0, v.ExitCode())

	g, gmain, gbase := selfRegFixture(t, nil)
	g.selfReg(gmain, gbase, "joiner", j)
	r := onlyRefusal(t, g.checkSelf(""))
	require.Equal(t, verify.RuleNoRecord, r.Rule, "no PKI: today's rule")
	require.NotContains(t, r.Reason, "self-registration", "no PKI: rule A is not even consulted")
}

// No leaf artifact for the id: no-record, saying why rule A did not admit.
func TestRuleA_NoCertIsNoRecord(t *testing.T) {
	p := newTestFleetPKI(t)
	j := namedSigner(t, "joiner")
	f, main, base := selfRegFixture(t, p.artifacts(p.crl(), nil))
	f.selfReg(main, base, "joiner", j)
	r := onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, verify.RuleNoRecord, r.Rule)
	require.Contains(t, r.Reason, "no artifacts/fleet/certs/joiner.pem at the fleet revision")
}

// A leaf whose serial is on the CRL: refused as revoked.
//
// SABOTAGE: drop the CRL from VerifyInstanceChain's input (pass a CRL with no
// entries) → admitted → red.
func TestRuleA_RevokedSerialRefused(t *testing.T) {
	p := newTestFleetPKI(t)
	j := namedSigner(t, "joiner")
	leaf, serial := p.leaf("joiner", j)
	f, main, base := selfRegFixture(t, p.artifacts(p.crl(serial), map[string]string{"joiner": leaf}))
	f.selfReg(main, base, "joiner", j)
	r := onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, verify.RuleRevoked, r.Rule)
	require.Contains(t, r.Reason, serial.Text(16))
}

// A leaf for ANOTHER key than the signing key: key-mismatch, even with no
// record (the leaf names a key, and it is not this one).
//
// SABOTAGE: drop the SameKey comparison in leafFor → admitted → red.
func TestRuleA_CertForOtherKeyIsKeyMismatch(t *testing.T) {
	p := newTestFleetPKI(t)
	j, other := namedSigner(t, "joiner"), namedSigner(t, "other")
	leaf, _ := p.leaf("joiner", other)
	f, main, base := selfRegFixture(t, p.artifacts(p.crl(), map[string]string{"joiner": leaf}))
	f.selfReg(main, base, "joiner", j)
	r := onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, verify.RuleKeyMismatch, r.Rule)
	require.Contains(t, r.Reason, "for another key than the signing key")
}

// A leaf for the right key that some OTHER root signed: refused; it does not
// chain to root.crt.
//
// SABOTAGE: verify the chain against the leaf's own issuer instead of
// root.crt → admitted → red.
func TestRuleA_CertNotChainingRefused(t *testing.T) {
	p, rogue := newTestFleetPKI(t), newTestFleetPKI(t)
	j := namedSigner(t, "joiner")
	leaf, _ := rogue.leaf("joiner", j)
	f, main, base := selfRegFixture(t, p.artifacts(p.crl(), map[string]string{"joiner": leaf}))
	f.selfReg(main, base, "joiner", j)
	r := onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, verify.RuleNoRecord, r.Rule)
	require.Contains(t, r.Reason, "does not chain to the fleet root")
}

// The shape of rule A is the record-owner rule: every changed path outside
// artifacts/ under members/<author id>/. A commit that also touches anything
// else, one that writes ANOTHER id's folder, and one that touches a dot path,
// are judged by the old rules only — with a valid leaf present, so a broken
// shape check admits. (Artifact paths are the artifact rules':
// TestRecordOwner_ArtifactPathsAreTheArtifactRules.)
//
// SABOTAGE: make recordOwner return ok → all three admitted → red.
func TestRuleA_ShapeOnlyOwnMembersFolder(t *testing.T) {
	p := newTestFleetPKI(t)
	j := namedSigner(t, "joiner")
	leaf, _ := p.leaf("joiner", j)
	arts := p.artifacts(p.crl(), map[string]string{"joiner": leaf})
	cases := map[string][]string{
		"also outside":      {"kb/members/joiner/1.md", "x", "kb/notes/evil.md", "x"},
		"other id's folder": {"kb/members/boss/2.md", "x"},
		"dot path":          {"kb/members/joiner/1.md", "x", ".gitignore", "x"},
	}
	ran := 0
	for name, kv := range cases {
		f, main, base := selfRegFixture(t, arts)
		c := f.commit(f.fleet, with(base, kv...), "joiner", j, main)
		f.ref(f.fleet, "cand", c)
		r := onlyRefusal(t, f.checkSelf(""))
		require.Equal(t, verify.RuleNoRecord, r.Rule, name)
		require.NotContains(t, r.Reason, "self-registration", "%s: rule A must not be consulted", name)
		ran++
	}
	require.Equal(t, 3, ran)
}

// A merge is a self-registration only when its diff against EVERY parent is
// under members/<id>/. Positive control: a side commit and a merge that only
// add the joiner's records are both admitted. Negative: a side commit that
// also touches a path outside makes the merge's diff against main leave the
// folder, so the merge is no-record (and the side commit too).
//
// SABOTAGE: compare against the first parent only → the negative merge is
// admitted → red.
func TestRuleA_MergeDiffAgainstEveryParent(t *testing.T) {
	p := newTestFleetPKI(t)
	j := namedSigner(t, "joiner")
	leaf, _ := p.leaf("joiner", j)
	arts := p.artifacts(p.crl(), map[string]string{"joiner": leaf})

	rec1 := memberFile("joiner", fact.MemberActive, j.PublicKey())
	rec2 := rec1 + "\nsecond version\n"

	f, main, base := selfRegFixture(t, arts)
	side := f.commit(f.fleet, with(base, "kb/members/joiner/1.md", rec1), "joiner", j, main)
	m := f.commit(f.fleet, with(base, "kb/members/joiner/1.md", rec2), "joiner", j, main, side)
	f.ref(f.fleet, "cand", m)
	v := f.checkSelf("")
	require.Equal(t, 2, v.Checked)
	require.Empty(t, v.Refused, "positive control: both stay inside the folder")

	g, gmain, gbase := selfRegFixture(t, arts)
	gside := g.commit(g.fleet, with(gbase, "kb/members/joiner/1.md", rec1, "kb/notes/x.md", "x"), "joiner", j, gmain)
	// Against gside (the FIRST parent) the merge only rewrites 1.md (inside);
	// against main (the second) it also brings x.md (outside). It is not M3
	// clean (it rewrites 1.md beyond both parents), so the rules judge it.
	gm := g.commit(g.fleet, with(gbase, "kb/members/joiner/1.md", rec2, "kb/notes/x.md", "x"), "joiner", j, gside, gmain)
	g.ref(g.fleet, "cand", gm)
	v = g.checkSelf("")
	require.Equal(t, 2, v.Checked)
	rules := map[string]string{}
	for _, r := range v.Refused {
		rules[r.Commit] = r.Rule
	}
	require.Equal(t, verify.RuleNoRecord, rules[gm.Hash.String()], "the merge leaves the folder against main")
	require.Equal(t, verify.RuleNoRecord, rules[gside.Hash.String()])
}

// R4 (review F2), as clarified by O3: rule A runs only when the id has NO
// active record AND the signing key is not the current key of a record for a
// DIFFERENT id. The id's OWN left record holding the same key does not block a
// rejoin: leave = revoke, so a same-key rejoin needs a fresh leaf the master
// issued after the leave, and that fresh leaf is the approval. Every case has
// a leaf for the signing key issued to "joiner", so a missing gate admits:
//   - a LEFT record under an OLD key: admitted (a rejoin under a new key);
//   - an ACTIVE record under another key: refused key-mismatch (the F2
//     takeover: an active record is never overwritten through rule A);
//   - a left record under the SAME key, the old leaf on the CRL and a fresh
//     leaf at certs/joiner.pem: admitted (the same-key rejoin);
//   - the same, but certs/joiner.pem is still the revoked leaf: refused
//     revoked by rule A itself (no fresh certificate, no rejoin);
//   - no record, but the signing key is boss's (active) current key: refused
//     no-record (one key never becomes the current key of two records);
//   - no record, but the signing key is the current key of ghost's LEFT
//     record: refused no-record (another id's record, of any state, blocks).
//
// SABOTAGE: make mayRegister return true → the gate rows are admitted → red;
// restore the any-record key check (len(byKey) == 0) → the same-key rejoin is
// refused inactive → red.
func TestRuleA_OnlyWithoutAnActiveRecordAndForAKeyNoOtherIDHolds(t *testing.T) {
	p := newTestFleetPKI(t)
	j, old, boss := namedSigner(t, "joiner"), namedSigner(t, "old"), namedSigner(t, "boss")
	leaf, _ := p.leaf("joiner", j)
	revokedLeaf, revokedSerial := p.leaf("joiner", j)
	freshLeaf, _ := p.leaf("joiner", j)
	bossLeafAsJoiner, _ := p.leaf("joiner", boss)
	const (
		admitted = iota
		gate     // refused before rule A: rule A is not consulted
		ruleA    // rule A consulted, and it refuses
	)
	joinerRec := func(state string, k ssh.PublicKey) map[string]string {
		return map[string]string{"kb/members/joiner/1.md": memberFile("joiner", state, k)}
	}
	cases := []struct {
		name    string
		records map[string]string // member records on main besides boss's
		leaf    string
		revoked []*big.Int
		signer  ssh.Signer
		want    string
		kind    int
	}{
		{"left record, old key", joinerRec(fact.MemberLeft, old.PublicKey()), leaf, nil, j, "", admitted},
		{"active record, other key", joinerRec(fact.MemberActive, old.PublicKey()), leaf, nil, j, verify.RuleKeyMismatch, gate},
		{"left record, same key, fresh leaf", joinerRec(fact.MemberLeft, j.PublicKey()), freshLeaf, []*big.Int{revokedSerial}, j, "", admitted},
		{"left record, same key, revoked leaf", joinerRec(fact.MemberLeft, j.PublicKey()), revokedLeaf, []*big.Int{revokedSerial}, j, verify.RuleRevoked, ruleA},
		{"no record, key is boss's", nil, bossLeafAsJoiner, nil, boss, verify.RuleNoRecord, gate},
		{"no record, key is another id's left record's",
			map[string]string{"kb/members/ghost/1.md": memberFile("ghost", fact.MemberLeft, j.PublicKey())},
			leaf, nil, j, verify.RuleNoRecord, gate},
	}
	ran := 0
	for _, tc := range cases {
		arts := p.artifacts(p.crl(tc.revoked...), map[string]string{"joiner": tc.leaf})
		for k, v := range tc.records {
			arts[k] = v
		}
		f, main, base := selfRegFixture(t, arts)
		f.selfReg(main, base, "joiner", tc.signer)
		v := f.checkSelf("")
		require.Equal(t, 1, v.Checked, tc.name)
		switch tc.kind {
		case admitted:
			require.Empty(t, v.Refused, tc.name)
		case gate:
			r := onlyRefusal(t, v)
			require.Equal(t, tc.want, r.Rule, tc.name)
			require.NotContains(t, r.Reason, "self-registration", "%s: rule A must not even be consulted", tc.name)
		case ruleA:
			r := onlyRefusal(t, v)
			require.Equal(t, tc.want, r.Rule, tc.name)
			require.Contains(t, r.Reason, "self-registration not admitted", "%s: rule A must be the one refusing", tc.name)
		}
		ran++
	}
	require.Equal(t, 6, ran)
}

// R2 (review P4): a leaf the root really issued, but to ANOTHER agent id,
// planted at this id's path, vouches for nothing — the email SAN id must equal
// the id. Rule B refuses the active member it would otherwise certify, and
// rule A refuses a self-registration on it.
//
// SABOTAGE: drop the AgentID comparison in leafFor → both admitted → red.
func TestRulesAB_LeafIssuedToAnotherIDIsRefused(t *testing.T) {
	p := newTestFleetPKI(t)
	x, j := namedSigner(t, "x"), namedSigner(t, "joiner")
	xAsBoss, _ := p.leaf("boss", x)
	f := newRangeFixture(t)
	f.abFleet(map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())},
		p.artifacts(p.crl(), map[string]string{"agent-x": xAsBoss}))
	f.kbWith(verify.VerifyEnforce, "agent-x", x)
	v, err := f.check(verify.VerifyEnforce)
	require.NoError(t, err)
	r := onlyRefusal(t, v)
	require.Equal(t, verify.RuleNoCert, r.Rule)
	require.Contains(t, r.Reason, `names agent "boss", not "agent-x"`)

	jAsOther, _ := p.leaf("other", j)
	g, gmain, gbase := selfRegFixture(t, p.artifacts(p.crl(), map[string]string{"joiner": jAsOther}))
	g.selfReg(gmain, gbase, "joiner", j)
	r = onlyRefusal(t, g.checkSelf(""))
	require.Equal(t, verify.RuleNoRecord, r.Rule)
	require.Contains(t, r.Reason, `names agent "other", not "joiner"`)
}

// Rule A exists only in the fleet repository: the same self-registration
// shape in an ordinary knowledge base (judged against the fleet) is
// no-record.
func TestRuleA_OnlyInTheFleetRepository(t *testing.T) {
	p := newTestFleetPKI(t)
	j := namedSigner(t, "joiner")
	leaf, _ := p.leaf("joiner", j)
	f := newRangeFixture(t)
	files := map[string]string{fact.OntologyFile: fleetOntologyYAML(t)}
	for k, v := range p.artifacts(p.crl(), map[string]string{"joiner": leaf}) {
		files[k] = v
	}
	f.fleetMain(files)
	base := map[string]string{fact.OntologyFile: kbOntology(verify.VerifyEnforce)}
	root := f.commit(f.kb, base, "", nil)
	f.ref(f.kb, "main", root)
	f.ref(f.kb, "cand", f.commit(f.kb, with(base, "kb/members/joiner/1.md", "x"), "joiner", j, root))
	v, err := f.check(verify.VerifyEnforce)
	require.NoError(t, err)
	r := onlyRefusal(t, v)
	require.Equal(t, verify.RuleNoRecord, r.Rule)
	require.NotContains(t, r.Reason, "self-registration")
}

// D18: nothing is derived from the id's text. An id ending in an 8-hex
// suffix that is NOT the key's fingerprint is admitted on a leaf for the
// signing key; an id ending in the signing key's REAL fp8 with a leaf for
// another key is refused. Only the keys decide.
func TestRuleA_IDTextMeansNothing(t *testing.T) {
	p := newTestFleetPKI(t)
	j, other := namedSigner(t, "joiner"), namedSigner(t, "other")
	fp8 := pki.Short(pki.Fingerprint(edPubOf(t, j)))
	misleading := "joiner-deadbeef"
	require.NotEqual(t, "deadbeef", fp8)

	leaf, _ := p.leaf(misleading, j)
	f, main, base := selfRegFixture(t, p.artifacts(p.crl(), map[string]string{misleading: leaf}))
	f.selfReg(main, base, misleading, j)
	require.Empty(t, f.checkSelf("").Refused, "a misleading suffix changes nothing")

	honest := "joiner-" + fp8
	oleaf, _ := p.leaf(honest, other)
	g, gmain, gbase := selfRegFixture(t, p.artifacts(p.crl(), map[string]string{honest: oleaf}))
	g.selfReg(gmain, gbase, honest, j)
	require.Equal(t, verify.RuleKeyMismatch, onlyRefusal(t, g.checkSelf("")).Rule, "a matching suffix admits nothing")
}

// An unreadable CRL fails rule A CLOSED (never admitted on a CRL nobody can
// read), and rule B too: an active member cannot show an unrevoked leaf.
func TestRulesAB_UnreadableCRL(t *testing.T) {
	p := newTestFleetPKI(t)
	j := namedSigner(t, "joiner")
	leaf, _ := p.leaf("joiner", j)
	f, main, base := selfRegFixture(t, p.artifacts("not a crl", map[string]string{"joiner": leaf}))
	f.selfReg(main, base, "joiner", j)
	r := onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, verify.RuleNoRecord, r.Rule)
	require.Contains(t, r.Reason, "CRL invalid")

	g, gmain, gbase := selfRegFixture(t, with(p.artifacts("", map[string]string{"joiner": leaf}),
		"kb/members/joiner/1.md", memberFile("joiner", fact.MemberActive, j.PublicKey())))
	g.ref(g.fleet, "cand", g.commit(g.fleet, with(gbase, "kb/members/joiner/1.md", "v2"), "joiner", j, gmain))
	r = onlyRefusal(t, g.checkSelf(""))
	require.Equal(t, verify.RuleNoCert, r.Rule, "an active member with no CRL at all is refused under PKI")
	require.Contains(t, r.Reason, "CRL missing")
}

// abFleet writes a separate fleet repository (members + artifacts) as the
// next version of its main and returns that commit.
func (f *rangeFixture) abFleet(members map[string]string, artifacts map[string]string) *object.Commit {
	f.t.Helper()
	all := map[string]string{fact.OntologyFile: fleetOntologyYAML(f.t)}
	for p, c := range members {
		all["kb/members/"+p] = c
	}
	for p, c := range artifacts {
		all[p] = c
	}
	return f.fleetMain(all)
}

// Rule B: a normal commit by an active member whose leaf (for the record's
// key, chaining to the root) is on the CRL is refused as revoked. Judged at
// the EARLIER fleet revision, before the CRL listed the serial, the SAME
// commit is accepted.
//
// SABOTAGE: make revoked always accept → the later revision admits → red.
func TestRuleB_RevokedAtLaterFleetRevisionOnly(t *testing.T) {
	p := newTestFleetPKI(t)
	x := namedSigner(t, "x")
	leaf, serial := p.leaf("agent-x", x)
	f := newRangeFixture(t)
	rec := map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())}
	before := f.abFleet(rec, p.artifacts(p.crl(), map[string]string{"agent-x": leaf}))
	after := f.abFleet(rec, p.artifacts(p.crl(serial), map[string]string{"agent-x": leaf}))
	f.kbWith(verify.VerifyEnforce, "agent-x", x)

	at := func(rev *object.Commit) verify.RangeVerdict {
		v, err := verify.CheckRange(verify.RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: rev.Hash.String()})
		require.NoError(t, err)
		return v
	}
	r := onlyRefusal(t, at(after))
	require.Equal(t, verify.RuleRevoked, r.Rule)
	require.Contains(t, r.Reason, serial.Text(16))
	v := at(before)
	require.Equal(t, 1, v.Checked)
	require.Empty(t, v.Refused, "before the CRL listed it, the same commit is accepted")
}

// Rule B fails CLOSED once the fleet has a PKI: an active member's commit
// needs a leaf for its id that names the record's key, chains to the root
// and is not revoked. A leaf for ANOTHER key is key-mismatch (even when that
// other leaf is the revoked one); a leaf a rogue root signed is no-cert; no
// leaf at all is no-cert. Control: a valid leaf admits.
//
// SABOTAGE: make certified accept when there is no leaf, or drop the key
// comparison or the chain check from leafFor → red.
func TestRuleB_FailsClosedUnderPKI(t *testing.T) {
	p, rogue := newTestFleetPKI(t), newTestFleetPKI(t)
	x, other := namedSigner(t, "x"), namedSigner(t, "other")
	rec := map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())}
	xleaf, _ := p.leaf("agent-x", x)
	oleaf, oserial := p.leaf("agent-x", other)
	rleaf, rserial := rogue.leaf("agent-x", x)
	cases := []struct {
		name   string
		arts   map[string]string
		want   string
		reason string
	}{
		{"valid leaf", p.artifacts(p.crl(), map[string]string{"agent-x": xleaf}), "", ""},
		{"leaf for another key", p.artifacts(p.crl(oserial), map[string]string{"agent-x": oleaf}), verify.RuleKeyMismatch, "for another key than the member record's"},
		{"leaf from a rogue root", p.artifacts(p.crl(rserial), map[string]string{"agent-x": rleaf}), verify.RuleNoCert, "does not chain to the fleet root"},
		{"no leaf", p.artifacts(p.crl(), nil), verify.RuleNoCert, "no artifacts/fleet/certs/agent-x.pem"},
	}
	ran := 0
	for _, tc := range cases {
		f := newRangeFixture(t)
		f.abFleet(rec, tc.arts)
		f.kbWith(verify.VerifyEnforce, "agent-x", x)
		v, err := f.check(verify.VerifyEnforce)
		require.NoError(t, err)
		require.Equal(t, 1, v.Checked, tc.name)
		if tc.want == "" {
			require.Empty(t, v.Refused, tc.name)
		} else {
			r := onlyRefusal(t, v)
			require.Equal(t, tc.want, r.Rule, tc.name)
			require.Contains(t, r.Reason, tc.reason, tc.name)
		}
		ran++
	}
	require.Equal(t, 4, ran)
}

// Rule A binds the record to the leaf: what the self-registration leaves in
// members/<id>/ must be exactly one record, for agent id, active, holding the
// key the leaf names. A record holding ANOTHER key K2 (signed with K1, whose
// leaf is valid) is refused, or K2 would sign as a member that no revocation
// of K1 reaches.
//
// SABOTAGE: make selfRecord return nil → every case admitted → red.
func TestRuleA_RecordMustMatchTheLeaf(t *testing.T) {
	p := newTestFleetPKI(t)
	k1, k2 := namedSigner(t, "joiner"), namedSigner(t, "second")
	leaf, _ := p.leaf("joiner", k1)
	arts := p.artifacts(p.crl(), map[string]string{"joiner": leaf})
	cases := []struct {
		name   string
		kv     []string
		reason string
	}{
		{"record holds another key", []string{"kb/members/joiner/1.md", memberFile("joiner", fact.MemberActive, k2.PublicKey())}, "not the signing key"},
		{"record is not active", []string{"kb/members/joiner/1.md", memberFile("joiner", fact.MemberLeft, k1.PublicKey())}, "may only write \"active\""},
		{"two records", []string{"kb/members/joiner/1.md", memberFile("joiner", fact.MemberActive, k1.PublicKey()),
			"kb/members/joiner/2.md", memberFile("joiner", fact.MemberActive, k1.PublicKey())}, "holds 2 files"},
		{"not a record", []string{"kb/members/joiner/1.md", "junk"}, "does not parse"},
	}
	ran := 0
	for _, tc := range cases {
		f, main, base := selfRegFixture(t, arts)
		f.ref(f.fleet, "cand", f.commit(f.fleet, with(base, tc.kv...), "joiner", k1, main))
		r := onlyRefusal(t, f.checkSelf(""))
		require.Equal(t, verify.RuleNoRecord, r.Rule, tc.name)
		require.Contains(t, r.Reason, "self-registration not admitted", tc.name)
		require.Contains(t, r.Reason, tc.reason, tc.name)
		ran++
	}
	require.Equal(t, 4, ran)

	// A record for another agent in the joiner's folder. Written by the
	// commit, the owner rule refuses it (B1), so rule A keeps the record
	// rules' refusal unchanged. Already on main (the loader skips it) with a
	// commit that changes nothing, the owner rule passes and selfRecord's
	// agent check refuses it.
	stray := memberFile("boss", fact.MemberActive, k1.PublicKey())
	f, main, base := selfRegFixture(t, arts)
	f.ref(f.fleet, "cand", f.commit(f.fleet, with(base, "kb/members/joiner/1.md", stray), "joiner", k1, main))
	r := onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, verify.RuleNoRecord, r.Rule)
	require.Equal(t, `no member record for agent "joiner"`, r.Reason, "the owner rule refused the shape")

	g, gmain, gbase := selfRegFixture(t, with(arts, "kb/members/joiner/1.md", stray))
	g.ref(g.fleet, "cand", g.commit(g.fleet, gbase, "joiner", k1, gmain))
	r = onlyRefusal(t, g.checkSelf(""))
	require.Equal(t, verify.RuleNoRecord, r.Rule)
	require.Contains(t, r.Reason, "self-registration not admitted")
	require.Contains(t, r.Reason, `names agent "boss"`)
}

// No PKI files at all: every old-rule verdict is what it was. The table
// replays the old rules' scenarios against a fleet with NO artifacts, and
// against one where every key has a valid leaf (so rule B is satisfied and
// only the old rules can refuse); both must give the pre-P3 rule. The row
// count is asserted so a skipped row is red.
func TestRulesAB_NoPKIIsToday(t *testing.T) {
	x, y := namedSigner(t, "x"), namedSigner(t, "y")
	p := newTestFleetPKI(t)
	rows := []struct {
		name    string
		members map[string]string
		author  string
		signer  ssh.Signer
		want    string // "" = admitted
	}{
		{"active", map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())}, "agent-x", x, ""},
		{"no record", map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())}, "agent-z", x, verify.RuleNoRecord},
		{"key mismatch", map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())}, "agent-x", y, verify.RuleKeyMismatch},
		{"ambiguous", map[string]string{
			"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey()),
			"agent-x/2.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())}, "agent-x", x, verify.RuleAmbiguous},
		{"duplicate key", map[string]string{
			"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey()),
			"agent-y/1.md": memberFile("agent-y", fact.MemberActive, x.PublicKey())}, "agent-x", x, verify.RuleDuplicateKey},
		{"inactive", map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberLeft, x.PublicKey())}, "agent-x", x, verify.RuleInactive},
		{"unsigned", map[string]string{"agent-x/1.md": memberFile("agent-x", fact.MemberActive, x.PublicKey())}, "agent-x", nil, verify.RuleSignature},
	}
	xl, _ := p.leaf("agent-x", x)
	yl, _ := p.leaf("agent-y", y)
	zl, _ := p.leaf("agent-z", x)
	withLeaves := p.artifacts(p.crl(), map[string]string{"agent-x": xl, "agent-y": yl, "agent-z": zl})
	ran := 0
	for _, row := range rows {
		for _, arts := range []map[string]string{nil, withLeaves} {
			f := newRangeFixture(t)
			f.abFleet(row.members, arts)
			f.kbWith(verify.VerifyEnforce, row.author, row.signer)
			v, err := f.check(verify.VerifyEnforce)
			require.NoError(t, err)
			require.Equal(t, 1, v.Checked, row.name)
			if row.want == "" {
				require.Empty(t, v.Refused, row.name)
			} else {
				require.Equal(t, row.want, onlyRefusal(t, v).Rule, "%s (artifacts: %v)", row.name, arts != nil)
			}
			ran++
		}
	}
	require.Equal(t, 14, ran)
}

// FleetAt's PKI reads nothing when there is no root.crt, and the three paths
// when there is, from the store's own branch.
func TestFleetAt_ReadsPKIArtifacts(t *testing.T) {
	p := newTestFleetPKI(t)
	x := namedSigner(t, "x")
	leaf, serial := p.leaf("agent-x", x)
	h := newFleetPushHost(t, with(p.artifacts(p.crl(serial), map[string]string{"agent-x": leaf})))
	fleet, err := h.svc.FleetAt("main")
	require.NoError(t, err)
	got := fleet.PKI
	require.NotNil(t, got)
	require.NoError(t, got.RootErr)
	require.Equal(t, p.root.Cert.Raw, got.Root.Raw)
	require.NoError(t, got.CRLErr)
	require.True(t, pki.IsRevoked(got.CRL, serial))
	c, err := got.Cert("agent-x")
	require.NoError(t, err)
	require.Equal(t, serial, c.SerialNumber)
	none, err := got.Cert("agent-y")
	require.NoError(t, err)
	require.Nil(t, none)
	// Ids are compared exactly, never case folded: AGENT-X is another id.
	upper, err := got.Cert("AGENT-X")
	require.NoError(t, err)
	require.Nil(t, upper, "no case folding")

	bare := newFleetPushHost(t, nil)
	bareFleet, err := bare.svc.FleetAt("main")
	require.NoError(t, err)
	require.Nil(t, bareFleet.PKI, "no root.crt: no PKI")
}
