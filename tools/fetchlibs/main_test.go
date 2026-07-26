package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failingReader yields some bytes and then fails, standing in for a dropped
// connection or a full disk part-way through a library download.
type failingReader struct {
	head string
	done bool
}

func (f *failingReader) Read(b []byte) (int, error) {
	if f.done {
		return 0, errors.New("connection reset")
	}
	f.done = true
	return copy(b, f.head), nil
}

// TestWriteFile_FailedCopyLeavesNoDestination is the regression test for the
// fetchlibs half of P0.6. fetch() is skip-if-present, so a partial write at the
// FINAL path is permanent: every later run reports "already present, skipping"
// and the truncated library surfaces much later as a link or dlopen failure.
//
// A failed write must therefore leave the destination absent, not truncated.
func TestWriteFile_FailedCopyLeavesNoDestination(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "libonnxruntime.so")

	err := writeFile(dst, &failingReader{head: "ELF-header-then-nothing"})
	if err == nil {
		t.Fatal("expected the copy failure to propagate")
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("destination must not exist after a failed write, got %v", statErr)
	}

	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".part-") {
			t.Errorf("temp file %q was left behind", e.Name())
		}
	}
}

// TestWriteFile_SucceedsAndIsExecutable: the happy path must produce the full
// content at the final path with the executable bit set — these are shared
// libraries that get dlopen'd, and os.CreateTemp's 0600 would not do.
func TestWriteFile_SucceedsAndIsExecutable(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "nested", "libexample.dylib")
	payload := strings.Repeat("mach-o", 100)

	if err := writeFile(dst, io.NopCloser(strings.NewReader(payload))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Errorf("content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("library must be executable, got mode %v", info.Mode().Perm())
	}
}
