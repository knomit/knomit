// Package netutil provides small networking helpers for the desktop app.
package netutil

import (
	"fmt"
	"net"
	"strconv"

	"github.com/rs/zerolog/log"
)

// PreferredPort is the knomit default HTTP port; we try this first so that
// any direct-configured MCP clients keep working without lockfile discovery.
const PreferredPort = 19278

// Listen binds a loopback TCP listener, preferring the given port and falling
// back to a kernel-assigned ephemeral port if that port is already taken. It
// returns the *bound* listener rather than a port number to re-bind:
// re-binding opens a TOCTOU window in which the chosen port could be taken
// between selection and listen, so callers serve on exactly the listener
// returned here. Read the chosen port from ln.Addr().(*net.TCPAddr).Port.
//
// preferred is config.Config.Port, which is a STRING and defaults to "19278".
// An empty or unparseable value falls back to PreferredPort rather than
// failing: a typo in knomit.toml should not stop the app from starting.
//
// A preferred port that is already taken falls back to an ephemeral one. That
// keeps the app usable when something else holds the port, at the cost of the
// configured and effective ports differing — which is exactly why the
// settings dialog shows both. The fallback is logged at warn level, because a
// silent one is how someone spends an hour wondering why their MCP client
// can't connect on the port they configured.
func Listen(preferred string) (net.Listener, error) {
	port := PreferredPort
	if preferred != "" {
		if p, err := strconv.Atoi(preferred); err == nil && p > 0 && p <= 65535 {
			port = p
		} else {
			log.Warn().Str("configured_port", preferred).Int("fallback_port", PreferredPort).
				Msg("netutil: configured port is not valid; falling back to the default port")
		}
	}
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
		return ln, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("pick free loopback port: %w", err)
	}
	log.Warn().Int("configured_port", port).Int("effective_port", ln.Addr().(*net.TCPAddr).Port).
		Msg("netutil: configured port is already in use; falling back to an ephemeral port")
	return ln, nil
}
