//go:build desktop && !darwin && !windows

package main

import (
	"os/exec"
	"path/filepath"
)

// revealInFileManager opens the log file's directory with the desktop's
// default handler.
//
// The directory, not the file: there is no portable "select this file" on the
// freedesktop side. macOS and Windows both have one and use it.
//
// Named _others rather than _linux so every non-darwin, non-windows build
// still has a definition — the filename suffix would otherwise restrict this
// to GOOS=linux alone, which is narrower than the restart_others.go it was
// split out of.
func revealInFileManager(path string) error {
	return exec.Command("xdg-open", filepath.Dir(path)).Start()
}
