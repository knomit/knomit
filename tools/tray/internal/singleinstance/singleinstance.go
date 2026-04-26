// Package singleinstance enforces one running tray per user via the lockfile.
package singleinstance

import (
	"errors"
	"os"
	"syscall"

	"knomit/tools/tray/internal/lockfile"
)

var ErrAlreadyRunning = errors.New("another knomit-tray is already running")

// Acquire returns nil if the process should proceed (no lockfile, or the
// recorded PID is dead). Returns ErrAlreadyRunning if the PID is alive.
func Acquire(path string) error {
	info, err := lockfile.Read(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.PID <= 0 {
		return nil
	}
	if isAlive(info.PID) {
		return ErrAlreadyRunning
	}
	// Stale lockfile; caller will overwrite it.
	return nil
}

// isAlive reports whether the process is running. On Unix, FindProcess never
// errors, so we send signal 0 to probe.
func isAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil
}
