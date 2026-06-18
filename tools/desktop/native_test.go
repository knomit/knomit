//go:build desktop

package main

import (
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
