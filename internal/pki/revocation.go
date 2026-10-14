package pki

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// The fleet repository records each issuance and each revocation as ONE
// root-signed file of its own (D7), written once and never changed, instead
// of an append-only ledger:
//
//   - an issuance file is the issued leaf's PEM itself — the root's signature
//     on it is the record (CheckIssuance verifies one);
//   - a revocation file is a RevocationStatement signed by the root
//     (SignRevocation / VerifyRevocation).
//
// A file's NAME is for uniqueness and readability only: nothing derives an
// order or a meaning from it (D18). Order and meaning come from the signed
// content.

// RevocationTag is the first line of every revocation statement: the domain
// separation of this use of the fleet root key. The root also signs X.509
// certificates and CRLs, which are DER and start with 0x30; a statement that
// starts with this text can never verify as either, nor they as it
// (kb/invariants/oauth/signed-approval-statement). The verb and the version
// live in the tag.
const RevocationTag = "knomit-revoke/v1"

// maxRevocationReason bounds a statement's reason.
const maxRevocationReason = 200

// revocationSigPrefix starts the signature line of a revocation file.
const revocationSigPrefix = "signature: "

// ErrRevocationStatement: a revocation file that is malformed, not canonical,
// for another root, or not signed by the root.
var ErrRevocationStatement = errors.New("pki: revocation statement invalid")

// RevocationStatement is what the fleet root signs to revoke one serial.
type RevocationStatement struct {
	// Root is Fingerprint of the fleet root's key (64 lowercase hex): the
	// statement is bound to one root, so another fleet's statement means
	// nothing here.
	Root string
	// Serial is the revoked certificate's serial (positive).
	Serial *big.Int
	// At is the revocation time, UTC, whole seconds.
	At time.Time
	// Reason is free text: printable ASCII, at most 200 bytes, may be "".
	Reason string
}

// Bytes is THE encoding of s, the only bytes ever signed or verified:
//
//	knomit-revoke/v1
//	<root fingerprint, 64 lowercase hex>
//	<serial, lowercase hex as big.Int.Text(16)>
//	<revocation time, RFC 3339 UTC, whole seconds>
//	<reason>
//
// joined with \n and no trailing newline. Every field is validated first, so
// none can hold a separator.
func (s RevocationStatement) Bytes() ([]byte, error) {
	if !isLowerHex(s.Root, 64) {
		return nil, fmt.Errorf("%w: root fingerprint %q is not 64 lowercase hex", ErrRevocationStatement, s.Root)
	}
	if s.Serial == nil || s.Serial.Sign() <= 0 {
		return nil, fmt.Errorf("%w: the serial must be positive", ErrRevocationStatement)
	}
	if s.At.IsZero() || s.At.Location() != time.UTC || !s.At.Equal(s.At.Truncate(time.Second)) {
		return nil, fmt.Errorf("%w: the revocation time must be UTC, whole seconds", ErrRevocationStatement)
	}
	if len(s.Reason) > maxRevocationReason || !printableOrSpace(s.Reason) {
		return nil, fmt.Errorf("%w: the reason must be printable ASCII, at most %d bytes", ErrRevocationStatement, maxRevocationReason)
	}
	return []byte(strings.Join([]string{
		RevocationTag,
		s.Root,
		s.Serial.Text(16),
		s.At.Format(time.RFC3339),
		s.Reason,
	}, "\n")), nil
}

// SignRevocation signs s with the fleet root and returns the revocation FILE:
// s.Bytes(), a newline, "signature: <base64 ed25519 signature>", a newline.
// s.Root must be the root's own fingerprint.
func SignRevocation(root Root, s RevocationStatement) ([]byte, error) {
	id, err := RootID(root.Cert)
	if err != nil {
		return nil, err
	}
	if s.Root != id {
		return nil, fmt.Errorf("%w: the statement names root %s, not this root %s", ErrRevocationStatement, s.Root, id)
	}
	msg, err := s.Bytes()
	if err != nil {
		return nil, err
	}
	sig, err := root.Signer.Sign(rand.Reader, msg, crypto.Hash(0))
	if err != nil {
		return nil, fmt.Errorf("pki: sign revocation: %w", err)
	}
	return revocationFile(msg, sig), nil
}

func revocationFile(msg, sig []byte) []byte {
	out := append(append([]byte{}, msg...), '\n')
	out = append(out, revocationSigPrefix...)
	out = append(out, base64.StdEncoding.EncodeToString(sig)...)
	return append(out, '\n')
}

