//go:build darwin

package memlimit

import "golang.org/x/sys/unix"

// osTotal reports physical RAM from the hw.memsize sysctl. macOS has no
// cgroups, so on a bare Mac this is the only source; Docker Desktop runs a
// Linux VM, so containers there take the Linux path instead.
func osTotal() (int64, error) {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, err
	}
	return int64(n), nil
}
