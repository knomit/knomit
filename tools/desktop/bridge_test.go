//go:build desktop

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinkInto(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "src", "knomit-bridge")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "home", "bin")

	// First install: creates the link pointing at target.
	link, err := linkInto(binDir, target)
	if err != nil {
		t.Fatalf("first linkInto: %v", err)
	}
	if want := filepath.Join(binDir, "knomit-bridge"); link != want {
		t.Errorf("link path = %q, want %q", link, want)
	}
	if got, _ := os.Readlink(link); got != target {
		t.Errorf("link target = %q, want %q", got, target)
	}

	// Idempotent: a second call with the same target leaves the link intact.
	if _, err := linkInto(binDir, target); err != nil {
		t.Fatalf("second linkInto: %v", err)
	}
	if got, _ := os.Readlink(link); got != target {
		t.Errorf("after re-run, link target = %q, want %q", got, target)
	}

	// Refresh: a stale link (app moved/updated) is repointed at the new target.
	newTarget := filepath.Join(dir, "src2", "knomit-bridge")
	if err := os.MkdirAll(filepath.Dir(newTarget), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newTarget, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := linkInto(binDir, newTarget); err != nil {
		t.Fatalf("refresh linkInto: %v", err)
	}
	if got, _ := os.Readlink(link); got != newTarget {
		t.Errorf("after refresh, link target = %q, want %q", got, newTarget)
	}
}

func TestLinkInto_ReplacesRegularFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "knomit-bridge")
	if err := os.WriteFile(target, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A pre-existing regular file at the link path must be replaced by the link.
	if err := os.WriteFile(filepath.Join(binDir, "knomit-bridge"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	link, err := linkInto(binDir, target)
	if err != nil {
		t.Fatalf("linkInto: %v", err)
	}
	if got, _ := os.Readlink(link); got != target {
		t.Errorf("link target = %q, want %q (regular file not replaced)", got, target)
	}
}

// installBridgeSymlink must fail gracefully (not panic) when no knomit-bridge
// sits next to the running executable — the dev `go run` case. The test binary
// has no sibling bridge, so this exercises the skip path.
func TestInstallBridgeSymlink_NoBundledBridge(t *testing.T) {
	if _, err := installBridgeSymlink(t.TempDir()); err == nil {
		t.Error("expected an error when no bundled knomit-bridge is present")
	}
}
