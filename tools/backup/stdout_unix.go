//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// claimProtocolStream moves the protocol pipe off file descriptor 1 and puts
// stderr there instead, returning a handle on the pipe.
//
// Reassigning os.Stdout is not enough on its own. It covers Go code that
// resolves os.Stdout at call time — which, verified across all 588 dependency
// directories, includes modernc.org/libc's C-stdio write() — but it cannot
// cover a package-level variable captured before main runs.
// litestream.LogWriter (litestream.go:104) is exactly that: it holds the real
// fd 1, which is the protocol pipe. Nothing in v0.5.15 writes to it, so it is
// dormant today; an upgrade that starts using it would corrupt the channel with
// no test able to catch it.
//
// So close the class instead of the instance: after this runs, fd 1 IS stderr.
// Anything holding "file descriptor 1", however it obtained it and whenever,
// writes to the log. The protocol lives on a fresh descriptor that nothing else
// has ever seen.
func claimProtocolStream() (*os.File, error) {
	// Duplicate the protocol pipe somewhere out of the way FIRST. After the
	// dup2 below, fd 1 is stderr, so anything still referring to it — including
	// os.Stdout — no longer names the pipe.
	fd, err := syscall.Dup(int(os.Stdout.Fd()))
	if err != nil {
		return nil, fmt.Errorf("duplicating the protocol stream: %w", err)
	}
	// Not inherited by anything this process might exec.
	syscall.CloseOnExec(fd)

	if err := dupOnto(int(os.Stderr.Fd()), int(os.Stdout.Fd())); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("redirecting fd 1 to stderr: %w", err)
	}
	return os.NewFile(uintptr(fd), "knomit-backup-protocol"), nil
}
