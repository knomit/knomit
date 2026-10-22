// Listener set: opens the listeners a knomit server answers on and tells the
// processes that look for it where they are. It is the ONE boot path:
// `knomit serve` calls it, and so will the desktop once it spawns the server
// as a child instead of building its own.
//
// # The order, and why it is load-bearing
//
// OpenListeners binds, in this order,
//
//  1. plaintext TCP (host:port), unless `http = false`
//  2. the local socket or pipe (auth.ListenLocal)
//  3. auth.RequireLocalListener
//  4. the mTLS listener (OpenTLSServer) -- the last listener Open binds,
//     so a refusal at 3 never has a network port to release
//
// and Advertise writes server.json LAST, after the caller has bound whatever
// else it owns (OAuth, diagnostics). Every fallible step closes ALL before it,
// the TLS listener included, so every address is re-bindable after any
// failure. server.json is last so that nothing ever advertises a port that
// nothing serves.
//
// Each listener's Close closes the net.Listener as well as the http.Server:
// http.Server.Close only closes listeners Serve has already registered, and
// Serve runs on a goroutine, so a failure or shutdown straight after boot
// could otherwise leave the port bound.

package app

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

	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/config"
	"knomit/internal/foundation/serveraddr"
)

// ListenerConfigError marks an Open failure that no retry will fix: a trust failure
// (a certificate that is installed but cannot be loaded), a local listener
// the configuration cannot have, or a refusal under [auth].require. A
// supervisor reads it as "do not restart"; `knomit serve --supervised` exits
// 2 for it.
type ListenerConfigError struct{ Err error }

func (e *ListenerConfigError) Error() string { return e.Err.Error() }
func (e *ListenerConfigError) Unwrap() error { return e.Err }

// The reasons a configured listener is not listening.
const (
	ListenerReasonAddrInUse     = "addr_in_use"    // another process holds the address
	ListenerReasonNoCertificate = "no_certificate" // [tls].addr set, nothing installed in [tls].dir
	ListenerReasonOff           = "off"            // switched off (http = false)
)

// ListenerState is what Open did with one configured listener, for server.json and
// for the log: the address as configured, the address bound, and why those
// differ.
type ListenerState struct {
	Configured string `json:"configured"` // "" = off
	Listening  string `json:"listening"`  // the bound address; "" when not listening
	Reason     string `json:"reason"`     // why Configured is not Listening
}

// ListenerOptions is everything Open needs. It takes the WHOLE config, not values
// derived from it: serve used to pass cfg.Socket and cfg.Auth.Require as two
// adjacent arguments, and mutating one to a constant left every test green
// (knomit#245), so the mapping from config to listener happens inside, where
// the tests reach it.
type ListenerOptions struct {
	Config  config.Config
	Handler http.Handler
	// Base is every server's BaseContext (cancel it to unblock SSE handlers)
	// and bounds the TLS listener's certificate re-check loop.
	Base    context.Context
	KeyPath string // the key app.New resolved; the TLS certificate must wrap it
	Version string // recorded in server.json

	// Supervised is `knomit serve --supervised`: the process is a child of
	// the desktop, which reaches it only over the local listener.
	//
	//   - the local listener is MANDATORY: a held one is auth.ErrSocketInUse
	//     (the supervisor attaches to its owner instead), an unusable path a
	//     *ListenerConfigError;
	//   - an address that is simply HELD (plaintext TCP, TLS) is an
	//     availability problem: WARN and serve without it;
	//   - a trust failure stays fatal (*ListenerConfigError).
	//
	// Unsupervised, `knomit serve` keeps its refusal: a held TCP or TLS
	// address fails the boot, and a held local path serves TCP only.
	Supervised bool

	// LockPath overrides where server.json goes (tests). Empty = the home's
	// file, config.LockfilePathFor(Config.Home).
	LockPath string

	// Serve starts serving a listener that Apply binds after boot (the boot
	// listeners are served by the caller). It must return at once and treat
	// the listener's retirement as a clean stop: see Listeners.Retired. Nil
	// means Apply cannot add a listener (it reports that instead).
	Serve func(what string, srv *http.Server, ln net.Listener)
	// OnAddress is told the new Address when a reload changes it (only when
	// the local listener did not bind, so the TCP listener IS the address).
	OnAddress func(addr string)
}

