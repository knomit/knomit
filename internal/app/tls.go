package app

import (
	"context"
	"errors"
	"fmt"
	stdlog "log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/pki"
)

// OpenTLSServer opens the mTLS listener for enrolled instances (F19 phase 2)
// when [tls].addr is set AND a certificate is installed in [tls].dir. Both
// server binaries call it — `knomit serve` and the desktop app (knomit#256) —
// so there is ONE TLS server to maintain: the #258 re-judging below was added
// after this first landed, and a second copy would have missed it.
//
// It returns a SEPARATE http.Server sharing like's Handler and BaseContext,
// whose ConnContext is auth.TLSConnContext: that server's mark
// on every connection is how AuthMiddleware knows a request came in on the
// TLS listener (never anonymous, reads included). The plaintext server is
// untouched — nothing about today's port changes. The caller serves the
// listener and shuts the server down beside the plaintext one.
//
// (nil, nil, nil) means the listener is off: no addr, or an addr with no
// certificate yet (logged at WARN, not fatal, so configuring [tls] before
// `knomit identity install` is harmless). A certificate that IS installed
// but cannot be loaded — CRL missing or malformed, a CRL older than
// crl.number, a certificate for another key — is an error: fail closed.
//
// Established connections are re-judged (knomit#258): the http.Server's
// ConnState feeds the pki.Server's registry, and pki.Server.Run is started
// here on ctx, so it stops when the caller's context does. Without both, a
// revoked peer keeps a kept-alive connection for as long as it keeps talking.
//
// The TIMEOUTS ARE ITS OWN (tlsReadHeaderTimeout and the rest), not like's.
// This is a network-facing port, and the two binaries' plaintext servers
// differ: the desktop's has no ReadTimeout at all. Copying them would give
// the desktop's TLS port no body-read deadline, and two TLS servers that
// differ wherever their plaintext servers do.
//
// A listen failure because the address is already in use is ErrTLSAddrInUse
// (wrapped): an AVAILABILITY failure, which a caller may choose to survive —
// the desktop warns and serves without TLS; `knomit serve` stays fatal.
// Every other error is a TRUST or configuration failure and fails closed.
func OpenTLSServer(ctx context.Context, tcfg config.TLSConfig, keyPath string, like *http.Server) (*http.Server, net.Listener, error) {
	if tcfg.Addr == "" {
		return nil, nil, nil
	}
	if !pki.HasInstanceCert(tcfg.Dir) {
		log.Warn().Str("dir", tcfg.Dir).Str("addr", tcfg.Addr).
			Msg("tls listener configured but no instance certificate installed; run `knomit identity install`; serving plaintext only")
		return nil, nil, nil
	}
	ps, err := pki.NewServer(tcfg.Dir, keyPath, logTLSReason, tlsRecheckInterval)
	if err != nil {
		return nil, nil, fmt.Errorf("tls listener on %s: %w", tcfg.Addr, err)
	}
	ln, _, err := auth.ListenTLS(tcfg.Addr, ps.TLSConfig())
	if err != nil {
		if isAddrInUse(err) {
			return nil, nil, fmt.Errorf("%w: %w", ErrTLSAddrInUse, err)
		}
		return nil, nil, err
	}
	go ps.Run(ctx)
	srv := &http.Server{
		Handler:           like.Handler,
		ReadHeaderTimeout: tlsReadHeaderTimeout,
		ReadTimeout:       tlsReadTimeout,
		WriteTimeout:      0, // 0 = no limit, required for SSE long-poll streams
		IdleTimeout:       tlsIdleTimeout,
		BaseContext:       like.BaseContext,
		ConnContext:       auth.TLSConnContext,
		ConnState:         ps.ConnState,
		// net/http logs failed handshakes ("TLS handshake error …") to
		// ErrorLog; route them into zerolog so they are not lost. The named
		// refusal reason arrives separately through logTLSReason.
		ErrorLog: stdlog.New(WarnWriter{}, "", 0),
	}
	return srv, ln, nil
}

// ErrTLSAddrInUse is OpenTLSServer's error when [tls].addr is already bound
// by another process — normally a `knomit serve` beside the desktop on the
// same home. See OpenTLSServer for why it is told apart from the rest.
var ErrTLSAddrInUse = errors.New("tls listener address already in use")

// The TLS listener's own timeouts: the values `knomit serve` has always
// served it with (cmd/serve.go's plaintext server), now fixed here so the
// desktop's TLS port gets the same ones. ReadHeaderTimeout bounds the
// handshake and headers; ReadTimeout the body; WriteTimeout stays 0 for SSE.
const (
	tlsReadHeaderTimeout = 10 * time.Second
	tlsReadTimeout       = 30 * time.Second
	tlsIdleTimeout       = 60 * time.Second
)

// tlsRecheckInterval is how often the TLS listener re-reads its files and
// re-judges established connections. A variable only so tests can shorten
// it; SetTLSRecheckIntervalForTest does so from another package.
var tlsRecheckInterval = pki.DefaultRecheckInterval

// SetTLSRecheckIntervalForTest shortens the recheck interval and returns a
// restore func. Tests only: the desktop's revocation test needs the #258
// sweep to fire within its deadline.
func SetTLSRecheckIntervalForTest(d time.Duration) (restore func()) {
	prev := tlsRecheckInterval
	tlsRecheckInterval = d
	return func() { tlsRecheckInterval = prev }
}

// logTLSReason is pki's Logf: one structured line per refusal or reload.
// A reload is routine and logs at INFO; refusals, rejected reloads and a
// stale CRL are WARN.
func logTLSReason(reason string, kv ...any) {
	ev := log.Warn()
	if len(kv) >= 2 && kv[0] == "event" && kv[1] == "tls_reloaded" {
		ev = log.Info()
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			ev = ev.Interface(k, kv[i+1])
		}
	}
	ev.Str("source", "tls").Msg(reason)
}

// WarnWriter adapts net/http's *log.Logger output to zerolog at WARN. It is
// exported because cmd's OAuth listener routes its ErrorLog the same way.
type WarnWriter struct{}

func (WarnWriter) Write(p []byte) (int, error) {
	log.Warn().Str("source", "net/http").Msg(strings.TrimSpace(string(p)))
	return len(p), nil
}
