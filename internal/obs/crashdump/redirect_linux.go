//go:build linux

package crashdump

import "syscall"

// dupToStderr points fd 2 at fd. Dup3 is available on every Linux arch
// (unlike Dup2, which is absent on arm64).
func dupToStderr(fd int) error {
	return syscall.Dup3(fd, 2, 0)
}
