//go:build desktop

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/wailsapp/wails/v3/pkg/application"

	knomitapp "knomit/internal/app"
	"knomit/internal/config"
	"knomit/internal/pki"
)

// The Settings window's "Fleet identity" section (knomit#256): enrolment of
// a desktop that has no `knomit` CLI. The install checks are pki's
// (pki.InstallBundle), the same function `knomit identity install` calls;
// this file only resolves the paths, gates the caller and shapes the answer.
//
// WHY BINDINGS, NOT HTTP: every route on the app handler is also served on
// the mTLS listener and by `knomit serve`, so an install route would be
// reachable by enrolled remote peers. Wails bindings are reachable only from
// the app's own webviews — which is still TOO WIDE on its own: the main
// window (the shared web/ UI, same wails:// origin) can call every binding
// too. So the methods that change trust material or listener config also
// check the CALLING WINDOW (callerIsSettings).

// settingsWindowName is the Name settingsWindowOptions gives the Settings
// window, and what callerIsSettings checks. Wails names every unnamed window
// "window-<id>", so no other window of this app carries it.
const settingsWindowName = "knomit-settings"

var errNotSettingsWindow = errors.New("this action is available only from the Settings window")

// callerIsSettings reports whether a binding call came from the Settings
// window. Wails puts the calling window in the call's context
// (application.WindowKey), resolved from the window-name header it sets on
// the webview's own request — not from anything the page sends — for every
// named window, which is every window (wails v3.0.0-beta.3,
// pkg/application/application.go webViewAssetRequest.Header,
// webview_window.go default "window-<id>" names, messageprocessor.go
// getTargetWindow). A call with no window is refused.
func callerIsSettings(ctx context.Context) bool {
	w, ok := ctx.Value(application.WindowKey).(interface{ Name() string })
	return ok && w != nil && w.Name() == settingsWindowName
}

// The states GetIdentity reports.
const (
	identityReady = "ready"
	identityNoKey = "no_key" // first launch: Settings opened before boot created the key
)

// FleetIdentity is what the section renders. It carries no CommonName, or
// any other free text from a bundle: fingerprints, the SAN (whose host
// pki.IdentityOf has already validated as hostname characters), and values
// derived from numbers and dates. That is what makes displaying it safe.
type FleetIdentity struct {
	State          string `json:"state"`
	Dir            string `json:"dir"` // [tls].dir this call read; a Finder-launched app sees no shell env
	KeyFingerprint string `json:"keyFingerprint"`
	Enrolled       bool   `json:"enrolled"`
	// Certificate is "none", "ok", or "unreadable" (instance.crt is present
	// but its identity does not parse).
	Certificate     string   `json:"certificate"`
	Principal       string   `json:"principal"`
	SAN             string   `json:"san"`
	Serial          string   `json:"serial"`
	NotAfter        string   `json:"notAfter"` // RFC 3339
	RootFingerprint string   `json:"rootFingerprint"`
	CRLNumber       string   `json:"crlNumber"`
	CRLNextUpdate   string   `json:"crlNextUpdate"` // RFC 3339
	CRLStale        bool     `json:"crlStale"`      // past NextUpdate: still enforced, but due
	TLS             FleetTLS `json:"tls"`
}

// FleetTLS is the mTLS listener as this process booted it, plus the
// [tls].addr in the config now, so the UI can tell "applies after restart".
type FleetTLS struct {
	Booted        bool   `json:"booted"` // false while the server is still starting
	Configured    string `json:"configured"`
	Listening     string `json:"listening"`
	Reason        string `json:"reason"` // tlsNoCertificate | tlsAddrInUse | ""
	ConfiguredNow string `json:"configuredNow"`
}

// The classes of install refusal, as the UI receives them. A class is the
// ONLY thing the UI learns about a bundle-derived refusal: Wails ships an
// error's text verbatim and logs it, so the text never carries the verdict.
const (
	classMalformed         = "malformed"
	classKeyMismatch       = "key_mismatch"
	classChain             = "chain"
	classCRLRollback       = "crl_rollback"
	classCRLInvalid        = "crl_invalid"
	classRootDiffers       = "root_differs"
	classConfirmationStale = "confirmation_stale"
	classRootUnconfirmed   = "root_unconfirmed" // a first install, shown for confirmation
	classError             = "error"            // I/O and the like: Message says what
)

// InstallResult is InstallBundle's answer. A refusal is a result with a
// Class, not an error; the two fingerprints (the installed one "" when there
// is none) and the principal are set for root_unconfirmed, root_differs and
// confirmation_stale, and the fingerprints are what a confirmation must send
// back.
type InstallResult struct {
	Installed                bool           `json:"installed"`
	Class                    string         `json:"class"`
	Message                  string         `json:"message"` // classError only; never bundle text
	InstalledRootFingerprint string         `json:"installedRootFingerprint"`
	BundleRootFingerprint    string         `json:"bundleRootFingerprint"`
	BundlePrincipal          string         `json:"bundlePrincipal"` // <kind>:<fingerprint>@cert the bundle's certificate names
	Identity                 *FleetIdentity `json:"identity"`
}

