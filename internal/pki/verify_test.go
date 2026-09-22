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
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/cryptobyte"
	cbasn1 "golang.org/x/crypto/cryptobyte/asn1"
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
func (f fleet) issue(t *testing.T, host string, role Role) (*x509.Certificate, ed25519.PublicKey) {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	p, _, err := IssueInstance(f.dir, f.root, pub, host, role, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return parsePEMCert(t, p), pub
}

// rawSANExt builds a subjectAltName extension carrying these URIs verbatim,
// so a test can express spellings url.URL would normalise away.
func rawSANExt(uris ...string) pkix.Extension {
	var b cryptobyte.Builder
	b.AddASN1(cbasn1.SEQUENCE, func(b *cryptobyte.Builder) {
		for _, u := range uris {
			b.AddASN1(cbasn1.Tag(6).ContextSpecific(), func(b *cryptobyte.Builder) { b.AddBytes([]byte(u)) })
		}
	})
	return pkix.Extension{Id: oidSubjectAltName, Value: b.BytesOrPanic()}
}

// signCrafted signs an arbitrary leaf with the fleet root. It returns the
// parse error too, because Go refuses some SANs before we ever see them.
func (f fleet) signCrafted(t *testing.T, pub crypto.PublicKey, uris ...string) (*x509.Certificate, error) {
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
	}
	if len(uris) > 0 {
		tmpl.ExtraExtensions = []pkix.Extension{rawSANExt(uris...)}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, f.root.Cert, pub, f.root.Signer)
	if err != nil {
		t.Fatal(err)
	}
	return x509.ParseCertificate(der)
}

// crafted is signCrafted for a certificate Go is expected to parse.
func (f fleet) crafted(t *testing.T, pub crypto.PublicKey, uris ...string) *x509.Certificate {
	t.Helper()
	c, err := f.signCrafted(t, pub, uris...)
	if err != nil {
		t.Fatalf("crafted certificate did not parse: %v", err)
	}
	return c
}

func (f fleet) verify(c *x509.Certificate, usage Usage) (Identity, error) {
	return VerifyInstanceChain(c, nil, f.root.Cert, f.crl, time.Now(), usage)
}

// Positive control, and the issue-then-parse round trip for BOTH roles.
func TestVerifyInstanceChain_RoundTripsBothRoles(t *testing.T) {
	f := newFleet(t)
	for _, role := range []Role{RoleInstance, RoleOperator} {
		c, pub := f.issue(t, "my-lap-top.local", role)
		for _, usage := range []Usage{UsageClient, UsageServer} {
			id, err := f.verify(c, usage)
			if err != nil {
				t.Fatalf("%s/%v: %v", role, usage, err)
			}
			if id.Fingerprint != Fingerprint(pub) || len(id.Fingerprint) != 64 {
				t.Fatalf("Fingerprint %q, want full %q", id.Fingerprint, Fingerprint(pub))
			}
			if id.Host != "my-lap-top.local" || id.Role != role {
				t.Fatalf("Host %q Role %q", id.Host, id.Role)
			}
			if id.Serial.Cmp(c.SerialNumber) != 0 || !id.NotAfter.Equal(c.NotAfter) {
				t.Fatal("serial/NotAfter not carried")
			}
		}
	}
}

func TestVerifyInstanceChain_OtherRootIsUntrusted(t *testing.T) {
	f, other := newFleet(t), newFleet(t)
	c, _ := other.issue(t, "h", RoleInstance)
	if _, err := f.verify(c, UsageClient); !errors.Is(err, ErrUntrustedRoot) {
		t.Fatalf("err=%v, want ErrUntrustedRoot", err)
	}
}

func TestVerifyInstanceChain_SelfSignedIsUntrusted(t *testing.T) {
	f := newFleet(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(5), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, ExtraExtensions: []pkix.Extension{rawSANExt(SAN(RoleInstance, "h", Fingerprint(pub)))}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	c, _ := x509.ParseCertificate(der)
	if _, err := f.verify(c, UsageClient); !errors.Is(err, ErrUntrustedRoot) {
		t.Fatalf("err=%v, want ErrUntrustedRoot", err)
	}
}

func TestVerifyInstanceChain_Expired(t *testing.T) {
	f := newFleet(t)
	c, _ := f.issue(t, "h", RoleInstance)
	_, err := VerifyInstanceChain(c, nil, f.root.Cert, f.crl, c.NotAfter.Add(time.Minute), UsageClient)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("err=%v, want ErrExpired", err)
	}
}

