package pki

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Install orchestration for the instance side of enrollment: what
// `knomit identity install` and `identity show` do, and what the desktop's
// Settings window does over its Wails bindings (knomit#256). It lives here,
// not in cmd or app, because both front-ends need it and it needs no server:
// ONE implementation of the checks, so the two cannot drift.
//
// The functions take the pki directory and the key path, never a config: pki
// imports no knomit package, and resolving [tls].dir and the key is the
// caller's job (app.ResolveKeyPath, cfg.TLS.Dir).

// The classes of install refusal. Each error InstallBundle returns wraps at
// most one of these (or a VerifyInstanceChain sentinel, for a chain that does
// not verify), so a UI can say WHAT is wrong without showing the bundle. The
// CLI prints Error(), whose text is unchanged from before the classes existed.
var (
	ErrMalformedBundle = errors.New("pki: malformed bundle")
	ErrKeyMismatch     = errors.New("pki: bundle certificate is for another key")
	ErrRootDiffers     = errors.New("pki: bundle is from a different fleet root")
	ErrCRLRollback     = errors.New("pki: bundle CRL is older than the one held")
)

// classified carries a class sentinel beside the error it classifies,
// keeping msg as the whole text.
type classified struct {
	class error
	err   error // may be nil
	msg   string
}

func (e *classified) Error() string { return e.msg }
func (e *classified) Unwrap() []error {
	if e.err == nil {
		return []error{e.class}
	}
	return []error{e.class, e.err}
}

func malformed(err error) error {
	return &classified{class: ErrMalformedBundle, err: err, msg: err.Error()}
}

// RootInfo names a fleet root: its CommonName (a label two roots can share)
// and its fingerprint (RootID, the one that identifies it).
type RootInfo struct {
	CommonName  string
	Fingerprint string
}

func rootInfo(c *x509.Certificate) RootInfo {
	fp, _ := RootID(c) // a non-Ed25519 root has none; it fails verification anyway
	return RootInfo{CommonName: c.Subject.CommonName, Fingerprint: fp}
}

// RootDiffersError is the refusal to move this instance to another fleet
// without being asked to. It names both roots so a confirmation can show
// what is being replaced by what.
type RootDiffersError struct {
	Installed RootInfo
	Bundle    RootInfo
}

func (e *RootDiffersError) Error() string {
	return fmt.Sprintf("this instance is enrolled under root %q and the bundle is from a different root %q; pass --replace-root to move it to the other fleet",
		e.Installed.CommonName, e.Bundle.CommonName)
}

func (e *RootDiffersError) Is(target error) bool { return target == ErrRootDiffers }

// Bundle is an enrollment bundle split into its three parts.
type Bundle struct {
	Leaf    *x509.Certificate
	Root    *x509.Certificate
	RootPEM []byte
	CRLPEM  []byte
	CRL     *x509.RevocationList
}

// SplitBundle returns the instance certificate, the root certificate and the
// CRL from a bundle, refusing anything else in it.
func SplitBundle(raw []byte) (Bundle, error) {
	var b Bundle
	for rest := raw; ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		switch blk.Type {
		case "CERTIFICATE":
			c, perr := x509.ParseCertificate(blk.Bytes)
			if perr != nil {
				return Bundle{}, malformed(perr)
			}
			if c.IsCA {
				if b.Root != nil {
					return Bundle{}, malformed(errors.New("bundle holds two root certificates"))
				}
				b.Root, b.RootPEM = c, pem.EncodeToMemory(blk)
			} else {
				if b.Leaf != nil {
					return Bundle{}, malformed(errors.New("bundle holds two instance certificates"))
				}
				b.Leaf = c
			}
		case "X509 CRL":
			if b.CRL != nil {
				return Bundle{}, malformed(errors.New("bundle holds two CRLs"))
			}
			b.CRLPEM = pem.EncodeToMemory(blk)
			crl, err := x509.ParseRevocationList(blk.Bytes)
			if err != nil {
				return Bundle{}, malformed(fmt.Errorf("%w: %v", ErrCRLInvalid, err))
			}
			b.CRL = crl
		default:
			return Bundle{}, malformed(fmt.Errorf("bundle holds an unexpected %q block", blk.Type))
		}
	}
	if b.Leaf == nil || b.Root == nil || b.CRL == nil {
		return Bundle{}, malformed(errors.New("bundle must hold an instance certificate, a root certificate and a CRL"))
	}
	return b, nil
}

