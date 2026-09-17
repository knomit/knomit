//go:build linux

package crashdump

import (
	"os"
	"syscall"
)

// dupToStderr points fd 2 at f. Dup3 is available on every Linux arch (unlike
// Dup2, which is absent on arm64). os.Stderr needs no reassignment: it wraps
// fd 2, and dup3 replaces what fd 2 refers to.
func dupToStderr(f *os.File) error {
	return syscall.Dup3(int(f.Fd()), 2, 0)
}
