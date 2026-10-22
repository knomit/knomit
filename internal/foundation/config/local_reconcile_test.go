package config

import (
	"testing"
	"time"
)

// The local-reconcile cadence is a config value, not a constant in the worker:
// how often an origin-less repo republishes its agent branch on main is a
// deployment choice, and 0 turns the worker off entirely.
func TestDefaults_LocalReconcileInterval(t *testing.T) {
	if got := Defaults().Git.LocalReconcileInterval; got != 30*time.Second {
		t.Errorf("default LocalReconcileInterval: want 30s, got %v", got)
	}
}

func TestLoad_LocalReconcileIntervalEnvOverride(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir()) // empty dir → no TOML, defaults + env only
	t.Setenv("KNOMIT_GIT_LOCAL_RECONCILE_INTERVAL", "5s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Git.LocalReconcileInterval; got != 5*time.Second {
		t.Errorf("env override LocalReconcileInterval: want 5s, got %v", got)
	}
}

// 0 must survive the overlay: it is how an operator disables the worker, so it
// cannot be treated as "unset, use the default".
func TestLoad_LocalReconcileIntervalZeroDisables(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir())
	t.Setenv("KNOMIT_GIT_LOCAL_RECONCILE_INTERVAL", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Git.LocalReconcileInterval; got != 0 {
		t.Errorf("env override to 0: want 0, got %v", got)
	}
}
