//go:build !darwin && !linux && !windows

package userdirs

import (
	"fmt"
	"runtime"
)

// stateDir has no answer on a platform with no designated per-user state
// directory. An error, not a guess: see the package doc.
//
// HomeDir still works here, which is the useful half for a plain server
// process; only callers wanting a designated state directory are stopped.
func stateDir() (string, error) {
	return "", fmt.Errorf("userdirs: no per-user state directory is defined for %s", runtime.GOOS)
}
