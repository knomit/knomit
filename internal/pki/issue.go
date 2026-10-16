package pki

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// AgentEmailDomain is the domain of every agent address: a commit's author
// <id>+<op>@agents.knomit.io, its committer <id>@agents.knomit.io, and an
// INSTANCE certificate's one email SAN <id>@agents.knomit.io.
const AgentEmailDomain = "agents.knomit.io"

// AgentIDOfEmail returns the agent id in an agent address,
// <id>[+<op>]@agents.knomit.io, or "" for any other address. It is the ONE
// string operation on an identity (D18): require the domain to be exactly
// agents.knomit.io, take the local part, drop any +<op>, and require what is
// left to be a valid agent id (ValidAgentID: lower case, no '@', no white
// space). NOTHING is case folded: Vic@agents.knomit.io and
// vic@AGENTS.KNOMIT.IO are not agent addresses at all. The id it returns is
// OPAQUE: it is only compared for equality, byte for byte, never parsed
// further.
func AgentIDOfEmail(email string) string {
	local, ok := strings.CutSuffix(email, "@"+AgentEmailDomain)
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(local, "+")
	if ValidAgentID(id) != nil {
		return ""
	}
	return id
}

// ErrAgentIDInvalid: an agent id that cannot be written as the local part of
// an agent address and read back unchanged by AgentIDOfEmail.
var ErrAgentIDInvalid = errors.New("pki: invalid agent id")

// ValidAgentID refuses an agent id that AgentIDOfEmail would not return from
// <id>@agents.knomit.io as itself: empty, holding an UPPER-CASE letter (agent
// ids are lower case everywhere, and every comparison of one is exact, so an
// upper-case id could never equal the lower-case member folder, file name or
// record that names it), '+' (everything after it would be read as an
// operation, so a+b would be judged as a), '@', '/' (the id names one folder,
// members/<id>/, and one leaf file, certs/<id>.pem: alice/x would own a
// subfolder of alice's and a leaf outside certs/*.pem), or white space or a
// control character. Every id it accepts reads back as itself. It is checked where an
// id is MINTED (repos.ResolveIdentity), where a stored id is read back at boot,
// and where a certificate is issued for one; it never extracts anything from
// an id. IssueLeaf additionally needs the whole address to be printable ASCII
// (an email SAN is an IA5String).
func ValidAgentID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: empty", ErrAgentIDInvalid)
	}
	if strings.ContainsAny(id, "+@") {
		return fmt.Errorf("%w: %q holds '+' or '@'", ErrAgentIDInvalid, id)
	}
	if strings.Contains(id, "/") {
		return fmt.Errorf("%w: %q holds '/' (an id names one folder, members/<id>/, and one file, certs/<id>.pem)", ErrAgentIDInvalid, id)
	}
	for _, c := range id {
		switch {
		case unicode.IsSpace(c) || unicode.IsControl(c):
			return fmt.Errorf("%w: %q holds white space or a control character", ErrAgentIDInvalid, id)
		case unicode.IsUpper(c):
			return fmt.Errorf("%w: %q holds an upper-case letter (agent ids are lower case)", ErrAgentIDInvalid, id)
		}
	}
	return nil
}

