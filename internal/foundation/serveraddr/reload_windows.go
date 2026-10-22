//go:build windows

package serveraddr

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"

	"knomit/internal/foundation/config"
)

// SignalReload sets the reload event of the server of home. The server's pid
// is not needed to find it (it is for unix); a missing event is "no server".
func SignalReload(home string, pid int) error {
	name, err := windows.UTF16PtrFromString(config.ReloadEventName(home))
	if err != nil {
		return err
	}
	ev, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return missingEventError(pid, processAlive(pid))
		}
		return fmt.Errorf("open the reload event: %w", err)
	}
	defer windows.CloseHandle(ev)
	if err := windows.SetEvent(ev); err != nil {
		return fmt.Errorf("set the reload event: %w", err)
	}
	return nil
}

// processAlive reports whether pid names a running process this user may query.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied still means it exists (another user's process).
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == 259 // STILL_ACTIVE
}
