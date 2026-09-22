package pki

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/cryptobyte"
	cbasn1 "golang.org/x/crypto/cryptobyte/asn1"
)

// One named error per refusal, so the TLS layer can log WHY a peer was
// turned away (the peer itself sees only a generic alert).
var (
	ErrUntrustedRoot = errors.New("pki: certificate does not chain to the fleet root")
	ErrExpired       = errors.New("pki: certificate expired or not yet valid")
	ErrRevoked       = errors.New("pki: certificate revoked")
	ErrNotEd25519    = errors.New("pki: certificate key is not ed25519")
	ErrSANMissing    = errors.New("pki: no single well-formed knomit:// URI SAN")
	ErrSANMismatch   = errors.New("pki: SAN fingerprint does not match the certificate key")
	ErrRoleUnknown   = errors.New("pki: unknown role in SAN")
)

// Identity is what a verified certificate establishes. Fingerprint is the
// full 64-hex value (the principal ID); Host is a label from the SAN and is
// never compared to anything.
type Identity struct {
	Fingerprint string
	Host        string
	Role        Role
	Serial      *big.Int
	NotAfter    time.Time
}

// Usage says which side of a connection the certificate is on.
type Usage int

const (
	// UsageClient: a peer connecting TO us presented this certificate.
	UsageClient Usage = iota
	// UsageServer: we connected to a peer and it presented this certificate.
	UsageServer
)

func (u Usage) eku() x509.ExtKeyUsage {
	if u == UsageServer {
		return x509.ExtKeyUsageServerAuth
	}
	return x509.ExtKeyUsageClientAuth
}

// VerifyInstanceChain is THE verification of a knomit certificate, for both
// directions. It is called from tls.Config.VerifyConnection on the server and
// the client, which is where every refusal happens; nothing downstream
// re-verifies. Steps, in order, each with its own error:
//
//  1. chain to a pool holding ONLY rootCert, at now, for the usage's EKU
//     (ErrUntrustedRoot, ErrExpired);
//  2. the CRL: nil is ErrCRLMissing (fail closed), a CRL not signed by the
//     root is ErrCRLInvalid, a listed serial is ErrRevoked;
//  3. the key is ed25519 (ErrNotEd25519);
//  4. exactly one knomit: URI SAN, byte-exact canonical form (ErrSANMissing);
//  5. its fp8 equals Short(Fingerprint(key)) (ErrSANMismatch) — the host
//     part is never compared;
//  6. its role is known (ErrRoleUnknown).
//
// A stale CRL (NextUpdate past) is still ENFORCED here; warning about it is
// the loader's job.
func VerifyInstanceChain(leaf *x509.Certificate, intermediates []*x509.Certificate, rootCert *x509.Certificate, crl *x509.RevocationList, now time.Time, usage Usage) (Identity, error) {
	if leaf == nil || rootCert == nil {
		return Identity{}, fmt.Errorf("%w: no certificate", ErrUntrustedRoot)
	}
	roots := x509.NewCertPool()
	roots.AddCert(rootCert)
	inter := x509.NewCertPool()
	for _, c := range intermediates {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{usage.eku()},
	}); err != nil {
		var inv x509.CertificateInvalidError
		if errors.As(err, &inv) && inv.Reason == x509.Expired {
			return Identity{}, fmt.Errorf("%w: %v", ErrExpired, err)
		}
		return Identity{}, fmt.Errorf("%w: %v", ErrUntrustedRoot, err)
	}

	if _, err := CheckCRL(crl, rootCert, nil); err != nil {
		return Identity{}, err
	}
	if IsRevoked(crl, leaf.SerialNumber) {
		return Identity{}, fmt.Errorf("%w: serial %s", ErrRevoked, leaf.SerialNumber.Text(16))
	}

	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return Identity{}, fmt.Errorf("%w: %T", ErrNotEd25519, leaf.PublicKey)
	}

	role, host, fp8, err := parseSAN(leaf)
	if err != nil {
		return Identity{}, err
	}
	fp := Fingerprint(pub)
	if Short(fp) != fp8 {
		return Identity{}, fmt.Errorf("%w: SAN carries %s, key is %s", ErrSANMismatch, fp8, Short(fp))
	}
	if !role.known() {
		return Identity{}, fmt.Errorf("%w: %q", ErrRoleUnknown, role)
	}
	return Identity{Fingerprint: fp, Host: host, Role: role, Serial: leaf.SerialNumber, NotAfter: leaf.NotAfter}, nil
}