// fleetPaths resolves the pki dir and the key exactly as the CLI and the
// running server do: config.Load (defaults, knomit.toml, env), then
// app.ResolveKeyPath and [tls].dir.
func fleetPaths() (cfg config.Config, dir, keyPath string, err error) {
	cfg, err = config.Load()
	if err != nil {
		return config.Config{}, "", "", fmt.Errorf("load config: %w", err)
	}
	return cfg, cfg.TLS.Dir, knomitapp.ResolveKeyPath(cfg), nil
}

// GetIdentity reports the enrolment state and the TLS listener's. It is
// read-only and open to any window: nothing in it is secret.
func (n *NativeService) GetIdentity() (FleetIdentity, error) {
	cfg, dir, keyPath, err := fleetPaths()
	if err != nil {
		return FleetIdentity{}, err
	}
	out := FleetIdentity{Dir: dir, TLS: FleetTLS{ConfiguredNow: cfg.TLS.Addr}}
	if n.tls != nil {
		if st, ok := n.tls.get(); ok {
			out.TLS.Booted = true
			out.TLS.Configured, out.TLS.Listening, out.TLS.Reason = st.Configured, st.Listening, st.Reason
		}
	}
	if _, err := os.Stat(keyPath); errors.Is(err, os.ErrNotExist) {
		out.State = identityNoKey
		return out, nil
	}
	st, err := pki.Status(dir, keyPath)
	if err != nil {
		return FleetIdentity{}, err
	}
	out.State = identityReady
	out.KeyFingerprint = st.Fingerprint
	out.Enrolled = st.Enrolled
	switch {
	case !st.Enrolled:
		out.Certificate = "none"
	case st.Cert != nil:
		id := st.Cert
		out.Certificate = "ok"
		out.Principal = pki.PrincipalKind(id.Role) + ":" + id.Fingerprint + "@cert"
		out.SAN = pki.SAN(id.Role, id.Host, id.Fingerprint)
		out.Serial = id.Serial.Text(16)
		out.NotAfter = id.NotAfter.UTC().Format(time.RFC3339)
	default:
		out.Certificate = "unreadable"
	}
	if st.Root != nil {
		out.RootFingerprint = st.Root.Fingerprint
	}
	if st.CRL != nil {
		out.CRLNumber = st.CRL.Number.String()
		if !st.CRL.NextUpdate.IsZero() {
			out.CRLNextUpdate = st.CRL.NextUpdate.UTC().Format(time.RFC3339)
			out.CRLStale = time.Now().After(st.CRL.NextUpdate)
		}
	}
	return out, nil
}

// PublicKeyLine returns the line to hand the fleet operator for `knomit
// identity enroll --pubkey`. Gated to the Settings window, like everything
// that feeds enrolment.
func (n *NativeService) PublicKeyLine(ctx context.Context) (string, error) {
	if !callerIsSettings(ctx) {
		return "", errNotSettingsWindow
	}
	_, _, keyPath, err := fleetPaths()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(keyPath); errors.Is(err, os.ErrNotExist) {
		return "", errors.New("the instance key does not exist yet; it is created once Knomit has started")
	}
	return knomitapp.PublicKeyLine(keyPath)
}

