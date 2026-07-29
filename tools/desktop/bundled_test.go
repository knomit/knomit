//go:build desktop

package main

import (
	"os"
	"path/filepath"
	"strings"
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

// The installers must fail gracefully (not panic) when the tool does not sit
// next to the running executable — the dev `go run` case. The test binary has
// no siblings, so this exercises the skip path for both.
func TestInstallTool_NoBundledTool(t *testing.T) {
	if _, err := installBridgeTool(t.TempDir()); err == nil {
		t.Error("expected an error when no bundled knomit-bridge is present")
	}
	if _, err := installOKFTool(t.TempDir()); err == nil {
		t.Error("expected an error when no bundled knomit-okf is present")
	}
}

// TestInstallSymlink_EachInstallerResolvesItsOwnBinary pins that the two
// installers ask for DISTINCT binaries. They now share one code path, which is
// exactly what makes it possible to pass the same exec name twice and hand a
// user a "knomit-okf" that is really the MCP bridge.
//
// Both resolve relative to os.Executable(), which a test cannot redirect, so
// both necessarily fail here — but the error names the binary each one looked
// for, and that is the wiring under test.
func TestInstallTool_EachInstallerResolvesItsOwnBinary(t *testing.T) {
	_, bridgeErr := installBridgeTool(t.TempDir())
	_, okfErr := installOKFTool(t.TempDir())
	if bridgeErr == nil || okfErr == nil {
		t.Fatal("expected both installers to fail with no bundled tools present")
	}
	if !strings.Contains(bridgeErr.Error(), bridgeExecName) {
		t.Errorf("bridge installer looked for the wrong binary: %v", bridgeErr)
	}
	if !strings.Contains(okfErr.Error(), okfExecName) {
		t.Errorf("okf installer looked for the wrong binary: %v", okfErr)
	}
	if strings.Contains(okfErr.Error(), bridgeExecName) {
		t.Errorf("okf installer resolved the BRIDGE binary — the tools are crossed: %v", okfErr)
	}
}

func TestCopyIntoReplacesStaleContent(t *testing.T) {
	// On Linux the bundled tools are COPIED, not symlinked: inside an AppImage
	// the source lives in a FUSE mount that vanishes when the app exits, so a
	// symlink would dangle and every MCP client wired to <home>/bin would break.
	dir := t.TempDir()
	target := filepath.Join(dir, "src", "knomit-bridge")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "home", "bin")

	got, err := copyInto(binDir, target)
	if err != nil {
		t.Fatalf("copyInto: %v", err)
	}
	if want := filepath.Join(binDir, "knomit-bridge"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}

	// It must be a real file, not a link into the (future) mount.
	fi, err := os.Lstat(got)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("copyInto produced a symlink; the AppImage mount it points into will vanish")
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("copied tool is not executable: mode %v", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(got); string(b) != "v1" {
		t.Errorf("content = %q, want v1", b)
	}

	// An update replaces the source; the installed copy must follow.
	if err := os.WriteFile(target, []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := copyInto(binDir, target); err != nil {
		t.Fatalf("second copyInto: %v", err)
	}
	if b, _ := os.ReadFile(got); string(b) != "v2" {
		t.Errorf("content after refresh = %q, want v2", b)
	}

	// No staging temp files left behind.
	entries, err := os.ReadDir(binDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("bin dir holds %v, want just the installed tool", names)
	}
}

func TestCopyIntoReplacesAnExistingSymlink(t *testing.T) {
	// Upgrading from a tarball install leaves a symlink behind at the
	// destination. It must be replaced by a real file, not written through.
	dir := t.TempDir()
	target := filepath.Join(dir, "src", "knomit-bridge")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "home", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "gone", "knomit-bridge")
	if err := os.Symlink(stale, filepath.Join(binDir, "knomit-bridge")); err != nil {
		t.Fatal(err)
	}

	got, err := copyInto(binDir, target)
	if err != nil {
		t.Fatalf("copyInto over a stale symlink: %v", err)
	}
	fi, err := os.Lstat(got)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("stale symlink survived; want a real file")
	}
	if b, _ := os.ReadFile(got); string(b) != "new" {
		t.Errorf("content = %q, want new", b)
	}
	// The dangling link's target must not have been created by writing through it.
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("wrote through the stale symlink instead of replacing it")
	}
}

// The copied tool must keep working after the source disappears — that is the
// entire point on Linux, where the source lives in a FUSE mount torn down when
// the app exits.
func TestCopyIntoSurvivesSourceRemoval(t *testing.T) {
	dir := t.TempDir()
	mount := filepath.Join(dir, "mnt", "usr", "bin")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(mount, "knomit-bridge")
	if err := os.WriteFile(target, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "home", "bin")

	got, err := copyInto(binDir, target)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the AppImage unmounting on app exit.
	if err := os.RemoveAll(filepath.Join(dir, "mnt")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("installed tool unreadable after the source vanished: %v", err)
	}
	if string(b) != "bin" {
		t.Errorf("content = %q, want bin", b)
	}
}

// The per-platform dispatch itself, exercised for both branches regardless of
// the host running the tests. On a macOS CI runner the linux branch would
// otherwise never execute, and it is the branch the AppImage depends on.
func TestPlaceToolDispatchesPerPlatform(t *testing.T) {
	tests := []struct {
		goos        string
		wantSymlink bool
	}{
		{"linux", false},  // AppImage: the source mount vanishes, so copy
		{"darwin", true},  // .app at a stable path, updated in place
		{"windows", true}, // not a release target; falls back to the default
	}

	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "src", "knomit-bridge")
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte("bin"), 0o755); err != nil {
				t.Fatal(err)
			}

			got, err := placeTool(tt.goos, filepath.Join(dir, "bin"), target)
			if err != nil {
				t.Fatalf("placeTool(%s): %v", tt.goos, err)
			}
			fi, err := os.Lstat(got)
			if err != nil {
				t.Fatal(err)
			}
			isSymlink := fi.Mode()&os.ModeSymlink != 0
			if isSymlink != tt.wantSymlink {
				t.Errorf("placeTool(%s) produced symlink=%v, want %v", tt.goos, isSymlink, tt.wantSymlink)
			}
		})
	}
}
