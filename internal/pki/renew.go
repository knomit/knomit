package pki

import (
	"crypto/x509"
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

func inFinalFraction(start, end, now time.Time) bool {
	life := end.Sub(start)
	if life <= 0 {
		return true
	}
	return end.Sub(now) < life/renewWindowDivisor
}