// InstallBundle installs an enrolment bundle, pasted or read from a file by
// the Settings window. raw is never logged, nor echoed back.
//
// confirmFrom and confirmTo are empty for an ordinary install. An install
// that would put a root in place the user has not seen is then refused,
// carrying the bundle root's fingerprint and the principal its certificate
// names, so the user can compare them with what the fleet operator gave them
// out of band (knomit#299):
//
//   - on a FIRST install (no root installed) as root_unconfirmed;
//   - on a move to another fleet as root_differs, which also carries the
//     installed root's fingerprint.
//
// The install is then a SECOND call that sends the shown pair back: ("",
// bundle root) for a first install, (installed root, bundle root) for a
// move. It installs only if the pair still describes the move — the
// installed root and the bundle's root, both re-read under installMu — and
// is refused as confirmation_stale (with the current pair) otherwise. The
// confirmed installed root is then passed to pki.InstallBundle as the root
// it must still find under its cross-process lock, so an install by another
// process (a CLI) after this re-read is refused too, never overwritten. So
// a confirmation is bound to what was shown, not to "install whatever is
// there". A bundle under the root already installed (a renewal) needs no
// confirmation.
func (n *NativeService) InstallBundle(ctx context.Context, raw, confirmFrom, confirmTo string) (InstallResult, error) {
	if !callerIsSettings(ctx) {
		return InstallResult{}, errNotSettingsWindow
	}
	// One install at a time in this process: Wails runs each call on its own
	// goroutine, and a double click must not interleave two. Another process
	// is held off by pki.InstallBundle's own lock.
	n.installMu.Lock()
	defer n.installMu.Unlock()

	_, dir, keyPath, err := fleetPaths()
	if err != nil {
		return InstallResult{}, err
	}
	preview, err := pki.PreviewBundle(keyPath, []byte(raw))
	if err != nil {
		return n.refused(err), nil
	}
	to := preview.Root.Fingerprint
	// An installed root.crt that does not load is refused HERE, before any
	// question: read as "none" it would be shown as a first install that pki
	// then refuses after the user confirmed.
	installed, err := pki.InstalledRoot(dir)
	if err != nil {
		return n.refused(err), nil
	}
	from := installed.Fingerprint
	pending := InstallResult{InstalledRootFingerprint: from, BundleRootFingerprint: to, BundlePrincipal: preview.Principal}
	confirmed := confirmFrom != "" || confirmTo != ""
	switch {
	case confirmed && (from != confirmFrom || to != confirmTo):
		return n.unconfirmed(pending, classConfirmationStale), nil
	case confirmed, from == to:
	case from == "":
		return n.unconfirmed(pending, classRootUnconfirmed), nil
	default:
		return n.unconfirmed(pending, classRootDiffers), nil
	}
	id, err := pki.InstallBundle(dir, keyPath, []byte(raw), pki.InstallOptions{ExpectInstalledRoot: from})
	if errors.Is(err, pki.ErrRootDiffers) {
		// The installed root changed after the re-read above. After a
		// confirmation, whatever the user confirmed was not this: stale. An
		// unconfirmed call (a renewal) asked nobody, so it is asked afresh
		// about the root now installed.
		var rd *pki.RootDiffersError
		if errors.As(err, &rd) {
			pending.InstalledRootFingerprint = rd.Installed.Fingerprint
		}
		switch {
		case confirmed:
			return n.unconfirmed(pending, classConfirmationStale), nil
		case pending.InstalledRootFingerprint == "":
			return n.unconfirmed(pending, classRootUnconfirmed), nil
		default:
			return n.unconfirmed(pending, classRootDiffers), nil
		}
	}
	if err != nil {
		return n.refused(err), nil
	}
	rootID := ""
	if cur, err := pki.LoadRootCert(filepath.Join(dir, pki.RootCertFile)); err == nil {
		rootID, _ = pki.RootID(cur)
	}
	log.Info().Str("principal", pki.PrincipalKind(id.Role)+":"+id.Fingerprint+"@cert").Str("root", rootID).
		Bool("replaced_root", from != "" && from != to).Str("dir", dir).Msg("fleet identity installed")
	res := InstallResult{Installed: true}
	if fi, err := n.GetIdentity(); err == nil {
		res.Identity = &fi
	}
	return res, nil
}

// unconfirmed is a refusal that asks the user to confirm pending's roots.
func (n *NativeService) unconfirmed(pending InstallResult, class string) InstallResult {
	log.Warn().Str("class", class).Msg("fleet identity install refused")
	pending.Class = class
	return pending
}

// refused maps a pki refusal to its class, logging the class and never the
// bundle.
func (n *NativeService) refused(err error) InstallResult {
	res := InstallResult{Class: classOf(err)}
	var rd *pki.RootDiffersError
	if errors.As(err, &rd) {
		res.InstalledRootFingerprint, res.BundleRootFingerprint = rd.Installed.Fingerprint, rd.Bundle.Fingerprint
	}
	if res.Class == classError {
		res.Message = err.Error()
		log.Warn().Err(err).Msg("fleet identity install failed")
	} else {
		log.Warn().Str("class", res.Class).Msg("fleet identity install refused")
	}
	return res
}

// classOf is the one mapping from pki's error classes to the UI's. Malformed
// is checked first: a CRL that does not parse wraps ErrCRLInvalid as well.
func classOf(err error) string {
	switch {
	case errors.Is(err, pki.ErrMalformedBundle):
		return classMalformed
	case errors.Is(err, pki.ErrKeyMismatch):
		return classKeyMismatch
	case errors.Is(err, pki.ErrRootDiffers):
		return classRootDiffers
	case errors.Is(err, pki.ErrCRLRollback):
		return classCRLRollback
	case errors.Is(err, pki.ErrCRLInvalid):
		return classCRLInvalid
	case errors.Is(err, pki.ErrUntrustedRoot), errors.Is(err, pki.ErrExpired), errors.Is(err, pki.ErrRevoked),
		errors.Is(err, pki.ErrNotEd25519), errors.Is(err, pki.ErrSANMissing), errors.Is(err, pki.ErrSANMismatch),
		errors.Is(err, pki.ErrRoleUnknown):
		return classChain
	}
	return classError
}
