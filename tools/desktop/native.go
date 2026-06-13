//go:build desktop

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeFile is the native file-write primitive, exercised directly by tests.
// It runs in-process with full OS access; absolute paths only.
func writeFile(path, contents string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path must be absolute: %q", path)
	}
	return os.WriteFile(path, []byte(contents), 0o644)
}

// NativeService is the Wails-bound service exposing native OS actions to the
// React UI. Reachable only via Wails IPC from the embedded window — never over
// the looknomitck API port. One method now to prove the pattern; extend as native
// features land.
type NativeService struct{}

// WriteFile writes contents to an absolute path on the local disk.
func (NativeService) WriteFile(path, contents string) error { return writeFile(path, contents) }
