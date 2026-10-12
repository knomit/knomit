package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// The Windows reload event is keyed by the data root, spelled on every OS.
func TestReloadEventName(t *testing.T) {
	a := ReloadEventName(filepath.Join("x", "home"))
	if !strings.HasPrefix(a, `Local\knomit-reload-`) || len(a) != len(`Local\knomit-reload-`)+16 {
		t.Fatalf("name %q is not Local\\knomit-reload-<16 hex>", a)
	}
	if a != ReloadEventName(filepath.Join("x", "home")+string(filepath.Separator)) {
		t.Fatal("a trailing separator must not change the event")
	}
	if a != ReloadEventName(strings.ToUpper(filepath.Join("x", "home"))) {
		t.Fatal("Windows paths are case-insensitive: the event must not depend on case (same rule as the pipe name)")
	}
	if a == ReloadEventName(filepath.Join("x", "other")) { // positive control: homes differ
		t.Fatal("two data roots share a reload event")
	}
}
