package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"knomit/internal/foundation/config"
	"knomit/test/testenv"
)

// TestNew_OriginSessionsHaveTheAppsLifetimeAndHomeDir pins the two things
// app.New gives web's origin-session manager: a sessions dir under the
// instance's home, which the first router build starts sweeping of DEAD runs'
// folders (in the background), and a lifetime that Close ends (which removes
// every session and its folder; api/origins' tests cover that half).
//
// The dead run is the layout api/origins makes (run-<id>.lock beside
// run-<id>/session-*), with its lock file not held, as a crashed process
// leaves it.
//
// SABOTAGE: drop OriginSessionsDir from the web.Server literal -> the dead
// run survives; drop the endLifetime call from Close (or Lifetime from the
// literal) -> the lifetime is still live after Close.
func TestNew_OriginSessionsHaveTheAppsLifetimeAndHomeDir(t *testing.T) {
	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	base := filepath.Join(cfg.Home, "origin-sessions")
	deadRun := filepath.Join(base, "run-crashed")
	if err := os.MkdirAll(filepath.Join(deadRun, "session-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deadRun+".lock", nil, 0o600); err != nil {
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
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, errDir := os.Stat(deadRun)
		_, errLock := os.Stat(deadRun + ".lock")
		if os.IsNotExist(errDir) && os.IsNotExist(errLock) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a dead run in <home>/origin-sessions survived the first router build (dir err = %v, lock err = %v)", errDir, errLock)
		}
		time.Sleep(5 * time.Millisecond)
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
