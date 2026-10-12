//go:build desktop && windows

package main

import "golang.org/x/sys/windows"

// processAlive waits on the process handle with a zero timeout: WAIT_TIMEOUT
// means still running. An open failure means no such process.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}
