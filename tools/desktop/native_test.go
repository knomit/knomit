//go:build desktop

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandboxHome points StateDir (and thus exportsDir) at a temp dir so the test
// never touches the real user home.
func sandboxHome(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)                                 // darwin StateDir
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg")) // linux StateDir
}

func TestWriteFile_WritesIntoExportsDir(t *testing.T) {
	sandboxHome(t)

	got, err := writeFile("note.txt", "hello")
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	data, err := os.ReadFile(got)
	if err != nil || string(data) != "hello" {
		t.Fatalf("file not written: %v %q", err, data)
	}
	base, _ := exportsDir()
	if !strings.HasPrefix(got, base+string(filepath.Separator)) {
		t.Errorf("wrote outside exports dir: %q not under %q", got, base)
	}
}

func TestWriteFile_RejectsAbsolutePath(t *testing.T) {
	sandboxHome(t)
	if _, err := writeFile("/etc/passwd", "x"); err == nil {
		t.Error("absolute path must be rejected")
	}
}

func TestWriteFile_RejectsEmptyName(t *testing.T) {
	sandboxHome(t)
	if _, err := writeFile("", "x"); err == nil {
		t.Error("empty name must be rejected")
	}
}

// Security regression: a "../" laden name must never write outside the exports
// dir. Traversal is neutralised (contained under base), so the sentinel file at
// the parent location must not exist afterwards.
func TestWriteFile_TraversalStaysSandboxed(t *testing.T) {
	sandboxHome(t)

	base, err := exportsDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := writeFile(strings.Repeat("../", 8)+"pwned.txt", "x")
	if err != nil {
		return // rejected outright is also acceptable
	}
	if !strings.HasPrefix(got, base+string(filepath.Separator)) {
		t.Fatalf("traversal escaped the exports dir: wrote %q, base %q", got, base)
	}
	// The escape target (8 levels above base) must not have been created.
	escape := base
	for i := 0; i < 8; i++ {
		escape = filepath.Dir(escape)
	}
	if _, statErr := os.Stat(filepath.Join(escape, "pwned.txt")); statErr == nil {
		t.Errorf("traversal wrote outside the sandbox at %q", filepath.Join(escape, "pwned.txt"))
	}
}

// RestartApp cannot be exercised end-to-end without actually shelling out
// (relaunch) and quitting the process (onRestart), so this stubs both to
// verify the one thing that is a correctness requirement, not an
// implementation detail: releaseInstance MUST complete before relaunch is
// invoked, and onRestart MUST NOT fire before relaunch succeeds. Getting this
// order wrong is exactly the bug this task exists to avoid — see the
// releaseInstance field comment on NativeService — so it is worth pinning
// with a real seam rather than leaving unverified.
func TestRestartApp_ReleasesInstanceBeforeRelaunchAndQuitsAfter(t *testing.T) {
	var order []string

	n := newNativeService("", "", &stubToggler{})
	n.releaseInstance = func() { order = append(order, "release") }
	n.relaunch = func() error { order = append(order, "relaunch"); return nil }
	n.onRestart = func() { order = append(order, "restart") }

	if err := n.RestartApp(); err != nil {
		t.Fatalf("RestartApp: %v", err)
	}

	want := []string{"release", "relaunch", "restart"}
	if len(order) != len(want) {
		t.Fatalf("got calls %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("got call order %v, want %v", order, want)
		}
	}
}

// A relaunch failure (e.g. a dev build with no bundle to reopen) must not
// quit this instance — quitting into nothing would leave the user with no
// running app at all instead of a clear error.
func TestRestartApp_DoesNotQuitWhenRelaunchFails(t *testing.T) {
	quit := false
	boom := errors.New("boom")

	n := newNativeService("", "", &stubToggler{})
	n.releaseInstance = func() {}
	n.relaunch = func() error { return boom }
	n.onRestart = func() { quit = true }

	err := n.RestartApp()
	if err == nil {
		t.Fatal("expected an error from a failing relaunch")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error does not wrap the relaunch failure: %v", err)
	}
	if quit {
		t.Error("onRestart fired despite relaunch failing")
	}
}

// RevealLogFile must target n.logPath specifically, not some other path
// (e.g. the config file) — that mistake would open the wrong directory in
// the file manager. revealInFileManager itself shells out and is not
// unit-tested; the real seam here is that RevealLogFile passes the right
// argument through to it.
func TestRevealLogFile_UsesTheLogPath(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "knomit-desktop.log")
	if err := os.WriteFile(logPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got string
	n := newNativeService("", logPath, &stubToggler{})
	n.revealInFileManager = func(path string) error { got = path; return nil }

	if err := n.RevealLogFile(); err != nil {
		t.Fatalf("RevealLogFile: %v", err)
	}
	if got != logPath {
		t.Errorf("got path %q, want %q", got, logPath)
	}
}
