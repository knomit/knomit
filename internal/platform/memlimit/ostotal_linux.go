//go:build linux

package memlimit

import "os"

// osTotal reports physical RAM from /proc/meminfo.
//
// Note this is the HOST's memory even inside a container — /proc/meminfo is not
// namespaced. That is precisely why it is the LAST source detect() consults and
// why a cgroup limit takes precedence over it.
func osTotal() (int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	return parseMemTotal(data)
}
