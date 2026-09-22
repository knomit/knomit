package knomitapi

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/config"
)

// SocketPath is where a same-machine server listens. It DELEGATES to
// internal/config rather than resolving the data root itself: `knomit serve`
// opens exactly that path, and a bridge that computed its own would not fail
// loudly — it would find no socket, fall back to TCP, and quietly lose the
// verified identity the socket exists to provide. "" when it cannot be
// resolved, or on windows, which is the same thing to every caller here.
func SocketPath() string {
	p, err := config.SocketPath()
	if err != nil {
		log.Debug().Err(err).Msg("bridge: cannot resolve the knomit socket path; using TCP")
		return ""
	}
	return p
}

// NewHTTPClient prefers the unix socket: it is the credential-free path — the
// kernel tells the server which uid is calling, so nothing is stored or
// presented — and it is what fills client_sessions.principal.
//
// An EXPLICIT base URL wins. "Explicit" means the user pointed at a
// particular server (a CLI argument or KNOMIT_BASE_URL), possibly a remote
// one, and silently rerouting that onto a local socket would answer a
// different question than the one asked. A port DISCOVERED from the lockfile
// is NOT explicit — nobody chose it — so the socket still wins over it.
//
// The request URL keeps its http://host form either way; on the unix path the
// host is ignored, and the dialler is what decides where the bytes go.
func NewHTTPClient(socketPath string, explicitURL bool, timeout time.Duration) *http.Client {
	c := &http.Client{Timeout: timeout}
	if explicitURL || socketPath == "" {
		return c
	}
	// Stat, not just existence: a stale regular file left at this path would
	// otherwise be dialled as a socket and fail EVERY request, where falling
	// back to TCP merely loses the verified identity.
	st, err := os.Stat(socketPath)
	if err != nil || st.Mode()&os.ModeSocket == 0 {
		return c
	}
	d := &net.Dialer{Timeout: timeout}
	c.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return c
}

// Transport names which path NewHTTPClient chose, for one startup log line.
// Without it a bridge that quietly fell back to TCP looks exactly like one
// that used the socket, and the difference is the whole feature.
func Transport(c *http.Client) string {
	if c != nil && c.Transport != nil {
		return "unix"
	}
	return "tcp"
}
