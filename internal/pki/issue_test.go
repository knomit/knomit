package pki

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testPass = []byte("correct horse battery staple")

func newTestRoot(t *testing.T) (string, Root) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "master")
	r, err := NewRoot(dir, "knomit-master-test", testPass, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return dir, r
}

func parsePEMCert(t *testing.T, b []byte) *x509.Certificate {
	t.Helper()
	blk, rest := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		t.Fatalf("want exactly one CERTIFICATE block, got %q", b)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewRoot_IsSelfSignedCAAndReloadsOnlyWithThePassphrase(t *testing.T) {
	dir, r := newTestRoot(t)
	if !r.Cert.IsCA || !r.Cert.BasicConstraintsValid {
		t.Fatal("root is not a CA")
	}
	if r.Cert.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign {
		t.Fatalf("root KeyUsage = %v", r.Cert.KeyUsage)
	}
	if err := r.Cert.CheckSignatureFrom(r.Cert); err != nil {
		t.Fatalf("root not self-signed: %v", err)
	}
	if _, err := LoadRoot(dir, []byte("wrong")); err == nil {
		t.Fatal("LoadRoot accepted a wrong passphrase")
	}
	back, err := LoadRoot(dir, testPass)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Cert.Equal(r.Cert) {
		t.Fatal("reloaded root certificate differs")
	}
	if !back.Signer.Public().(ed25519.PublicKey).Equal(r.Cert.PublicKey) {
		t.Fatal("reloaded root key does not match its certificate")
	}
	// The key on disk is encrypted: an unencrypted parse must not succeed.
	raw, _ := os.ReadFile(filepath.Join(dir, RootKeyFile))
	if !strings.Contains(string(raw), "OPENSSH PRIVATE KEY") {
		t.Fatalf("root.key is not OpenSSH format: %.40q", raw)
	}
	if _, _, err := LoadSigner(filepath.Join(dir, RootKeyFile)); err == nil {
		t.Fatal("root.key parsed WITHOUT a passphrase; it must be encrypted")
	}
	if fi, _ := os.Stat(filepath.Join(dir, RootKeyFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("root.key mode %v, want 0600", fi.Mode().Perm())
	}
	if c, err := LoadRootCert(filepath.Join(dir, RootCertFile)); err != nil || !c.Equal(r.Cert) {
		t.Fatalf("LoadRootCert: %v", err)
	}
}

func TestNewRoot_RefusesToOverwriteAndRefusesEmptyPassphrase(t *testing.T) {
	dir, _ := newTestRoot(t)
	before, _ := os.ReadFile(filepath.Join(dir, RootKeyFile))
	if _, err := NewRoot(dir, "again", testPass, time.Hour); err == nil {
		t.Fatal("NewRoot overwrote an existing root")
	}
	after, _ := os.ReadFile(filepath.Join(dir, RootKeyFile))
	if string(before) != string(after) {
		t.Fatal("root.key changed on a refused NewRoot")
	}
	if _, err := NewRoot(filepath.Join(t.TempDir(), "m"), "x", nil, time.Hour); err == nil {
		t.Fatal("NewRoot accepted an empty passphrase")
	}
}

func TestIssueInstance_LeafChainsToRootWithRoleInSANAndIsLogged(t *testing.T) {
	dir, root := newTestRoot(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	certPEM, rec, err := IssueInstance(dir, root, pub, "laptop.local", RoleInstance, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c := parsePEMCert(t, certPEM)

	pool := x509.NewCertPool()
	pool.AddCert(root.Cert)
	for _, eku := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
		if _, err := c.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{eku}}); err != nil {
			t.Fatalf("leaf does not chain to root for EKU %v: %v", eku, err)
		}
	}
	if c.IsCA {
		t.Fatal("instance certificate must be a LEAF")
	}
	if c.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Fatalf("KeyUsage = %v", c.KeyUsage)
	}
	fp := Fingerprint(pub)
	wantSAN := "knomit://instance/laptop.local-" + Short(fp)
	if len(c.URIs) != 1 || c.URIs[0].String() != wantSAN {
		t.Fatalf("URIs = %v, want [%s]", c.URIs, wantSAN)
	}
	if SAN(RoleInstance, "laptop.local", fp) != wantSAN {
		t.Fatalf("SAN() = %s", SAN(RoleInstance, "laptop.local", fp))
	}
	// No private extension: the role lives in the SAN (Go cannot parse a
	// 2.25 UUID-arc extension OID, crypto/x509/parser.go:233).
	allowed := map[string]bool{
		"2.5.29.14": true, // subjectKeyIdentifier
		"2.5.29.15": true, // keyUsage
		"2.5.29.17": true, // subjectAltName (the URI carrying the role)
		"2.5.29.19": true, // basicConstraints
		"2.5.29.35": true, // authorityKeyIdentifier
		"2.5.29.37": true, // extKeyUsage
	}
	var ids []string
	for _, ext := range c.Extensions {
		ids = append(ids, ext.Id.String())
		if !allowed[ext.Id.String()] {
			t.Fatalf("leaf carries extension %s outside the standard set", ext.Id)
		}
	}
	if len(ids) < 4 { // positive control: the loop saw the real extensions
		t.Fatalf("only %d extensions seen: %v", len(ids), ids)
	}
	if !c.PublicKey.(ed25519.PublicKey).Equal(pub) {
		t.Fatal("certificate wraps a different key")
	}
	if c.SerialNumber.BitLen() < 64 {
		t.Fatalf("serial has only %d bits", c.SerialNumber.BitLen())
	}

	// Issuance log: exactly one line, same serial, full fingerprint.
	f, err := os.Open(filepath.Join(dir, IssuedLogFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []Issued
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var got Issued
		if err := json.Unmarshal(sc.Bytes(), &got); err != nil {
			t.Fatalf("bad log line %q: %v", sc.Text(), err)
		}
		lines = append(lines, got)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("issued.jsonl has %d lines, want 1", len(lines))
	}
	if lines[0].Serial != c.SerialNumber.Text(16) || lines[0].Serial != rec.Serial {
		t.Fatalf("logged serial %s, cert %s, returned %s", lines[0].Serial, c.SerialNumber.Text(16), rec.Serial)
	}
	if lines[0].Fingerprint != fp || lines[0].SAN != wantSAN || !lines[0].NotAfter.Equal(c.NotAfter) {
		t.Fatalf("log record %+v", lines[0])
	}
	if fi, _ := os.Stat(filepath.Join(dir, IssuedLogFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("issued.jsonl mode %v", fi.Mode().Perm())
	}

	// A second issuance APPENDS.
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, _, err := IssueInstance(dir, root, pub2, "other", RoleOperator, time.Hour); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, IssuedLogFile))
	if n := strings.Count(string(raw), "\n"); n != 2 {
		t.Fatalf("after two issuances the log has %d lines", n)
	}
}

func TestIssueInstance_RefusesUnknownRoleAndUnsafeHost(t *testing.T) {
	dir, root := newTestRoot(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, _, err := IssueInstance(dir, root, pub, "h", Role("agent"), time.Hour); err == nil {
		t.Fatal("issued a certificate for an unknown role")
	}
	for _, host := range []string{"", "a/b", "a b", "x?y"} {
		if _, _, err := IssueInstance(dir, root, pub, host, RoleInstance, time.Hour); err == nil {
			t.Fatalf("issued a certificate for host %q", host)
		}
	}
	// Positive control: a hyphenated host is fine (fp8 is taken from the END).
	if _, _, err := IssueInstance(dir, root, pub, "my-lap-top", RoleInstance, time.Hour); err != nil {
		t.Fatalf("hyphenated host refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, IssuedLogFile)); err != nil {
		t.Fatal("the one valid issuance was not logged")
	}
}
