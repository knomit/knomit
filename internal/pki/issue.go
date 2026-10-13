package pki

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Role is the certificate's coarse TYPE. It is not a permission set:
// permissions resolve from grants by principal, because a certificate lives
// for months and a permission baked into it is stale the day the operator
// changes their mind.
//
// The role is carried in the URI SAN — knomit://<role>/<host>-<fp8> — and
// NOT in a private extension. A private OID under a 2.25 UUID arc cannot be
// written in Go (pkix.Extension.Id is []int) and is refused on parse
// (crypto/x509/parser.go:233, "malformed extension OID field"); an IANA
// enterprise number costs days of latency for one integer; the SAN is
// already parsed and cross-checked by VerifyInstanceChain.
type Role string

const (
	RoleInstance Role = "instance"
	RoleOperator Role = "operator"
)

func (r Role) known() bool { return r == RoleInstance || r == RoleOperator }

// SANScheme is the URI scheme of every knomit certificate name.
const SANScheme = "knomit"

// SAN is the certificate name for a key: knomit://<role>/<host>-<fp8>.
//
// Go's url.Parse puts <role> in the URI AUTHORITY (u.Host), not the path;
// the path is exactly "/<host>-<fp8>". The role is deliberately not "agent"
// even though the branch is agent/<host>-<fp8>: the SAN names a ROLE, the
// branch names a namespace, and reusing the word would invite reading the
// SAN's path as the branch name. Only the fp8 is ever compared; the host is
// a label.
func SAN(role Role, host, fp string) string {
	return SANScheme + "://" + string(role) + "/" + host + "-" + Short(fp)
}

// validHost reports whether host is safe as the single path segment of the
// SAN: non-empty, and only hostname characters. Anything else would either
// change what url.Parse returns (a '/', '?', '#') or need escaping, and an
// escaped name is two spellings of one identity.
func validHost(host string) bool {
	if host == "" {
		return false
	}
	for _, c := range host {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// Issued is one line of the master's issuance log. Revocation references the
// serial, so an issuance that could not be logged is not returned.
type Issued struct {
	Serial      string    `json:"serial"` // lowercase hex, as big.Int.Text(16)
	SAN         string    `json:"san"`
	Fingerprint string    `json:"fingerprint"` // full 64 hex
	NotAfter    time.Time `json:"not_after"`
	IssuedAt    time.Time `json:"issued_at"`
}

// Issuance is one signed leaf as bytes: what IssueLeaf returns and the
// caller places (a file in the master directory, or an artifact in the
// fleet repo).
type Issuance struct {
	CertPEM []byte
	Record  Issued
	// Line is Record as one ledger line: JSON and a trailing newline, the
	// exact bytes IssueInstance appends to issued.jsonl.
	Line []byte
}

// newSerial is the serial source. A variable only so a test can pin it to
// compare two issuances field by field; production never reassigns it.
var newSerial = randomSerial

// IssueLeaf signs pub — the instance's EXISTING key; the master never
// generates instance keys — into a leaf certificate usable for both client
// and server authentication, valid from now, and returns it with its ledger
// line. It writes nothing. There is no CSR: the operator hands over the
// public key. The SAN's fp8 is computed from pub, never read from a name.
func IssueLeaf(root Root, pub ed25519.PublicKey, host string, role Role, validity time.Duration, now time.Time) (Issuance, error) {
	if !role.known() {
		return Issuance{}, fmt.Errorf("pki: unknown role %q", role)
	}
	if !validHost(host) {
		return Issuance{}, fmt.Errorf("pki: host %q must be non-empty and use only [A-Za-z0-9._-]", host)
	}
	if len(pub) != ed25519.PublicKeySize {
		return Issuance{}, fmt.Errorf("pki: public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	fp := Fingerprint(pub)
	san, err := url.Parse(SAN(role, host, fp))
	if err != nil {
		return Issuance{}, fmt.Errorf("pki: SAN: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return Issuance{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host + "-" + Short(fp)},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(validity),
		BasicConstraintsValid: true,
		IsCA:                  false,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		URIs:                  []*url.URL{san},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, root.Cert, pub, root.Signer)
	if err != nil {
		return Issuance{}, fmt.Errorf("pki: sign instance certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return Issuance{}, err
	}
	rec := Issued{
		Serial:      cert.SerialNumber.Text(16),
		SAN:         san.String(),
		Fingerprint: fp,
		NotAfter:    cert.NotAfter,
		IssuedAt:    now.UTC(),
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return Issuance{}, err
	}
	return Issuance{CertPEM: certPEM(der), Record: rec, Line: append(line, '\n')}, nil
}

// IssueInstance is IssueLeaf as of time.Now() that also appends the ledger
// line to <dir>/issued.jsonl: the master-directory form `identity enroll`
// uses. A certificate whose line could not be logged is not returned, since
// revocation finds serials in that log.
func IssueInstance(dir string, root Root, pub ed25519.PublicKey, host string, role Role, validity time.Duration) ([]byte, Issued, error) {
	is, err := IssueLeaf(root, pub, host, role, validity, time.Now())
	if err != nil {
		return nil, Issued{}, err
	}
	if err := appendLine(filepath.Join(dir, IssuedLogFile), is.Line); err != nil {
		return nil, Issued{}, fmt.Errorf("pki: issuance log (certificate discarded, since it could never be revoked by serial): %w", err)
	}
	return is.CertPEM, is.Record, nil
}

// appendLine appends one ledger line (already newline-terminated), 0600.
// The master's logs are append-only files; history is the audit trail.
func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