// Listeners is what Open bound. Serving is the caller's: it runs
// HTTP.Serve(TCP) and HTTP.Serve(Local) (both share one http.Server) and
// TLSServer.Serve(TLS), nil entries skipped, and calls Close when it is done.
type Listeners struct {
	// HTTP is the plaintext server. It serves TCP (when on) and Local.
	HTTP *http.Server
	TCP  net.Listener // nil when http = false or its address was held (supervised)
	// Local is the socket/pipe listener, nil when this process did not bind
	// one (held by another process, or a path that cannot be a socket).
	Local     net.Listener
	LocalPath string

	TLSServer *http.Server // nil when the mTLS listener is off
	TLS       net.Listener

	// Address is this server's own address in KNOMIT_SERVER's spelling: the
	// local listener only if it BOUND, else the TCP listener. A held socket
	// belongs to another instance and must never be named.
	Address string

	HTTPState ListenerState
	TLSState  ListenerState

	lockPath string
	opts     ListenerOptions
	// skipLock: the local listener is held by another live process, which
	// owns this home's server.json; writing ours would repoint kb at a
	// TCP-only sibling.
	skipLock bool

	mu          sync.Mutex
	closed      bool
	closeSocket func()
	wroteLock   bool

	// Reload state (listeners_reload.go). applyMu serializes Apply and
	// Close against it; mu guards the fields Apply replaces (TCP, TLS,
	// TLSServer, the states, Address) against Snapshot and Close.
	applyMu       sync.Mutex
	cfg           config.Config // the config the listeners currently reflect
	builtHost     string        // Config.Host when app.New built its middleware
	generation    int
	restartNeeded []string
	reloadError   string // last reload's load/validation error, "" after a good one
	retired       map[net.Listener]bool
	reloadable    bool    // a reload trigger is installed (server.json "reload")
	level         *string //[log].level last applied by a reload; nil = the boot value
	boundHTTP     string  // host:port the TCP listener was bound for; "" = none
	boundTLS      string  // [tls].addr + dir the TLS listener was opened for; "" = none
	boundTLSCfg   *config.TLSConfig
	// tlsCfg is the [tls] config the mTLS listener reflects: the boot's, then
	// each reload's (reconcileTLS). StartTLSWhenInstalled starts THIS one.
	tlsCfg    config.TLSConfig
	tlsCancel context.CancelFunc // stops the TLS certificate re-check loop
}

