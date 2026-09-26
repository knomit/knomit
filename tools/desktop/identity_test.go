//go:build desktop

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	knomitapp "knomit/internal/app"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
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
	n.tls = &tlsStatus{}
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

// tree is every file under dir with its mode and bytes ("" map when absent).
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[rel] = fi.Mode().String() + " " + string(b)
		return nil
	})
	return out
}

// R1: trust material, and the listener config that opens a network port,
// change only from the Settings window. The main window shares the Wails
// origin and can call every binding; a page running there — injected script
// included — must be refused, and must change nothing.
func TestFleetIdentity_BindingsRefuseAnyCallerButTheSettingsWindow(t *testing.T) {
	home, keyPath := fleetHome(t)
	f := pkitest.New(t)
	m := f.Enroll(t, "laptop", pki.RoleInstance, keyPath)
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
			if _, err := n.InstallBundle(c.ctx, bundle(t, f, m), "", ""); !errors.Is(err, errNotSettingsWindow) {
				t.Fatalf("InstallBundle: %v", err)
			}
			if _, err := n.PublicKeyLine(c.ctx); !errors.Is(err, errNotSettingsWindow) {
				t.Fatalf("PublicKeyLine: %v", err)
			}
			if err := n.SaveSettings(c.ctx, Settings{Port: "20001", LogLevel: "info", LogFormat: "console"}); !errors.Is(err, errNotSettingsWindow) {
				t.Fatalf("SaveSettings: %v", err)
			}
			for _, p := range []string{pkiDir, toml} {
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("a refused call wrote %s (%v)", p, err)
				}
			}
		})
	}

	// Positive control: the same three calls from Settings succeed.
	res, err := n.InstallBundle(fromSettings, bundle(t, f, m), "", "")
	if err != nil || !res.Installed {
		t.Fatalf("InstallBundle from Settings: %+v %v", res, err)
	}
	if line, err := n.PublicKeyLine(fromSettings); err != nil || !strings.HasPrefix(line, "ssh-ed25519 ") {
		t.Fatalf("PublicKeyLine from Settings: %q %v", line, err)
	}
	if err := n.SaveSettings(fromSettings, Settings{Port: "20001", LogLevel: "info", LogFormat: "console"}); err != nil {
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

// The binding is a front-end for pki.InstallBundle and nothing else: the
// files it leaves are byte- and mode-identical to the ones pki (and so
// `knomit identity install`, which calls the same function) leaves for the
// same bundle.
func TestFleetIdentity_InstallWritesWhatPKIWrites(t *testing.T) {
	home, keyPath := fleetHome(t)
	f := pkitest.New(t)
	m := f.Enroll(t, "laptop", pki.RoleInstance, keyPath)
	n := fleetService(t, home)

	res, err := n.InstallBundle(fromSettings, bundle(t, f, m), "", "")
	if err != nil || !res.Installed || res.Class != "" {
		t.Fatalf("install: %+v %v", res, err)
	}
	direct := filepath.Join(t.TempDir(), "pki")
	if _, err := pki.InstallBundle(direct, keyPath, []byte(bundle(t, f, m)), false); err != nil {
		t.Fatal(err)
	}
	got, want := tree(t, filepath.Join(home, "pki")), tree(t, direct)
	if len(got) != 4 || !reflect.DeepEqual(got, want) {
		t.Fatalf("binding wrote %v, pki writes %v", keysOf(got), keysOf(want))
	}
	if res.Identity == nil || !res.Identity.Enrolled || res.Identity.KeyFingerprint != m.Fingerprint() {
		t.Fatalf("result identity %+v", res.Identity)
	}
}

// Every refusal arrives as a CLASS in the result — never parsed from error
// text, which Wails ships and logs verbatim — and changes nothing.
func TestFleetIdentity_RefusalsAreClassesAndWriteNothing(t *testing.T) {
	home, keyPath := fleetHome(t)
	f, f1 := pkitest.New(t), pkitest.New(t)
	m := f.Enroll(t, "laptop", pki.RoleInstance, keyPath)
	n := fleetService(t, home)
	if res, err := n.InstallBundle(fromSettings, bundle(t, f, m), "", ""); err != nil || !res.Installed {
		t.Fatalf("first install: %+v %v", res, err)
	}
	// Move this fleet's CRL to #2 and install it, so a #1 bundle is a rollback.
	oldBundle := bundle(t, f, m)
	if _, err := pki.IssueCRL(f.Dir, f.Root, nil, big.NewInt(2), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if res, err := n.InstallBundle(fromSettings, bundle(t, f, m), "", ""); err != nil || !res.Installed {
		t.Fatalf("CRL #2 install: %+v %v", res, err)
	}
	other := f.Enroll(t, "other", pki.RoleInstance)
	foreignLeaf := f1.Enroll(t, "laptop", pki.RoleInstance, keyPath)
	rootPEM, _ := os.ReadFile(filepath.Join(f.Dir, pki.RootCertFile))
	crlPEM, _ := os.ReadFile(filepath.Join(f.Dir, pki.CRLFile))
	untrusted := string(foreignLeaf.CertPEM) + string(rootPEM) + string(crlPEM) // f1's leaf, f's root

	for _, tc := range []struct {
		name, raw, class string
	}{
		{"malformed", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n", classMalformed},
		{"key mismatch", bundle(t, f, other), classKeyMismatch},
		{"chain", untrusted, classChain},
		{"crl rollback", oldBundle, classCRLRollback},
		{"root differs", bundle(t, f1, foreignLeaf), classRootDiffers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tree(t, filepath.Join(home, "pki"))
			res, err := n.InstallBundle(fromSettings, tc.raw, "", "")
			if err != nil {
				t.Fatalf("a refusal came back as an error, not a class: %v", err)
			}
			if res.Installed || res.Class != tc.class {
				t.Fatalf("result %+v, want class %s", res, tc.class)
			}
			if res.Message != "" {
				t.Fatalf("a bundle-derived refusal carries text: %q", res.Message)
			}
			if !reflect.DeepEqual(before, tree(t, filepath.Join(home, "pki"))) {
				t.Fatal("a refusal changed <home>/pki")
			}
		})
	}
}

// R2: replacing the fleet root is bound to the two fingerprints the user
// was shown. The first call reports them; only a call carrying exactly that
// pair replaces, and a pair that no longer describes what is installed and
// what is being installed is refused with the current pair.
func TestFleetIdentity_ReplaceRootNeedsTheFingerprintsThatWereShown(t *testing.T) {
	home, keyPath := fleetHome(t)
	f, f1, f2 := pkitest.New(t), pkitest.New(t), pkitest.New(t)
	n := fleetService(t, home)
	pkiDir := filepath.Join(home, "pki")
	if res, _ := n.InstallBundle(fromSettings, bundle(t, f, f.Enroll(t, "laptop", pki.RoleInstance, keyPath)), "", ""); !res.Installed {
		t.Fatalf("first install: %+v", res)
	}
	foreign := bundle(t, f1, f1.Enroll(t, "laptop", pki.RoleInstance, keyPath))
	oldFP, newFP := rootFP(t, f), rootFP(t, f1)

	// The preview: refused, naming both.
	res, err := n.InstallBundle(fromSettings, foreign, "", "")
	if err != nil || res.Class != classRootDiffers || res.InstalledRootFingerprint != oldFP || res.BundleRootFingerprint != newFP {
		t.Fatalf("preview %+v %v", res, err)
	}

	for _, tc := range []struct{ name, from, to string }{
		{"wrong from", newFP, newFP},
		{"wrong to", oldFP, oldFP},
		{"only to", "", newFP},
		{"only from", oldFP, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tree(t, pkiDir)
			res, err := n.InstallBundle(fromSettings, foreign, tc.from, tc.to)
			if err != nil || res.Installed || res.Class != classConfirmationStale ||
				res.InstalledRootFingerprint != oldFP || res.BundleRootFingerprint != newFP {
				t.Fatalf("%+v %v", res, err)
			}
			if !reflect.DeepEqual(before, tree(t, pkiDir)) {
				t.Fatal("a refused confirmation changed <home>/pki")
			}
		})
	}

	// The installed root changes between the preview and the confirmation
	// (another fleet installed underneath, e.g. by the CLI): the confirmed
	// pair no longer describes the move, so it is refused.
	if _, err := pki.InstallBundle(pkiDir, keyPath, []byte(bundle(t, f2, f2.Enroll(t, "laptop", pki.RoleInstance, keyPath))), true); err != nil {
		t.Fatal(err)
	}
	res, err = n.InstallBundle(fromSettings, foreign, oldFP, newFP)
	if err != nil || res.Installed || res.Class != classConfirmationStale || res.InstalledRootFingerprint != rootFP(t, f2) {
		t.Fatalf("stale preview accepted: %+v %v", res, err)
	}

	// The right pair replaces.
	res, err = n.InstallBundle(fromSettings, foreign, rootFP(t, f2), newFP)
	if err != nil || !res.Installed {
		t.Fatalf("confirmed replace: %+v %v", res, err)
	}
	cur, err := pki.LoadRootCert(filepath.Join(pkiDir, pki.RootCertFile))
	if err != nil || !cur.Equal(f1.Root.Cert) {
		t.Fatalf("root after replace: %v", err)
	}
}

// Neither the raw bundle nor any part of it reaches the log, whatever the
// outcome.
func TestFleetIdentity_TheBundleIsNeverLogged(t *testing.T) {
	logs := captureLog(t)
	home, keyPath := fleetHome(t)
	f, f1 := pkitest.New(t), pkitest.New(t)
	n := fleetService(t, home)
	good := bundle(t, f, f.Enroll(t, "laptop", pki.RoleInstance, keyPath))
	foreign := bundle(t, f1, f1.Enroll(t, "laptop", pki.RoleInstance, keyPath))
	n.InstallBundle(fromSettings, good, "", "")
	n.InstallBundle(fromSettings, foreign, "", "")
	n.InstallBundle(fromSettings, "junk "+good[40:120], "", "")
	out := logs.String()
	if !strings.Contains(out, "fleet identity") {
		t.Fatalf("the installs logged nothing at all (fixture broken?):\n%s", out)
	}
	for _, frag := range []string{"BEGIN", good[40:80], foreign[40:80]} {
		if strings.Contains(out, frag) {
			t.Fatalf("the log carries bundle text %q:\n%s", frag, out)
		}
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
	m := f.Enroll(t, "laptop", pki.RoleInstance, keyPath)
	if res, _ := n.InstallBundle(fromSettings, bundle(t, f, m), "", ""); !res.Installed {
		t.Fatalf("install: %+v", res)
	}
	n.tls.set(tlsState{Configured: "0.0.0.0:19279", Reason: tlsNoCertificate})
	t.Setenv("KNOMIT_TLS_ADDR", "0.0.0.0:19279")
	id, err = n.GetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if !id.Enrolled || id.Principal != "instance:"+m.Fingerprint()+"@cert" || id.SAN != pki.SAN(pki.RoleInstance, "laptop", m.Fingerprint()) ||
		id.Serial != m.Cert.SerialNumber.Text(16) || id.RootFingerprint != rootFP(t, f) || id.CRLNumber != "1" || id.Dir != filepath.Join(home, "pki") {
		t.Fatalf("enrolled: %+v", id)
	}
	if !id.TLS.Booted || id.TLS.Reason != tlsNoCertificate || id.TLS.ConfiguredNow != "0.0.0.0:19279" {
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
	want, err := knomitapp.PublicKeyLine(keyPath)
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
		"identity":      FleetIdentity{},
		"tls":           FleetTLS{},
		"installResult": InstallResult{},
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
		"GetIdentity", "GetSettings", "InstallBundle", "PublicKeyLine",
		"RestartApp", "RevealLogFile", "SaveSettings", "WriteFile",
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

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keysOfAny(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
