package pki_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"knomit/internal/foundation/pki"
	"knomit/internal/foundation/pki/pkitest"
)

// InstalledStanding reads this instance's own certificate against its own
// installed root and CRL, at the time asked: valid; revoked once the
// installed CRL lists it; expired after NotAfter; untrusted under another
// root; none with nothing (or part) installed.
// SABOTAGE: map ErrRevoked to StandingValid -> the revoked row is red; ignore
// now (time.Now()) -> the expired row is red.
func TestInstalledStanding(t *testing.T) {
	f, other := pkitest.New(t), pkitest.New(t)
	m := f.Enroll(t, "laptop@agents.knomit.io")
	dir := filepath.Join(t.TempDir(), "pki")
	now := time.Now()

	if st, leaf := pki.InstalledStanding(dir, now); st != pki.StandingNone || leaf != nil {
		t.Fatalf("nothing installed: %q %v", st, leaf)
	}
	f.Install(t, m, dir)
	st, leaf := pki.InstalledStanding(dir, now)
	if st != pki.StandingValid || leaf == nil || leaf.SerialNumber.Cmp(m.Cert.SerialNumber) != 0 {
		t.Fatalf("installed: %q %v", st, leaf)
	}
	if st, _ := pki.InstalledStanding(dir, m.Cert.NotAfter.Add(time.Minute)); st != pki.StandingExpired {
		t.Fatalf("after NotAfter: %q, want expired", st)
	}

	f.Revoke(t, m, dir)
	if st, _ := pki.InstalledStanding(dir, now); st != pki.StandingRevoked {
		t.Fatalf("the installed CRL lists it: %q, want revoked", st)
	}

	// Another fleet's root installed under this certificate.
	rootPEM, err := os.ReadFile(filepath.Join(other.Dir, pki.RootCertFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pki.RootCertFile), rootPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ := pki.InstalledStanding(dir, now); st != pki.StandingUntrusted {
		t.Fatalf("another root: %q, want untrusted", st)
	}

	if err := os.Remove(filepath.Join(dir, pki.CRLFile)); err != nil {
		t.Fatal(err)
	}
	if st, _ := pki.InstalledStanding(dir, now); st != pki.StandingNone {
		t.Fatalf("no CRL: %q, want none", st)
	}
}
