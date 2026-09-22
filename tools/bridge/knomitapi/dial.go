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

	"knomit/internal/auth"
	"knomit/internal/config"
)

// SocketPath is where a same-machine server listens: a unix socket, or a
// named pipe on Windows. It DELEGATES to internal/config rather than
// resolving the data root itself: `knomit serve` opens exactly that path, and
// a bridge that computed its own would not fail loudly — it would find no
// listener, fall back to TCP, and quietly lose the verified identity the
// listener exists to provide. "" when it cannot be resolved.
//
// It used to be "" on Windows as well, because phase 1 had no credential
// there. It is not any more (knomit#245); a caller still treating "" as "this
// is Windows" would simply never try the pipe.
func SocketPath() string {
	p, err := config.SocketPath()
	if err != nil {
		log.Debug().Err(err).Msg("bridge: cannot resolve the knomit local listener path; using TCP")
		return ""
	}
	return p
}

// NewHTTPClient prefers the local listener: it is the credential-free path —
// the OS tells the server who is calling, so nothing is stored or presented —
// and it is what fills client_sessions.principal.
//
// An EXPLICIT base URL wins. "Explicit" means the user pointed at a
// particular server (a CLI argument or KNOMIT_BASE_URL), possibly a remote
// one, and silently rerouting that onto a local transport would answer a
// different question than the one asked. A port DISCOVERED from the lockfile
// is NOT explicit — nobody chose it — so the local listener still wins over
// it.
//
// THE CHOICE IS MADE PER DIAL, NOT HERE. That matters: the socket file
// outlives any ungraceful exit, because cmd/serve.go only removes it on a
// graceful one, so a SIGKILL, panic, OOM or power loss leaves a socket inode
// with nothing accepting on it. Deciding once from os.Stat would install a
// socket-only transport and every request would then fail with "connect:
// connection refused" — naming a socket the caller never asked for — where
// before there was a working TCP path. A dead listener must cost the verified
// identity, not all connectivity.
//
// A Windows pipe cannot go stale the same way — the kernel reaps it with the
// server — but it can be there and refuse us, which lands in the same branch
// for the same reason.
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

// socketPreferringClient dials the local listener first and falls back to the
// address the request actually asked for. socketPath is consulted on EVERY
// dial, and "" means "do not try it at all".
//
// auth.DialLocal is the other half of auth.ListenLocal, which is what
// `knomit serve` opens: keeping both in one package is what stops the client
// and the server disagreeing about what the transport is. On Windows it also
// carries the impersonation level the server needs to read a SID at all — a
// plain winio.DialPipeContext would connect and then be anonymous.
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
				conn, err := auth.DialLocal(ctx, p, timeout)
				if err == nil {
					return conn, nil
				}
				// NO LISTENER AT ALL is the ordinary case — no server
				// running, or one older than the socket — and warning about
				// it would dilute the signal this log line exists for. The
				// anomaly worth a WARN is a listener that EXISTS and does not
				// answer: a socket inode left by an ungracefully killed
				// server, or a pipe that refuses us.
				//
				// errors.Is, not string matching, on both platforms: Windows
				// returns ERROR_FILE_NOT_FOUND inside an *os.PathError, and
				// syscall.Errno.Is maps that onto fs.ErrNotExist, so the one
				// branch covers ENOENT and 0x2 alike.
				if errors.Is(err, fs.ErrNotExist) {
					log.Debug().Str("socket", p).Str("via", string(auth.LocalVia)).
						Msg("bridge: no local listener, using TCP")
					return d.DialContext(ctx, network, addr)
				}
				// Once, not per dial: a stale listener would otherwise repeat
				// this on every connection. Silence here is the failure mode
				// worth avoiding — a dead socket beside a live server looks
				// exactly like a healthy bridge until someone reads this.
				warnOnce.Do(func() {
					log.Warn().Err(err).Str("socket", p).Str("via", string(auth.LocalVia)).Str("addr", addr).
						Msg("bridge: local listener unreachable, falling back to TCP; the server's verified identity is not available on this path")
				})
				return d.DialContext(ctx, network, addr)
			},
		},
	}
}

// TransportPreference names which path a client will TRY first, for one
// startup log line. It is a preference and not a fact: the dial decides, and
// a listener that does not answer falls back to TCP with its own warning. Do
// not log this as though a connection had already been made on it.
//
// The local value names the MECHANISM (socket-preferred, pipe-preferred), not
// the address family, because that is what a reader needs in order to tell
// which credential a session will carry — the same distinction auth.ViaPipe
// keeps in the grants key.
func TransportPreference(socketPath string, explicitURL bool) string {
	if explicitURL || socketPath == "" {
		return "tcp"
	}
	return string(auth.LocalVia) + "-preferred"
}