// OpenListeners binds the listeners in the documented order. On any error it has
// closed everything it opened, and the error is returned with nothing
// serving. See ListenerOptions for how Supervised changes which failures are fatal.
func OpenListeners(ctx context.Context, o ListenerOptions) (*Listeners, error) {
	cfg := o.Config
	l := &Listeners{
		opts: o, closeSocket: func() {},
		cfg: cfg, builtHost: cfg.Host, generation: 1,
		retired: map[net.Listener]bool{},
	}
	l.lockPath = o.LockPath
	if l.lockPath == "" {
		l.lockPath = config.LockfilePathFor(cfg.Home)
	}
	l.HTTP = &http.Server{
		Handler:           o.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // 0 = no limit for SSE long-poll
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return o.Base },
		// ConnContext runs once per accepted connection, the only moment the
		// net.Conn exists: it asks the kernel who is on the other end of a
		// unix socket and puts the answer where internal/web's AuthMiddleware
		// can read it. TCP connections are left untouched.
		ConnContext: auth.ConnContext,
	}

	// 1. plaintext TCP.
	listenAddr := cfg.Host + ":" + cfg.Port
	if cfg.HTTP {
		l.HTTP.Addr = listenAddr
		l.HTTPState.Configured = listenAddr
		ln, err := listenTCP(listenAddr)
		switch {
		case err == nil:
			l.TCP = ln
			l.boundHTTP = listenAddr
			l.HTTPState.Listening = ln.Addr().String()
		case o.Supervised && IsAddrInUse(err):
			l.HTTPState.Reason = ListenerReasonAddrInUse
			log.Warn().Err(err).Str("addr", listenAddr).
				Msg("plaintext listener address is held by another process; serving without it")
		default:
			log.Error().Err(err).Msg("listen failed")
			return nil, l.fail(err)
		}
	} else {
		l.HTTPState.Reason = ListenerReasonOff
		log.Info().Msg("http = false: no plaintext TCP listener; serving on the local listener only")
	}

	// 2. + 3. the local socket or pipe, and whether serving without it is
	// acceptable.
	ul, closeSocket, benign, err := openLocal(cfg)
	if o.Supervised && ul == nil && benign != nil {
		// The desktop has no other way in. A held path means a server is
		// already there and the supervisor attaches to it (the bare
		// auth.ErrSocketInUse is its signal); any other cause is the
		// configuration's and no retry fixes it.
		closeSocket()
		if errors.Is(benign, auth.ErrSocketInUse) {
			return nil, l.fail(fmt.Errorf("local listener: %w", benign))
		}
		return nil, l.fail(&ListenerConfigError{Err: fmt.Errorf("local listener: %w", benign)})
	}
	if benign != nil {
		log.Warn().Err(benign).Str("socket", cfg.Socket).
			Msg("no local authenticated listener of this process' own; serving TCP only")
	}
	if err == nil && ul == nil && l.TCP == nil {
		// Nothing bound at all: http = false and the local listener held,
		// unusable or unconfigured.
		err = errors.New("nothing to serve on: http is off and this process bound no local listener")
	}
	if err != nil {
		if o.Supervised {
			err = &ListenerConfigError{Err: err}
		}
		return nil, l.fail(err)
	}
	l.Local, l.closeSocket = ul, closeSocket
	if ul != nil {
		l.LocalPath = cfg.Socket
	} else if errors.Is(benign, auth.ErrSocketInUse) {
		l.skipLock = true
	}

	// The address this server's own children (recipe `exec`) get as
	// KNOMIT_SERVER.
	tcpAddr := ""
	if l.TCP != nil {
		tcpAddr = l.TCP.Addr().String()
	}
	l.Address = serverAddress(cfg, ul, tcpAddr)

	// 4. the mTLS listener: its OWN http.Server over the same handler, off
	// until [tls].addr is set and a fleet join or create has placed a
	// certificate.
	tcfg := cfg.TLS
	l.tlsCfg = tcfg
	l.TLSState.Configured = tcfg.Addr
	tctx, tcancel := context.WithCancel(o.Base)
	tlsSrv, tln, err := OpenTLSServer(tctx, tcfg, o.KeyPath, l.HTTP)
	if tlsSrv != nil {
		l.tlsCancel = tcancel // Close and a later drop stop its re-check loop
	} else {
		tcancel() // nothing runs on it
	}
	switch {
	case errors.Is(err, ErrTLSAddrInUse) && o.Supervised:
		l.TLSState.Reason = ListenerReasonAddrInUse
		log.Warn().Err(err).Str("addr", tcfg.Addr).
			Msg("tls listener address is held by another process; serving without the mTLS listener")
	case err != nil:
		// Fail closed: serve does not run without a listener it was
		// configured to open. A held address is the only availability
		// failure (and only supervised survives it); the rest is trust.
		log.Error().Err(err).Str("dir", tcfg.Dir).Msg("tls listener failed")
		if !errors.Is(err, ErrTLSAddrInUse) {
			err = &ListenerConfigError{Err: err}
		}
		return nil, l.fail(err)
	case tlsSrv == nil && tcfg.Addr != "":
		l.TLSState.Reason = ListenerReasonNoCertificate
	case tlsSrv != nil:
		l.TLSServer, l.TLS = tlsSrv, tln
		l.boundTLS = tcfg.Addr + "\x00" + tcfg.Dir
		c := tcfg
		l.boundTLSCfg = &c
		l.TLSState.Listening = tln.Addr().String()
	}
	return l, nil
}

