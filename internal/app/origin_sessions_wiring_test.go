package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"knomit/internal/foundation/config"
	"knomit/test/testenv"
)

// TestNew_OriginSessionsHaveTheAppsLifetimeAndHomeDir pins the two things
// app.New gives web's origin-session manager: a sessions dir under the
// instance's home, which the first router build clears of a previous run's
// session folders, and a lifetime that Close ends (which removes every
// session and its folder; api/origins' tests cover that half).
//
// SABOTAGE: drop OriginSessionsDir from the web.Server literal -> the stale
// folder survives; drop the endLifetime call from Close (or Lifetime from the
// literal) -> the lifetime is still live after Close.
func TestNew_OriginSessionsHaveTheAppsLifetimeAndHomeDir(t *testing.T) {
	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	stale := filepath.Join(cfg.Home, "origin-sessions", "session-from-a-previous-run")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}

	a, err := New(context.Background(), cfg, Options{APIOnly: true, Embedder: &testenv.DeterministicEmbedder{}})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			a.Close()
		}
	}()

	_ = a.Handler() // builds the router, and with it the origin-session manager
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("a previous run's session folder survived the first router build (stat err = %v)", err)
	}

	life := a.Server().Lifetime
	if life == nil {
		t.Fatal("app.New gave the server no Lifetime")
	}
	if life.Err() != nil {
		t.Fatal("the server's lifetime ended before Close")
	}
	a.Close()
	closed = true
	if life.Err() == nil {
		t.Fatal("Close did not end the server's lifetime: origin sessions would outlive the app")
	}
}
