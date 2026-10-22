package pki

import (
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Standing is what this instance can tell about its OWN certificate from the
// trust material installed in its pki directory alone: no peer is asked.
type Standing string

const (
	// StandingNone: no instance certificate, root or CRL is installed, or one
	// of them does not load.
	StandingNone Standing = ""
	// StandingValid: the installed certificate chains to the installed root,
	// is inside its validity at the time asked, and the installed CRL does
	// not list it.
	StandingValid Standing = "valid"
	// StandingRevoked: the installed CRL lists the installed certificate.
	StandingRevoked Standing = "revoked"
	// StandingExpired: the certificate is outside its validity (expired, or
	// not valid yet) at the time asked.
	StandingExpired Standing = "expired"
	// StandingUntrusted: the certificate does not chain to the installed
	// root, or the CRL is not the root's.
	StandingUntrusted Standing = "untrusted"
)

// InstalledStanding is the Standing of the instance certificate installed in
// dir at now, by VerifyInstanceChain against the installed root and CRL, and
// that certificate (nil with StandingNone).
func InstalledStanding(dir string, now time.Time) (Standing, *x509.Certificate) {
	leaf, err := LoadInstanceCert(dir)
	if err != nil {
		return StandingNone, nil
	}
	root, err := LoadRootCert(filepath.Join(dir, RootCertFile))
	if err != nil {
		return StandingNone, nil
	}
	crl, err := LoadCRL(filepath.Join(dir, CRLFile))
	if err != nil {
		return StandingNone, nil
	}
	switch _, err := VerifyInstanceChain(leaf, nil, root, crl, now, UsageClient); {
	case err == nil:
		return StandingValid, leaf
	case errors.Is(err, ErrRevoked):
		return StandingRevoked, leaf
	case errors.Is(err, ErrExpired):
		return StandingExpired, leaf
	default:
		return StandingUntrusted, leaf
	}
}

// LoadInstanceCert reads the instance certificate installed in dir.
func LoadInstanceCert(dir string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(filepath.Join(dir, InstanceCertFile))
	if err != nil {
		return nil, err
	}
	return parseOneCert(raw)
}
