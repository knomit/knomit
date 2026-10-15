package pki

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mustParseCertPEM(t *testing.T, raw []byte) *x509.Certificate {
	t.Helper()
	c, err := parseOneCert(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustParseCRLPEM(t *testing.T, raw []byte) *x509.RevocationList {
	t.Helper()
	c, err := parseCRL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// pinSerial makes newSerial return n for the rest of the test.
func pinSerial(t *testing.T, n int64) {
	t.Helper()
	prev := newSerial
	newSerial = func() (*big.Int, error) { return big.NewInt(n), nil }
	t.Cleanup(func() { newSerial = prev })
}

// The pure IssueLeaf, given the file variant's clock and serial, yields the
// same certificate fields and the byte-identical ledger line IssueInstance
// appended. DER is not compared: an Ed25519 signature is deterministic but
// the comparison must not depend on it.
func TestIssueLeaf_EquivalentToIssueInstance(t *testing.T) {
	dir := t.TempDir()
	root, err := NewRoot(dir, "knomit-fleet-root-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	pinSerial(t, 0x5eed)

	fileCertPEM, fileRec, err := IssueInstance(dir, root, pub, "laptop@agents.knomit.io", 90*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fileLine, err := os.ReadFile(filepath.Join(dir, IssuedLogFile))
	if err != nil {
		t.Fatal(err)
	}

	is, err := IssueLeaf(root, pub, "laptop@agents.knomit.io", 90*time.Hour, fileRec.IssuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(is.Line, fileLine) {
		t.Fatalf("ledger line differs:\n pure %s file %s", is.Line, fileLine)
	}
	if is.Record != fileRec {
		t.Fatalf("record differs: %+v vs %+v", is.Record, fileRec)
	}
	a, b := mustParseCertPEM(t, is.CertPEM), mustParseCertPEM(t, fileCertPEM)
	if a.SerialNumber.Cmp(big.NewInt(0x5eed)) != 0 || b.SerialNumber.Cmp(a.SerialNumber) != 0 {
		t.Fatalf("serials %v / %v, want 0x5eed both", a.SerialNumber, b.SerialNumber)
	}
	for name, eq := range map[string]bool{
		"subject":   a.Subject.String() == b.Subject.String(),
		"issuer":    a.Issuer.String() == b.Issuer.String(),
		"notBefore": a.NotBefore.Equal(b.NotBefore),
		"notAfter":  a.NotAfter.Equal(b.NotAfter),
		"uris":      reflect.DeepEqual(a.URIs, b.URIs),
		"keyUsage":  a.KeyUsage == b.KeyUsage,
		"extKey":    reflect.DeepEqual(a.ExtKeyUsage, b.ExtKeyUsage),
		"publicKey": a.PublicKey.(ed25519.PublicKey).Equal(b.PublicKey),
		"isCA":      a.IsCA == b.IsCA,
	} {
		if !eq {
			t.Errorf("certificate field %s differs", name)
		}
	}
	if err := a.CheckSignatureFrom(root.Cert); err != nil {
		t.Fatalf("pure leaf does not chain to the root: %v", err)
	}
	// IssueLeaf wrote nothing: the ledger still holds the file variant's line only.
	if after, _ := os.ReadFile(filepath.Join(dir, IssuedLogFile)); !bytes.Equal(after, fileLine) {
		t.Fatal("IssueLeaf wrote to the ledger")
	}
}

func crlFields(c *x509.RevocationList) []any {
	var entries []string
	for _, e := range c.RevokedCertificateEntries {
		entries = append(entries, e.SerialNumber.Text(16)+"@"+e.RevocationTime.UTC().Format(time.RFC3339))
	}
	return []any{c.Number.String(), c.ThisUpdate.UTC().Format(time.RFC3339), c.NextUpdate.UTC().Format(time.RFC3339), entries, c.Issuer.String()}
}

// Two revocations in a row: the pure RevokeSerial, given the CURRENT CRL,
// yields the same local log line and a CRL with the same Number, times and
// entries as the file variant Revoke — and a root-signed revocation
// statement for the serial that VerifyRevocation accepts.
func TestRevokeSerial_EquivalentToRevoke(t *testing.T) {
	dir := t.TempDir()
	root, err := NewRoot(dir, "knomit-fleet-root-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := IssueCRL(dir, root, nil, big.NewInt(1), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	next := time.Now().Add(30 * time.Hour)
	var ledger []byte
	for i, serial := range []int64{0xa1, 0xb2} {
		current, err := LoadCRL(filepath.Join(dir, CRLFile))
		if err != nil {
			t.Fatal(err)
		}
		r := Revoked{Serial: big.NewInt(serial), At: time.Now().UTC(), Reason: "test"}
		num, err := Revoke(dir, root, r, next)
		if err != nil {
			t.Fatal(err)
		}
		full, _ := os.ReadFile(filepath.Join(dir, RevokedLogFile))
		fileLine := full[len(ledger):]
		ledger = full
		fileCRL, err := LoadCRL(filepath.Join(dir, CRLFile))
		if err != nil {
			t.Fatal(err)
		}

		rv, err := RevokeSerial(root, current, r, fileCRL.ThisUpdate, next)
		if err != nil {
			t.Fatal(err)
		}
		if rv.Number.Cmp(num) != 0 || num.Int64() != int64(i+2) {
			t.Fatalf("revocation %d: numbers pure %v file %v, want %d", i, rv.Number, num, i+2)
		}
		if !bytes.Equal(rv.Line, fileLine) {
			t.Fatalf("revocation %d: log line pure %q file %q", i, rv.Line, fileLine)
		}
		pure := mustParseCRLPEM(t, rv.CRLPEM)
		if got, want := crlFields(pure), crlFields(fileCRL); !reflect.DeepEqual(got, want) {
			t.Fatalf("revocation %d: CRL fields\n pure %v\n file %v", i, got, want)
		}
		if len(pure.RevokedCertificateEntries) != i+1 {
			t.Fatalf("revocation %d: %d entries", i, len(pure.RevokedCertificateEntries))
		}
		if _, err := CheckCRL(pure, root.Cert, nil); err != nil {
			t.Fatal(err)
		}
		st, err := VerifyRevocation(rv.Statement, root.Cert)
		if err != nil {
			t.Fatalf("revocation %d: statement: %v", i, err)
		}
		if st.Serial.Int64() != serial || st.Reason != "test" || !st.At.Equal(r.At.Truncate(time.Second)) {
			t.Fatalf("revocation %d: statement says %+v", i, st)
		}
	}
	if _, err := RevokeSerial(root, nil, Revoked{Serial: big.NewInt(0), At: time.Now()}, time.Now(), next); err == nil {
		t.Fatal("a zero serial was revoked")
	}
	if rv, err := RevokeSerial(root, nil, Revoked{Serial: big.NewInt(7), At: time.Now()}, time.Now(), next); err != nil || rv.Number.Int64() != 1 {
		t.Fatalf("no current CRL: %v, number %v; want 1", err, rv.Number)
	}
}

// The new CRL is built from the CURRENT CRL, not from a ledger: every serial
// the current CRL lists stays listed, with its own revocation time, even
// when no local log knows it; a serial already listed is ErrAlreadyRevoked;
// a current CRL another root signed is refused.
//
// SABOTAGE: drop the copy of current's entries → the superset assertion is
// red; drop the IsRevoked check → the already-revoked row is red.
func TestRevokeSerial_CRLIsASupersetOfTheCurrentCRL(t *testing.T) {
	root, err := NewRoot(t.TempDir(), "knomit-fleet-root-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := NewRoot(t.TempDir(), "knomit-fleet-root-other", time.Hour)
	old := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	cur, err := SignCRL(root, []Revoked{{Serial: big.NewInt(0x11), At: old}, {Serial: big.NewInt(0x22), At: old}}, big.NewInt(5), time.Now(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	current := mustParseCRLPEM(t, cur)
	next := time.Now().Add(time.Hour)
	rv, err := RevokeSerial(root, current, Revoked{Serial: big.NewInt(0x33), At: time.Now()}, time.Now(), next)
	if err != nil {
		t.Fatal(err)
	}
	got := mustParseCRLPEM(t, rv.CRLPEM)
	if got.Number.Int64() != 6 {
		t.Fatalf("Number %v, want 6", got.Number)
	}
	listed := map[string]time.Time{}
	for _, e := range got.RevokedCertificateEntries {
		listed[e.SerialNumber.Text(16)] = e.RevocationTime
	}
	for _, s := range []string{"11", "22", "33"} {
		if _, ok := listed[s]; !ok {
			t.Fatalf("the new CRL drops serial %s: %v", s, listed)
		}
	}
	if !listed["11"].Equal(old) {
		t.Fatalf("serial 11 keeps its revocation time: %v, want %v", listed["11"], old)
	}
	if _, err := RevokeSerial(root, current, Revoked{Serial: big.NewInt(0x22), At: time.Now()}, time.Now(), next); !errors.Is(err, ErrAlreadyRevoked) {
		t.Fatalf("already revoked: %v, want ErrAlreadyRevoked", err)
	}
	foreign := mustParseCRLPEM(t, signedCRL(t, other, 9))
	if _, err := RevokeSerial(root, foreign, Revoked{Serial: big.NewInt(0x44), At: time.Now()}, time.Now(), next); !errors.Is(err, ErrCRLInvalid) {
		t.Fatalf("another root's current CRL: %v, want ErrCRLInvalid", err)
	}
}

// The file variant refuses to sign a CRL #1 when crl.pem is gone but the
// local log already holds revocations: that CRL would drop them.
func TestRevoke_MissingCRLWithALoggedRevocationIsRefused(t *testing.T) {
	dir := t.TempDir()
	root, err := NewRoot(dir, "knomit-fleet-root-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Revoke(dir, root, Revoked{Serial: big.NewInt(1), At: time.Now()}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, CRLFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Revoke(dir, root, Revoked{Serial: big.NewInt(2), At: time.Now()}, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("a CRL #1 that drops a logged revocation was signed")
	}
}

// A present crl.pem that lags the local log (an older CRL restored from a
// backup) is refused too (review L3): the new CRL is built from the current
// one, so it would silently drop the logged revocation the old CRL lacks.
// Nothing is written: the log and crl.pem are as they were. With the CRL
// that lists every logged serial back in place, the revocation goes through.
//
// SABOTAGE: drop the lag check in Revoke → the revocation against the old
// CRL succeeds → red.
func TestRevoke_CRLLaggingTheLocalLogIsRefused(t *testing.T) {
	dir := t.TempDir()
	root, err := NewRoot(dir, "knomit-fleet-root-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	next := time.Now().Add(time.Hour)
	if _, err := Revoke(dir, root, Revoked{Serial: big.NewInt(1), At: time.Now()}, next); err != nil {
		t.Fatal(err)
	}
	crlPath, logPath := filepath.Join(dir, CRLFile), filepath.Join(dir, RevokedLogFile)
	old, _ := os.ReadFile(crlPath)
	if _, err := Revoke(dir, root, Revoked{Serial: big.NewInt(2), At: time.Now()}, next); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(crlPath)
	if err := os.WriteFile(crlPath, old, 0o644); err != nil {
		t.Fatal(err)
	}
	logBefore, _ := os.ReadFile(logPath)
	_, err = Revoke(dir, root, Revoked{Serial: big.NewInt(3), At: time.Now()}, next)
	if err == nil || !strings.Contains(err.Error(), "does not list serial 2") {
		t.Fatalf("revoke against a CRL lagging the log: %v", err)
	}
	if got, _ := os.ReadFile(crlPath); !bytes.Equal(got, old) {
		t.Fatal("the refused revoke rewrote crl.pem")
	}
	if got, _ := os.ReadFile(logPath); !bytes.Equal(got, logBefore) {
		t.Fatal("the refused revoke appended to the log")
	}
	// Control: with the CRL that lists every logged serial, it goes through.
	if err := os.WriteFile(crlPath, current, 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := Revoke(dir, root, Revoked{Serial: big.NewInt(3), At: time.Now()}, next); err != nil || n.Cmp(big.NewInt(3)) != 0 {
		t.Fatalf("control: revoke against the current CRL: #%v %v", n, err)
	}
}

// instanceWithRoot installs root.crt (only) into a fresh instance pki dir.
func instanceWithRoot(t *testing.T, root Root) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pki")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, RootCertFile), certPEM(root.Cert.Raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func signedCRL(t *testing.T, root Root, n int64, serials ...int64) []byte {
	t.Helper()
	var rs []Revoked
	for _, s := range serials {
		rs = append(rs, Revoked{Serial: big.NewInt(s), At: time.Now()})
	}
	raw, err := SignCRL(root, rs, big.NewInt(n), time.Now(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestInstallCRL_OnlyAboveTheWatermark(t *testing.T) {
	root, err := NewRoot(t.TempDir(), "knomit-fleet-root-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dir := instanceWithRoot(t, root)
	crlPath := filepath.Join(dir, CRLFile)

	if n, err := InstallCRL(dir, root.Cert, signedCRL(t, root, 3)); err != nil || n.Int64() != 3 {
		t.Fatalf("first CRL: %v, %v", n, err)
	}
	held, _ := os.ReadFile(crlPath)
	if w, _ := AcceptedNumber(dir, root.Cert); w == nil || w.Int64() != 3 {
		t.Fatalf("watermark %v after #3", w)
	}

	for _, n := range []int64{2, 3} { // below, and equal with a different list
		_, err := InstallCRL(dir, root.Cert, signedCRL(t, root, n, 0x99))
		if !errors.Is(err, ErrCRLRollback) {
			t.Fatalf("#%d at watermark 3: err %v, want ErrCRLRollback", n, err)
		}
		if now, _ := os.ReadFile(crlPath); !bytes.Equal(now, held) {
			t.Fatalf("#%d refused but crl.pem changed", n)
		}
		if w, _ := AcceptedNumber(dir, root.Cert); w.Int64() != 3 {
			t.Fatalf("#%d refused but watermark moved to %v", n, w)
		}
	}

	four := signedCRL(t, root, 4, 0x99)
	if n, err := InstallCRL(dir, root.Cert, four); err != nil || n.Int64() != 4 {
		t.Fatalf("#4: %v, %v", n, err)
	}
	if now, _ := os.ReadFile(crlPath); !bytes.Equal(now, four) {
		t.Fatal("#4 accepted but crl.pem is not it")
	}
	if w, _ := AcceptedNumber(dir, root.Cert); w.Int64() != 4 {
		t.Fatalf("watermark %v after #4", w)
	}
}

func TestInstallCRL_RefusesAnotherRootAndAForgedCRL(t *testing.T) {
	a, _ := NewRoot(t.TempDir(), "same-cn", time.Hour)
	b, _ := NewRoot(t.TempDir(), "same-cn", time.Hour)
	dir := instanceWithRoot(t, a)
	crlPath := filepath.Join(dir, CRLFile)

	// rootCert is not the installed root: refused before anything is read.
	var rd *RootDiffersError
	if _, err := InstallCRL(dir, b.Cert, signedCRL(t, b, 9)); !errors.As(err, &rd) {
		t.Fatalf("CRL for another root: %v, want RootDiffersError", err)
	}
	// Signed by B but presented as A's.
	if _, err := InstallCRL(dir, a.Cert, signedCRL(t, b, 9)); !errors.Is(err, ErrCRLInvalid) {
		t.Fatalf("forged CRL: %v, want ErrCRLInvalid", err)
	}
	// Two blocks.
	two := append(signedCRL(t, a, 5), signedCRL(t, a, 6)...)
	if _, err := InstallCRL(dir, a.Cert, two); !errors.Is(err, ErrCRLInvalid) {
		t.Fatalf("two CRLs: %v", err)
	}
	if _, err := os.Stat(crlPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused CRL was written: %v", err)
	}
	if w, _ := AcceptedNumber(dir, a.Cert); w != nil {
		t.Fatalf("a refused CRL set the watermark to %v", w)
	}
	// No root installed at all: refused, it never installs a root.
	if _, err := InstallCRL(filepath.Join(t.TempDir(), "pki"), a.Cert, signedCRL(t, a, 1)); !errors.As(err, &rd) {
		t.Fatalf("no installed root: %v, want RootDiffersError", err)
	}
}

func TestRenewalWindow_IsAThirdOfTheOwnLifetime(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	life := 90 * time.Hour
	end := start.Add(life)
	at := func(remainingFraction float64) time.Time {
		return end.Add(-time.Duration(remainingFraction * float64(life)))
	}
	cert := &x509.Certificate{NotBefore: start, NotAfter: end}
	crl := &x509.RevocationList{ThisUpdate: start, NextUpdate: end}
	for _, c := range []struct {
		remaining float64
		want      bool
	}{{0.32, true}, {0.34, false}, {0.9, false}, {-0.1, true}} {
		if got := NeedsRenewal(cert, at(c.remaining)); got != c.want {
			t.Errorf("NeedsRenewal with %.2f of the lifetime left = %v, want %v", c.remaining, got, c.want)
		}
		if got := CRLNeedsResign(crl, at(c.remaining)); got != c.want {
			t.Errorf("CRLNeedsResign with %.2f of the lifetime left = %v, want %v", c.remaining, got, c.want)
		}
	}
	// Derived, not a constant: a 9-hour lifetime at 0.32 left is due too.
	short := &x509.Certificate{NotBefore: start, NotAfter: start.Add(9 * time.Hour)}
	if !NeedsRenewal(short, start.Add(9*time.Hour-time.Duration(0.32*float64(9*time.Hour)))) {
		t.Error("a short lifetime is not judged by its own length")
	}
	if CRLNeedsResign(&x509.RevocationList{ThisUpdate: start}, end.Add(time.Hour)) {
		t.Error("a CRL with no NextUpdate fell due")
	}
}
