//go:build !unix && !windows

package cmd

import "errors"

// signalReload: no reload trigger exists on this platform.
func signalReload(home string, pid int) error {
	return errors.New("reloading a running server is not supported on this platform; restart it")
}
