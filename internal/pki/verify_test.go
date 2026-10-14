package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fleet is one root with a current, empty CRL: the fixture every
// verification test starts from.
type fleet struct {
	dir  string
	root Root
	crl  *x509.RevocationList
}

func newFleet(t *testing.T) fleet {
	t.Helper()
	dir, root := newTestRoot(t)
	if _, err := IssueCRL(dir, root, nil, big.NewInt(1), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	crl, err := LoadCRL(filepath.Join(dir, CRLFile))
	if err != nil {
		t.Fatal(err)
	}
	return fleet{dir: dir, root: root, crl: crl}
}

// issue is the real issuance path.
func (f fleet) issue(t *testing.T, email string) (*x509.Certificate, ed25519.PublicKey) {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	p, _, err := IssueInstance(f.dir, f.root, pub, email, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return parsePEMCert(t, p), pub
}

// crafted signs an arbitrary leaf with the fleet root, carrying exactly these
// email SANs and URIs, so a test can express names IssueLeaf never writes.
func (f fleet) crafted(t *testing.T, pub crypto.PublicKey, emails []string, uris ...string) *x509.Certificate {
	t.Helper()
	serial, _ := randomSerial()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "crafted"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		EmailAddresses:        emails,
	}
	for _, u := range uris {
		pu, err := url.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = append(tmpl.URIs, pu)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, f.root.Cert, pub, f.root.Signer)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("crafted certificate did not parse: %v", err)
	}
	return c
}

func (f fleet) verify(c *x509.Certificate, usage Usage) (Identity, error) {
	return VerifyInstanceChain(c, nil, f.root.Cert, f.crl, time.Now(), usage)
}

// Positive control, and the issue-then-parse round trip for an instance and
// an operator certificate, in both directions.
func TestVerifyInstanceChain_RoundTripsInstanceAndOperator(t *testing.T) {
	f := newFleet(t)
	for _, c := range []struct {
		email, agentID, kind string
	}{
		{"laptop-ab12cd34@agents.knomit.io", "laptop-ab12cd34", "instance"},
		{"ops@example.com", "", "operator"},
		{"", "", "operator"},
	} {
		leaf, pub := f.issue(t, c.email)
		for _, usage := range []Usage{UsageClient, UsageServer} {
			id, err := f.verify(leaf, usage)
			if err != nil {
				t.Fatalf("%q/%v: %v", c.email, usage, err)
			}
			if id.Fingerprint != Fingerprint(pub) || len(id.Fingerprint) != 64 {
				t.Fatalf("Fingerprint %q, want full %q", id.Fingerprint, Fingerprint(pub))
			}
			if id.Email != c.email || id.AgentID != c.agentID || id.PrincipalKind() != c.kind {
				t.Fatalf("%q: email %q agent %q kind %q", c.email, id.Email, id.AgentID, id.PrincipalKind())
			}
			if id.Serial.Cmp(leaf.SerialNumber) != 0 || !id.NotAfter.Equal(leaf.NotAfter) {
				t.Fatal("serial/NotAfter not carried")
			}
		}
		if len(leaf.URIs) != 0 {
			t.Fatalf("%q: the leaf carries URIs %v; a certificate carries no knomit:// name (R1)", c.email, leaf.URIs)
		}
	}
}

