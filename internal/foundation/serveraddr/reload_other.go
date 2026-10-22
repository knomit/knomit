//go:build !unix && !windows

package serveraddr

import "errors"

// SignalReload: no reload trigger exists on this platform.
func SignalReload(home string, pid int) error {
	return errors.New("reloading a running server is not supported on this platform; restart it")
}
