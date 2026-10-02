package knomitapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/serveraddr"
)

// DefaultServer is the address `kb` uses when nothing names one and no
// desktop lockfile advertises a port.
const DefaultServer = "http://localhost:19278"

// Where a resolved address came from, for logs and error messages.
const (
	SourceArgument = "argument"
	SourceEnv      = serveraddr.EnvVar
	SourceLockfile = "lockfile"
	SourceDefault  = "default"
)

// Server is the ONE address every call `kb` makes goes to: the MCP proxy, the
// agent-branch discovery before it, the session's closing DELETE, and every
// hook.
type Server struct {
	serveraddr.Addr
	// Named is true when the user chose the address (the server argument or
	// KNOMIT_SERVER). A named address is used EXACTLY as given: a named http
	// address is never rerouted onto the local listener, and a named local
	// listener never falls back to TCP. Only an address nobody chose (the
	// lockfile, the default) keeps "try the local listener first".
	Named bool
	// Source is one of the Source* constants.
	Source string
}

// ResolveServer applies the precedence: the command-line argument (arg, ""
// when not given) > KNOMIT_SERVER > the desktop lockfile's port >
// DefaultServer. KNOMIT_HOME is NOT an input: it is the instance's folder,
// and on the default path it only decides where the local listener is.
//
// A malformed argument or KNOMIT_SERVER is an error naming which one — never
// a quiet fall to the next level, which would reach some other server (the
// bug this resolver exists to remove). The environment and the lockfile are
// read on every call, never cached, so nothing freezes ambient state.
func ResolveServer(arg string) (Server, error) {
	if arg != "" {
		a, err := serveraddr.Parse(arg)
		if err != nil {
			return Server{}, fmt.Errorf("the server argument: %w", err)
		}
		return Server{Addr: a, Named: true, Source: SourceArgument}, nil
	}
	if v := os.Getenv(serveraddr.EnvVar); v != "" {
		a, err := serveraddr.Parse(v)
		if err != nil {
			return Server{}, fmt.Errorf("%s: %w", serveraddr.EnvVar, err)
		}
		return Server{Addr: a, Named: true, Source: SourceEnv}, nil
	}
	u, err := readLockfileBaseURL()
	if err != nil {
		log.Debug().Err(err).Msg("lockfile read failed, falling back to the default server")
	}
	if u != "" {
		return Server{Addr: serveraddr.Addr{Raw: u, Base: u}, Source: SourceLockfile}, nil
	}
	return Server{Addr: serveraddr.Addr{Raw: DefaultServer, Base: DefaultServer}, Source: SourceDefault}, nil
}

// NewServerClient is the client for s:
//
//   - a named local listener: that listener ONLY. Missing, dead or foreign,
//     the request fails naming the address; it never reaches TCP.
//   - a named http(s) address: plain TCP (with `kb login`'s bearer token for
//     its host), never the local listener.
//   - an address nobody chose: the local listener first, TCP after — the
//     socket-preferring client, deciding per dial.
//
// timeout 0 means no limit (the proxy holds SSE long-polls open).
func NewServerClient(s Server, timeout time.Duration) *http.Client {
	switch {
	case s.IsLocal():
		return &http.Client{Timeout: timeout, Transport: withBearer(&http.Transport{
			DialContext: localOnlyDialer(s.Addr, timeout),
		})}
	case s.Named:
		return NewHTTPClient("", true, timeout)
	default:
		return NewHTTPClient(SocketPath(), false, timeout)
	}
}

// localOnlyDialer dials the named local listener whatever address the request
// URL carries (the URL's host is serveraddr.LocalBase's placeholder).
func localOnlyDialer(a serveraddr.Addr, timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := auth.DialLocal(ctx, a.Local, localDialBudget(timeout))
		if err != nil {
			return nil, fmt.Errorf("server %s: %w", a.Raw, err)
		}
		return conn, nil
	}
}

// TransportFor names how a client for s connects, for one startup log line.
// For an address nobody chose it is a PREFERENCE (see TransportPreference).
func TransportFor(s Server) string {
	switch {
	case s.IsLocal():
		return string(auth.LocalVia)
	case s.Named:
		return "tcp"
	default:
		return TransportPreference(SocketPath(), false)
	}
}

// readLockfileBaseURL returns http://127.0.0.1:<port> from the desktop's
// lockfile (server.json), or ("", nil) if the file does not exist.
func readLockfileBaseURL() (string, error) {
	path, err := lockfilePath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var info struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return "", fmt.Errorf("parse lockfile %s: %w", path, err)
	}
	if info.Port <= 0 {
		return "", nil
	}
	return fmt.Sprintf("http://127.0.0.1:%d", info.Port), nil
}

// lockfilePath is <state dir>/server.json — the same file the desktop writes.
//
// It delegates rather than re-deriving. The bridge used to carry its own copy
// of the per-OS switch with cases for darwin and linux only, so on Windows it
// returned "unsupported platform windows"; the caller logged that at Debug and
// fell back to the default address, so `kb` silently talked to the wrong port
// while the desktop's lockfile sat in %LOCALAPPDATA%\knomit unread. The bridge
// cannot import tools/desktop/internal/paths (Go's internal rule), which is
// why the copy existed at all — internal/config is the shared owner both of
// them can reach.
func lockfilePath() (string, error) {
	return config.LockfilePath()
}
