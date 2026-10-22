// Package modroot locates the repository's module root for tests that scan
// source files across packages.
//
// A guard that reads another package's files by a relative path ("../store",
// "../../web") breaks silently or loudly every time a directory moves, because
// the depth and the sibling names both change. Resolving from the module root
// ("internal/engine/store/...") makes the path a property of the file, not of
// where the test happens to live.
//
// Test-only: it imports "testing", and TestHarnessNotLinkedIntoShippedBinaries
// keeps it out of every shipped binary.
package modroot

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Dir returns the absolute path of the directory holding go.mod, found by
// walking up from the test's working directory (the package directory under
// `go test`). It fails the test when there is no go.mod above: a scan that
// cannot find its tree must not pass.
func Dir(t testing.TB) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("modroot: getwd: %v", err)
	}
	root, err := Find(wd)
	if err != nil {
		t.Fatalf("modroot: %v — the cross-package scans cannot run and would not be enforced", err)
	}
	return root
}

// Find walks up from dir to the nearest directory holding go.mod.
func Find(dir string) (string, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		d = parent
	}
}

// Path joins elements (slash-separated, relative to the module root) onto the
// module root.
func Path(t testing.TB, elem ...string) string {
	t.Helper()
	parts := append([]string{Dir(t)}, elem...)
	return filepath.Join(parts...)
}
