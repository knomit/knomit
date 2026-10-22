//go:build desktop

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"knomit/internal/foundation/config"
	"knomit/internal/foundation/serveraddr"
)

// v2Lock is a server.json as a v2 `knomit serve` writes it, listening on a
// loopback HTTP port and a fleet port.
func v2Lock(pid int) serveraddr.LockInfo {
	return serveraddr.LockInfo{
		PID: pid, Port: 19300, Version: "0.5.3.abc1234", Schema: serveraddr.LockSchemaV2,
		HTTP: "http://127.0.0.1:19300", TLS: "0.0.0.0:19379", Generation: 3, Reload: true,
		Listeners: map[string]serveraddr.LockListener{
			"http": {Configured: "127.0.0.1:19300", Listening: "127.0.0.1:19300"},
			"tls":  {Configured: "0.0.0.0:19379", Listening: "0.0.0.0:19379"},
		},
	}
}

func writeLockFor(t *testing.T, home string, info serveraddr.LockInfo) {
	t.Helper()
	if err := serveraddr.WriteLock(config.LockfilePathFor(home), info); err != nil {
		t.Fatal(err)
	}
}

func decodeCfg(t *testing.T, path string) config.Config {
	t.Helper()
	cfg := config.Defaults()
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// GetSettings reports the server's own account of itself from a v2 server.json,
// for a server the app spawned: the URL, the fleet port, the generation and
// whether Restart server is on offer.
func TestGetSettings_SpawnedServerFromAV2Lock(t *testing.T) {
	home := desktopHome(t)
	n := newNativeService(filepath.Join(home, "knomit.toml"), filepath.Join(home, "desktop.log"), &stubToggler{})
	n.desktopVersion = "0.5.3.def5678"
	n.restartServer = func(context.Context) error { return nil }
	n.isAttached = func() bool { return false }

	// No server.json yet: nothing is claimed.
	got, err := n.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.Server.Running || got.Server.CanRestart || got.Server.RestartNeeded == nil {
		t.Fatalf("no server.json, but %+v (RestartNeeded must be [] not null on the wire)", got.Server)
	}

	info := v2Lock(4242)
	info.RestartNeeded = []string{"auth.require"}
	info.ReloadError = "2026-10-12T10:00:00Z: toml: bad"
	info.Listeners["tls"] = serveraddr.LockListener{Configured: "0.0.0.0:19379", Reason: "no_certificate"}
	info.TLS = ""
	writeLockFor(t, home, info)
	got, err = n.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	st := got.Server
	if !st.Running || st.PID != 4242 || st.Attached || !st.CanRestart || !st.CanReload {
		t.Fatalf("spawned server state: %+v", st)
	}
	if st.HTTPURL != "http://127.0.0.1:19300" || st.Generation != 3 || st.Version != "0.5.3.abc1234" || st.DesktopVersion != "0.5.3.def5678" {
		t.Fatalf("server state: %+v", st)
	}
	if st.TLSListening != "" || st.TLSReason != "no_certificate" || st.HTTPReason != "" {
		t.Fatalf("listener reasons: %+v", st)
	}
	if !slices.Equal(st.RestartNeeded, []string{"auth.require"}) || !strings.Contains(st.ReloadError, "toml: bad") {
		t.Fatalf("restart_needed / reload_error not carried: %+v", st)
	}
	if st.VersionSkew {
		t.Fatal("a spawned server is this build's own: no version skew is reported")
	}
}

// Attach mode: the server was started outside the app. Restart is off, the pid
// is named, and a major.minor mismatch is flagged (a patch difference is not).
func TestGetSettings_AttachedServerNamesItselfAndFlagsVersionSkew(t *testing.T) {
	home := desktopHome(t)
	n := newNativeService(filepath.Join(home, "knomit.toml"), filepath.Join(home, "desktop.log"), &stubToggler{})
	n.restartServer = func(context.Context) error { return nil }
	n.isAttached = func() bool { return true }
	writeLockFor(t, home, v2Lock(777))

	for _, c := range []struct {
		desktop string
		skew    bool
	}{{"0.6.0.zzz", true}, {"0.5.9.zzz", false}, {"", false}} {
		n.desktopVersion = c.desktop
		got, err := n.GetSettings()
		if err != nil {
			t.Fatal(err)
		}
		st := got.Server
		if !st.Attached || st.CanRestart || st.PID != 777 {
			t.Fatalf("desktop %q: attach state %+v", c.desktop, st)
		}
		if st.VersionSkew != c.skew {
			t.Fatalf("desktop %q vs server %q: VersionSkew = %v, want %v", c.desktop, st.Version, st.VersionSkew, c.skew)
		}
	}
}

func TestScopeOf(t *testing.T) {
	for _, c := range []struct {
		http bool
		host string
		want string
	}{
		{false, "0.0.0.0", scopeOff}, {false, "localhost", scopeOff},
		{true, "localhost", scopeLocal}, {true, "127.0.0.1", scopeLocal}, {true, "::1", scopeLocal}, {true, "[::1]", scopeLocal},
		{true, "0.0.0.0", scopeAll}, {true, "", scopeAll}, {true, "::", scopeAll}, {true, "[::]", scopeAll},
		{true, "192.168.1.5", scopeOther}, {true, "knomit.lan", scopeOther},
	} {
		if got := scopeOf(c.http, c.host); got != c.want {
			t.Errorf("scopeOf(%v, %q) = %q, want %q", c.http, c.host, got, c.want)
		}
	}
}

func TestGetSettings_ScopeComesFromTheFile(t *testing.T) {
	home := desktopHome(t)
	path := filepath.Join(home, "knomit.toml")
	n := newNativeService(path, filepath.Join(home, "desktop.log"), &stubToggler{})
	for _, c := range []struct{ toml, want string }{
		{"", scopeLocal}, // the defaults: http on, host localhost
		{"http = false\n", scopeOff},
		{"host = \"0.0.0.0\"\n", scopeAll},
		{"host = \"10.1.2.3\"\n", scopeOther},
	} {
		if err := os.WriteFile(path, []byte(c.toml), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := n.GetSettings()
		if err != nil {
			t.Fatal(err)
		}
		if got.HTTPScope != c.want {
			t.Errorf("%q: scope %q, want %q", c.toml, got.HTTPScope, c.want)
		}
	}
}

func scopeSettings(scope string) Settings {
	return Settings{Port: "19278", LogLevel: "info", LogFormat: "console", HTTPScope: scope}
}

// Only the keys the move needs are written. A scope the file already means
// writes neither; "off" never touches host; "local" over a loopback host
// leaves the host as the user spelled it.
func TestApplySettings_ScopeWritesOnlyTheKeysItMoves(t *testing.T) {
	for _, c := range []struct {
		name, start, scope string
		wantHTTP           *bool // nil: the key must be absent
		wantHost           string
	}{
		{"local over defaults writes nothing", "", scopeLocal, nil, ""},
		{"all over defaults writes host only", "", scopeAll, nil, "0.0.0.0"},
		{"off over defaults writes http only", "", scopeOff, ptr(false), ""},
		{"local over off writes http only", "http = false\n", scopeLocal, ptr(true), ""},
		{"all over off writes both", "http = false\n", scopeAll, ptr(true), "0.0.0.0"},
		{"off over all leaves host alone", "host = \"0.0.0.0\"\n", scopeOff, ptr(false), "0.0.0.0"},
		{"local over all rewrites host", "host = \"0.0.0.0\"\n", scopeLocal, nil, "127.0.0.1"},
		{"local over a spelled loopback host keeps it", "host = \"::1\"\n", scopeLocal, nil, "::1"},
		{"other writes nothing", "http = false\nhost = \"10.1.2.3\"\n", scopeOther, ptr(false), "10.1.2.3"},
		{"empty scope writes nothing", "http = false\n", "", ptr(false), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "knomit.toml")
			if err := os.WriteFile(path, []byte(c.start), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := applySettings(scopeSettings(c.scope), path, &stubToggler{}, nil); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(path)
			var m map[string]any
			if _, err := toml.Decode(string(raw), &m); err != nil {
				t.Fatal(err)
			}
			http, hasHTTP := m["http"]
			if c.wantHTTP == nil && hasHTTP {
				t.Errorf("http written (%v) but the scope did not need it:\n%s", http, raw)
			}
			if c.wantHTTP != nil && (!hasHTTP || http != *c.wantHTTP) {
				t.Errorf("http = %v (present %v), want %v:\n%s", http, hasHTTP, *c.wantHTTP, raw)
			}
			host, hasHost := m["host"]
			if c.wantHost == "" && hasHost {
				t.Errorf("host written (%v) but the scope did not need it:\n%s", host, raw)
			}
			if c.wantHost != "" && host != c.wantHost {
				t.Errorf("host = %v, want %q:\n%s", host, c.wantHost, raw)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// A comment on the line being moved survives, and what the scope does not own
// is untouched.
func TestApplySettings_ScopeKeepsCommentsAndOtherKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knomit.toml")
	start := "# mine\nhttp = false  # socket only for now\n\n[auth]\nrequire = true\n"
	if err := os.WriteFile(path, []byte(start), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applySettings(scopeSettings(scopeLocal), path, &stubToggler{}, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# mine") || !strings.Contains(string(raw), "http = true  # socket only for now") || !strings.Contains(string(raw), "require = true") {
		t.Fatalf("an edit lost something:\n%s", raw)
	}
}

// The scope is refused whole while either variable owns a half of it, and an
// unknown scope is refused before anything is written.
func TestApplySettings_ScopeRefusals(t *testing.T) {
	for _, env := range []string{"KNOMIT_HTTP", "KNOMIT_HOST"} {
		path := filepath.Join(t.TempDir(), "knomit.toml")
		if err := os.WriteFile(path, []byte("http = false\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := applySettings(scopeSettings(scopeAll), path, &stubToggler{}, []string{env}); err != nil {
			t.Fatal(err)
		}
		cfg := decodeCfg(t, path)
		if cfg.HTTP || cfg.Host != "localhost" {
			t.Errorf("%s set: the scope was still written (http=%v host=%q)", env, cfg.HTTP, cfg.Host)
		}
	}

	path := filepath.Join(t.TempDir(), "knomit.toml")
	if err := os.WriteFile(path, []byte("http = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := applySettings(scopeSettings("everywhere"), path, &stubToggler{}, nil)
	if err == nil || !strings.Contains(err.Error(), "http scope") {
		t.Fatalf("an unknown scope: %v", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "http = false\n" {
		t.Fatalf("a refused save changed the file:\n%s", raw)
	}
}

func TestEnvOverrides_NameTheScopeVariables(t *testing.T) {
	got := envOverrides(func(k string) string {
		if k == "KNOMIT_HTTP" || k == "KNOMIT_HOST" {
			return "x"
		}
		return ""
	})
	if !slices.Equal(got, []string{"KNOMIT_HTTP", "KNOMIT_HOST"}) {
		t.Fatalf("overrides = %v", got)
	}
}

// reloadFake stands in for serveraddr.RequestReload and records the call.
type reloadFake struct {
	calls int
	home  string
	wait  time.Duration
	info  serveraddr.LockInfo
	err   error
}

func (f *reloadFake) request(_ context.Context, home string, wait time.Duration) (serveraddr.LockInfo, error) {
	f.calls++
	f.home, f.wait = home, wait
	return f.info, f.err
}

func saveFixture(t *testing.T) (*NativeService, *reloadFake, string) {
	t.Helper()
	home := desktopHome(t)
	path := filepath.Join(home, "knomit.toml")
	n := newNativeService(path, filepath.Join(home, "desktop.log"), &stubToggler{})
	f := &reloadFake{}
	n.requestReload = f.request
	n.reloadWait = 3 * time.Second
	writeLockFor(t, home, v2Lock(4242))
	return n, f, home
}

// SaveSettings writes the file, then reloads through the shared client with
// the home and the wait, and turns each answer into an ack.
func TestSaveSettings_ReloadsAndAcknowledges(t *testing.T) {
	applied := v2Lock(4242)
	applied.Generation = 4
	needs := applied
	needs.RestartNeeded = []string{"auth.require"}
	failedInfo := applied
	failedInfo.Listeners = map[string]serveraddr.LockListener{
		"http": {Configured: "127.0.0.1:19400", Listening: "127.0.0.1:19300", Reason: "addr_in_use"},
	}

	for _, c := range []struct {
		name    string
		info    serveraddr.LockInfo
		err     error
		outcome string
		check   func(*testing.T, SaveAck)
	}{
		{"applied", applied, nil, "applied", func(t *testing.T, a SaveAck) {
			if a.Generation != 4 || a.HTTPURL != "http://127.0.0.1:19300" || a.TLSListening != "0.0.0.0:19379" || len(a.RestartNeeded) != 0 || a.Error != "" {
				t.Errorf("ack %+v", a)
			}
		}},
		{"restart_needed", needs, nil, "restart_needed", func(t *testing.T, a SaveAck) {
			if !slices.Equal(a.RestartNeeded, []string{"auth.require"}) {
				t.Errorf("ack %+v", a)
			}
		}},
		{"listener_failed", failedInfo, &serveraddr.ReloadListenerError{Info: failedInfo, Failed: []string{"http: addr_in_use"}}, "listener_failed", func(t *testing.T, a SaveAck) {
			if a.HTTPReason != "addr_in_use" || !strings.Contains(a.Error, "addr_in_use") || a.HTTPURL == "" {
				t.Errorf("ack %+v", a)
			}
		}},
		{"refused", v2Lock(4242), &serveraddr.ReloadRefusedError{Message: "2026-10-12T10:00:00Z: toml: line 1"}, "refused", func(t *testing.T, a SaveAck) {
			if !strings.Contains(a.Error, "toml: line 1") || strings.Contains(a.Error, "reload refused") {
				t.Errorf("the reason alone belongs in Error, got %q", a.Error)
			}
		}},
		{"timeout", v2Lock(4242), &serveraddr.ReloadTimeoutError{Wait: 3 * time.Second, Generation: 3}, "timeout", func(t *testing.T, a SaveAck) {
			if !strings.Contains(a.Error, "did not acknowledge") {
				t.Errorf("ack %+v", a)
			}
		}},
		{"unavailable", v2Lock(4242), errors.New("the server (pid 4242, 0.4.0) does not listen for reload: restart it to apply configuration changes"), "unavailable", func(t *testing.T, a SaveAck) {
			if !strings.Contains(a.Error, "does not listen for reload") {
				t.Errorf("ack %+v", a)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, f, home := saveFixture(t)
			f.info, f.err = c.info, c.err
			ack, err := n.SaveSettings(fromSettings, scopeSettings(scopeAll))
			if err != nil {
				t.Fatalf("SaveSettings: %v (a reload problem is an ack, the file is saved)", err)
			}
			if ack.Outcome != c.outcome {
				t.Fatalf("outcome %q, want %q (%+v)", ack.Outcome, c.outcome, ack)
			}
			c.check(t, ack)
			if ack.RestartNeeded == nil {
				t.Error("RestartNeeded must be [] on the wire, not null")
			}
			if f.calls != 1 || f.home != home || f.wait != 3*time.Second {
				t.Errorf("reload called %d times with home %q wait %v", f.calls, f.home, f.wait)
			}
			// The file was written before the reload was asked for.
			if cfg := decodeCfg(t, filepath.Join(home, "knomit.toml")); cfg.Host != "0.0.0.0" {
				t.Errorf("host = %q: the scope was not saved", cfg.Host)
			}
		})
	}
}

// No server.json: there is nothing to tell. The save stands, nothing is
// signalled, and the ack says it applies when the server starts.
func TestSaveSettings_NoServerNothingToReload(t *testing.T) {
	home := desktopHome(t)
	n := newNativeService(filepath.Join(home, "knomit.toml"), filepath.Join(home, "desktop.log"), &stubToggler{})
	f := &reloadFake{}
	n.requestReload = f.request
	ack, err := n.SaveSettings(fromSettings, scopeSettings(scopeAll))
	if err != nil || ack.Outcome != "not_running" || f.calls != 0 {
		t.Fatalf("ack %+v err %v, reload calls %d", ack, err, f.calls)
	}
}

// A caller that is not the Settings window changes nothing and signals
// nothing (invariant desktop/fleet/install-surface).
func TestSaveSettings_NonSettingsCallerIsRefusedBeforeAnyWriteOrReload(t *testing.T) {
	n, f, home := saveFixture(t)
	path := filepath.Join(home, "knomit.toml")
	for _, ctx := range []context.Context{fromMain, context.Background()} {
		_, err := n.SaveSettings(ctx, scopeSettings(scopeAll))
		if !errors.Is(err, errNotSettingsWindow) {
			t.Fatalf("SaveSettings = %v, want errNotSettingsWindow", err)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused save wrote knomit.toml")
	}
	if f.calls != 0 {
		t.Fatalf("a refused save asked for %d reloads", f.calls)
	}
	// Positive control: the same call from Settings writes and reloads.
	if _, err := n.SaveSettings(fromSettings, scopeSettings(scopeAll)); err != nil || f.calls != 1 {
		t.Fatalf("from Settings: err %v, reloads %d", err, f.calls)
	}
}

// A save that fails validation never reaches the server.
func TestSaveSettings_RejectedSaveDoesNotReload(t *testing.T) {
	n, f, _ := saveFixture(t)
	s := scopeSettings(scopeAll)
	s.Port = "80"
	if _, err := n.SaveSettings(fromSettings, s); err == nil {
		t.Fatal("port 80 accepted")
	}
	if f.calls != 0 {
		t.Fatalf("a rejected save asked for %d reloads", f.calls)
	}
}

// The reload is the shared client's, not a shell-out or a route: the default
// is exactly serveraddr.RequestReload, and against a lock that does not claim
// reload it refuses without signalling anything.
func TestSaveSettings_DefaultReloadIsTheSharedClient(t *testing.T) {
	home := desktopHome(t)
	n := newNativeService(filepath.Join(home, "knomit.toml"), filepath.Join(home, "desktop.log"), &stubToggler{})
	n.reloadWait = time.Second
	old := v2Lock(os.Getpid())
	old.Reload = false // a server from before reload existed: SIGHUP would end it
	old.Home = home
	writeLockFor(t, home, old)
	ack, err := n.SaveSettings(fromSettings, scopeSettings(scopeLocal))
	if err != nil {
		t.Fatal(err)
	}
	if ack.Outcome != "unavailable" || !strings.Contains(ack.Error, "does not listen for reload") {
		t.Fatalf("ack %+v", ack)
	}
}

// RestartServer is bounded: a replacement that never answers comes back as an
// error the window can show.
func TestRestartServer_TimesOut(t *testing.T) {
	n := newNativeService("", "", &stubToggler{})
	n.restartWait = 50 * time.Millisecond
	n.restartServer = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	start := time.Now()
	err := n.RestartServer(fromSettings)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("RestartServer = %v after %v", err, time.Since(start))
	}
}

// Fresh vs upgrade: only a home with neither knomit.toml nor control.db gets
// the socket-only default.
func TestEnsureFreshInstallDefaults(t *testing.T) {
	t.Run("fresh home gets http = false and a config that loads", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "home") // not even created yet
		wrote, err := ensureFreshInstallDefaults(home)
		if err != nil || !wrote {
			t.Fatalf("wrote=%v err=%v", wrote, err)
		}
		t.Setenv("KNOMIT_HOME", home)
		for _, k := range envKeys {
			t.Setenv(k, "")
		}
		// config.Load validates: a fresh install must be a config that starts
		// (http = false is only legal with a local socket, which Load derives).
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("a fresh install must start: %v", err)
		}
		if cfg.HTTP {
			t.Fatal("http is still on in a fresh install")
		}
		// Settings shows the resulting state.
		n := newNativeService(filepath.Join(home, "knomit.toml"), filepath.Join(home, "desktop.log"), &stubToggler{})
		got, err := n.GetSettings()
		if err != nil || got.HTTPScope != scopeOff {
			t.Fatalf("Settings reports scope %q (err %v) on a fresh install", got.HTTPScope, err)
		}
		// Idempotent: a second launch does not rewrite it.
		if wrote, err := ensureFreshInstallDefaults(home); err != nil || wrote {
			t.Fatalf("second call wrote=%v err=%v", wrote, err)
		}
	})
	t.Run("an existing knomit.toml is untouched", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, "knomit.toml")
		if err := os.WriteFile(path, []byte("port = 20000\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if wrote, err := ensureFreshInstallDefaults(home); err != nil || wrote {
			t.Fatalf("wrote=%v err=%v", wrote, err)
		}
		if raw, _ := os.ReadFile(path); string(raw) != "port = 20000\n" {
			t.Fatalf("an upgrade's knomit.toml changed:\n%s", raw)
		}
	})
	t.Run("a control.db alone is an upgrade: no file is created", func(t *testing.T) {
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, "control.db"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if wrote, err := ensureFreshInstallDefaults(home); err != nil || wrote {
			t.Fatalf("wrote=%v err=%v", wrote, err)
		}
		if _, err := os.Stat(filepath.Join(home, "knomit.toml")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("a knomit.toml appeared beside an existing control.db: the upgrade would lose its HTTP port")
		}
	})
}
