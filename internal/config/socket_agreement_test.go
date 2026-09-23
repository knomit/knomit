package config_test

import (
	"os"
	"path/filepath"
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
	want := filepath.Join(t.TempDir(), "env.sock")
	t.Setenv("KNOMIT_SOCKET", want)
	requireAgreement(t, want)
}

func TestSocketPath_AgreesWithLoad_TOMLSocket(t *testing.T) {
	home := t.TempDir()
	isolateSocketEnv(t, home)
	want := filepath.Join(t.TempDir(), "toml.sock")
	writeSocketTOML(t, home, want)
	requireAgreement(t, want)
}

func TestSocketPath_AgreesWithLoad_EnvBeatsTOML(t *testing.T) {
	home := t.TempDir()
	isolateSocketEnv(t, home)
	writeSocketTOML(t, home, filepath.Join(t.TempDir(), "toml.sock"))
	want := filepath.Join(t.TempDir(), "env.sock")
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
	if strings.Contains(want, "~") {
		t.Fatalf("listener %q still carries a literal ~", want)
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
	want := filepath.Join(t.TempDir(), "toml.sock")
	writeSocketTOML(t, expanded, want)
	requireAgreement(t, want)
}

// A knomit.toml the bridge cannot decode is an error from SocketPath, not a
// silent fall-through to the default listener: Load refuses the same file, so
// a default here would be a listener the server never opens.
func TestSocketPath_MalformedTOMLIsAnError(t *testing.T) {
	home := t.TempDir()
	isolateSocketEnv(t, home)
	if err := os.WriteFile(filepath.Join(home, "knomit.toml"), []byte("socket = [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := config.SocketPath(); err == nil {
		t.Fatalf("SocketPath() = %q with a malformed knomit.toml; want an error", got)
	}
}