var oidSubjectAltName = asn1.ObjectIdentifier{2, 5, 29, 17}

// rawURIs returns the uniformResourceIdentifier entries of the leaf's SAN
// extension AS WRITTEN. Go's parsed leaf.URIs have been through url.Parse,
// which lowercases the scheme; reading the DER keeps the check case-exact.
func rawURIs(leaf *x509.Certificate) ([]string, error) {
	var out []string
	for _, ext := range leaf.Extensions {
		if !ext.Id.Equal(oidSubjectAltName) {
			continue
		}
		in := cryptobyte.String(ext.Value)
		var seq cryptobyte.String
		if !in.ReadASN1(&seq, cbasn1.SEQUENCE) || !in.Empty() {
			return nil, errors.New("malformed subjectAltName")
		}
		for !seq.Empty() {
			var v cryptobyte.String
			var tag cbasn1.Tag
			if !seq.ReadAnyASN1(&v, &tag) {
				return nil, errors.New("malformed subjectAltName entry")
			}
			if tag == cbasn1.Tag(6).ContextSpecific() { // [6] uniformResourceIdentifier
				out = append(out, string(v))
			}
		}
	}
	return out, nil
}

// parseSAN extracts (role, host, fp8) from the ONE knomit: URI SAN. Strict
// and case-exact, because a name with two spellings is two identities:
//
//   - exactly one URI whose scheme is knomit in any case; two are refused even
//     if both are valid (no first-wins), and a non-lowercase scheme is refused;
//   - the role is u.Host (the URI authority): no userinfo, no port;
//   - the path is exactly "/<host>-<fp8>": one segment, no escapes, fp8 eight
//     lowercase hex after the LAST '-', host from [A-Za-z0-9._-];
//   - no query, no fragment.
//
// Together these leave exactly one spelling: the raw string is then
// knomit://<role>/<host>-<fp8> byte for byte, so no canonical-rebuild
// comparison is needed (it was tried, and no case reached it).
//
// An unknown role is returned, not refused, so the caller can map it to
// ErrRoleUnknown after the fingerprint check.
func parseSAN(leaf *x509.Certificate) (Role, string, string, error) {
	uris, err := rawURIs(leaf)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: %v", ErrSANMissing, err)
	}
	var mine []string
	for _, s := range uris {
		if len(s) >= len(SANScheme)+1 && strings.EqualFold(s[:len(SANScheme)+1], SANScheme+":") {
			mine = append(mine, s)
		}
	}
	switch len(mine) {
	case 0:
		return "", "", "", ErrSANMissing
	case 1:
	default:
		return "", "", "", fmt.Errorf("%w: %d knomit: URIs, want exactly one", ErrSANMissing, len(mine))
	}
	raw := mine[0]
	bad := func(why string) (Role, string, string, error) {
		return "", "", "", fmt.Errorf("%w: %q: %s", ErrSANMissing, raw, why)
	}
	if !strings.HasPrefix(raw, SANScheme+"://") {
		return bad("scheme must be exactly " + SANScheme + "://")
	}
	if strings.ContainsAny(raw, "?#%@") {
		return bad("query, fragment, escapes and userinfo are not allowed")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return bad(err.Error())
	}
	if u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return bad("only knomit://<role>/<host>-<fp8> is allowed")
	}
	if strings.Contains(u.Host, ":") {
		return bad("a port is not allowed")
	}
	seg, ok := strings.CutPrefix(u.Path, "/")
	if !ok || seg == "" || strings.Contains(seg, "/") {
		return bad("path must be exactly one segment")
	}
	i := strings.LastIndexByte(seg, '-')
	if i <= 0 {
		return bad("path must be <host>-<fp8>")
	}
	host, fp8 := seg[:i], seg[i+1:]
	if !validHost(host) || !isLowerHex8(fp8) {
		return bad("path must be <host>-<fp8> with fp8 eight lowercase hex")
	}
	return Role(u.Host), host, fp8, nil
}

func isLowerHex8(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
