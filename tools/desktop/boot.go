//go:build desktop

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	knomitapp "knomit/internal/app"
	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/tools/desktop/internal/lockfile"
	"knomit/tools/desktop/internal/netutil"
)

// server bundles a running http.Server with its discovery lockfile path and a
// cancel for the per-request base context.
type server struct {
	http        *http.Server
	tls         *http.Server // the mTLS listener's own server; nil when it is off
	tlsAddr     string       // where tls listens ("" when off); the log line and tests read it
	tlsState    tlsState     // what Settings shows about the TLS listener
	closeTLS    func()       // closes tls AND its listener; a noop when it is off
	cancel      context.CancelFunc
	lockPath    string
	closeSocket func() // from auth.ListenLocal; a noop when no socket was opened
}

// shutdown stops the server and removes the discovery lockfile. It cancels the
// request base context FIRST so long-lived handlers (the SSE /events stream,
// which selects on r.Context().Done()) return immediately — otherwise
// http.Server.Shutdown blocks for the full timeout waiting for that connection
// to drain, making Quit feel slow.
//
// The TLS server is shut down with the rest, and its pki re-judging loop
// stops with the base context: a Settings restart relaunches straight after
// this returns, and a successor that found [tls].addr still bound would come
// up without the listener.
func (s *server) shutdown() {
	s.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.http.Shutdown(ctx)
	if s.tls != nil {
		_ = s.tls.Shutdown(ctx)
	}
	s.closeTLS() // Shutdown alone misses a listener Serve has not registered yet
	s.closeSocket()
	_ = lockfile.Remove(s.lockPath)
}

