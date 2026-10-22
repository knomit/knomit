package pki_test

import (
	"bytes"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knomit/internal/foundation/pki"
	"knomit/internal/foundation/pki/pkitest"
)

// InstallLeaf installs a renewed certificate for the installed identity and
// nothing else: another key, another root, a revoked or an older certificate,
// another identity, or a home with nothing installed are refused and write
// nothing; the renewal (same key, same root, later NotBefore) replaces
// instance.crt and only it.
// SABOTAGE: drop the NotBefore check -> the older certificate installs ->
// red; verify against the bundle's (the leaf's own) chain instead of the
// installed root -> the other root's leaf installs -> red.
func TestInstallLeaf(t *testing.T) {
	f, other := pkitest.New(t), pkitest.New(t)
	m := f.Enroll(t, "laptop@agents.knomit.io")
	dir := filepath.Join(t.TempDir(), "pki")

	renewed := f.Enroll(t, "laptop@agents.knomit.io", m.KeyPath)
	if _, err := pki.InstallLeaf(dir, m.KeyPath, renewed.CertPEM); err == nil {
		t.Fatal("nothing installed: a renewal installed")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused renewal created %s (%v)", dir, err)
	}

	f.Install(t, m, dir)
	time.Sleep(1100 * time.Millisecond) // a NotBefore later than m's, at second resolution
	renewed = f.Enroll(t, "laptop@agents.knomit.io", m.KeyPath)
	revoked := f.Enroll(t, "laptop@agents.knomit.io", m.KeyPath)
	f.Revoke(t, revoked, dir) // the installed CRL lists it
	before := snapshot(t, dir)

	for _, tc := range []struct {
		name  string
		raw   []byte
		class error
	}{
		{"another key", f.Enroll(t, "laptop@agents.knomit.io").CertPEM, pki.ErrKeyMismatch},
		{"another root", other.Enroll(t, "laptop@agents.knomit.io", m.KeyPath).CertPEM, pki.ErrUntrustedRoot},
		{"revoked", revoked.CertPEM, pki.ErrRevoked},
		{"not newer (the installed one)", m.CertPEM, pki.ErrLeafNotNewer},
		{"two blocks", append(append([]byte{}, renewed.CertPEM...), renewed.CertPEM...), pki.ErrMalformedBundle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pki.InstallLeaf(dir, m.KeyPath, tc.raw); !errors.Is(err, tc.class) {
				t.Fatalf("err %v, want %v", err, tc.class)
			}
			sameTree(t, before, snapshot(t, dir))
		})
	}
	if _, err := pki.InstallLeaf(dir, m.KeyPath, f.Enroll(t, "desk@agents.knomit.io", m.KeyPath).CertPEM); err == nil || !strings.Contains(err.Error(), "keeps the identity") {
		t.Fatalf("another identity: %v", err)
	}
	sameTree(t, before, snapshot(t, dir))

	id, err := pki.InstallLeaf(dir, m.KeyPath, renewed.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	if id.Serial.Cmp(renewed.Cert.SerialNumber) != 0 || id.AgentID != "laptop" {
		t.Fatalf("identity %+v", id)
	}
	got, _ := os.ReadFile(filepath.Join(dir, pki.InstanceCertFile))
	if !bytes.Equal(got, renewed.CertPEM) {
		t.Fatal("instance.crt is not the renewal")
	}
	after := snapshot(t, dir)
	for _, name := range []string{pki.RootCertFile, pki.CRLFile} {
		if after[name] != before[name] {
			t.Fatalf("%s changed with a leaf renewal", name)
		}
	}
}

// ReissueCRL keeps every entry, raises the Number by one, and takes the
// times given; the result passes the fleet repository's crl-newer rule shape.
// SABOTAGE: build it with no entries -> red.
func TestReissueCRL(t *testing.T) {
	f := pkitest.New(t)
	m := f.Enroll(t, "laptop@agents.knomit.io")
	f.Revoke(t, m)
	cur, err := pki.LoadCRL(filepath.Join(f.Dir, pki.CRLFile))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	raw, n, err := pki.ReissueCRL(f.Root, cur, now, now.Add(pki.CRLLifetime(cur)))
	if err != nil {
		t.Fatal(err)
	}
	if n.Cmp(new(big.Int).Add(cur.Number, big.NewInt(1))) != 0 {
		t.Fatalf("Number %v after %v", n, cur.Number)
	}
	dir := t.TempDir()
	if err := pki.RecordCRL(dir, raw); err != nil {
		t.Fatal(err)
	}
	next, err := pki.LoadCRL(filepath.Join(dir, pki.CRLFile))
	if err != nil {
		t.Fatal(err)
	}
	if !pki.IsRevoked(next, m.Cert.SerialNumber) || len(next.RevokedCertificateEntries) != len(cur.RevokedCertificateEntries) {
		t.Fatalf("entries %v, want %v", next.RevokedCertificateEntries, cur.RevokedCertificateEntries)
	}
	if !next.ThisUpdate.Equal(now) || pki.CRLLifetime(next) != pki.CRLLifetime(cur) {
		t.Fatalf("times %v..%v, lifetime %v want %v", next.ThisUpdate, next.NextUpdate, pki.CRLLifetime(next), pki.CRLLifetime(cur))
	}
}
