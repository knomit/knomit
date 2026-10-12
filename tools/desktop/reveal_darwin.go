//go:build desktop

package main

import "os/exec"

// revealInFileManager opens the containing directory with the file selected.
func revealInFileManager(path string) error {
	return exec.Command("open", "-R", path).Start()
}
