//go:build !windows

package app

import (
	"errors"
	"syscall"
)

// isAddrInUse reports whether a listen error is "address already in use".
func isAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
