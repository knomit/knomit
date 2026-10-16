//go:build desktop

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// sandboxHome points StateDir (and thus exportsDir) at a temp dir so the test
// never touches the real state dir. Each OS reads a different variable, and
// one left unset writes into the real user's directory: Windows runs wrote
// note.txt and pwned.txt into the real %LOCALAPPDATA%\knomit\exports until
// LOCALAPPDATA was set here too.
func sandboxHome(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)                                 // darwin StateDir
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg")) // linux StateDir
	t.Setenv("LOCALAPPDATA", tmp)                         // windows StateDir
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
	// The Fleet page saves the fleet root key through WriteFile (PR #528 S1):
	// owner-only. SABOTAGE: write 0o644 -> red.
	if fi, err := os.Stat(got); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
}

// Every spelling of "not relative to the exports dir" must be refused, not
// quietly rewritten into something inside the sandbox.
//
// filepath.IsAbs alone is not that check on Windows: it answers FALSE for
// "/etc/passwd", for "\etc\passwd" and for the drive-relative "C:passwd", all
// of which resolve outside the exports dir there. Only the first of these was
// tested, and it was the one that failed on Windows.
func TestWriteFile_RejectsAbsolutePath(t *testing.T) {
	sandboxHome(t)

	// Rejected everywhere.
	names := []string{"/etc/passwd"} // rooted, POSIX spelling

	// The Windows spellings are only non-relative ON Windows. A backslash is
	// an ordinary filename character on macOS and Linux, so `\etc\passwd` and
	// `C:passwd` are legal RELATIVE names there and writeFile is right to
	// accept them — asserting otherwise would fail the existing macOS/Linux
	// desktop job, which is what an unguarded version of this table did.
	if filepath.Separator != '/' {
		names = append(names,
			`\etc\passwd`,       // rooted: relative to the current DRIVE
			`C:\Windows\me.txt`, // fully absolute
			"C:passwd",          // drive-relative: the current dir of drive C
			`\\srv\share\x`,     // UNC: another machine altogether
		)
	}

	for _, name := range names {
		if _, err := writeFile(name, "x"); err == nil {
			t.Errorf("writeFile(%q) was accepted; a non-relative name must be rejected", name)
		}
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

// RevealLogFile must target n.logPath specifically, not some other path
// (e.g. the config file) — that mistake would open the wrong directory in
// the file manager. revealInFileManager itself shells out and is not
// unit-tested; the real seam here is that RevealLogFile passes the right
// argument through to it.
func TestRevealLogFile_UsesTheLogPath(t *testing.T) {
	// revealInFileManager is stubbed below and never touches the filesystem,
	// so the path need not actually exist — only that RevealLogFile forwards
	// exactly this path, not some other one.
	logPath := filepath.Join(t.TempDir(), "knomit-desktop.log")

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

// RestartServer now restarts the SERVER, and like everything that changes what
// the server listens on it is the Settings window's alone (kb invariant
// desktop/fleet/install-surface): the main window and a context with no window
// are refused before anything is stopped.
func TestRestartServer_RestartsTheServerOnlyFromSettings(t *testing.T) {
	calls := 0
	n := newNativeService("", "", &stubToggler{})
	n.restartServer = func(context.Context) error { calls++; return nil }

	for _, ctx := range []context.Context{fromMain, context.Background()} {
		if err := n.RestartServer(ctx); !errors.Is(err, errNotSettingsWindow) {
			t.Fatalf("RestartServer from a non-Settings caller = %v, want errNotSettingsWindow", err)
		}
	}
	if calls != 0 {
		t.Fatalf("a refused RestartServer restarted the server %d time(s)", calls)
	}
	// Positive control: the same call from Settings reaches the supervisor.
	if err := n.RestartServer(fromSettings); err != nil {
		t.Fatalf("RestartServer from Settings: %v", err)
	}
	if calls != 1 {
		t.Fatalf("restartServer called %d times from Settings, want 1", calls)
	}
}

// The supervisor's refusal (attach mode: the server is not ours) reaches the
// caller unchanged, so the Settings window can show "started outside the app".
func TestRestartServer_PassesTheSupervisorsRefusalThrough(t *testing.T) {
	n := newNativeService("", "", &stubToggler{})
	n.restartServer = func(context.Context) error { return errAttachedServer{pid: 4242} }
	err := n.RestartServer(fromSettings)
	var att errAttachedServer
	if !errors.As(err, &att) || att.pid != 4242 || !strings.Contains(err.Error(), "started outside the app (pid 4242)") {
		t.Fatalf("RestartServer = %v, want the attached-server refusal naming pid 4242", err)
	}
}
