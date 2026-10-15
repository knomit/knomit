package pki

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bareBundle(t *testing.T, f fleet, m member) []byte {
	t.Helper()
	root, err := os.ReadFile(filepath.Join(f.dir, RootCertFile))
	if err != nil {
		t.Fatal(err)
	}
	crl, err := os.ReadFile(filepath.Join(f.dir, CRLFile))
	if err != nil {
		t.Fatal(err)
	}
	return append(append(append([]byte{}, m.certPEM...), root...), crl...)
}

// The v1 bundle document: the header names the fleet URL, the agent, the
// root's fingerprint and the leaf's serial, then the PEM blocks. SplitBundle
// reads the URL from it and refuses a header that disagrees with the PEM
// blocks, an unknown version, an unknown or missing line. A bare PEM bundle
// (the create's own install) still splits, with no URL.
// SABOTAGE: SplitBundle not comparing the header to the certificates -> the
// "another agent" / "another root" / "another serial" rows split -> red.
func TestBundleDocument_RoundTripAndRefusals(t *testing.T) {
	f := newFleet(t)
	m := f.enroll(t, "alpha")
	bare := bareBundle(t, f, m)

	doc, err := FleetBundle(bare, "https://hub.example:19279/git/fleet")
	if err != nil {
		t.Fatal(err)
	}
	rootID, _ := RootID(f.root.Cert)
	wantHead := BundleHeader + "\nurl: https://hub.example:19279/git/fleet\nagent: alpha\nroot: " + rootID + "\nserial: " + m.cert.SerialNumber.Text(16) + "\n-----BEGIN "
	if !strings.HasPrefix(string(doc), wantHead) {
		t.Fatalf("document:\n%s\nwant it to start with:\n%s", doc, wantHead)
	}
	b, err := SplitBundle(doc)
	if err != nil || b.FleetURL != "https://hub.example:19279/git/fleet" || !b.Leaf.Equal(m.cert) {
		t.Fatalf("split: %v url %q", err, b.FleetURL)
	}
	if b, err := SplitBundle(bare); err != nil || b.FleetURL != "" {
		t.Fatalf("bare PEM: %v url %q", err, b.FleetURL)
	}
	if _, err := FleetBundle(doc, "https://other.example"); err == nil {
		t.Fatal("a second URL was added to a document")
	}
	if _, err := FleetBundle(bare, "https://h\nroot: x"); err == nil {
		t.Fatal("a URL with a newline became header lines")
	}

	head := func(lines ...string) []byte {
		return append([]byte(strings.Join(lines, "\n")+"\n"), bare...)
	}
	url, agent, root, serial := "url: https://h/git/fleet", "agent: alpha", "root: "+rootID, "serial: "+m.cert.SerialNumber.Text(16)
	for name, raw := range map[string][]byte{
		"another version":  head("# knomit fleet bundle v2", url, agent, root, serial),
		"another agent":    head(BundleHeader, url, "agent: bravo", root, serial),
		"another root":     head(BundleHeader, url, agent, "root: "+strings.Repeat("0", len(rootID)), serial),
		"another serial":   head(BundleHeader, url, agent, root, "serial: 1"),
		"no url":           head(BundleHeader, agent, root, serial),
		"an unknown line":  head(BundleHeader, url, agent, root, serial, "token: ghp_x"),
		"a duplicate line": head(BundleHeader, url, url, agent, root, serial),
	} {
		if _, err := SplitBundle(raw); !errors.Is(err, ErrMalformedBundle) {
			t.Errorf("%s: err=%v, want ErrMalformedBundle", name, err)
		}
	}
}
