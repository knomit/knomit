//go:build desktop && windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// These pin the premise of attachParentConsole's per-stream guard: writable()
// must say NO for the handle a console-less GUI process is handed, and YES for
// a stream the shell actually redirected. Get that backwards and the bug is
// silent either way — a false NO sends `--version > out.txt` to a console
// nobody is watching, a false YES prints the version nowhere at all.
//
// Hermetic on purpose: no AttachConsole, no CONOUT$, no child processes. Those
// need a real parent console and would make this untrustworthy under CI, where
// there is none. What is left is the one decision the guard actually makes.
//
// NOTE ON COVERAGE: until the Windows compile gate in tests.yml, nothing in CI
// compiled this file at all — the `desktop` job is macos-latest. That gate is
// build+vet only, so this test still runs only on a developer's Windows box.
// That is the intended arrangement, not an oversight: it is cheap to run by
// hand and the file it guards cannot even be compiled on the platform that
// would run the test suite.

// TestWritableRejectsTheNullHandle pins the broken case. Handle 0 is what
// GetStdHandle(STD_OUTPUT_HANDLE) actually returns in a -H windowsgui process
// launched with no redirection (measured), and it is the ONLY case
// attachParentConsole is allowed to repair.
func TestWritableRejectsTheNullHandle(t *testing.T) {
	// Not Closed: os.NewFile(0, ...) wraps an invalid handle, so there is
	// nothing to close and Stat is the whole point.
	f := os.NewFile(0, "null-handle")
	if f == nil {
		t.Fatal("os.NewFile(0) returned nil; the test cannot pin what it means to")
	}
	if writable(f) {
		t.Error("writable(handle 0) = true, want false: the guard would skip the " +
			"one case it exists to repair, and --version would print nowhere")
	}
}

// TestWritableAcceptsRedirectedStreams pins the two cases that must be LEFT
// ALONE. A GUI-subsystem process does inherit a working stdout when the shell
// redirects one — file (FILE_TYPE_DISK) and pipe (FILE_TYPE_PIPE) — and
// repointing either at CONOUT$ would discard output the caller asked for.
func TestWritableAcceptsRedirectedStreams(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		f, err := os.Create(filepath.Join(t.TempDir(), "out.txt"))
		if err != nil {
			t.Fatalf("create temp file: %v", err)
		}
		defer f.Close()
		if !writable(f) {
			t.Error("writable(regular file) = false, want true: a `> out.txt` " +
				"redirect would be hijacked to the console")
		}
	})

	t.Run("pipe", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		defer r.Close()
		defer w.Close()
		if !writable(w) {
			t.Error("writable(pipe) = false, want true: a `| something` " +
				"pipeline would be hijacked to the console")
		}
	})
}
