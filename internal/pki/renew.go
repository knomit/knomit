package pki

import (
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"time"
)

// The renewal window (fleet design D17) is DERIVED from each artifact's own
// validity, never a day count: a certificate is renewed, and a CRL
// re-signed, once less than renewWindowDivisor-th of its own lifetime
// remains. A 90-day leaf renews in its last 30 days and a 30-day CRL is
// re-signed in its last 10, with no "30 days" or "7 days" literal anywhere.
const renewWindowDivisor = 3

// NeedsRenewal reports whether cert is inside its renewal window at now:
// NotAfter - now is below a third of NotAfter - NotBefore. An expired
// certificate, and one with no positive lifetime, needs renewal.
func NeedsRenewal(cert *x509.Certificate, now time.Time) bool {
	return inFinalFraction(cert.NotBefore, cert.NotAfter, now)
}

// CRLNeedsResign is NeedsRenewal for a CRL, over NextUpdate - ThisUpdate. A
// CRL with no NextUpdate never falls due.
func CRLNeedsResign(crl *x509.RevocationList, now time.Time) bool {
	if crl.NextUpdate.IsZero() {
		return false
	}
	return inFinalFraction(crl.ThisUpdate, crl.NextUpdate, now)
}

// Lifetime is a certificate's own lifetime, NotAfter - NotBefore: what a
// renewal of it is issued for, so a renewal never invents a validity.
func Lifetime(cert *x509.Certificate) time.Duration {
	return cert.NotAfter.Sub(cert.NotBefore)
}

// RenewalValidity is the validity to pass IssueLeaf so the renewal of cert
// has cert's own Lifetime: IssueLeaf back-dates NotBefore by its clock-skew
// allowance and adds validity to now, so passing Lifetime itself would grow
// every renewal by that allowance.
func RenewalValidity(cert *x509.Certificate) time.Duration {
	return Lifetime(cert) - clockSkew
}

// CRLLifetime is a CRL's own lifetime, NextUpdate - ThisUpdate (0 with no
// NextUpdate): what its re-signing is issued for.
func CRLLifetime(crl *x509.RevocationList) time.Duration {
	if crl.NextUpdate.IsZero() {
		return 0
	}
	return crl.NextUpdate.Sub(crl.ThisUpdate)
}

// ReissueCRL re-signs current under root: the SAME entries (serials and
// revocation times), Number one above current's, ThisUpdate thisUpdate and
// NextUpdate next. It is the CRL a master publishes when the published one
// nears its NextUpdate with nothing new to revoke; the fleet repository's
// crl-newer rule (Number strictly greater, revoked_set superset) holds by
// construction. current must be signed by root. It writes nothing.
func ReissueCRL(root Root, current *x509.RevocationList, thisUpdate, next time.Time) ([]byte, *big.Int, error) {
	if current == nil || current.Number == nil {
		return nil, nil, errors.New("pki: reissue needs the current CRL and its Number")
	}
	if _, err := CheckCRL(current, root.Cert, nil); err != nil {
		return nil, nil, fmt.Errorf("pki: the current CRL: %w", err)
	}
	entries := make([]Revoked, 0, len(current.RevokedCertificateEntries))
	for _, e := range current.RevokedCertificateEntries {
		entries = append(entries, Revoked{Serial: e.SerialNumber, At: e.RevocationTime})
	}
	number := new(big.Int).Add(current.Number, big.NewInt(1))
	out, err := SignCRL(root, entries, number, thisUpdate, next)
	if err != nil {
		return nil, nil, err
	}
	return out, number, nil
}

// RecordCRL replaces the fleet root directory's own copy of the CRL
// (<dir>/crl.pem, what GET /fleet/root hands the next master) with raw,
// atomically. The fleet repository's main is the authority; this is a copy.
func RecordCRL(dir string, raw []byte) error {
	return writeFileAtomic(filepath.Join(dir, CRLFile), raw, 0o644)
}

func inFinalFraction(start, end, now time.Time) bool {
	life := end.Sub(start)
	if life <= 0 {
		return true
	}
	return end.Sub(now) < life/renewWindowDivisor
}
