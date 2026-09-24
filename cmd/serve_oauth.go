package cmd

import (
	"fmt"
	stdlog "log"
	"net"
	"net/http"

	"knomit/internal/config"
)

// openOAuthServer opens the OAuth listener (F19 phase 3a, R1) when [oauth]
// is configured: its OWN http.Server on [oauth].addr over the OAuth router
// (web.Server.OAuthHandler) — never the plain listener's handler — with the
// plain server's timeouts and BaseContext. A reverse proxy with real TLS
// fronts THIS listener, and only this one: it is where a bearer token is the
// only way to a principal, so a same-host proxy connecting from 127.0.0.1
// gains nothing by being loopback. The plain, local and TLS listeners are
// untouched.
//
// No ConnContext: nothing on this listener reads peer credentials or a
// listener mark, because its router has exactly one edge middleware.
//
// (nil, nil, nil) means off. A bind failure is an error: an operator who
// configured OAuth and gets no listener should not find out from a client.
func openOAuthServer(o config.OAuthConfig, h http.Handler, like *http.Server) (*http.Server, net.Listener, error) {
	if !o.Enabled() {
		return nil, nil, nil
	}
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return nil, nil, fmt.Errorf("oauth listener on %s: %w", o.Addr, err)
	}
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: like.ReadHeaderTimeout,
		ReadTimeout:       like.ReadTimeout,
		WriteTimeout:      like.WriteTimeout,
		IdleTimeout:       like.IdleTimeout,
		BaseContext:       like.BaseContext,
		ErrorLog:          stdlog.New(warnWriter{}, "", 0),
	}, ln, nil
}
