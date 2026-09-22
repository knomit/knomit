//go:build windows

package auth

import (
	"errors"
	"os"
)

// lockExclusive is unreachable on windows: ListenLocal returns before it,
// because there is no unix socket to guard in this phase (knomit/knomit#245).
func lockExclusive(*os.File) error {
	return errors.New("socket lock: not supported on windows (knomit/knomit#245)")
}