// fail undoes everything opened so far and returns err.
func (l *Listeners) fail(err error) error {
	l.Close()
	return err
}

// Advertise writes server.json, the discovery file every kb, hook and the
// desktop read -- LAST, so it never names a port nothing serves. Call it
// after every listener the caller owns is bound. On error the file is not
// there, and the caller returns the error and Closes.
//
// It is skipped when this process does not hold its home's local listener
// because another live process does: that process is the home's server and
// owns the file.
func (l *Listeners) Advertise() error {
	if l.skipLock {
		log.Info().Str("socket", l.opts.Config.Socket).
			Msg("not writing server.json: another process holds this home's local listener and owns it")
		return nil
	}
	l.mu.Lock()
	info := l.lockInfoLocked()
	l.mu.Unlock()
	if err := serveraddr.WriteLock(l.lockPath, info); err != nil {
		return fmt.Errorf("write lockfile: %w", err)
	}
	l.mu.Lock()
	l.wroteLock = true
	l.mu.Unlock()
	return nil
}

// lockInfoLocked is the content of server.json for the current listeners.
// l.mu must be held.
func (l *Listeners) lockInfoLocked() serveraddr.LockInfo {
	info := serveraddr.LockInfo{
		PID:           os.Getpid(),
		Version:       l.opts.Version,
		Schema:        serveraddr.LockSchemaV2,
		Home:          l.opts.Config.Home,
		Address:       l.Address,
		Socket:        l.LocalPath,
		TLS:           l.TLSState.Listening,
		Generation:    l.generation,
		Reload:        l.reloadable,
		RestartNeeded: append([]string(nil), l.restartNeeded...),
		ReloadError:   l.reloadError,
		Listeners: map[string]serveraddr.LockListener{
			"http": lockListener(l.HTTPState),
			"tls":  lockListener(l.TLSState),
		},
	}
	if l.TCP != nil {
		info.Port = l.TCP.Addr().(*net.TCPAddr).Port
		info.HTTP = serveraddr.ForTCP(l.TCP.Addr().String())
	}
	return info
}

func lockListener(s ListenerState) serveraddr.LockListener {
	return serveraddr.LockListener{Configured: s.Configured, Listening: s.Listening, Reason: s.Reason}
}

// LockPath is where Advertise writes.
func (l *Listeners) LockPath() string { return l.lockPath }

// Close stops everything Open bound and removes the discovery file if it is
// still this process's. It closes each server AND its listener, then frees the
// local path for a successor. Safe to call more than once.
func (l *Listeners) Close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	wrote := l.wroteLock
	l.mu.Unlock()
	// Wait for an Apply in flight: it must not bind a listener after this.
	l.applyMu.Lock()
	defer l.applyMu.Unlock()
	l.mu.Lock()
	tlsSrv, tlsLn, tcp, tlsCancel := l.TLSServer, l.TLS, l.TCP, l.tlsCancel
	l.mu.Unlock()

	if wrote {
		_ = serveraddr.RemoveLockIfOwner(l.lockPath, os.Getpid())
	}
	if tlsSrv != nil {
		_ = tlsSrv.Close()
	}
	if tlsCancel != nil {
		tlsCancel()
	}
	if tlsLn != nil {
		_ = tlsLn.Close()
	}
	if l.HTTP != nil {
		_ = l.HTTP.Close()
	}
	if tcp != nil {
		_ = tcp.Close()
	}
	if l.Local != nil {
		_ = l.Local.Close()
	}
	l.closeSocket()
}

// listenTCP binds the plaintext listener. It is separate from http.Server's
// ListenAndServe so serve can bind synchronously, before it reports ready, and
// return the error.
func listenTCP(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("plaintext listener: %w", err) // err names the address
	}
	return newTrackedListener(ln), nil
}

