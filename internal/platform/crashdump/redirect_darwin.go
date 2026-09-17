//go:build darwin

package crashdump

import (
	"os"
	"syscall"
)

// dupToStderr points fd 2 at f. os.Stderr needs no reassignment: it wraps fd
// 2, and dup2 replaces what fd 2 refers to.
func dupToStderr(f *os.File) error {
	return syscall.Dup2(int(f.Fd()), 2)
}