// InstallBundle is `identity install`: it installs raw into dir for the key
// at keyPath. Every check runs BEFORE anything is written, so a refusal
// leaves dir exactly as it was.
func InstallBundle(dir, keyPath string, raw []byte, replaceRoot bool) (Identity, error) {
	b, err := SplitBundle(raw)
	if err != nil {
		return Identity{}, err
	}
	leaf, root, crl := b.Leaf, b.Root, b.CRL
	_, pub, err := LoadSigner(keyPath)
	if err != nil {
		return Identity{}, fmt.Errorf("instance key: %w", err)
	}
	if !pub.Equal(leaf.PublicKey) {
		return Identity{}, &classified{class: ErrKeyMismatch, msg: fmt.Sprintf("the bundle's certificate is for key %s, not this instance's %s (%s)",
			Short(fingerprintOf(leaf)), Short(Fingerprint(pub)), keyPath)}
	}
	id, err := VerifyInstanceChain(leaf, nil, root, crl, time.Now(), UsageClient)
	if err != nil {
		return Identity{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Identity{}, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return Identity{}, err
	}
	// A different root is a different fleet: refuse unless asked. Nothing is
	// reset when it is asked: the CRL watermark is kept PER ROOT
	// (AcceptedNumber), so the new fleet's CRL #1 is judged against the
	// new root's entry, while an old bundle of a root seen before — after a
	// detour A -> B -> A — is still judged against that root's own entry and
	// refused if older.
	if cur, err := LoadRootCert(filepath.Join(dir, RootCertFile)); err == nil && !cur.Equal(root) && !replaceRoot {
		return Identity{}, &RootDiffersError{Installed: rootInfo(cur), Bundle: rootInfo(root)}
	}
	last, err := AcceptedNumber(dir, root) // the BUNDLE's root, not the installed one
	if err != nil {
		return Identity{}, err
	}
	if _, err := CheckCRL(crl, root, last); err != nil {
		e := &classified{err: err, msg: fmt.Sprintf("the bundle's CRL is older than the one this instance already holds: %v", err)}
		if last != nil && crl.Number != nil && crl.Number.Cmp(last) < 0 {
			e.class = ErrCRLRollback
		} else {
			e.class = ErrCRLInvalid
		}
		return Identity{}, e
	}
	for _, f := range []struct {
		name string
		data []byte
	}{
		// The server's reloader adopts the three only as a set that verifies
		// together, so the rename order cannot expose a torn state.
		{RootCertFile, b.RootPEM},
		{CRLFile, b.CRLPEM},
		{InstanceCertFile, certPEM(leaf.Raw)},
	} {
		if err := writeFileAtomic(filepath.Join(dir, f.name), f.data, 0o600); err != nil {
			return Identity{}, err
		}
	}
	if err := RecordAcceptedNumber(dir, root, crl.Number); err != nil {
		return Identity{}, err
	}
	return id, nil
}

func fingerprintOf(c *x509.Certificate) string {
	if pub, ok := c.PublicKey.(ed25519.PublicKey); ok && len(pub) == ed25519.PublicKeySize {
		return Fingerprint(pub)
	}
	return "????????"
}

// PrincipalKind is the principal kind a role authenticates as: "operator" or
// "instance".
func PrincipalKind(r Role) string {
	if r == RoleOperator {
		return "operator"
	}
	return "instance"
}

// EnrollmentStatus is `identity show`'s data: the instance key, and whatever
// is installed in the pki dir. A part that is absent or unreadable is nil;
// IdentityErr is set when the certificate parses but its identity does not.
type EnrollmentStatus struct {
	KeyPath     string
	Fingerprint string
	Enrolled    bool      // instance.crt is present
	Cert        *Identity // nil when not enrolled, or instance.crt does not parse
	IdentityErr error
	Root        *RootInfo
	CRL         *CRLStatus
}

// CRLStatus is the installed CRL's number and when the next one is due.
type CRLStatus struct {
	Number     *big.Int
	NextUpdate time.Time
}

// Status reads the enrollment state of the instance whose key is at keyPath,
// with its pki files in dir. It fails only when the key cannot be loaded.
func Status(dir, keyPath string) (EnrollmentStatus, error) {
	_, pub, err := LoadSigner(keyPath)
	if err != nil {
		return EnrollmentStatus{}, fmt.Errorf("instance key: %w", err)
	}
	st := EnrollmentStatus{KeyPath: keyPath, Fingerprint: Fingerprint(pub), Enrolled: HasInstanceCert(dir)}
	if st.Enrolled {
		if raw, err := os.ReadFile(filepath.Join(dir, InstanceCertFile)); err == nil {
			if blk, _ := pem.Decode(raw); blk != nil {
				if c, err := x509.ParseCertificate(blk.Bytes); err == nil {
					if id, ierr := IdentityOf(c); ierr != nil {
						st.IdentityErr = ierr
					} else {
						st.Cert = &id
					}
				}
			}
		}
	}
	if c, err := LoadRootCert(filepath.Join(dir, RootCertFile)); err == nil {
		ri := rootInfo(c)
		st.Root = &ri
	}
	if crl, err := LoadCRL(filepath.Join(dir, CRLFile)); err == nil {
		st.CRL = &CRLStatus{Number: crl.Number, NextUpdate: crl.NextUpdate}
	}
	return st, nil
}
