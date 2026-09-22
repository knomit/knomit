//go:build desktop

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"knomit/internal/auth"
	"knomit/tools/desktop/internal/lockfile"
	"knomit/tools/desktop/internal/netutil"
)

// server bundles a running http.Server with its discovery lockfile path and a
// cancel for the per-request base context.
type server struct {
	http        *http.Server
	cancel      context.CancelFunc
	lockPath    string
	closeSocket func() // from auth.ListenLocal; a noop when no socket was opened
}

// shutdown stops the server and removes the discovery lockfile. It cancels the
// request base context FIRST so long-lived handlers (the SSE /events stream,
// which selects on r.Context().Done()) return immediately — otherwise
// http.Server.Shutdown blocks for the full timeout waiting for that connection
// to drain, making Quit feel slow.
func (s *server) shutdown() {
	s.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.http.Shutdown(ctx)
	s.closeSocket()
	_ = lockfile.Remove(s.lockPath)
}

// bootServer binds a looknomitck listener (netutil.Listen prefers
// preferredPort — normally cfg.Port — falling back to an ephemeral port only
// if that port is taken), serves handler on it, writes the discovery
// lockfile, and returns the running server and chosen port. External MCP
// clients discover the port via the lockfile. It also opens the local
// authenticated listener at socketPath (auth.ListenLocal; "" skips it) on the
// same server, so a same-machine bridge is identified by the uid or SID the OS
// reports for it.
//
// requireAuth is cfg.Auth.Require. With it set there is no anonymous path, so
// a boot that could not bind the listener must FAIL rather than serve TCP
// only: see auth.RequireLocalListener for why that is not what the benign
// ErrSocketInUse branch below does on its own.
//
// parent is the application context; it is propagated into every request via
// BaseContext so a single cancel (on shutdown, or when parent is cancelled)
// unblocks streaming handlers promptly.
func bootServer(parent context.Context, handler http.Handler, lockPath, version, preferredPort, socketPath string, requireAuth bool) (*server, int, error) {
	ln, err := netutil.Listen(preferredPort)
	if err != nil {
		return nil, 0, fmt.Errorf("listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srvCtx, cancel := context.WithCancel(parent)
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // 0 = no limit, required for SSE long-poll streams
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return srvCtx },
		// Once per accepted connection: attaches the unix socket peer's
		// kernel-reported uid/pid; a TCP connection is left untouched.
		ConnContext: auth.ConnContext,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "desktop serve error: %v\n", err)
		}
	}()
	// Local authenticated listener beside the TCP one. The lockfile keeps
	// advertising the TCP port; the bridge prefers the socket when it exists
	// and falls back to TCP (knomitapi/dial.go). Without this the desktop —
	// the normal install — never opened the socket at all (issue #248). It
	// runs BEFORE lockfile.Write: no lockfile until every listener that can
	// fail has been opened, so a socket failure never advertises a dead port.
	ul, closeSocket, err := auth.ListenLocal(socketPath)
	switch {
	case errors.Is(err, auth.ErrSocketInUse):
		// Another process holds the path — normally a `knomit serve` or a
		// second desktop, but all that is KNOWN is that it is held: on
		// Windows the pipe namespace is flat and world-creatable, so the
		// owner need not be knomit at all. Do not steal it either way; serve
		// TCP only and say so.
		fmt.Fprintf(os.Stderr, "desktop: %v; serving TCP only\n", err)
	case err != nil:
		cancel()
		_ = srv.Close()
		return nil, 0, fmt.Errorf("local listener: %w", err)
	}
	// ...unless serving TCP only would mean serving NOTHING. With
	// [auth].require = true there is no anonymous path, so carrying on past
	// the benign branch above would answer every request 403 while looking
	// healthy. Before the lockfile, for the same reason as everything else
	// here: no port is advertised until every listener that can fail is open.
	if rerr := auth.RequireLocalListener(requireAuth, ul, socketPath, err); rerr != nil {
		cancel()
		_ = srv.Close()
		closeSocket()
		return nil, 0, rerr
	}
	if ul != nil {
		go func() {
			if err := srv.Serve(ul); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "desktop unix socket serve error: %v\n", err)
			}
		}()
	}
	if err := lockfile.Write(lockPath, lockfile.Info{PID: os.Getpid(), Port: port, Version: version}); err != nil {
		cancel()
		_ = srv.Close()
		closeSocket()
		return nil, 0, fmt.Errorf("write lockfile: %w", err)
	}
	return &server{http: srv, cancel: cancel, lockPath: lockPath, closeSocket: closeSocket}, port, nil
}
