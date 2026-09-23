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

// IssueInstance signs pub — the instance's EXISTING key; the master never
// generates instance keys — into a leaf certificate usable for both client
// and server authentication, and appends the issuance to <dir>/issued.jsonl.
// There is no CSR: the operator hands over the public key.
func IssueInstance(dir string, root Root, pub ed25519.PublicKey, host string, role Role, validity time.Duration) ([]byte, Issued, error) {
	if !role.known() {
		return nil, Issued{}, fmt.Errorf("pki: unknown role %q", role)
	}
	if !validHost(host) {
		return nil, Issued{}, fmt.Errorf("pki: host %q must be non-empty and use only [A-Za-z0-9._-]", host)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, Issued{}, fmt.Errorf("pki: public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	fp := Fingerprint(pub)
	san, err := url.Parse(SAN(role, host, fp))
	if err != nil {
		return nil, Issued{}, fmt.Errorf("pki: SAN: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, Issued{}, err
	}
	now := time.Now()
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
		return nil, Issued{}, fmt.Errorf("pki: sign instance certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, Issued{}, err
	}
	rec := Issued{
		Serial:      cert.SerialNumber.Text(16),
		SAN:         san.String(),
		Fingerprint: fp,
		NotAfter:    cert.NotAfter,
		IssuedAt:    now.UTC(),
	}
	if err := appendJSONLine(filepath.Join(dir, IssuedLogFile), rec); err != nil {
		return nil, Issued{}, fmt.Errorf("pki: issuance log (certificate discarded, since it could never be revoked by serial): %w", err)
	}
	return certPEM(der), rec, nil
}

// appendJSONLine appends one JSON document and a newline, 0600. The master's
// logs are append-only files; history is the audit trail.
func appendJSONLine(path string, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
