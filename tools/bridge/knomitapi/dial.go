package knomitapi

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"sync"
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
// THE CHOICE IS MADE PER DIAL, NOT HERE. That matters: the socket file
// outlives any ungraceful exit, because cmd/serve.go only removes it on a
// graceful one, so a SIGKILL, panic, OOM or power loss leaves a socket inode
// with nothing accepting on it. Deciding once from os.Stat would install a
// socket-only transport and every request would then fail with "connect:
// connection refused" — naming a socket the caller never asked for — where
// before there was a working TCP path. A dead socket must cost the verified
// identity, not all connectivity.
func NewHTTPClient(socketPath string, explicitURL bool, timeout time.Duration) *http.Client {
	if explicitURL {
		return &http.Client{Timeout: timeout}
	}
	return socketPreferringClient(timeout, func() string { return socketPath })
}

// newLazyHooksClient is the hooks' client. Its socket decision is deferred to
// dial time for BOTH halves — whether an explicit URL was named, and where the
// socket is — because this client is reached through a package-level accessor
// and would otherwise freeze its transport at package-init time, before any
// caller (or any test's t.Setenv) can say where the server is.
func newLazyHooksClient(timeout time.Duration) *http.Client {
	return socketPreferringClient(timeout, func() string {
		if os.Getenv("KNOMIT_BASE_URL") != "" {
			return "" // the operator named a server; do not reroute it
		}
		return SocketPath()
	})
}

// socketPreferringClient dials the socket first and falls back to the address
// the request actually asked for. socketPath is consulted on EVERY dial, and
// "" means "do not try the socket at all".
func socketPreferringClient(timeout time.Duration, socketPath func() string) *http.Client {
	d := &net.Dialer{Timeout: timeout}
	var warnOnce sync.Once
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				p := socketPath()
				if p == "" {
					return d.DialContext(ctx, network, addr)
				}
				conn, err := d.DialContext(ctx, "unix", p)
				if err == nil {
					return conn, nil
				}
				// NO SOCKET AT ALL is the ordinary case — no server running,
				// or one older than the socket — and warning about it would
				// dilute the signal this log line exists for. The anomaly
				// worth a WARN is a socket that EXISTS and does not answer,
				// which is what an ungracefully killed server leaves behind.
				if errors.Is(err, fs.ErrNotExist) {
					log.Debug().Str("socket", p).Msg("bridge: no unix socket, using TCP")
					return d.DialContext(ctx, network, addr)
				}
				// Once, not per dial: a stale socket would otherwise repeat
				// this on every connection. Silence here is the failure mode
				// worth avoiding — a dead socket beside a live server looks
				// exactly like a healthy bridge until someone reads this.
				warnOnce.Do(func() {
					log.Warn().Err(err).Str("socket", p).Str("addr", addr).
						Msg("bridge: unix socket unreachable, falling back to TCP; the server's verified identity is not available on this path")
				})
				return d.DialContext(ctx, network, addr)
			},
		},
	}
}

// TransportPreference names which path a client will TRY first, for one
// startup log line. It is a preference and not a fact: the dial decides, and
// a socket that does not answer falls back to TCP with its own warning. Do
// not log this as though a connection had already been made on it.
func TransportPreference(socketPath string, explicitURL bool) string {
	if explicitURL || socketPath == "" {
		return "tcp"
	}
	return "unix-preferred"
}