func TestVerifyInstanceChain_Revoked(t *testing.T) {
	f := newFleet(t)
	c, _ := f.issue(t, "h", RoleInstance)
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
	c, _ := f.issue(t, "h", RoleInstance)
	if _, err := VerifyInstanceChain(c, nil, f.root.Cert, nil, time.Now(), UsageClient); !errors.Is(err, ErrCRLMissing) {
		t.Fatalf("nil CRL: err=%v, want ErrCRLMissing", err)
	}
	if _, err := VerifyInstanceChain(c, nil, f.root.Cert, other.crl, time.Now(), UsageClient); !errors.Is(err, ErrCRLInvalid) {
		t.Fatalf("foreign CRL: err=%v, want ErrCRLInvalid", err)
	}
}

func TestVerifyInstanceChain_NotEd25519(t *testing.T) {
	f := newFleet(t)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c := f.crafted(t, &k.PublicKey, "knomit://instance/h-00000000")
	if _, err := f.verify(c, UsageClient); !errors.Is(err, ErrNotEd25519) {
		t.Fatalf("err=%v, want ErrNotEd25519", err)
	}
}

func TestVerifyInstanceChain_SANFp8MustMatchTheKey(t *testing.T) {
	f := newFleet(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	c := f.crafted(t, pub, SAN(RoleInstance, "h", Fingerprint(other)))
	if _, err := f.verify(c, UsageClient); !errors.Is(err, ErrSANMismatch) {
		t.Fatalf("err=%v, want ErrSANMismatch", err)
	}
	// The HOST is never compared: any label with the right fp8 verifies.
	c = f.crafted(t, pub, SAN(RoleInstance, "anything-at-all", Fingerprint(pub)))
	if _, err := f.verify(c, UsageClient); err != nil {
		t.Fatalf("host label must not matter: %v", err)
	}
}

func TestVerifyInstanceChain_UnknownRole(t *testing.T) {
	f := newFleet(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	for _, role := range []string{"agent", "Instance", "admin"} {
		c := f.crafted(t, pub, "knomit://"+role+"/h-"+Short(Fingerprint(pub)))
		if _, err := f.verify(c, UsageClient); !errors.Is(err, ErrRoleUnknown) {
			t.Fatalf("role %q: err=%v, want ErrRoleUnknown", role, err)
		}
	}
}

func TestVerifyInstanceChain_SANShapeIsStrict(t *testing.T) {
	f := newFleet(t)
	// The "uppercase fp8" case is only a NEGATIVE case if fp8 has a letter;
	// an all-digit fp8 uppercases to itself and is valid. Draw keys until
	// it has one, so the case cannot pass by chance.
	var pub ed25519.PublicKey
	for {
		pub, _, _ = ed25519.GenerateKey(rand.Reader)
		if upper(Short(Fingerprint(pub))) != Short(Fingerprint(pub)) {
			break
		}
	}
	fp8 := Short(Fingerprint(pub))
	good := "knomit://instance/h-" + fp8
	cases := map[string][]string{
		"no SAN":            nil,
		"non-knomit only":   {"https://example.com/h-" + fp8},
		"two knomit URIs":   {good, good},
		"two different":     {good, "knomit://operator/h-" + fp8},
		"uppercase scheme":  {"KNOMIT://instance/h-" + fp8},
		"mixed-case scheme": {"Knomit://instance/h-" + fp8},
		"userinfo":          {"knomit://u@instance/h-" + fp8},
		"query":             {good + "?x=1"},
		"empty query":       {good + "?"},
		"fragment":          {good + "#f"},
		"two segments":      {"knomit://instance/a/h-" + fp8},
		"trailing slash":    {good + "/"},
		"escaped":           {"knomit://instance/h%2D" + fp8},
		"no host part":      {"knomit://instance/-" + fp8},
		"uppercase fp8":     {"knomit://instance/h-" + upper(fp8)},
		"short fp8":         {"knomit://instance/h-" + fp8[:7]},
		// crypto/x509 PARSES this (parser.go:424 domainNameValid accepts
		// "instance:443"), so parseSAN's own port check is the only guard.
		"port":       {"knomit://instance:443/h-" + fp8},
		"empty port": {"knomit://instance:/h-" + fp8},
		"opaque":     {"knomit:instance/h-" + fp8},
	}
	for name, uris := range cases {
		c := f.crafted(t, pub, uris...)
		if _, err := f.verify(c, UsageClient); !errors.Is(err, ErrSANMissing) {
			t.Errorf("%s %v: err=%v, want ErrSANMissing", name, uris, err)
		}
	}
	// Positive control: the canonical form, crafted the same way, verifies,
	// and so does a knomit SAN alongside an unrelated URI.
	for _, uris := range [][]string{{good}, {"https://example.com/", good}} {
		c := f.crafted(t, pub, uris...)
		if _, err := f.verify(c, UsageClient); err != nil {
			t.Fatalf("canonical %v refused: %v", uris, err)
		}
	}
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}
