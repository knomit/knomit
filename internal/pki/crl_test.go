package pki

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// staleCRL signs a CRL whose NextUpdate is already past. IssueCRL cannot
// produce one (ThisUpdate is always now), so the test builds it directly.
func staleCRL(t *testing.T, root Root, serials ...*big.Int) *x509.RevocationList {
	t.Helper()
	var entries []x509.RevocationListEntry
	for _, s := range serials {
		entries = append(entries, x509.RevocationListEntry{SerialNumber: s, RevocationTime: time.Now().Add(-48 * time.Hour)})
	}
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number:                    big.NewInt(7),
		ThisUpdate:                time.Now().Add(-48 * time.Hour),
		NextUpdate:                time.Now().Add(-24 * time.Hour),
		RevokedCertificateEntries: entries,
	}, root.Cert, root.Signer)
	if err != nil {
		t.Fatal(err)
	}
	crl, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatal(err)
	}
	return crl
}

func TestIssueCRL_RoundTripsAndChecksAgainstItsRoot(t *testing.T) {
	dir, root := newTestRoot(t)
	serial := big.NewInt(0xabc)
	if _, err := IssueCRL(dir, root, []Revoked{{Serial: serial, At: time.Now(), Reason: "test"}}, big.NewInt(1), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	crl, err := LoadCRL(filepath.Join(dir, CRLFile))
	if err != nil {
		t.Fatal(err)
	}
	stale, err := CheckCRL(crl, root.Cert, nil)
	if err != nil || stale {
		t.Fatalf("fresh CRL: stale=%v err=%v", stale, err)
	}
	if !IsRevoked(crl, serial) {
		t.Fatal("listed serial not revoked")
	}
	if IsRevoked(crl, big.NewInt(0xabd)) { // positive control for the lookup
		t.Fatal("unlisted serial reported revoked")
	}
}

func TestCheckCRL_RefusesAnotherRootsCRL(t *testing.T) {
	_, root := newTestRoot(t)
	otherDir, other := newTestRoot(t)
	if _, err := IssueCRL(otherDir, other, nil, big.NewInt(1), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	crl, _ := LoadCRL(filepath.Join(otherDir, CRLFile))
	if _, err := CheckCRL(crl, root.Cert, nil); !errors.Is(err, ErrCRLInvalid) {
		t.Fatalf("CRL from another root: err=%v, want ErrCRLInvalid", err)
	}
	if _, err := CheckCRL(crl, other.Cert, nil); err != nil { // positive control
		t.Fatalf("CRL refused by its own root: %v", err)
	}
}

func TestCheckCRL_RefusesALowerNumber(t *testing.T) {
	dir, root := newTestRoot(t)
	if _, err := IssueCRL(dir, root, nil, big.NewInt(4), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	crl, _ := LoadCRL(filepath.Join(dir, CRLFile))
	if _, err := CheckCRL(crl, root.Cert, big.NewInt(5)); !errors.Is(err, ErrCRLInvalid) || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("Number 4 after 5 accepted: err=%v", err)
	}
	if _, err := CheckCRL(crl, root.Cert, big.NewInt(4)); err != nil { // equal: the same CRL re-read
		t.Fatalf("same Number refused: %v", err)
	}
}

func TestCheckCRL_StaleIsReportedButStillEnforced(t *testing.T) {
	_, root := newTestRoot(t)
	revoked := big.NewInt(99)
	crl := staleCRL(t, root, revoked)
	stale, err := CheckCRL(crl, root.Cert, nil)
	if err != nil || !stale {
		t.Fatalf("stale CRL: stale=%v err=%v, want stale=true err=nil", stale, err)
	}
	if !IsRevoked(crl, revoked) {
		t.Fatal("a stale CRL stopped enforcing its own list")
	}
	if IsRevoked(crl, big.NewInt(100)) {
		t.Fatal("unlisted serial reported revoked")
	}
}

func TestCheckCRL_NilAndMalformedFailClosed(t *testing.T) {
	_, root := newTestRoot(t)
	if _, err := CheckCRL(nil, root.Cert, nil); !errors.Is(err, ErrCRLMissing) {
		t.Fatalf("nil CRL: %v", err)
	}
	p := filepath.Join(t.TempDir(), CRLFile)
	os.WriteFile(p, []byte("-----BEGIN X509 CRL-----\nAAAA\n-----END X509 CRL-----\n"), 0o600)
	if _, err := LoadCRL(p); !errors.Is(err, ErrCRLInvalid) {
		t.Fatalf("malformed CRL: %v", err)
	}
	if _, err := LoadCRL(filepath.Join(t.TempDir(), "absent.pem")); !errors.Is(err, ErrCRLMissing) {
		t.Fatalf("absent CRL: %v", err)
	}
}

// The master's revocation list is the append-only file; the CRL is a
// projection of it with a Number one higher than the last.
func TestRevoke_AppendsAndTheNextCRLCarriesEveryPriorEntry(t *testing.T) {
	dir, root := newTestRoot(t)
	var serials []*big.Int
	var lastNum *big.Int
	for range 3 {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		_, rec, err := IssueInstance(dir, root, pub, "h", RoleInstance, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		s, _ := new(big.Int).SetString(rec.Serial, 16)
		serials = append(serials, s)
		num, err := Revoke(dir, root, Revoked{Serial: s, At: time.Now(), Reason: "test"}, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if lastNum != nil && num.Cmp(new(big.Int).Add(lastNum, big.NewInt(1))) != 0 {
			t.Fatalf("CRL Number %v after %v, want +1", num, lastNum)
		}
		lastNum = num
	}
	crl, err := LoadCRL(filepath.Join(dir, CRLFile))
	if err != nil {
		t.Fatal(err)
	}
	if crl.Number.Cmp(lastNum) != 0 {
		t.Fatalf("crl.pem Number %v, Revoke returned %v", crl.Number, lastNum)
	}
	for _, s := range serials {
		if !IsRevoked(crl, s) {
			t.Fatalf("serial %x dropped from the reissued CRL", s)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, RevokedLogFile))
	if n := strings.Count(string(raw), "\n"); n != 3 {
		t.Fatalf("revoked.jsonl has %d lines", n)
	}
	if len(crl.RevokedCertificateEntries) != 3 {
		t.Fatalf("CRL has %d entries", len(crl.RevokedCertificateEntries))
	}
}

// The watermark is PER ROOT, keyed by RootID (the fingerprint of the root
// key). Two roots with the SAME CommonName — newTestRoot gives every fleet
// "knomit-master-test" — keep separate entries, so a CN-keyed record would
// fail here.
func TestAcceptedNumber_IsPerRootKeyedByFingerprintNotCN(t *testing.T) {
	dir := t.TempDir()
	_, a := newTestRoot(t)
	_, b := newTestRoot(t)
	if a.Cert.Subject.CommonName != b.Cert.Subject.CommonName {
		t.Fatal("fixture: the two roots must share a CommonName")
	}
	if n, err := AcceptedNumber(dir, a.Cert); err != nil || n != nil {
		t.Fatalf("fresh dir: n=%v err=%v", n, err)
	}
	if err := RecordAcceptedNumber(dir, a.Cert, big.NewInt(7)); err != nil {
		t.Fatal(err)
	}
	if err := RecordAcceptedNumber(dir, b.Cert, big.NewInt(1)); err != nil {
		t.Fatal(err)
	}
	if n, _ := AcceptedNumber(dir, a.Cert); n == nil || n.Cmp(big.NewInt(7)) != 0 {
		t.Fatalf("root A watermark = %v, want 7 (root B's entry must not overwrite it)", n)
	}
	if n, _ := AcceptedNumber(dir, b.Cert); n == nil || n.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("root B watermark = %v, want 1", n)
	}
	// Never lowered.
	if err := RecordAcceptedNumber(dir, a.Cert, big.NewInt(3)); err != nil {
		t.Fatal(err)
	}
	if n, _ := AcceptedNumber(dir, a.Cert); n.Cmp(big.NewInt(7)) != 0 {
		t.Fatalf("watermark lowered to %v", n)
	}
	// Garbled fails closed rather than resetting to accept-any.
	os.WriteFile(filepath.Join(dir, CRLNumberFile), []byte("12\n"), 0o600) // the old single-number format
	if _, err := AcceptedNumber(dir, a.Cert); err == nil {
		t.Fatal("a garbled crl.number must fail closed, not reset to 'accept any'")
	}
}
