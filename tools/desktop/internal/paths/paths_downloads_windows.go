//go:build windows

package paths

import "golang.org/x/sys/windows"

// downloadsDir asks Windows for the Downloads known folder (it can be moved off
// %USERPROFILE%\\Downloads), and falls back to the conventional place.
func downloadsDir() string {
	if p, err := windows.KnownFolderPath(windows.FOLDERID_Downloads, 0); err == nil && p != "" {
		return p
	}
	return homeDownloads()
}
