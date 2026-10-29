//go:build desktop

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"

	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki"
	"knomit/internal/foundation/pki/pkitest"
	"knomit/internal/foundation/serveraddr"
)

// stubWindow stands in for the Wails window a binding call carries in its
// context (application.WindowKey). What this can test is OUR check against
// a window's name; that Wails sets the name from the webview that made the
// request, not from anything the page sends, is established by reading
// Wails' source and is not reproducible here.
type stubWindow struct{ name string }

func (w stubWindow) Name() string { return w.name }

func callerCtx(name string) context.Context {
	return context.WithValue(context.Background(), application.WindowKey, stubWindow{name})
}

var (
	fromSettings = callerCtx(settingsWindowName)
	fromMain     = callerCtx("window-1") // what Wails names the unnamed main window
)

// fleetHome is a desktop home whose instance key is at <home>/id_ed25519,
// with every environment override that would reach a developer's real key
// or pki dir neutralised.
func fleetHome(t *testing.T) (home, keyPath string) {
	t.Helper()
	home = desktopHome(t)
	for _, k := range []string{"KNOMIT_REMOTE_SSH_KEY", "KNOMIT_TLS_DIR", "KNOMIT_TLS_ADDR"} {
		t.Setenv(k, "")
	}
	src, _ := pkitest.NewKey(t)
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(home, "id_ed25519")
	if err := os.WriteFile(keyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return home, keyPath
}

func fleetService(t *testing.T, home string) *NativeService {
	t.Helper()
	n := newNativeService(filepath.Join(home, "knomit.toml"), filepath.Join(home, "desktop.log"), &stubToggler{})
	return n
}

func bundle(t *testing.T, f *pkitest.Fleet, m pkitest.Member) string {
	t.Helper()
	rootPEM, err := os.ReadFile(filepath.Join(f.Dir, pki.RootCertFile))
	if err != nil {
		t.Fatal(err)
	}
	crlPEM, err := os.ReadFile(filepath.Join(f.Dir, pki.CRLFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(m.CertPEM) + string(rootPEM) + string(crlPEM)
}

func rootFP(t *testing.T, f *pkitest.Fleet) string {
	t.Helper()
	fp, err := pki.RootID(f.Root.Cert)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// R1: trust material, and the listener config that opens a network port,
// change only from the Settings window. The main window shares the Wails
// origin and can call every binding; a page running there — injected script
// included — must be refused, and must change nothing.
func TestFleetIdentity_BindingsRefuseAnyCallerButTheSettingsWindow(t *testing.T) {
	home, _ := fleetHome(t)
	n := fleetService(t, home)
	pkiDir := filepath.Join(home, "pki")
	toml := filepath.Join(home, "knomit.toml")

	for _, c := range []struct {
		name string
		ctx  context.Context
	}{
		{"main window", fromMain},
		{"no window", context.Background()},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := n.PublicKeyLine(c.ctx); !errors.Is(err, errNotSettingsWindow) {
				t.Fatalf("PublicKeyLine: %v", err)
			}
			if _, err := n.SaveSettings(c.ctx, Settings{Port: "20001", LogLevel: "info", LogFormat: "console"}); !errors.Is(err, errNotSettingsWindow) {
				t.Fatalf("SaveSettings: %v", err)
			}
			for _, p := range []string{pkiDir, toml} {
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("a refused call wrote %s (%v)", p, err)
				}
			}
		})
	}

	// Positive control: the same two calls from Settings succeed.
	if line, err := n.PublicKeyLine(fromSettings); err != nil || !strings.HasPrefix(line, "ssh-ed25519 ") {
		t.Fatalf("PublicKeyLine from Settings: %q %v", line, err)
	}
	if _, err := n.SaveSettings(fromSettings, Settings{Port: "20001", LogLevel: "info", LogFormat: "console"}); err != nil {
		t.Fatalf("SaveSettings from Settings: %v", err)
	}
}

// The Settings window is identified by the name its options give it, which
// is what a call's context carries.
func TestFleetIdentity_SettingsWindowCarriesTheCheckedName(t *testing.T) {
	if settingsWindowOptions().Name != settingsWindowName || settingsWindowName == "" {
		t.Fatalf("settings window name %q, check compares %q", settingsWindowOptions().Name, settingsWindowName)
	}
}

func TestFleetIdentity_GetIdentityStates(t *testing.T) {
	home := desktopHome(t)
	for _, k := range []string{"KNOMIT_REMOTE_SSH_KEY", "KNOMIT_TLS_DIR", "KNOMIT_TLS_ADDR"} {
		t.Setenv(k, "")
	}
	n := fleetService(t, home)

	// First launch: Settings can open before boot has created the key.
	id, err := n.GetIdentity()
	if err != nil || id.State != identityNoKey {
		t.Fatalf("before the key exists: %+v %v", id, err)
	}
	if id.TLS.Booted {
		t.Fatal("TLS state reported before the boot published it")
	}

	_, keyPath := fleetHome(t)
	home = filepath.Dir(keyPath)
	n = fleetService(t, home)
	id, err = n.GetIdentity()
	if err != nil || id.State != identityReady || id.Enrolled || id.KeyFingerprint == "" {
		t.Fatalf("not enrolled: %+v %v", id, err)
	}
	f := pkitest.New(t)
	m := f.Enroll(t, "laptop@agents.knomit.io", keyPath)
	if _, err := pki.InstallBundle(filepath.Join(home, "pki"), keyPath, []byte(bundle(t, f, m))); err != nil {
		t.Fatalf("install: %v", err)
	}
	// The running server's own account of its TLS listener, as `knomit serve`
	// writes it into this home's server.json.
	if err := serveraddr.WriteLock(config.LockfilePathFor(home), serveraddr.LockInfo{
		PID: 1, Version: "x", Schema: serveraddr.LockSchemaV2,
		Listeners: map[string]serveraddr.LockListener{"tls": {Configured: "0.0.0.0:19279", Reason: "no_certificate"}},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KNOMIT_TLS_ADDR", "0.0.0.0:19279")
	id, err = n.GetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if !id.Enrolled || id.Principal != "instance:"+m.Fingerprint()+"@cert" || id.SAN != "laptop@agents.knomit.io" ||
		id.Serial != m.Cert.SerialNumber.Text(16) || id.RootFingerprint != rootFP(t, f) || id.CRLNumber != "1" || id.Dir != filepath.Join(home, "pki") {
		t.Fatalf("enrolled: %+v", id)
	}
	if !id.TLS.Booted || id.TLS.Reason != "no_certificate" || id.TLS.Configured != "0.0.0.0:19279" || id.TLS.ConfiguredNow != "0.0.0.0:19279" {
		t.Fatalf("tls: %+v", id.TLS)
	}
}

func TestFleetIdentity_PublicKeyLineIsTheAppLine(t *testing.T) {
	home, keyPath := fleetHome(t)
	n := fleetService(t, home)
	got, err := n.PublicKeyLine(fromSettings)
	if err != nil {
		t.Fatal(err)
	}
	want, err := pki.PublicKeyLine(keyPath)
	if err != nil || got != want {
		t.Fatalf("binding %q, app %q (%v)", got, want, err)
	}
}

// S9: the JSON the UI reads is pinned against the file the UI's own tests
// read (ui/src/fleetWire.json), so a Go field renamed or added — a
// CommonName, say — fails here rather than silently rendering nothing.
func TestFleetIdentity_WireKeysMatchTheUIFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("ui", "src", "fleetWire.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string][]string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{
		"identity": FleetIdentity{},
		"tls":      FleetTLS{},
	} {
		b, _ := json.Marshal(v)
		var m map[string]any
		json.Unmarshal(b, &m)
		got := keysOfAny(m)
		w := slices.Clone(want[name])
		sort.Strings(w)
		if !slices.Equal(got, w) {
			t.Errorf("%s: Go sends %v, the UI fixture expects %v", name, got, w)
		}
	}
	if bytes.Contains(bytes.ToLower(raw), []byte("commonname")) {
		t.Error("a CommonName is on the wire: it is bundle-controlled free text (S5)")
	}
}

// S8: every exported method of NativeService is a Wails binding, callable
// from any window. The set is pinned so that a new one is a decision, and so
// the UI's Call.ByName strings have a Go-side list to match.
func TestFleetIdentity_NativeServiceMethodSetIsPinned(t *testing.T) {
	want := []string{
		"GetIdentity", "GetSettings", "PublicKeyLine",
		"RestartServer", "RevealLogFile", "SaveFileAs", "SaveSettings", "WriteFile",
		// SaveFileAs (Materialize Export) takes only a suggested name and the bytes; the
		// destination is chosen by the user in the native save dialog, so it is not a
		// page-controlled write (see NativeService.SaveFileAs).
	}
	var got []string
	ty := reflect.TypeOf(&NativeService{})
	for i := 0; i < ty.NumMethod(); i++ {
		got = append(got, ty.Method(i).Name)
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Fatalf("NativeService bindings %v, pinned %v — a new exported method is a new IPC entry point any window can call", got, want)
	}
}

func keysOfAny(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
