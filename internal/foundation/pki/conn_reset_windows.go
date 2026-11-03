//go:build windows

package pki

import (
	"errors"
	"syscall"
)

// isConnReset reports whether err is the connection reset or aborted under
// it, as Windows reports them: WSAECONNRESET (10054) and WSAECONNABORTED
// (10053). They must be matched by value: errors.Is(err, syscall.ECONNRESET)
// is FALSE for them on Windows, where syscall.ECONNRESET is an invented
// Errno and Errno.Is compares only the oserror sentinels.
func isConnReset(err error) bool {
	var en syscall.Errno
	return errors.As(err, &en) && (en == syscall.WSAECONNRESET || en == syscall.WSAECONNABORTED)
}
