//go:build !windows

package knomitapi

import (
	"strings"
	"testing"

	"knomit/internal/auth"
	"knomit/internal/config"
)

// knomit#253, from the bridge's side: for a data root too long for sun_path,
// the path the bridge DIALS (SocketPath) is the path the server OPENS
// (config.Load().Socket, what `knomit serve` and the desktop pass to
// auth.ListenLocal), and it is the short fallback, not the overlong path the
// server can no longer bind. A bridge that grew its own path logic would
// fail this instead of silently losing the verified identity.
func TestSocketPath_LongHomeMatchesTheServersFallback(t *testing.T) {
	home := "/tmp/" + strings.Repeat("b", 115)
	t.Setenv("KNOMIT_HOME", home)
	t.Setenv("KNOMIT_SOCKET", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	got := SocketPath()
	if got != cfg.Socket {
		t.Fatalf("bridge SocketPath() = %q, server Load().Socket = %q", got, cfg.Socket)
	}
	if len(got) >= auth.SunPathCap() || !strings.HasPrefix(got, auth.FallbackSocketDir()+"/") {
		t.Fatalf("SocketPath() = %q is not the short fallback under %q", got, auth.FallbackSocketDir())
	}
}