func printableASCII(s string) bool {
	for _, c := range s {
		if c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

// AgentEmail is the email SAN of an INSTANCE certificate for agent id:
// <id>@agents.knomit.io, the same address the instance commits as. It refuses
// an id ValidAgentID refuses.
func AgentEmail(id string) (string, error) {
	if err := ValidAgentID(id); err != nil {
		return "", err
	}
	return id + "@" + AgentEmailDomain, nil
}

// agentDomainSpelling refuses a domain that is the agent domain in another
// case spelling (AGENTS.knomit.io). Such an address is no agent address
// (AgentIDOfEmail is exact), so a certificate carrying it would silently be
// an OPERATOR certificate for what was meant as an instance: refusing it is
// louder. It folds case only to refuse; it never makes two spellings equal.
func agentDomainSpelling(domain string) error {
	if domain != AgentEmailDomain && strings.EqualFold(domain, AgentEmailDomain) {
		return fmt.Errorf("%w: the agent domain is written %q; it is %q, lower case", ErrAgentIDInvalid, domain, AgentEmailDomain)
	}
	return nil
}

// Issued is one line of the fleet root's issuance log. Revocation references the
// serial, so an issuance that could not be logged is not returned.
type Issued struct {
	Serial      string    `json:"serial"`          // lowercase hex, as big.Int.Text(16)
	Email       string    `json:"email,omitempty"` // the email SAN; "" for an operator certificate with none
	Fingerprint string    `json:"fingerprint"`     // full 64 hex
	NotAfter    time.Time `json:"not_after"`
	IssuedAt    time.Time `json:"issued_at"`
}

// Issuance is one signed leaf as bytes: what IssueLeaf returns and the
// caller places. In the fleet repository, CertPEM is BOTH the member's leaf
// (certs/<id>.pem) and the issuance record (issued/<name>.pem, one file per
// issuance, CheckIssuance); there is no ledger there. Line is for the fleet
// root directory's LOCAL issuance log only.
type Issuance struct {
	CertPEM []byte
	Record  Issued
	// Line is Record as one ledger line: JSON and a trailing newline, the
	// exact bytes IssueInstance appends to issued.jsonl.
	Line []byte
}

// IssueTimeAfter is the earliest issue time (IssueLeaf's now) whose leaf's
// NotBefore is strictly later than prev, at the one-second precision a
// certificate's times have: a leaf that REPLACES another must start after it
// (the fleet ontology's replace_when: leaf-renewal), even when both are
// issued within one second.
func IssueTimeAfter(prev time.Time) time.Time {
	return prev.Truncate(time.Second).Add(time.Second + clockSkew)
}

// newSerial is the serial source. A variable only so a test can pin it to
// compare two issuances field by field; production never reassigns it.
var newSerial = randomSerial

// IssueLeaf signs pub — the instance's EXISTING key; the fleet root never
// generates instance keys — into a leaf certificate usable for both client
// and server authentication, valid from now, and returns it with its ledger
// line. It writes nothing. There is no CSR: the operator hands over the
// public key.
//
// The certificate's identity is its one email SAN (R1). For an INSTANCE it is
// AgentEmail(id), the address the instance commits as; any other address, or
// email "" (no email SAN at all), makes an OPERATOR certificate. There is no
// role field and no knomit:// URI: IdentityOf reads the kind from the
// address, and the principal is always the KEY's fingerprint.
func IssueLeaf(root Root, pub ed25519.PublicKey, email string, validity time.Duration, now time.Time) (Issuance, error) {
	if len(pub) != ed25519.PublicKeySize {
		return Issuance{}, fmt.Errorf("pki: public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	if email != "" {
		local, domain, ok := strings.Cut(email, "@")
		if !ok || local == "" || domain == "" || strings.Contains(domain, "@") || !printableASCII(email) {
			return Issuance{}, fmt.Errorf("pki: %q is not an email address", email)
		}
		if err := agentDomainSpelling(domain); err != nil {
			return Issuance{}, err
		}
		if domain == AgentEmailDomain {
			if err := ValidAgentID(local); err != nil {
				return Issuance{}, err
			}
		}
	}
	fp := Fingerprint(pub)
	serial, err := newSerial()
	if err != nil {
		return Issuance{}, err
	}
	cn := email
	if cn == "" {
		cn = "knomit operator " + Short(fp)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(validity),
		BasicConstraintsValid: true,
		IsCA:                  false,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	if email != "" {
		tmpl.EmailAddresses = []string{email}
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
		Email:       email,
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

// IssueInstance is IssueLeaf as of time.Now() that also records the issuance
// in the fleet root directory dir (RecordIssuance): the form fleet create
// uses for the master's own leaf. A certificate whose line could not be
// logged is not returned.
func IssueInstance(dir string, root Root, pub ed25519.PublicKey, email string, validity time.Duration) ([]byte, Issued, error) {
	is, err := IssueLeaf(root, pub, email, validity, time.Now())
	if err != nil {
		return nil, Issued{}, err
	}
	if err := RecordIssuance(dir, is); err != nil {
		return nil, Issued{}, fmt.Errorf("pki: issuance log (certificate discarded): %w", err)
	}
	return is.CertPEM, is.Record, nil
}

// RecordIssuance appends is's line to <dir>/issued.jsonl, the fleet root's
// local audit log of every leaf it issued.
func RecordIssuance(dir string, is Issuance) error {
	return appendLine(filepath.Join(dir, IssuedLogFile), is.Line)
}

// LoadIssued reads the fleet root's issuance log (<dir>/issued.jsonl), in the
// order written. An absent log is empty; blank lines are skipped; any other
// line that is not an Issued is an error.
func LoadIssued(dir string) ([]Issued, error) {
	raw, err := os.ReadFile(filepath.Join(dir, IssuedLogFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Issued
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var is Issued
		if err := json.Unmarshal(sc.Bytes(), &is); err != nil {
			return nil, fmt.Errorf("pki: %s: %w", IssuedLogFile, err)
		}
		out = append(out, is)
	}
	return out, sc.Err()
}

// appendLine appends one ledger line (already newline-terminated), 0600.
// The fleet root's logs are append-only files; history is the audit trail.
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
