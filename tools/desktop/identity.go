//go:build desktop

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki"
)

// The Settings window's "Fleet identity" section (knomit#256): it shows this
// instance's key, enrolment and mTLS listener, and hands out the public key.
// It installs nothing: a desktop joins a fleet from the main window (Fleet →
// New fleet → Join a fleet), which calls POST /api/v1/fleet/join.
//
// WHY BINDINGS, NOT HTTP: every route on the app handler is also served on
// the mTLS listener and by `knomit serve`, so a route reading this state
// would be reachable by enrolled remote peers. Wails bindings are reachable only from
// the app's own webviews — which is still TOO WIDE on its own: the main
// window (the shared web/ UI, same wails:// origin) can call every binding
// too. So the methods that hand out key material or change listener config also
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
// named window, which is every window (wails v3.0.0,
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
// any other free text from a bundle: fingerprints, the email SAN (shown only
// when displayEmail admits its characters), and values derived from numbers
// and dates. That is what makes displaying it safe.
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
	Reason        string `json:"reason"` // the server's listener reason: no_certificate | addr_in_use | tls_failed | ...; "" when listening or off
	ConfiguredNow string `json:"configuredNow"`
}

// fleetPaths resolves the pki dir and the key exactly as the CLI and the
// running server do: config.Load (defaults, knomit.toml, env), then
// config.ResolveKeyPath and [tls].dir.
func fleetPaths() (cfg config.Config, dir, keyPath string, err error) {
	cfg, err = config.Load()
	if err != nil {
		return config.Config{}, "", "", fmt.Errorf("load config: %w", err)
	}
	return cfg, cfg.TLS.Dir, config.ResolveKeyPath(cfg), nil
}

// GetIdentity reports the enrolment state and the TLS listener's. It is
// read-only and open to any window: nothing in it is secret.
//
// The TLS listener's state is the running server's own record in server.json
// (`listeners.tls`); Booted is false while no server has written one. A
// listener the server reports as "off" is reported with no reason, as before.
func (n *NativeService) GetIdentity() (FleetIdentity, error) {
	cfg, dir, keyPath, err := fleetPaths()
	if err != nil {
		return FleetIdentity{}, err
	}
	out := FleetIdentity{Dir: dir, TLS: FleetTLS{ConfiguredNow: cfg.TLS.Addr}}
	if info, ok := n.serverLock(); ok {
		st := info.Listeners["tls"]
		out.TLS.Booted = true
		out.TLS.Configured, out.TLS.Listening, out.TLS.Reason = st.Configured, st.Listening, st.Reason
		if st.Reason == "off" {
			out.TLS.Reason = ""
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
		out.Principal = id.PrincipalKind() + ":" + id.Fingerprint + "@cert"
		out.SAN = displayEmail(id.Email)
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

// PublicKeyLine returns the line to hand the fleet's master for its enroll
// (POST /api/v1/fleet/enroll, public_key). Gated to the Settings window, like everything
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
	return pki.PublicKeyLine(keyPath)
}

// displayEmail returns the certificate's email SAN for display when it holds
// only address characters ([A-Za-z0-9._+@-]), and "" otherwise: the root
// signed it, but the section shows no free text from a bundle.
func displayEmail(email string) string {
	for _, c := range email {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.ContainsRune("._+@-", c):
		default:
			return ""
		}
	}
	return email
}
