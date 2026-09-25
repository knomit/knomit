package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"knomit/internal/config"
)

// knomit#271: the listener the bridge DIALS (config.SocketPath, which
// tools/bridge/knomitapi delegates to) is the listener the server OPENS
// (config.Load().Socket) for every layer the operator can set — KNOMIT_SOCKET,
// the TOML `socket` key, and a "~/" in KNOMIT_HOME — not only for the default.
// A disagreement does not fail loudly: the bridge finds no listener, falls
// back to TCP and quietly loses the verified identity.
//
// Platform-neutral on purpose: KNOMIT_SOCKET and the TOML key are honoured on
// Windows too.

// isolateSocketEnv points every input either resolver reads at the test, and
// clears EVERY KNOMIT_* variable: requireAgreement runs the full Load, with
// Validate, so a stray KNOMIT_LOG_* in a developer's shell would otherwise fail
// a socket test for a reason that has nothing to do with the socket.
func isolateSocketEnv(t *testing.T, home string) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "KNOMIT_") {
			t.Setenv(k, "")
		}
	}
	t.Setenv("KNOMIT_HOME", home)
}

// requireAgreement asserts server and bridge resolve the same listener, and
// that it is want.
func requireAgreement(t *testing.T, want string) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	got, err := config.SocketPath()
	if err != nil {
		t.Fatalf("config.SocketPath: %v", err)
	}
	if got != cfg.Socket {
		t.Fatalf("bridge SocketPath() = %q, server Load().Socket = %q", got, cfg.Socket)
	}
	if got != want {
		t.Fatalf("SocketPath() = Load().Socket = %q, want %q", got, want)
	}
}

func writeSocketTOML(t *testing.T, home, socket string) {
	t.Helper()
	// A TOML literal string, so a Windows path's backslashes stay literal.
	body := "socket = '" + socket + "'\n"
	if err := os.WriteFile(filepath.Join(home, "knomit.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSocketPath_AgreesWithLoad_KnomitSocketEnv(t *testing.T) {
	isolateSocketEnv(t, t.TempDir())
	want := config.ExplicitSocket(t, "env")
	t.Setenv("KNOMIT_SOCKET", want)
	requireAgreement(t, want)
}

func TestSocketPath_AgreesWithLoad_TOMLSocket(t *testing.T) {
	home := t.TempDir()
	isolateSocketEnv(t, home)
	want := config.ExplicitSocket(t, "toml")
	writeSocketTOML(t, home, want)
	requireAgreement(t, want)
}

func TestSocketPath_AgreesWithLoad_EnvBeatsTOML(t *testing.T) {
	home := t.TempDir()
	isolateSocketEnv(t, home)
	writeSocketTOML(t, home, config.ExplicitSocket(t, "toml"))
	want := config.ExplicitSocket(t, "env")
	t.Setenv("KNOMIT_SOCKET", want)
	requireAgreement(t, want)
}

func TestSocketPath_AgreesWithLoad_TildeInKnomitHome(t *testing.T) {
	osHome := t.TempDir()
	t.Setenv("HOME", osHome)
	t.Setenv("USERPROFILE", osHome)
	expanded := filepath.Join(osHome, "kh")
	if err := os.MkdirAll(expanded, 0o700); err != nil {
		t.Fatal(err)
	}

	// The default for the already-expanded root is the reference answer.
	isolateSocketEnv(t, expanded)
	want, err := config.SocketPath()
	if err != nil {
		t.Fatalf("config.SocketPath for the expanded home: %v", err)
	}

	t.Setenv("KNOMIT_HOME", "~/kh")
	requireAgreement(t, want)
	got, err := config.SocketPath()
	if err != nil {
		t.Fatalf("config.SocketPath: %v", err)
	}
	if strings.Contains(got, "~") {
		t.Fatalf("SocketPath() = %q still carries a literal ~", got)
	}
}

// A data root spelled with "~/" finds its knomit.toml on BOTH sides. Load used
// to look for the literal "~/kh/knomit.toml" before expanding Home, so it
// ignored the file the bridge (which expands first) read — the two disagreed.
func TestSocketPath_AgreesWithLoad_TildeHomeWithTOMLSocket(t *testing.T) {
	osHome := t.TempDir()
	t.Setenv("HOME", osHome)
	t.Setenv("USERPROFILE", osHome)
	expanded := filepath.Join(osHome, "kh")
	if err := os.MkdirAll(expanded, 0o700); err != nil {
		t.Fatal(err)
	}
	isolateSocketEnv(t, "~/kh")
	want := config.ExplicitSocket(t, "toml")
	writeSocketTOML(t, expanded, want)
	requireAgreement(t, want)
}

// A knomit.toml the bridge cannot decode is an error from SocketPath, not a
// silent fall-through to the default listener: Load refuses the same file, so
// a default here would be a listener the server never opens.
func TestSocketPath_MalformedTOMLIsAnError(t *testing.T) {
	home := t.TempDir()
	isolateSocketEnv(t, home)
	path := filepath.Join(home, "knomit.toml")
	if err := os.WriteFile(path, []byte("socket = [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := config.SocketPath()
	if err == nil {
		t.Fatalf("SocketPath() = %q with a malformed knomit.toml; want an error", got)
	}
	// The operator needs to be told WHICH knomit.toml is broken.
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not name the broken file %q", err, path)
	}
}

// A relative KNOMIT_HOME is made absolute against the working directory of the
// process that reads it, once, before knomit.toml is searched for and before
// the default listener is derived from it.
func TestSocketPath_AgreesWithLoad_RelativeHomeIsAbsolutised(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "rel", "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)

	isolateSocketEnv(t, filepath.Join(base, "rel", "home"))
	want, err := config.SocketPath()
	if err != nil {
		t.Fatalf("config.SocketPath for the absolute home: %v", err)
	}

	t.Setenv("KNOMIT_HOME", filepath.Join("rel", "home"))
	requireAgreement(t, want)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if abs := filepath.Join(base, "rel", "home"); cfg.Home != abs {
		t.Fatalf("Load().Home = %q, want %q", cfg.Home, abs)
	}
}

// A relative socket would resolve against each process's own working
// directory, and the server and the bridge do not share one. Both sides refuse
// it with the same error, which states this platform's rule.
func TestSocketPath_RelativeSocketRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(t *testing.T, home string)
	}{
		{"env", func(t *testing.T, _ string) { t.Setenv("KNOMIT_SOCKET", "rel.sock") }},
		{"toml", func(t *testing.T, home string) { writeSocketTOML(t, home, "rel.sock") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			isolateSocketEnv(t, home)
			tc.set(t, home)

			_, loadErr := config.Load()
			got, pathErr := config.SocketPath()
			if loadErr == nil || pathErr == nil {
				t.Fatalf("Load err = %v, SocketPath() = %q, err = %v; want both to refuse a relative socket",
					loadErr, got, pathErr)
			}
			if loadErr.Error() != pathErr.Error() {
				t.Fatalf("Load and SocketPath disagree:\n  Load:       %v\n  SocketPath: %v", loadErr, pathErr)
			}
			rule := "must be an absolute path"
			if runtime.GOOS == "windows" {
				rule = "must be a pipe name"
			}
			if !strings.Contains(pathErr.Error(), rule) {
				t.Fatalf("error %q does not state the rule", pathErr)
			}
		})
	}
}

// setOSHome points os.UserHomeDir at a temp directory on every platform.
func setOSHome(t *testing.T) string {
	t.Helper()
	osHome := t.TempDir()
	t.Setenv("HOME", osHome)
	t.Setenv("USERPROFILE", osHome)
	return osHome
}
