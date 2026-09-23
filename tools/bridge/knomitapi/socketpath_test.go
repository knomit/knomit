package knomitapi

import (
	"path/filepath"
	"testing"

	"knomit/internal/config"
)

// knomit#271 pins the delegation: the bridge's SocketPath is
// config.SocketPath, so an operator override the server honours reaches the
// bridge too. The layering itself is tested in internal/config, against
// config.Load; this only proves the bridge does not resolve anything itself.
func TestSocketPath_DelegatesToConfig_KnomitSocketEnv(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir())
	t.Setenv("KNOMIT_REPO", "")
	want := filepath.Join(t.TempDir(), "env.sock")
	t.Setenv("KNOMIT_SOCKET", want)

	fromConfig, err := config.SocketPath()
	if err != nil {
		t.Fatalf("config.SocketPath: %v", err)
	}
	if got := SocketPath(); got != fromConfig || got != want {
		t.Fatalf("bridge SocketPath() = %q, config.SocketPath() = %q, want both %q", got, fromConfig, want)
	}
}
