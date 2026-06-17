//go:build desktop

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"knomit/tools/desktop/internal/paths"
)

// exportsDir is the ONLY directory NativeService is permitted to write into:
// <StateDir>/exports. Confining writes here is what keeps the Wails binding
// from being an arbitrary-file-write primitive reachable from the webview — a
// single UI XSS could otherwise overwrite, say, the user's shell rc and gain
// code execution. The directory is created lazily on first write.
func exportsDir() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "exports"), nil
}

// writeFile writes contents to name inside the exports dir and returns the
// absolute path written. name is treated as a path RELATIVE to the exports dir;
// absolute paths are rejected and any "../" traversal is neutralised so a write
// can never escape the sandbox. Exercised directly by tests.
func writeFile(name, contents string) (string, error) {
	if name == "" {
		return "", errors.New("name must not be empty")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("name must be relative to the exports dir: %q", name)
	}
	base, err := exportsDir()
	if err != nil {
		return "", err
	}
	// Clean("/"+name) collapses any leading "../" against root, so the joined
	// result can never climb above base. The filepath.Rel check below is
	// defense-in-depth, making the containment invariant explicit.
	target := filepath.Join(base, filepath.Clean("/"+name))
	if rel, relErr := filepath.Rel(base, target); relErr != nil ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("name escapes the exports dir: %q", name)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("create exports dir: %w", err)
	}
	if err := os.WriteFile(target, []byte(contents), 0o644); err != nil {
		return "", err
	}
	return target, nil
}

// NativeService is the Wails-bound service exposing native OS actions to the
// React UI. Reachable only via Wails IPC from the embedded window — never over
// the looknomitck API port. One method now to prove the pattern; extend as native
// features land.
type NativeService struct{}

// WriteFile writes contents to a file named within the app's exports directory
// (<StateDir>/exports) and returns the absolute path written. name is confined
// to that directory; absolute paths and "../" traversal are rejected, so the UI
// can never use this to write outside the sandbox.
func (NativeService) WriteFile(name, contents string) (string, error) {
	return writeFile(name, contents)
}
