//go:build darwin

package crashdump

import "syscall"

// dupToStderr points fd 2 at fd.
func dupToStderr(fd int) error {
	return syscall.Dup2(fd, 2)
}
