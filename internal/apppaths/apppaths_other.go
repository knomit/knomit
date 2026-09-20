//go:build !darwin && !linux && !windows

package apppaths

import (
	"fmt"
	"runtime"
)

// stateDir has no answer on a platform the desktop app does not target.
//
// DefaultHome deliberately still works here (see apppaths_unix.go): `knomit
// serve` and `kb` run anywhere Go does, and only the desktop's lockfile, logs
// and updater state need a designated per-OS state directory.
func stateDir() (string, error) {
	return "", fmt.Errorf("apppaths: no state directory defined for %s", runtime.GOOS)
}
