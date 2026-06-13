// Package netutil provides small networking helpers for the tray.
package netutil

import (
	"fmt"
	"net"
)

// PreferredPort is the knomit default HTTP port; we try this first so that
// any direct-configured MCP clients keep working without lockfile discovery.
const PreferredPort = 19278

// PickPort returns PreferredPort if it is free on 127.0.0.1, otherwise an
// ephemeral free port assigned by the kernel.
func PickPort() (int, error) {
	if isFree(PreferredPort) {
		return PreferredPort, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("pick free port: %w", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func isFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}