// R3: instance versus operator is decided by the email SAN ALONE. Exactly one
// email SAN whose domain is exactly agents.knomit.io and whose local part,
// +<op> dropped, is a valid (lower-case) agent id is an instance; anything
// else is an operator. Nothing is case folded. A URI SAN (such as knomit://instance/<label>) makes nothing an
// instance. The principal is the key's fingerprint in every case.
func TestIdentityOf_KindFromTheEmailSANAlone(t *testing.T) {
	f := newFleet(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	for _, c := range []struct {
		name    string
		emails  []string
		uris    []string
		agentID string // "" = operator
	}{
		{"agent address", []string{"laptop@agents.knomit.io"}, nil, "laptop"},
		{"op suffix dropped", []string{"laptop+learn@agents.knomit.io"}, nil, "laptop"},
		{"upper-case id", []string{"LapTop@agents.knomit.io"}, nil, ""},
		{"upper-case domain", []string{"laptop@Agents.Knomit.IO"}, nil, ""},
		{"other domain", []string{"laptop@example.com"}, nil, ""},
		{"lookalike suffix", []string{"laptop@agents.knomit.io.example.com"}, nil, ""},
		{"subdomain", []string{"laptop@x.agents.knomit.io"}, nil, ""},
		{"no email SAN", nil, nil, ""},
		{"two emails, one an agent address", []string{"laptop@agents.knomit.io", "ops@example.com"}, nil, ""},
		{"instance URI SAN only", nil, []string{"knomit://instance/laptop"}, ""},
	} {
		leaf := f.crafted(t, pub, c.emails, c.uris...)
		id, err := f.verify(leaf, UsageClient)
		if err != nil {
			t.Fatalf("%s: refused: %v", c.name, err)
		}
		if id.AgentID != c.agentID || id.IsInstance() != (c.agentID != "") {
			t.Fatalf("%s: agent %q instance %v, want agent %q", c.name, id.AgentID, id.IsInstance(), c.agentID)
		}
		if id.Fingerprint != Fingerprint(pub) {
			t.Fatalf("%s: identity %s, want the key's fingerprint", c.name, id.Fingerprint)
		}
	}
}

// R2: an agent id holding '+' (or '@', or white space) is never issued: it
// would read back from <id>@agents.knomit.io as another id.
func TestIssueLeaf_RefusesAnAgentIDThatDoesNotReadBack(t *testing.T) {
	f := newFleet(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	for _, email := range []string{"a+b@agents.knomit.io", "a b@agents.knomit.io", "@agents.knomit.io", "no-at-sign", "x@y@z", "café@example.com",
		"Laptop@agents.knomit.io", "laptop@AGENTS.knomit.io", "laptop@Agents.Knomit.IO"} {
		if _, err := IssueLeaf(f.root, pub, email, time.Hour, time.Now()); err == nil {
			t.Errorf("IssueLeaf(%q) issued", email)
		}
	}
	for _, id := range []string{"a+b", "a@b", "", "a b", "Laptop", "laptop-AB12CD34", "Pauls-MacBook-Pro.local-ab12cd34"} {
		if _, err := AgentEmail(id); !errors.Is(err, ErrAgentIDInvalid) {
			t.Errorf("AgentEmail(%q): err=%v, want ErrAgentIDInvalid", id, err)
		}
		if err := ValidAgentID(id); !errors.Is(err, ErrAgentIDInvalid) {
			t.Errorf("ValidAgentID(%q): err=%v, want ErrAgentIDInvalid", id, err)
		}
		// Every id ValidAgentID refuses is also no agent id when read back
		// from an address: the two agree.
		if id != "" && AgentIDOfEmail(id+"@"+AgentEmailDomain) == id {
			t.Errorf("AgentIDOfEmail reads %q back as itself, but ValidAgentID refuses it", id)
		}
	}
	// Upper case is refused, never folded: the error names it.
	if err := ValidAgentID("Laptop"); err == nil || !strings.Contains(err.Error(), "upper-case") {
		t.Errorf("ValidAgentID(Laptop): %v, want an upper-case refusal", err)
	}
	// Positive control: the plain id issues and reads back as itself.
	email, err := AgentEmail("laptop-ab12cd34")
	if err != nil {
		t.Fatal(err)
	}
	is, err := IssueLeaf(f.root, pub, email, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if id, err := IdentityOf(parsePEMCert(t, is.CertPEM)); err != nil || id.AgentID != "laptop-ab12cd34" || is.Record.Email != email {
		t.Fatalf("round trip: %+v %v record %q", id, err, is.Record.Email)
	}
}

func TestVerifyInstanceChain_SelfSignedIsUntrusted(t *testing.T) {
	f := newFleet(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(5), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, EmailAddresses: []string{"h@agents.knomit.io"}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	c, _ := x509.ParseCertificate(der)
	if _, err := f.verify(c, UsageClient); !errors.Is(err, ErrUntrustedRoot) {
		t.Fatalf("err=%v, want ErrUntrustedRoot", err)
	}
}

func TestVerifyInstanceChain_NotEd25519(t *testing.T) {
	f := newFleet(t)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c := f.crafted(t, &k.PublicKey, []string{"h@agents.knomit.io"})
	if _, err := f.verify(c, UsageClient); !errors.Is(err, ErrNotEd25519) {
		t.Fatalf("err=%v, want ErrNotEd25519", err)
	}
}

func TestVerifyInstanceChain_OtherRootIsUntrusted(t *testing.T) {
	f, other := newFleet(t), newFleet(t)
	c, _ := other.issue(t, "h@agents.knomit.io")
	if _, err := f.verify(c, UsageClient); !errors.Is(err, ErrUntrustedRoot) {
		t.Fatalf("err=%v, want ErrUntrustedRoot", err)
	}
}

func TestVerifyInstanceChain_Expired(t *testing.T) {
	f := newFleet(t)
	c, _ := f.issue(t, "h@agents.knomit.io")
	_, err := VerifyInstanceChain(c, nil, f.root.Cert, f.crl, c.NotAfter.Add(time.Minute), UsageClient)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("err=%v, want ErrExpired", err)
	}
}

func TestVerifyInstanceChain_Revoked(t *testing.T) {
	f := newFleet(t)
	c, _ := f.issue(t, "h@agents.knomit.io")
	if _, err := f.verify(c, UsageClient); err != nil { // positive control: fine before revocation
		t.Fatal(err)
	}
	if _, err := Revoke(f.dir, f.root, Revoked{Serial: c.SerialNumber, At: time.Now()}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	crl, _ := LoadCRL(filepath.Join(f.dir, CRLFile))
	_, err := VerifyInstanceChain(c, nil, f.root.Cert, crl, time.Now(), UsageClient)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("err=%v, want ErrRevoked", err)
	}
}

func TestVerifyInstanceChain_CRLMissingOrForeignFailsClosed(t *testing.T) {
	f, other := newFleet(t), newFleet(t)
	c, _ := f.issue(t, "h@agents.knomit.io")
	if _, err := VerifyInstanceChain(c, nil, f.root.Cert, nil, time.Now(), UsageClient); !errors.Is(err, ErrCRLMissing) {
		t.Fatalf("nil CRL: err=%v, want ErrCRLMissing", err)
	}
	if _, err := VerifyInstanceChain(c, nil, f.root.Cert, other.crl, time.Now(), UsageClient); !errors.Is(err, ErrCRLInvalid) {
		t.Fatalf("foreign CRL: err=%v, want ErrCRLInvalid", err)
	}
}

// The trust anchor is the fleet root ALONE. A peer's own certificates are
// intermediates and must never become roots: a stranger who presents
// [leaf signed by own CA, own CA] is not a fleet member.
//
// SABOTAGE (reviewer's S-D, run when this was written): adding the
// `intermediates` to the `roots` pool in VerifyInstanceChain turns this test
// red. Before this test existed that sabotage left the suite green.
func TestVerifyInstanceChain_PeerSuppliedCAIsNeverATrustAnchor(t *testing.T) {
	fleetF := newFleet(t)
	attacker := newFleet(t) // self-signed CA the fleet never trusted
	leaf, _ := attacker.issue(t, "intruder@agents.knomit.io")
	_, err := VerifyInstanceChain(leaf, []*x509.Certificate{attacker.root.Cert}, fleetF.root.Cert, fleetF.crl, time.Now(), UsageClient)
	if !errors.Is(err, ErrUntrustedRoot) {
		t.Fatalf("leaf + its own CA as an intermediate: err=%v, want ErrUntrustedRoot", err)
	}
	// Positive control: the fixture is well formed — the same leaf verifies
	// when its CA IS the root, so the refusal above is the pool's doing.
	if _, err := VerifyInstanceChain(leaf, nil, attacker.root.Cert, attacker.crl, time.Now(), UsageClient); err != nil {
		t.Fatalf("control: the attacker's leaf does not verify under its own root: %v", err)
	}
}
