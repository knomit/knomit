package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/pki"
)

// Review finding 4: the fingerprint `install --root` takes can be printed
// from root.crt alone, with the key moved away.
func TestIdentity_RootFingerprintNeedsOnlyRootCrt(t *testing.T) {
	dir := fleetRoot(t)
	c, _ := pki.LoadRootCert(filepath.Join(dir, pki.RootCertFile))
	want, _ := pki.RootID(c)
	if err := os.Remove(filepath.Join(dir, pki.RootKeyFile)); err != nil { // no key anywhere
		t.Fatal(err)
	}
	out, err := run(t, "", "identity", "root-fingerprint", "--dir", dir)
	if err != nil || strings.TrimSpace(out) != want {
		t.Fatalf("root-fingerprint: %v %q, want %s", err, out, want)
	}
	if strings.Contains(rootFingerprintForm, "root.key") {
		t.Fatal("the --root help still sends operators to root.key")
	}
}
