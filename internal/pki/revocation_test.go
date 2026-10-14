package pki

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

func testStatement(t *testing.T, root Root) RevocationStatement {
	t.Helper()
	id, err := RootID(root.Cert)
	if err != nil {
		t.Fatal(err)
	}
	return RevocationStatement{Root: id, Serial: big.NewInt(0xbeef), At: time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC), Reason: "key lost"}
}

// A signed revocation file is the canonical statement plus the root's
// signature, and verifies against that root. Each tampered field, a forged
// file (signed by another root, or re-signed over another root's
// fingerprint), and a non-canonical spelling are refused with
// ErrRevocationStatement, naming why.
//
// SABOTAGE: verify the signature over the file's own first five lines instead
// of the rebuilt bytes, or skip the root-fingerprint check → the "names
// another root" row is red; skip ed25519.Verify → every tamper row is red.
func TestRevocationStatement_SignedByTheRootAndRebuiltToVerify(t *testing.T) {
	_, root := newTestRoot(t)
	_, other := newTestRoot(t)
	s := testStatement(t, root)
	raw, err := SignRevocation(root, s)
	if err != nil {
		t.Fatal(err)
	}
	want := "knomit-revoke/v1\n" + s.Root + "\nbeef\n2026-10-14T09:30:00Z\nkey lost\nsignature: "
	if !strings.HasPrefix(string(raw), want) || !strings.HasSuffix(string(raw), "\n") {
		t.Fatalf("file:\n%s\nwant prefix\n%s", raw, want)
	}
	got, err := VerifyRevocation(raw, root.Cert)
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != s.Root || got.Serial.Cmp(s.Serial) != 0 || !got.At.Equal(s.At) || got.Reason != s.Reason {
		t.Fatalf("read back %+v, want %+v", got, s)
	}

	line := func(i int, v string) []byte {
		ls := strings.Split(string(raw), "\n")
		ls[i] = v
		return []byte(strings.Join(ls, "\n"))
	}
	otherID, _ := RootID(other.Cert)
	forged, err := SignRevocation(other, RevocationStatement{Root: otherID, Serial: s.Serial, At: s.At, Reason: s.Reason})
	if err != nil {
		t.Fatal(err)
	}
	// A statement naming THIS root's fingerprint but signed by another key.
	msg, _ := s.Bytes()
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	wrongKey := revocationFile(msg, ed25519.Sign(otherPriv, msg))
	// A DER CRL signature can never pass for a statement (domain tag).
	crlPEM := signedCRL(t, root, 1, 0xbeef)

	for _, c := range []struct {
		name, why string
		raw       []byte
	}{
		{"serial tampered", "signature does not verify", line(2, "beee")},
		{"time tampered", "signature does not verify", line(3, "2026-10-14T09:31:00Z")},
		{"reason tampered", "signature does not verify", line(4, "routine")},
		{"tag changed", "first line", line(0, "knomit-oauth-approve/v1")},
		{"names another root", "not this fleet's root", line(1, otherID)},
		{"forged: another root signed it", "not this fleet's root", forged},
		{"forged: this root's fingerprint, another key", "signature does not verify", wrongKey},
		{"serial with a leading zero", "canonical", line(2, "0beef")},
		{"time in another zone", "canonical", line(3, "2026-10-14T11:30:00+02:00")},
		{"upper-case serial", "canonical", line(2, "BEEF")},
		{"trailing space on the signature", "base64", append(bytes.Clone(bytes.TrimSuffix(raw, []byte("\n"))), " \n"...)},
		{"trailing space on the reason", "signature does not verify", line(4, "key lost ")},
		{"no final newline", "statement lines", bytes.TrimSuffix(raw, []byte("\n"))},
		{"extra line", "statement lines", append(append([]byte{}, raw...), "x\n"...)},
		{"bad signature encoding", "base64", line(5, "signature: !!")},
		{"a CRL", "statement lines", crlPEM},
		{"reason with a control character", "printable", line(4, "a\tb")},
	} {
		_, err := VerifyRevocation(c.raw, root.Cert)
		if !errors.Is(err, ErrRevocationStatement) || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: err %v, want ErrRevocationStatement naming %q", c.name, err, c.why)
		}
	}
	// The signature is over the rebuilt bytes, which are the file's first
	// five lines: re-encoding the same signature over the same fields passes.
	sig, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(strings.Split(string(raw), "\n")[5], revocationSigPrefix))
	if _, err := VerifyRevocation(revocationFile(msg, sig), root.Cert); err != nil {
		t.Fatalf("positive control: %v", err)
	}
}

