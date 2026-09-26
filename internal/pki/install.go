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

// Error names the two roots by fingerprint only. A CommonName is free text
// chosen by whoever minted the root, so a front-end that shows one formats
// it itself (the CLI does, %q-quoted); this text is safe to log as is.
func (e *RootDiffersError) Error() string {
	return fmt.Sprintf("pki: bundle is from a different fleet root (installed %s, bundle %s)",
		e.Installed.Fingerprint, e.Bundle.Fingerprint)
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

// InstallOptions is what the caller has SEEN before asking for an install.
type InstallOptions struct {
	// ExpectInstalledRoot is the fingerprint (RootID) of the root the caller
	// read as installed — and showed the user, where there is one — or ""
	// for "no root installed". The install goes ahead only if that is still
	// the installed root when checked under the install lock; otherwise it
	// is refused with a RootDiffersError naming the root found. It is also
	// the ONLY consent pki takes to change fleets: a bundle from another
	// root installs when ExpectInstalledRoot equals the installed root.
	// Deciding that the user agreed to that root — a first install included
	// — is the caller's job (knomit#299).
	ExpectInstalledRoot string
}

// InstallBundle is `identity install`: it installs raw into dir for the key
// at keyPath. Every check runs BEFORE anything is written — the directory
// itself included — so a VERIFICATION failure leaves dir exactly as it was.
// An I/O failure between the three renames can leave a mixed set; the
// reloader adopts only a set that verifies together, and the next install
// repairs it.
//
// The installed-root check and the writes run under a cross-process lock
// (LockPath: "<dir>.lock", beside dir, not in it), so two installs — the
// desktop's and a CLI's — cannot interleave, and the root compared with
// opts.ExpectInstalledRoot is the root the writes replace.
func InstallBundle(dir, keyPath string, raw []byte, opts InstallOptions) (Identity, error) {
	b, id, err := checkBundle(keyPath, raw)
	if err != nil {
		return Identity{}, err
	}
	root, crl := b.Root, b.CRL
	// The checks above read only the bundle and the key; from here on they
	// read dir, so they run under the lock.
	unlock, err := lockInstall(dir)
	if err != nil {
		return Identity{}, err
	}
	defer unlock()
	// The installed root must be the one the caller saw. A different bundle
	// root is then a move to another fleet the caller has vouched for.
	// Nothing is reset on such a move: the CRL watermark is kept PER ROOT
	// (AcceptedNumber), so the new fleet's CRL #1 is judged against the new
	// root's entry, while an old bundle of a root seen before — after a
	// detour A -> B -> A — is still judged against that root's own entry and
	// refused if older.
	//
	// Only a MISSING root.crt means "no root installed". One that exists but
	// does not load (or has no RootID) fails closed, always: reading it as
	// "none" would let a caller that expects none install over it without
	// anyone having seen what it was. Removing the file is the way out.
	installed, err := InstalledRoot(dir)
	if err != nil {
		return Identity{}, err
	}
	if installed.Fingerprint != opts.ExpectInstalledRoot {
		return Identity{}, &RootDiffersError{Installed: installed, Bundle: rootInfo(root)}
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
	// Every check has passed: only now is the directory created (or its mode
	// reset), so a refused bundle leaves <pki> exactly as it was — on a fresh
	// home, absent. AcceptedNumber and LoadRootCert above read a missing dir
	// as "nothing installed".
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Identity{}, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return Identity{}, err
	}
	for _, f := range []struct {
		name string
		data []byte
	}{
		// The server's reloader adopts the three only as a set that verifies
		// together, so the rename order cannot expose a torn state.
		{RootCertFile, b.RootPEM},
		{CRLFile, b.CRLPEM},
		{InstanceCertFile, certPEM(b.Leaf.Raw)},
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

// checkBundle runs the checks that read only the bundle and the key: its
// shape, that its certificate is for this instance's key, and that the
// certificate chains to the bundle's own root under its own CRL.
func checkBundle(keyPath string, raw []byte) (Bundle, Identity, error) {
	b, err := SplitBundle(raw)
	if err != nil {
		return Bundle{}, Identity{}, err
	}
	_, pub, err := LoadSigner(keyPath)
	if err != nil {
		return Bundle{}, Identity{}, fmt.Errorf("instance key: %w", err)
	}
	if !pub.Equal(b.Leaf.PublicKey) {
		return Bundle{}, Identity{}, &classified{class: ErrKeyMismatch, msg: fmt.Sprintf("the bundle's certificate is for key %s, not this instance's %s (%s)",
			Short(fingerprintOf(b.Leaf)), Short(Fingerprint(pub)), keyPath)}
	}
	id, err := VerifyInstanceChain(b.Leaf, nil, b.Root, b.CRL, time.Now(), UsageClient)
	if err != nil {
		return Bundle{}, Identity{}, err
	}
	return b, id, nil
}

// InstalledRoot is the root installed in dir, as a caller reads it to show
// the user and to pass as InstallOptions.ExpectInstalledRoot: the zero
// RootInfo when root.crt is MISSING (nothing installed), an error naming the
// file when it exists but does not load or is not Ed25519. A front-end must
// refuse on that error before asking anything: reading an unreadable root
// as "none" would show a first-install question that InstallBundle then
// refuses (knomit#299 review).
func InstalledRoot(dir string) (RootInfo, error) {
	p := filepath.Join(dir, RootCertFile)
	cur, err := LoadRootCert(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return RootInfo{}, nil
	case err != nil:
		return RootInfo{}, fmt.Errorf("the installed root certificate cannot be read, so the bundle cannot be compared with it (remove %s to install anyway): %w", p, err)
	}
	ri := rootInfo(cur)
	if ri.Fingerprint == "" {
		return RootInfo{}, fmt.Errorf("the installed root certificate has no fleet fingerprint (not Ed25519), so the bundle cannot be compared with it (remove %s to install anyway)", p)
	}
	return ri, nil
}

// BundlePreview is what a front-end shows before a first install or a move
// to another fleet: the bundle's root and the principal its certificate
// names. Only Root.CommonName is free text a display must quote.
type BundlePreview struct {
	Root      RootInfo
	Principal string // <kind>:<fingerprint>@cert
}

// PreviewBundle runs InstallBundle's checks that read only the bundle and
// the key at keyPath — so a bundle that could never install is refused
// before anyone is asked to trust its root — and returns what to show.
// It reads nothing installed; InstallBundle checks that under its lock.
func PreviewBundle(keyPath string, raw []byte) (BundlePreview, error) {
	b, id, err := checkBundle(keyPath, raw)
	if err != nil {
		return BundlePreview{}, err
	}
	return BundlePreview{Root: rootInfo(b.Root), Principal: PrincipalKind(id.Role) + ":" + id.Fingerprint + "@cert"}, nil
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