// openLocalListener opens the local authenticated listener for cfg and decides
// whether this process may serve without one.
//
// It takes the WHOLE CONFIG rather than a path and a flag. That is the point
// of it existing: serve used to pass `cfg.Socket` and `cfg.Auth.Require` as
// two separate arguments at the call site, and nothing pinned that they were
// the right two -- mutating the flag to a constant left every test green. There
// is now one argument and no pair to wire up the wrong way round, and the
// policy below is exercised directly by listen_test.go.
//
// Three outcomes, and the middle one is the subtle case:
//
//   - a listener, which the caller serves alongside its TCP one;
//   - NO listener and NO error, because another process holds the path and
//     [auth].require is off: serve TCP only, which is right, since a second
//     instance must not steal the first one's listener;
//   - an error, which fails the boot.
//
// The cleanup is always non-nil, so the caller can defer it unconditionally.
func openLocalListener(cfg config.Config) (net.Listener, func(), error) {
	ul, closeSocket, benign, err := openLocal(cfg)
	if benign != nil && err == nil {
		log.Warn().Err(benign).Str("socket", cfg.Socket).
			Msg("no local authenticated listener of this process' own; serving TCP only")
	}
	return ul, closeSocket, err
}

// openLocal is openLocalListener's decision, with the benign cause kept: when
// it returns no listener and no error, benign says why (auth.ErrSocketInUse,
// auth.ErrPathTooLong or auth.ErrUnsafeSocketDir), because the caller's
// policy differs by cause -- a held path means another process owns this
// home's discovery file, and a supervised caller attaches to it. It logs
// nothing: what to say depends on the caller.
//
// benign may also accompany an error (the [auth].require refusal wraps it).
func openLocal(cfg config.Config) (ln net.Listener, cleanup func(), benign, err error) {
	ul, closeSocket, err := auth.ListenLocal(cfg.Socket)
	switch {
	case errors.Is(err, auth.ErrSocketInUse):
		// Another process holds the path -- normally the desktop app, but all
		// that is KNOWN is that it is held: on Windows the pipe namespace is
		// flat and world-creatable, so the owner need not be knomit at all.
		// Do not steal it either way: bridges belong to whatever was there
		// first.
		benign = err
	case errors.Is(err, auth.ErrPathTooLong), errors.Is(err, auth.ErrUnsafeSocketDir):
		// knomit#253: the path cannot be a unix socket (the error names its
		// length and this platform's cap), or it is in the shared fallback
		// directory and that directory is not private to this user. Neither
		// is a reason to refuse the boot: serve TCP only and say why. A long
		// DATA ROOT alone never lands here (config falls back to a short
		// path); an explicit overlong [socket] does.
		benign = err
	case err != nil:
		closeSocket()
		return nil, func() {}, nil, err
	}
	// ...unless serving TCP only would mean serving NOTHING. With
	// [auth].require = true there is no anonymous path, so carrying on past
	// the benign branch above would answer every request 403 while looking
	// healthy -- the silent lockout app.checkLocalListener refuses at config
	// time, arriving through the one door it cannot see.
	//
	// err here is nil, ErrSocketInUse, ErrPathTooLong or ErrUnsafeSocketDir
	// -- anything else returned above -- so passing it straight through
	// names the right cause.
	if rerr := auth.RequireLocalListener(cfg.Auth.Require, ul, cfg.Socket, err); rerr != nil {
		closeSocket()
		return nil, func() {}, benign, rerr
	}
	return ul, closeSocket, benign, nil
}

// serverAddress is this server's own address in KNOMIT_SERVER's spelling, for
// the processes it starts (a recipe's `exec` child; repos.Manager hands it
// on). It is the local listener when one BOUND -- its path is cfg.Socket,
// which may be KNOMIT_SOCKET, the toml `socket` or the /tmp fallback for a
// long data root, never a re-derived <home>/knomit.sock -- and the TCP
// listener otherwise. "Otherwise" includes ErrSocketInUse: another instance
// holds that socket, so naming it would send this server's children THERE.
//
// local is what openLocalListener returned; tcpAddr is the TCP listener's
// bound address.
func serverAddress(cfg config.Config, local net.Listener, tcpAddr string) string {
	if local != nil {
		return serveraddr.ForLocal(cfg.Socket)
	}
	return serveraddr.ForTCP(tcpAddr)
}