// VerifyRevocation reads a revocation file and verifies it against the fleet
// root. It parses the five statement fields, validates each, REBUILDS the
// statement's bytes from the validated fields (Bytes) and verifies the root's
// signature over what it rebuilt — never over bytes taken from the file. The
// file must also be exactly the canonical encoding of what it says (no other
// spelling of a field, no extra line or space), and the statement must name
// this root's fingerprint.
func VerifyRevocation(raw []byte, rootCert *x509.Certificate) (RevocationStatement, error) {
	bad := func(format string, a ...any) (RevocationStatement, error) {
		return RevocationStatement{}, fmt.Errorf("%w: "+format, append([]any{ErrRevocationStatement}, a...)...)
	}
	if rootCert == nil {
		return bad("no root to verify it against")
	}
	pub, ok := rootCert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return bad("the root key is %T, want ed25519", rootCert.PublicKey)
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) != 7 || lines[6] != "" {
		return bad("want 5 statement lines and a signature line, each ending in a newline")
	}
	if lines[0] != RevocationTag {
		return bad("the first line is %q, want %q", lines[0], RevocationTag)
	}
	serial, ok := new(big.Int).SetString(lines[2], 16)
	if !ok {
		return bad("serial %q is not hex", lines[2])
	}
	at, err := time.Parse(time.RFC3339, lines[3])
	if err != nil {
		return bad("revocation time %q: %v", lines[3], err)
	}
	s := RevocationStatement{Root: lines[1], Serial: serial, At: at.UTC(), Reason: lines[4]}
	msg, err := s.Bytes()
	if err != nil {
		return RevocationStatement{}, err
	}
	sigB64, ok := strings.CutPrefix(lines[5], revocationSigPrefix)
	if !ok {
		return bad("the last line does not start with %q", revocationSigPrefix)
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return bad("the signature is not a base64 ed25519 signature")
	}
	// Canonical: the file is exactly what its fields encode to, so one
	// statement has one spelling (no leading zeros, other time zones, spaces).
	if !bytes.Equal(raw, revocationFile(msg, sig)) {
		return bad("the file is not the canonical encoding of its fields")
	}
	if want := Fingerprint(pub); s.Root != want {
		return bad("it names root %s, not this fleet's root %s", s.Root, want)
	}
	if !ed25519.Verify(pub, msg, sig) {
		return bad("the signature does not verify against the fleet root")
	}
	return s, nil
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// printableOrSpace: printable ASCII, the space included.
func printableOrSpace(s string) bool {
	for _, c := range s {
		if c < ' ' || c > '~' {
			return false
		}
	}
	return true
}

// ErrIssuanceInvalid: an issuance file that is not a leaf the fleet root
// issued as IssueLeaf issues them.
var ErrIssuanceInvalid = errors.New("pki: issuance record invalid")

// CheckIssuance verifies an issuance file — the issued leaf's PEM — against
// the fleet root: exactly one CERTIFICATE block and nothing else, byte-equal
// to that block's own PEM encoding (what IssueLeaf writes), not a CA,
// chaining to a pool holding ONLY rootCert for client authentication at a
// moment inside the
// leaf's own validity (the record is historical: expiry and the CRL are not
// judged, as a later revocation does not unmake an issuance), an ed25519 key,
// no URI SAN (R1), and IssueLeaf's email SAN rules (R1/R2): an address in the
// agent domain makes it an INSTANCE certificate, which must carry that one
// address only, exactly <id>@agents.knomit.io for a ValidAgentID id (no +op,
// no upper case); the agent domain spelled in another case is refused; any
// other address, or none, is an OPERATOR certificate. It returns the leaf's
// identity.
func CheckIssuance(raw []byte, rootCert *x509.Certificate) (Identity, error) {
	bad := func(format string, a ...any) (Identity, error) {
		return Identity{}, fmt.Errorf("%w: "+format, append([]any{ErrIssuanceInvalid}, a...)...)
	}
	if rootCert == nil {
		return bad("no root to check it against")
	}
	// Canonical, like the revocation file: the file must be exactly the
	// block's own encoding, so nothing rides along before or after it (no
	// junk, no second block, no headers, no other line endings).
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return bad("no CERTIFICATE PEM block")
	}
	if !bytes.Equal(raw, pem.EncodeToMemory(&pem.Block{Type: blk.Type, Bytes: blk.Bytes})) {
		return bad("the file is not exactly one CERTIFICATE PEM block, encoded as IssueLeaf encodes it, and nothing else")
	}
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return bad("%v", err)
	}
	if leaf.IsCA {
		return bad("it is a CA certificate, not a leaf")
	}
	if len(leaf.URIs) != 0 {
		return bad("it carries a URI SAN; a knomit certificate's identity is its email SAN only")
	}
	at := leaf.NotBefore
	if rootCert.NotBefore.After(at) {
		at = rootCert.NotBefore
	}
	roots := x509.NewCertPool()
	roots.AddCert(rootCert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return bad("%v: %v", ErrUntrustedRoot, err)
	}
	agent := false
	for _, e := range leaf.EmailAddresses {
		i := strings.LastIndexByte(e, '@')
		if i < 0 {
			return bad("email SAN %q is not an address", e)
		}
		domain := e[i+1:]
		if err := agentDomainSpelling(domain); err != nil {
			return bad("%v", err)
		}
		agent = agent || domain == AgentEmailDomain
	}
	if agent {
		if len(leaf.EmailAddresses) != 1 {
			return bad("an instance certificate carries exactly one email SAN, its agent address; this one carries %d", len(leaf.EmailAddresses))
		}
		email := leaf.EmailAddresses[0]
		local := strings.TrimSuffix(email, "@"+AgentEmailDomain)
		if err := ValidAgentID(local); err != nil {
			return bad("email SAN %q: %v", email, err)
		}
	}
	id, err := IdentityOf(leaf)
	if err != nil {
		return bad("%v", err)
	}
	return id, nil
}