// bootServer binds a loopback listener (netutil.Listen prefers cfg.Port,
// falling back to an ephemeral port only if that port is taken), serves
// handler on it, writes the discovery lockfile, and returns the running
// server and chosen port. External MCP clients discover the port via the
// lockfile. It also opens the local authenticated listener (auth.ListenLocal;
// an empty cfg.Socket skips it) on the same server, so a same-machine bridge
// is identified by the uid or SID the OS reports for it, and — when
// [tls].addr is set and a certificate is installed — the mTLS listener for
// enrolled fleet peers (knomit#256), on its OWN server from
// app.OpenTLSServer.
//
// It takes the WHOLE config and the key path, not values derived from them.
// A derived pair at the call site is what knomit#245 found could be mutated
// with every test green; here localListenerFrom and tlsListenerFrom run
// inside, where the boot tests reach them, and boot_localpeer_test.go pins
// both mappings.
//
// The local listener's Require half is cfg.Auth.Require. With it set there
// is no anonymous path, so a boot that could not bind the listener must FAIL
// rather than serve TCP only: see auth.RequireLocalListener for why that is
// not what the benign ErrSocketInUse branch below does on its own. The TLS
// listener does not count towards it: it serves peers, never the local user.
//
// THE ORDER IS LOAD-BEARING, and every fallible step undoes all before it:
//
//  1. plaintext TCP (netutil.Listen)
//  2. the local socket or pipe (auth.ListenLocal)
//  3. auth.RequireLocalListener
//  4. the TLS listener — LAST of the listeners, so a refusal at 3 never has
//     a network port to release
//  5. lockfile.Write — no lockfile until every listener that can fail is
//     open, so nothing advertises a dead port
//
// A held [tls].addr (app.ErrTLSAddrInUse) is NOT a failure here: it is an
// availability problem, the same shape as ErrSocketInUse, and normally means
// a `knomit serve` on this home is already serving the fleet. The desktop
// warns and serves without TLS rather than losing its whole UI and MCP
// server over a port. `knomit serve` makes the opposite choice. A TRUST
// failure (a certificate installed but unloadable) still fails the boot.
//
// parent is the application context; it is propagated into every request via
// BaseContext so a single cancel (on shutdown, or when parent is cancelled)
// unblocks streaming handlers promptly. It also bounds the TLS listener's
// re-judging loop.
func bootServer(parent context.Context, handler http.Handler, lockPath, version string, cfg config.Config, keyPath string) (*server, int, error) {
	local := localListenerFrom(cfg)
	tl := tlsListenerFrom(cfg, keyPath)
	ln, err := netutil.Listen(cfg.Port)
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
	// closeTCP closes the listener as well as the server: http.Server.Close
	// closes only listeners Serve has already registered, and the goroutine
	// above may not have got that far when a later step fails.
	closeTCP := func() { _ = srv.Close(); _ = ln.Close() }
	// Local authenticated listener beside the TCP one. The lockfile keeps
	// advertising the TCP port; the bridge prefers the socket when it exists
	// and falls back to TCP (knomitapi/dial.go). Without this the desktop —
	// the normal install — never opened the socket at all (issue #248). It
	// runs BEFORE lockfile.Write: no lockfile until every listener that can
	// fail has been opened, so a socket failure never advertises a dead port.
	ul, closeSocket, err := auth.ListenLocal(local.Path)
	switch {
	case errors.Is(err, auth.ErrSocketInUse):
		// Another process holds the path — normally a `knomit serve` or a
		// second desktop, but all that is KNOWN is that it is held: on
		// Windows the pipe namespace is flat and world-creatable, so the
		// owner need not be knomit at all. Do not steal it either way; serve
		// TCP only and say so.
		fmt.Fprintf(os.Stderr, "desktop: %v; serving TCP only\n", err)
	case errors.Is(err, auth.ErrPathTooLong), errors.Is(err, auth.ErrUnsafeSocketDir):
		// knomit#253: the path cannot be a unix socket (the error names its
		// length and this platform's cap), or it sits in the shared fallback
		// directory and that directory is not private to this user. Same
		// shape as the in-use branch: warn, serve TCP only, and let
		// RequireLocalListener below refuse the boot if TCP-only is useless.
		fmt.Fprintf(os.Stderr, "desktop: %v; serving TCP only\n", err)
	case err != nil:
		cancel()
		closeTCP()
		return nil, 0, fmt.Errorf("local listener: %w", err)
	}
	// ...unless serving TCP only would mean serving NOTHING. With
	// [auth].require = true there is no anonymous path, so carrying on past
	// the benign branch above would answer every request 403 while looking
	// healthy. Before the lockfile, for the same reason as everything else
	// here: no port is advertised until every listener that can fail is open.
	if rerr := auth.RequireLocalListener(local.Require, ul, local.Path, err); rerr != nil {
		cancel()
		closeTCP()
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
	// Step 4: the mTLS listener, the last listener that can fail.
	tlsSrv, tln, err := knomitapp.OpenTLSServer(srvCtx, config.TLSConfig{Addr: tl.Addr, Dir: tl.Dir}, tl.KeyPath, srv)
	tstate := tlsState{Configured: tl.Addr}
	switch {
	case errors.Is(err, knomitapp.ErrTLSAddrInUse):
		tstate.Reason = tlsAddrInUse
		log.Warn().Err(err).Str("addr", tl.Addr).
			Msg("tls listener address is held by another process (normally a `knomit serve` on this home); serving without the mTLS listener")
	case err == nil && tlsSrv == nil && tl.Addr != "":
		// OpenTLSServer's (nil, nil, nil) with an addr set: no certificate yet.
		tstate.Reason = tlsNoCertificate
	case err != nil:
		cancel()
		closeTCP()
		closeSocket()
		return nil, 0, fmt.Errorf("tls listener: %w", err)
	}
	closeTLS := func() {}
	tlsAddr := ""
	if tlsSrv != nil {
		tlsAddr = tln.Addr().String()
		tstate.Listening = tlsAddr
		log.Info().Str("tls", "https://"+tlsAddr).Str("dir", tl.Dir).Msg("mTLS listener for enrolled instances")
		// Close both: http.Server.Close closes only listeners Serve has
		// registered, and the goroutine below may not have got there yet.
		closeTLS = func() { _ = tlsSrv.Close(); _ = tln.Close() }
		go func() {
			if err := tlsSrv.Serve(tln); err != nil && err != http.ErrServerClosed {
				log.Error().Err(err).Msg("desktop tls serve error")
			}
		}()
	}
	if err := lockfile.Write(lockPath, lockfile.Info{PID: os.Getpid(), Port: port, Version: version}); err != nil {
		cancel() // also stops the TLS listener's pki re-judging loop
		closeTCP()
		closeTLS()
		closeSocket()
		return nil, 0, fmt.Errorf("write lockfile: %w", err)
	}
	return &server{http: srv, tls: tlsSrv, tlsAddr: tlsAddr, tlsState: tstate, closeTLS: closeTLS, cancel: cancel, lockPath: lockPath, closeSocket: closeSocket}, port, nil
}

// localListener is WHERE the local authenticated listener goes and WHETHER
// the process may serve without it. The two are one value because they are one
// decision, and because two adjacent arguments — a path and a bool — are
// exactly the shape that gets wired up the wrong way round without anything
// noticing (knomit#245 review).
type localListener struct {
	Path    string
	Require bool
}

// localListenerFrom is the ONE place config becomes that value. It exists to
// be pinned: with the fields passed separately at the call site, mutating
// cfg.Auth.Require there to a constant left the whole desktop suite green,
// because every test called bootServer with a value of its own.
func localListenerFrom(cfg config.Config) localListener {
	return localListener{Path: cfg.Socket, Require: cfg.Auth.Require}
}

// tlsListener is WHERE the mTLS listener goes, WHICH directory holds its
// certificate, and WHICH key that certificate must wrap. An empty Addr means
// off.
type tlsListener struct {
	Addr    string
	Dir     string
	KeyPath string
}

// tlsListenerFrom is the ONE place config and the app's key path become that
// value, for the same reason localListenerFrom exists; boot_localpeer_test.go
// pins it. keyPath is App.KeyPath(), the key app.New resolved.
func tlsListenerFrom(cfg config.Config, keyPath string) tlsListener {
	return tlsListener{Addr: cfg.TLS.Addr, Dir: cfg.TLS.Dir, KeyPath: keyPath}
}

// tlsState is what this boot did with [tls].addr, for the Settings window's
// Fleet identity section: off (the zero value), listening, or configured but
// not listening and why. It is recorded where bootServer takes each branch,
// so it cannot disagree with the WARN the log shows. Only the availability
// and not-yet-enrolled cases can reach it — a trust failure fails the boot.
type tlsState struct {
	Configured string `json:"configured"` // [tls].addr as this boot read it; "" = off
	Listening  string `json:"listening"`  // the bound address; "" when not listening
	Reason     string `json:"reason"`     // why Configured is not Listening
}

// The reasons a configured TLS listener is not listening.
const (
	tlsNoCertificate = "no_certificate" // [tls].addr set, nothing installed in [tls].dir
	tlsAddrInUse     = "addr_in_use"    // app.ErrTLSAddrInUse: another process holds it
)

// tlsStatus carries a boot's tlsState to the Settings bindings. The boot
// goroutine writes it once the server is up; GetIdentity reads it at any
// time, including before (zero value, reported as "starting").
type tlsStatus struct {
	mu    sync.Mutex
	st    tlsState
	known bool
}

func (t *tlsStatus) set(st tlsState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.st, t.known = st, true
}

func (t *tlsStatus) get() (tlsState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st, t.known
}
