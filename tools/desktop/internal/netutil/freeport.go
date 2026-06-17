// Package netutil provides small networking helpers for the desktop app.
package netutil

import (
	"fmt"
	"net"
)

// PreferredPort is the knomit default HTTP port; we try this first so that
// any direct-configured MCP clients keep working without lockfile discovery.
const PreferredPort = 19278

// Listen binds a looknomitck TCP listener, preferring PreferredPort and falling
// back to a kernel-assigned ephemeral port if it is already taken. It returns
// the *bound* listener rather than a port number to re-bind: re-binding opens a
// TOCTOU window in which the chosen port could be taken between selection and
// listen, so callers serve on exactly the listener returned here. Read the
// chosen port from ln.Addr().(*net.TCPAddr).Port.
func Listen() (net.Listener, error) {
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", PreferredPort)); err == nil {
		return ln, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("pick free looknomitck port: %w", err)
	}
	return ln, nil
}
