//go:build desktop

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"knomit/internal/platform/privdir"
)

// freshInstallConfig is the whole knomit.toml a fresh desktop install starts
// with. The app talks to its server over the local socket (or pipe), so the
// plaintext TCP port is off until the user turns it on in Settings: a new
// install opens no network port it was not asked to. An UPGRADE keeps the
// port it always had (the default is `http = true`), so MCP-over-HTTP clients
// and browser tabs keep working.
const freshInstallConfig = `# Written by Knomit on first launch. The app talks to its server over a local
# socket, so the HTTP port is off until you turn it on in Settings.
http = false
`

// isFreshInstall reports whether home holds neither a knomit.toml nor a
// control.db: nothing has ever been configured or served from it. Anything
// else (including an unreadable home) is not fresh, so the default is never
// written over somebody's data root.
func isFreshInstall(home string) bool {
	for _, name := range []string{"knomit.toml", "control.db"} {
		if _, err := os.Stat(filepath.Join(home, name)); !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return true
}

// ensureFreshInstallDefaults writes freshInstallConfig into a fresh home,
// before the first server is spawned, and reports whether it did. The file is
// created exclusively, so a server or a person that wrote one first wins.
// The home directory is made private first, as the server would on boot.
func ensureFreshInstallDefaults(home string) (bool, error) {
	if !isFreshInstall(home) {
		return false, nil
	}
	if err := privdir.Ensure(home); err != nil {
		return false, fmt.Errorf("prepare %s: %w", home, err)
	}
	f, err := os.OpenFile(filepath.Join(home, "knomit.toml"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, configFileMode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	if _, err := f.WriteString(freshInstallConfig); err != nil {
		f.Close()
		return false, err
	}
	return true, f.Close()
}
