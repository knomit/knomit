//go:build windows

package singleinstance

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// acquire creates the named mutex MutexName(path). CreateMutex succeeds on an
// existing mutex too and reports ERROR_ALREADY_EXISTS, which is how a second
// instance (in this process or another) is detected; the handle the kernel
// keeps for the first holder is released when that process exits.
//
// The mutex is never OWNED (initial owner false): ownership is thread-affine,
// and the lock here is the object's existence, not a wait on it.
func acquire(path string) (func() error, error) {
	name, err := windows.UTF16PtrFromString(MutexName(path))
	if err != nil {
		return nil, fmt.Errorf("mutex name: %w", err)
	}
	h, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
		return nil, ErrAlreadyRunning
	}
	if err != nil {
		// ACCESS_DENIED: a mutex of that name exists and is not ours to open,
		// which still means it is held.
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("create instance mutex: %w", err)
	}
	return func() error { return windows.CloseHandle(h) }, nil
}
