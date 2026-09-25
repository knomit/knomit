//go:build windows

package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"knomit/internal/config"
)

// On Windows a leading `~\` is the user's home directory, exactly as "~/" is:
// for the socket on both sides, and for every other path field Load expands.
func TestSocketPath_AgreesWithLoad_BackslashTildeInSocket(t *testing.T) {
	osHome := setOSHome(t)
	isolateSocketEnv(t, t.TempDir())
	t.Setenv("KNOMIT_SOCKET", `~\x.sock`)
	requireAgreement(t, filepath.Join(osHome, "x.sock"))
}

func TestLoad_BackslashTildeInPathField(t *testing.T) {
	osHome := setOSHome(t)
	home := t.TempDir()
	isolateSocketEnv(t, home)
	body := "[remote]\nknown_hosts = '~\\kh'\n"
	if err := os.WriteFile(filepath.Join(home, "knomit.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if want := filepath.Join(osHome, "kh"); cfg.Remote.KnownHosts != want {
		t.Fatalf("Remote.KnownHosts = %q, want %q", cfg.Remote.KnownHosts, want)
	}
}
