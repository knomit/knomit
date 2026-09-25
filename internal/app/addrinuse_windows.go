//go:build windows

package app

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isAddrInUse reports whether a listen error is "address already in use".
// Winsock reports it as WSAEADDRINUSE, which is not syscall.EADDRINUSE there.
func isAddrInUse(err error) bool { return errors.Is(err, windows.WSAEADDRINUSE) }
