package pki

import (
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// One named error per refusal, so the TLS layer can log WHY a peer was
// turned away (the peer itself sees only a generic alert).
var (
	ErrUntrustedRoot = errors.New("pki: certificate does not chain to the fleet root")
	ErrExpired       = errors.New("pki: certificate expired or not yet valid")
	ErrRevoked       = errors.New("pki: certificate revoked")
	ErrNotEd25519    = errors.New("pki: certificate key is not ed25519")
	// ErrRefusedByPeer: WE verified the peer, but the peer refused OUR
	// certificate. The peer's log carries the reason; we only see an alert.
	ErrRefusedByPeer = errors.New("pki: the peer refused this instance's certificate")
)

// Identity is what a verified certificate establishes.
//
// Fingerprint is the full 64-hex fingerprint of the leaf's KEY: the principal
// ID, whatever the certificate's names say. Email is the certificate's email
// SAN when it carries exactly one, else "". AgentID is AgentIDOfEmail(Email):
// non-empty exactly when that address is an agent address
// (<id>@agents.knomit.io), which is what makes the certificate an INSTANCE
// certificate (R3). Any other certificate — another address, several, or
// none — is an OPERATOR certificate. There is no role field.
type Identity struct {
	Fingerprint string
	Email       string
	AgentID     string
	Serial      *big.Int
	NotAfter    time.Time
}

// IsInstance reports whether the certificate is an instance certificate: its
// one email SAN is an agent address.
func (id Identity) IsInstance() bool { return id.AgentID != "" }

// PrincipalKind is the principal kind the certificate authenticates as:
// "instance" for an instance certificate, "operator" for every other one.
func (id Identity) PrincipalKind() string {
	if id.IsInstance() {
		return "instance"
	}
	return "operator"
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
//  3. the key is ed25519 (ErrNotEd25519).
//
// The identity it returns is IdentityOf's: the key's fingerprint, and the
// email SAN that says instance or operator. Nothing about the certificate's
// names is refused here: a certificate the root signed is admitted, and what
// its name makes it (instance or operator) decides what it may do.
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
	return IdentityOf(leaf)
}

// IdentityOf reads the Identity of a leaf WITHOUT verifying it: fingerprint
// and email SAN only. It must not be used to decide whether to trust a
// certificate; its callers sit behind VerifyInstanceChain (internal/edge/web's
// AuthMiddleware on the TLS listener, where every connection has already
// passed VerifyConnection; the fleet judge, after its own chain check). The
// fingerprint is computed from the leaf's public key, never read from a
// name, so whatever the certificate's names say, the principal is the key's.
//
// The kind is decided by the address alone (R3): exactly one email SAN that
// is an agent address is an instance; anything else is an operator. Only
// AgentIDOfEmail touches the address; the id it yields is never parsed.
func IdentityOf(leaf *x509.Certificate) (Identity, error) {
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return Identity{}, fmt.Errorf("%w: %T", ErrNotEd25519, leaf.PublicKey)
	}
	id := Identity{Fingerprint: Fingerprint(pub), Serial: leaf.SerialNumber, NotAfter: leaf.NotAfter}
	if len(leaf.EmailAddresses) == 1 {
		id.Email = leaf.EmailAddresses[0]
		id.AgentID = AgentIDOfEmail(id.Email)
	}
	return id, nil
}