// Bytes validates every field, so none can carry a separator; SignRevocation
// signs only a statement naming its own root.
func TestRevocationStatement_FieldsAreValidatedBeforeSigning(t *testing.T) {
	_, root := newTestRoot(t)
	s := testStatement(t, root)
	for name, mut := range map[string]func(*RevocationStatement){
		"zero serial":       func(s *RevocationStatement) { s.Serial = big.NewInt(0) },
		"no serial":         func(s *RevocationStatement) { s.Serial = nil },
		"reason newline":    func(s *RevocationStatement) { s.Reason = "a\nb" },
		"reason non-ASCII":  func(s *RevocationStatement) { s.Reason = "clé perdue" },
		"reason too long":   func(s *RevocationStatement) { s.Reason = strings.Repeat("x", 201) },
		"sub-second time":   func(s *RevocationStatement) { s.At = s.At.Add(time.Millisecond) },
		"local time":        func(s *RevocationStatement) { s.At = s.At.In(time.FixedZone("x", 3600)) },
		"zero time":         func(s *RevocationStatement) { s.At = time.Time{} },
		"short fingerprint": func(s *RevocationStatement) { s.Root = s.Root[:63] },
		"upper fingerprint": func(s *RevocationStatement) { s.Root = strings.ToUpper(s.Root) },
		"another root's fp": func(s *RevocationStatement) { s.Root = strings.Repeat("a", 64) },
	} {
		c := s
		mut(&c)
		if _, err := SignRevocation(root, c); !errors.Is(err, ErrRevocationStatement) {
			t.Errorf("%s: signed (%v)", name, err)
		}
	}
}

// An issuance file is the issued leaf itself. It must chain to the fleet
// root and follow IssueLeaf's email SAN rules: an instance leaf carries one
// address, <id>@agents.knomit.io for a valid (lower-case, no +op) id; an
// operator leaf any other address or none. A CA, another root's leaf, a URI
// SAN, junk, and a case variant of the agent domain are refused.
//
// SABOTAGE: drop the chain check → "another root" is admitted → red; drop
// the agent-id check → "upper-case id" is admitted → red.
func TestCheckIssuance_ChainsAndFollowsTheEmailRules(t *testing.T) {
	f := newFleet(t)
	other := newFleet(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	issue := func(fl fleet, email string) []byte {
		is, err := IssueLeaf(fl.root, pub, email, time.Hour, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return is.CertPEM
	}
	crafted := func(emails []string, uris ...string) []byte {
		return certPEM(f.crafted(t, pub, emails, uris...).Raw)
	}
	for _, c := range []struct {
		name    string
		raw     []byte
		agentID string
		ok      bool
	}{
		{"instance", issue(f, "laptop@agents.knomit.io"), "laptop", true},
		{"operator with an address", issue(f, "ops@example.com"), "", true},
		{"operator with none", issue(f, ""), "", true},
		{"another root", issue(other, "laptop@agents.knomit.io"), "", false},
		{"the root itself (a CA)", certPEM(f.root.Cert.Raw), "", false},
		{"junk", []byte("junk"), "", false},
		{"upper-case id", crafted([]string{"Laptop@agents.knomit.io"}), "", false},
		{"op suffix", crafted([]string{"laptop+learn@agents.knomit.io"}), "", false},
		{"agent domain in upper case", crafted([]string{"laptop@AGENTS.knomit.io"}), "", false},
		{"two addresses, one an agent's", crafted([]string{"laptop@agents.knomit.io", "ops@example.com"}), "", false},
		{"URI SAN", crafted([]string{"laptop@agents.knomit.io"}, "knomit://instance/laptop"), "", false},
	} {
		id, err := CheckIssuance(c.raw, f.root.Cert)
		switch {
		case c.ok && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case !c.ok && !errors.Is(err, ErrIssuanceInvalid):
			t.Errorf("%s: err %v, want ErrIssuanceInvalid", c.name, err)
		case c.ok && id.AgentID != c.agentID:
			t.Errorf("%s: agent %q, want %q", c.name, id.AgentID, c.agentID)
		}
	}
}
