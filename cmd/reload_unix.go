//go:build unix

package cmd

import (
	"errors"
	"fmt"
	"syscall"
)

// signalReload sends SIGHUP to the server whose server.json named pid. home
// is the Windows event's key and unused here.
func signalReload(home string, pid int) error {
	if pid <= 1 {
		return fmt.Errorf("server.json names pid %d, which is not a server", pid)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("pid %d from server.json is not running (a stale file; start the server)", pid)
		}
		if !errors.Is(err, syscall.EPERM) {
			return err
		}
		// EPERM: it exists but is another user's: Kill below fails the same way.
	}
	if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	return nil
}
