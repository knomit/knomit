// Package singleinstance enforces one running knomit-desktop per data root
// with an OS lock that dies with the process: flock(2) on a desktop.lock file
// on unix, a named mutex on Windows.
//
// It used to read the PID in server.json and probe it. That file is the
// SERVER's now (every `knomit serve` writes its own home's), so a PID found
// there says nothing about a tray, and a PID probe could not tell a recycled
// PID from a live desktop. An OS lock has neither problem: the kernel releases
// it when the holder exits, however it exits, and nothing reads a stale value.
package singleinstance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
)

// ErrAlreadyRunning is returned by Acquire when another live process holds the
// lock.
var ErrAlreadyRunning = errors.New("another knomit-desktop is already running")

// Lock is a held instance lock. The OS releases it when the process ends;
// Release frees it earlier and is idempotent.
type Lock struct {
	once    sync.Once
	release func() error
	err     error
}

// Release frees the lock. Safe to call more than once and from any goroutine.
func (l *Lock) Release() error {
	l.once.Do(func() { l.err = l.release() })
	return l.err
}

// Acquire takes the instance lock named by path, or returns ErrAlreadyRunning
// when another live process holds it. On unix path is the lock FILE (created
// 0600 if missing, its directory with it; the file is never removed, because
// unlinking a file someone else may be about to flock is a race). On Windows
// the lock is the session-local mutex MutexName(path) and no file is written.
//
// The handle is not inherited by children (Go opens files close-on-exec and
// creates the mutex non-inheritable), so the server the desktop spawns can
// never keep the desktop's lock alive after the desktop has gone.
func Acquire(path string) (*Lock, error) {
	release, err := acquire(path)
	if err != nil {
		return nil, err
	}
	return &Lock{release: release}, nil
}

// MutexName is the Windows mutex that stands for path: `Local\knomit-desktop-`
// and a hash of the cleaned, lower-cased path (Windows paths are
// case-insensitive). Local\ because a Global\ object needs
// SeCreateGlobalPrivilege and an instance is per user session anyway; the hash
// of the path, which the desktop derives from the data root, is what lets a
// desktop on a scratch KNOMIT_HOME run beside the user's (kb gotcha
// desktop/rebuilding-kills-the-running-server). Spelled on every OS so it is
// testable off Windows.
func MutexName(path string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(path))))
	return `Local\knomit-desktop-` + hex.EncodeToString(sum[:8])
}
