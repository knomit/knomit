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
	"strings"
	"time"
)

// Install orchestration for the instance side of enrollment: what
// a fleet join and `identity show` do, and what the desktop's
// Settings window does over its Wails bindings (knomit#256). It lives here,
// not in cmd or app, because both front-ends need it and it needs no server:
// ONE implementation of the checks, so the two cannot drift.
//
// The functions take the pki directory and the key path, never a config: pki
// imports no knomit package, and resolving [tls].dir and the key is the
// caller's job (config.ResolveKeyPath, cfg.TLS.Dir).

// The classes of install refusal. Each error InstallBundle returns wraps at
// most one of these (or a VerifyInstanceChain sentinel, for a chain that does
// not verify), so a UI can say WHAT is wrong without showing the bundle. The
// CLI prints Error(), whose text is unchanged from before the classes existed.
var (
	ErrMalformedBundle = errors.New("pki: malformed bundle")
	ErrKeyMismatch     = errors.New("pki: bundle certificate is for another key")
	ErrRootDiffers     = errors.New("pki: bundle is from a different fleet root")
	ErrCRLRollback     = errors.New("pki: bundle CRL is older than the one held")
	// ErrAlreadyInstalled: InstallBundle found this bundle's own root
	// installed. A bundle installs only where nothing is (a first install);
	// a renewed leaf installs alone (InstallLeaf).
	ErrAlreadyInstalled = errors.New("pki: fleet trust material is already installed")
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

// RootDiffersError is the refusal to install the material of another fleet
// root over the installed one (InstallBundle), or a CRL of another root
// (InstallCRL). It names both roots.
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

// Bundle is an enrollment bundle split into its parts.
type Bundle struct {
	Leaf    *x509.Certificate
	Root    *x509.Certificate
	RootPEM []byte
	CRLPEM  []byte
	CRL     *x509.RevocationList
	// FleetURL is the fleet repository URL members use (the hub's https URL
	// at the external address the master chose, or the forge's URL), from
	// the bundle document's `url:` line; "" for a bare PEM bundle (the
	// create's own install). It is not trust material: the join classifies
	// it (ResolveOrigin) and checks the cloned repository's root against the
	// bundle's.
	FleetURL string
}

// BundleHeader is the first line of an enroll's bundle document. The
// document is the header, `url:`, `agent:`, `root:` (fingerprint) and
// `serial:` lines, then the PEM blocks (leaf, root, CRL). The agent, root
// and serial lines are for a reader; SplitBundle checks each against the
// PEM blocks, so they can never disagree with what is installed.
const BundleHeader = "# knomit fleet bundle v1"

// FleetBundle is the bundle document for raw (bare PEM: leaf, root, CRL)
// naming url as the fleet repository URL.
func FleetBundle(raw []byte, url string) ([]byte, error) {
	b, err := SplitBundle(raw)
	if err != nil {
		return nil, err
	}
	if b.FleetURL != "" {
		return nil, malformed(errors.New("bundle already names a fleet URL"))
	}
	if url == "" || strings.ContainsAny(url, "\r\n") {
		return nil, malformed(fmt.Errorf("fleet URL %q is not one line", url))
	}
	agent, root, serial, err := bundleFacts(b)
	if err != nil {
		return nil, err
	}
	head := fmt.Sprintf("%s\nurl: %s\nagent: %s\nroot: %s\nserial: %s\n", BundleHeader, url, agent, root, serial)
	return append([]byte(head), raw...), nil
}

// bundleFacts are the header's agent, root fingerprint and serial, read from
// the PEM blocks.
func bundleFacts(b Bundle) (agent, root, serial string, err error) {
	if len(b.Leaf.EmailAddresses) == 1 {
		agent = AgentIDOfEmail(b.Leaf.EmailAddresses[0])
	}
	root, err = RootID(b.Root)
	if err != nil {
		return "", "", "", malformed(err)
	}
	return agent, root, b.Leaf.SerialNumber.Text(16), nil
}

// splitHeader separates a bundle document's header lines from its PEM. A
// bare PEM bundle has none.
func splitHeader(raw []byte) (map[string]string, []byte, error) {
	text := string(raw)
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if !strings.HasPrefix(trimmed, "#") {
		return nil, raw, nil
	}
	i := strings.Index(trimmed, "-----BEGIN ")
	if i < 0 {
		i = len(trimmed)
	}
	lines := strings.Split(strings.TrimSpace(trimmed[:i]), "\n")
	if strings.TrimSpace(lines[0]) != BundleHeader {
		return nil, nil, malformed(fmt.Errorf("not a %q document: %q", BundleHeader, strings.TrimSpace(lines[0])))
	}
	fields := map[string]string{}
	for _, l := range lines[1:] {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || v == "" {
			return nil, nil, malformed(fmt.Errorf("bundle header line %q is not key: value", l))
		}
		switch k {
		case "url", "agent", "root", "serial":
		default:
			return nil, nil, malformed(fmt.Errorf("bundle header has an unexpected %q line", k))
		}
		if _, dup := fields[k]; dup {
			return nil, nil, malformed(fmt.Errorf("bundle header has two %q lines", k))
		}
		fields[k] = v
	}
	for _, k := range []string{"url", "agent", "root", "serial"} {
		if fields[k] == "" {
			return nil, nil, malformed(fmt.Errorf("bundle header has no %q line", k))
		}
	}
	return fields, []byte(trimmed[i:]), nil
}

// SplitBundle returns the instance certificate, the root certificate and the
// CRL (exactly one of each) and, for a bundle document, its fleet URL,
// refusing anything else in it and a header whose agent, root or serial
// does not match the PEM blocks.
func SplitBundle(raw []byte) (Bundle, error) {
	header, raw, err := splitHeader(raw)
	if err != nil {
		return Bundle{}, err
	}
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
	if header != nil {
		agent, root, serial, err := bundleFacts(b)
		if err != nil {
			return Bundle{}, err
		}
		for k, want := range map[string]string{"agent": agent, "root": root, "serial": serial} {
			if !strings.EqualFold(header[k], want) {
				return Bundle{}, malformed(fmt.Errorf("bundle header says %s %q, its certificates say %q", k, header[k], want))
			}
		}
		b.FleetURL = header["url"]
	}
	return b, nil
}

// InstallBundle installs raw (an enrollment bundle) into dir for the key
// at keyPath, on an instance with NO fleet trust material installed: the
// first install of a fleet create or join. Every check runs BEFORE anything
// is written — the directory itself included — so a refusal leaves dir
// exactly as it was. An I/O failure between the three renames can leave a
// mixed set; the reloader adopts only a set that verifies together.
//
// It never replaces installed material: an installed root (any root, this
// bundle's own included) is refused, a different one with a
// RootDiffersError naming both, the same one with ErrAlreadyInstalled. Moving
// to another fleet is a leave (Uninstall) and then a join; the CRL alone
// moves with InstallCRL, a renewed leaf with InstallLeaf.
//
// The installed-root check and the writes run under a cross-process lock
// (LockPath: "<dir>.lock", beside dir, not in it), so two installs — or an
// install and an Uninstall — cannot interleave, and the "nothing installed"
// that was checked is what the writes land on.
func InstallBundle(dir, keyPath string, raw []byte) (Identity, error) {
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
	// Nothing may be installed. Only a MISSING root.crt means "no root
	// installed". One that exists but does not load (or has no RootID) fails
	// closed, always: reading it as "none" would install over material
	// nobody has seen. Removing the file is the way out.
	//
	// Nothing is reset by an Uninstall: the CRL watermark is kept PER ROOT
	// (AcceptedNumber), so another fleet's CRL #1 is judged against its own
	// root's entry, while an old bundle of a root seen before — after a
	// detour A -> B -> A through two leaves and joins — is still judged
	// against that root's own entry and refused if older.
	installed, err := InstalledRoot(dir)
	if err != nil {
		return Identity{}, err
	}
	if installed.Fingerprint != "" {
		if rootInfo(root).Fingerprint == installed.Fingerprint {
			return Identity{}, &classified{class: ErrAlreadyInstalled,
				msg: fmt.Sprintf("pki: this instance already has the trust material of fleet root %s installed: a bundle installs only where none is (leave the fleet first)", installed.Fingerprint)}
		}
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

// Uninstall removes the fleet trust material from dir: instance.crt, crl.pem
// and root.crt, under the same cross-process lock as InstallBundle, so it
// cannot interleave with an install. A file already absent is not an error.
// crl.number (the per-root CRL watermark) is kept: it is filed per root, so
// it changes nothing for another fleet, and it still refuses an older CRL if
// this root is ever installed again. The instance key is never touched.
//
// The fleet transport's client resolves dir at each session, so its next session
// finds no certificate (ErrNotEnrolled). A running mTLS listener adopts the
// uninstalled state (all three files absent) on its next handshake or tick:
// it refuses every handshake and cuts every established connection, without
// a restart.
func Uninstall(dir string) error {
	unlock, err := lockInstall(dir)
	if err != nil {
		return err
	}
	defer unlock()
	var errs []error
	for _, f := range []string{InstanceCertFile, CRLFile, RootCertFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// InstallCRL installs a CRL on its own (fleet design D13), for a member that
// finds a newer one in the fleet repo: no leaf, no root change. Under the
// same cross-process lock as InstallBundle it checks that rootCert is the
// root INSTALLED in dir (a RootDiffersError otherwise: a CRL never moves an
// instance to another fleet), that raw is one CRL signed by that root, and
// that its Number is ABOVE the root's watermark (AcceptedNumber). A Number
// at or below it is refused with ErrCRLRollback — an equal one too: it is
// either the CRL already held or a different list under a reused Number,
// and neither should replace crl.pem. Only then is crl.pem written and the
// watermark raised. It returns the installed Number.
func InstallCRL(dir string, rootCert *x509.Certificate, raw []byte) (*big.Int, error) {
	blk, rest := pem.Decode(raw)
	if blk == nil || blk.Type != "X509 CRL" {
		return nil, fmt.Errorf("%w: no X509 CRL PEM block", ErrCRLInvalid)
	}
	if extra, _ := pem.Decode(rest); extra != nil {
		return nil, fmt.Errorf("%w: more than one PEM block (found %q after the CRL)", ErrCRLInvalid, extra.Type)
	}
	crl, err := x509.ParseRevocationList(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCRLInvalid, err)
	}
	want, err := RootID(rootCert)
	if err != nil {
		return nil, err
	}
	unlock, err := lockInstall(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	installed, err := InstalledRoot(dir)
	if err != nil {
		return nil, err
	}
	if installed.Fingerprint != want {
		return nil, &RootDiffersError{Installed: installed, Bundle: rootInfo(rootCert)}
	}
	last, err := AcceptedNumber(dir, rootCert)
	if err != nil {
		return nil, err
	}
	if _, err := CheckCRL(crl, rootCert, last); err != nil {
		if last != nil && crl.Number != nil && crl.Number.Cmp(last) < 0 {
			return nil, &classified{class: ErrCRLRollback, err: err, msg: err.Error()}
		}
		return nil, err
	}
	if last != nil && crl.Number.Cmp(last) == 0 {
		return nil, &classified{class: ErrCRLRollback,
			msg: fmt.Sprintf("pki: CRL Number %v is not above the %v already accepted for this root", crl.Number, last)}
	}
	if err := writeFileAtomic(filepath.Join(dir, CRLFile), crlPEM(crl.Raw), 0o600); err != nil {
		return nil, err
	}
	if err := RecordAcceptedNumber(dir, rootCert, crl.Number); err != nil {
		return nil, err
	}
	return crl.Number, nil
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
// the user: the zero RootInfo when root.crt is MISSING (nothing installed),
// an error naming the file when it exists but does not load or is not
// Ed25519. A front-end must refuse on that error before asking anything:
// reading an unreadable root as "none" would show a first-install question
// that InstallBundle then refuses (knomit#299 review).
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
	return BundlePreview{Root: rootInfo(b.Root), Principal: id.PrincipalKind() + ":" + id.Fingerprint + "@cert"}, nil
}

func fingerprintOf(c *x509.Certificate) string {
	if pub, ok := c.PublicKey.(ed25519.PublicKey); ok && len(pub) == ed25519.PublicKeySize {
		return Fingerprint(pub)
	}
	return "????????"
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
