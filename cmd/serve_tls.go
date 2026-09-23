package cmd

import (
	"context"
	"fmt"
	stdlog "log"
	"net"
	"net/http"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/pki"
)

// openTLSServer opens the mTLS listener for enrolled instances (F19 phase 2)
// when [tls].addr is set AND a certificate is installed in [tls].dir.
//
// It returns a SEPARATE http.Server sharing like's Handler, timeouts and
// BaseContext, whose ConnContext is auth.TLSConnContext: that server's mark
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
// here on ctx, so it stops when the serve command's context does. Without
// both, a revoked peer keeps a kept-alive connection for as long as it keeps
// talking.
func openTLSServer(ctx context.Context, tcfg config.TLSConfig, keyPath string, like *http.Server) (*http.Server, net.Listener, error) {
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
		return nil, nil, fmt.Errorf("tls listener: %w", err)
	}
	ln, _, err := auth.ListenTLS(tcfg.Addr, ps.TLSConfig())
	if err != nil {
		return nil, nil, err
	}
	go ps.Run(ctx)
	srv := &http.Server{
		Handler:           like.Handler,
		ReadHeaderTimeout: like.ReadHeaderTimeout,
		ReadTimeout:       like.ReadTimeout,
		WriteTimeout:      like.WriteTimeout,
		IdleTimeout:       like.IdleTimeout,
		BaseContext:       like.BaseContext,
		ConnContext:       auth.TLSConnContext,
		ConnState:         ps.ConnState,
		// net/http logs failed handshakes ("TLS handshake error …") to
		// ErrorLog; route them into zerolog so they are not lost. The named
		// refusal reason arrives separately through logTLSReason.
		ErrorLog: stdlog.New(warnWriter{}, "", 0),
	}
	return srv, ln, nil
}

// tlsRecheckInterval is how often the TLS listener re-reads its files and
// re-judges established connections. A variable only so cmd's tests can
// shorten it.
var tlsRecheckInterval = pki.DefaultRecheckInterval

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

// warnWriter adapts net/http's *log.Logger output to zerolog at WARN.
type warnWriter struct{}

func (warnWriter) Write(p []byte) (int, error) {
	log.Warn().Str("source", "net/http").Msg(strings.TrimSpace(string(p)))
	return len(p), nil
}
