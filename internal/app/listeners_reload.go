// Reload: re-reads the configuration of a RUNNING server and applies the part
// of it that can change without a restart -- the plaintext TCP listener
// (`http`, `host`, `port`), the mTLS listener (`[tls].addr`, `[tls].dir`, and
// starting it once a certificate has been installed) and `[log].level`.
// Everything else is reported, never applied.
//
// # Who may call it
//
// Exactly one production caller: the signal (unix SIGHUP) or named-event
// (Windows) loop in package cmd. It is NOT reachable from an HTTP route, and
// must never be: every route of the app handler is also served on the mTLS
// listener and on whatever interface `http` binds, so a reload route would let
// a fleet peer or a network neighbour re-bind the server's ports. The
// listener config changes only through the CLI or the desktop Settings
// window; this is the same rule as for trust material. test/archtest
// (TestListenersApplyHasOnlyTheSignalLoopAsCaller) pins it.
//
// # Order, and what a failure does
//
// For each listener Apply binds the NEW address before it closes the old one,
// so a held port, a bad certificate or a refused host leaves the previous
// listener serving; the failure is logged and recorded in server.json
// (listeners.<name>.reason). The one exception is a rebind to the SAME port
// under another host spelling (127.0.0.1 -> 0.0.0.0), which the kernel may
// refuse while the old socket is open: the old one is closed first and, if the
// new bind still fails, re-bound. server.json is rewritten LAST, with
// `generation` incremented (017f5f06: never advertise what is not serving).
// Applies are serialized.
//
// Connections accepted on a retired plaintext listener are closed with it
// (trackedListener), so an in-flight SSE stream on a port that was switched
// off ends. The local socket/pipe, which shares the plaintext http.Server, is
// untouched.

package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/serveraddr"
)

// Reasons Apply records for a configured listener it could not bring to the
// configured state (beside ListenerReasonAddrInUse / NoCertificate / Off).
const (
	ListenerReasonBindFailed = "bind_failed" // the bind failed for another reason; see the log
	ListenerReasonTLSFailed  = "tls_failed"  // a certificate is installed but cannot be used
	ListenerReasonNoRoute    = "refused"     // applying it would leave nothing to serve on
)

// ApplyResult is what one reload did.
type ApplyResult struct {
	// Generation is server.json's generation after this reload.
	Generation int
	// Applied lists what changed: "http", "tls", "log.level".
	Applied []string
	// RestartNeeded lists the config keys that differ from what the process
	// is running with and that only a restart applies. Names only, never
	// values (the config holds secrets).
	RestartNeeded []string
	// Failed maps a reloadable item to why it was left as it was.
	Failed map[string]string
	// LockErr is set when server.json could not be rewritten.
	LockErr error
}

// restartOnly are the reloadable keys, which the restart-needed diff skips.
var reloadableKeys = map[string]bool{
	"http": true, "host": true, "port": true,
	"tls.addr": true, "tls.dir": true,
	"log.level": true,
}

// Snapshot is a consistent copy of the listener state, for tests and logging.
type Snapshot struct {
	HTTP, TLS  ListenerState
	TCPAddr    string // "" when no plaintext listener is bound
	TLSAddr    string
	Address    string
	Generation int
}

// Snapshot returns the current state.
func (l *Listeners) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := Snapshot{HTTP: l.HTTPState, TLS: l.TLSState, Address: l.Address, Generation: l.generation}
	if l.TCP != nil {
		s.TCPAddr = l.TCP.Addr().String()
	}
	if l.TLS != nil {
		s.TLSAddr = l.TLS.Addr().String()
	}
	return s
}

// SetReloadable records, before Advertise, whether the caller installed a
// reload trigger; server.json says so ("reload": true), and `knomit reload`
// signals only a server that does.
func (l *Listeners) SetReloadable(on bool) {
	l.mu.Lock()
	l.reloadable = on
	l.mu.Unlock()
}

// Retired reports whether ln was closed on purpose by Apply, so the goroutine
// serving it can tell the resulting "use of closed network connection" from a
// listener failure.
func (l *Listeners) Retired(ln net.Listener) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.retired[ln]
}

// Reload loads the configuration with load -- the same loader and the same
// command-line overrides as boot -- and applies it. A load or validation
// error is logged at ERROR, keeps the running configuration and does NOT
// advance the generation.
func (l *Listeners) Reload(load func() (config.Config, error)) (ApplyResult, error) {
	cfg, err := load()
	if err != nil {
		log.Error().Err(err).Msg("reload: configuration not applied; the running configuration is unchanged")
		return ApplyResult{Generation: l.Snapshot().Generation}, err
	}
	return l.Apply(cfg), nil
}

// Apply reconciles the running listeners with newCfg. See the file comment.
func (l *Listeners) Apply(newCfg config.Config) ApplyResult {
	l.applyMu.Lock()
	defer l.applyMu.Unlock()
	res := ApplyResult{Failed: map[string]string{}}
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	if closed {
		res.Failed["all"] = "server is shutting down"
		return res
	}

	// What cannot change without a restart.
	restart := changedKeys(l.cfg, newCfg)
	if l.hostNeedsRestart(newCfg) {
		restart = append(restart, "host")
	}
	slices.Sort(restart)
	for _, k := range restart {
		log.Warn().Str("key", k).Msg("reload: this setting only takes effect after a restart; left as it is")
	}

	// [log].level, live.
	if newCfg.Log.Level != l.curLevel() {
		lvl, err := zerolog.ParseLevel(levelOrInfo(newCfg.Log.Level))
		if err != nil {
			res.Failed["log.level"] = err.Error()
			log.Error().Err(err).Msg("reload: log level not applied")
		} else {
			zerolog.SetGlobalLevel(lvl)
			l.setLevel(newCfg.Log.Level)
			res.Applied = append(res.Applied, "log.level")
			log.Info().Str("level", lvl.String()).Msg("reload: log level applied")
		}
	}

	if changed, err := l.reconcileHTTP(newCfg); err != nil {
		res.Failed["http"] = err.Error()
	} else if changed {
		res.Applied = append(res.Applied, "http")
	}
	if changed, err := l.reconcileTLS(newCfg); err != nil {
		res.Failed["tls"] = err.Error()
	} else if changed {
		res.Applied = append(res.Applied, "tls")
	}

	// server.json LAST, with the generation advanced: the reload was
	// processed, whatever it could and could not do.
	l.mu.Lock()
	l.generation++
	l.restartNeeded = restart
	res.Generation = l.generation
	res.RestartNeeded = append([]string(nil), restart...)
	info := l.lockInfoLocked()
	wrote := l.wroteLock
	l.mu.Unlock()
	if wrote {
		res.LockErr = l.rewriteLock(info)
		if res.LockErr != nil {
			log.Error().Err(res.LockErr).Str("path", l.lockPath).
				Msg("reload: server.json not rewritten; its generation and addresses are stale")
		}
	}
	log.Info().Int("generation", res.Generation).Strs("applied", res.Applied).
		Strs("restart_needed", res.RestartNeeded).Int("failed", len(res.Failed)).Msg("reload done")
	return res
}

// rewriteLock replaces server.json with info -- but only while the file still
// records this process, the same owner check Close uses: a successor's file
// is not ours to overwrite.
func (l *Listeners) rewriteLock(info serveraddr.LockInfo) error {
	cur, err := serveraddr.ReadLock(l.lockPath)
	if err != nil {
		return fmt.Errorf("read server.json before rewriting it: %w", err)
	}
	if cur.PID != info.PID {
		return fmt.Errorf("server.json now records pid %d, not this process (%d); left alone", cur.PID, info.PID)
	}
	return serveraddr.WriteLock(l.lockPath, info)
}

func (l *Listeners) curLevel() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.level == nil {
		return l.cfg.Log.Level
	}
	return *l.level
}

func (l *Listeners) setLevel(v string) {
	l.mu.Lock()
	l.level = &v
	l.mu.Unlock()
}

func levelOrInfo(s string) string {
	if s == "" {
		return "info"
	}
	return s
}

// hostNeedsRestart: the server's own Host-header allowlist (a loopback peer's
// Host must name this machine) was built from the bind host when app.New ran,
// so a new bind host that is a DNS name the allowlist does not know is bound
// but not yet accepted in Host headers.
func (l *Listeners) hostNeedsRestart(newCfg config.Config) bool {
	if newCfg.Host == l.builtHost {
		return false
	}
	return !slices.Equal(l.cfg.Auth.EffectiveLoopbackHosts(newCfg.Host), l.cfg.Auth.EffectiveLoopbackHosts(l.builtHost))
}

// changedKeys lists the toml-spelled keys at which a and b differ, skipping
// the reloadable ones. Leaves are compared with reflect.DeepEqual; unexported
// fields are skipped.
func changedKeys(a, b config.Config) []string {
	var out []string
	diffStruct(reflect.ValueOf(a), reflect.ValueOf(b), "", &out)
	return out
}

func diffStruct(a, b reflect.Value, prefix string, out *[]string) {
	t := a.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := strings.ToLower(f.Name)
		if tag, _, _ := strings.Cut(f.Tag.Get("toml"), ","); tag != "" && tag != "-" {
			name = tag
		}
		key := prefix + name
		av, bv := a.Field(i), b.Field(i)
		if f.Type.Kind() == reflect.Struct && hasExported(f.Type) {
			diffStruct(av, bv, key+".", out)
			continue
		}
		if reloadableKeys[key] {
			continue
		}
		if !reflect.DeepEqual(av.Interface(), bv.Interface()) {
			*out = append(*out, key)
		}
	}
}

func hasExported(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).IsExported() {
			return true
		}
	}
	return false
}

// retire marks ln as closed on purpose.
func (l *Listeners) retire(ln net.Listener) {
	l.mu.Lock()
	l.retired[ln] = true
	l.mu.Unlock()
}

func bindReason(err error) string {
	if IsAddrInUse(err) {
		return ListenerReasonAddrInUse
	}
	return ListenerReasonBindFailed
}

// reconcileHTTP brings the plaintext TCP listener to newCfg's http/host/port.
// changed is true when the bound listener changed; err when it could not be
// brought to the configured state (the previous listener, if any, still
// serves).
func (l *Listeners) reconcileHTTP(newCfg config.Config) (changed bool, err error) {
	want := ""
	if newCfg.HTTP {
		want = newCfg.Host + ":" + newCfg.Port
	}
	l.mu.Lock()
	old, have := l.TCP, l.boundHTTP
	hasLocal := l.Local != nil
	l.mu.Unlock()
	if want == have && (want == "" || old != nil) {
		l.noteState("http", ListenerState{Configured: want, Listening: l.listening(old), Reason: offReason(want)})
		return false, nil
	}
	fail := func(reason string, err error) (bool, error) {
		log.Error().Err(err).Str("addr", want).Msg("reload: plaintext listener not changed; the previous one keeps serving")
		l.noteState("http", ListenerState{Configured: want, Listening: l.listening(old), Reason: reason})
		return false, err
	}

	if want == "" {
		if !hasLocal {
			return fail(ListenerReasonNoRoute, errors.New("http = false would leave nothing to serve on: this process holds no local listener"))
		}
		l.dropTCP(old)
		l.noteState("http", ListenerState{Reason: ListenerReasonOff})
		log.Info().Msg("reload: plaintext listener closed (http = false)")
		return true, nil
	}
	if l.opts.Serve == nil {
		return fail(ListenerReasonBindFailed, errors.New("this Listeners cannot start a listener after boot (no Serve hook)"))
	}

	ln, err := listenTCP(want)
	if err != nil && old != nil && IsAddrInUse(err) && samePort(old.Addr().String(), want) {
		// The new spelling collides with the socket still open on the same
		// port. Close the old one, bind the new, and put the old back if
		// that fails: the previous listener must outlive a failed reload.
		oldAddr := old.Addr().String()
		l.dropTCP(old)
		old = nil
		if ln, err = listenTCP(want); err != nil {
			if rln, rerr := listenTCP(oldAddr); rerr == nil {
				l.installTCP(rln, have)
				log.Warn().Str("addr", oldAddr).Msg("reload: previous plaintext listener re-bound")
			} else {
				log.Error().Err(rerr).Str("addr", oldAddr).Msg("reload: previous plaintext listener could not be re-bound")
			}
			l.mu.Lock()
			cur := l.TCP
			l.mu.Unlock()
			old = cur
		}
	}
	if err != nil {
		return fail(bindReason(err), err)
	}
	l.installTCP(ln, want)
	if old != nil {
		l.retireAndClose(old)
	}
	log.Info().Str("addr", ln.Addr().String()).Msg("reload: plaintext listener bound")
	return true, nil
}

// dropTCP retires and closes ln (and its connections) and clears it.
func (l *Listeners) dropTCP(ln net.Listener) {
	l.mu.Lock()
	if l.TCP == ln {
		l.TCP, l.boundHTTP = nil, ""
	}
	l.mu.Unlock()
	l.retireAndClose(ln)
	l.refreshAddress()
}

func (l *Listeners) retireAndClose(ln net.Listener) {
	if ln == nil {
		return
	}
	l.retire(ln)
	if t, ok := ln.(*trackedListener); ok {
		t.closeAll()
		return
	}
	_ = ln.Close()
}

// installTCP makes ln the plaintext listener, bound for want, and starts
// serving it.
func (l *Listeners) installTCP(ln net.Listener, want string) {
	l.mu.Lock()
	l.TCP, l.boundHTTP = ln, want
	l.HTTPState = ListenerState{Configured: want, Listening: ln.Addr().String()}
	l.mu.Unlock()
	l.refreshAddress()
	l.opts.Serve("listen", l.HTTP, ln)
}

// refreshAddress recomputes Address (KNOMIT_SERVER). It only moves when the
// local listener did not bind: otherwise Address is the socket's.
func (l *Listeners) refreshAddress() {
	l.mu.Lock()
	if l.Local != nil {
		l.mu.Unlock()
		return
	}
	tcp := ""
	if l.TCP != nil {
		tcp = l.TCP.Addr().String()
	}
	addr := serverAddress(l.cfg, nil, tcp)
	changed := addr != l.Address
	l.Address = addr
	l.mu.Unlock()
	if changed && l.opts.OnAddress != nil {
		l.opts.OnAddress(addr)
	}
}

func (l *Listeners) listening(ln net.Listener) string {
	if ln == nil {
		return ""
	}
	return ln.Addr().String()
}

func offReason(want string) string {
	if want == "" {
		return ListenerReasonOff
	}
	return ""
}

// noteState stores a listener's state.
func (l *Listeners) noteState(which string, s ListenerState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if which == "http" {
		l.HTTPState = s
	} else {
		l.TLSState = s
	}
}

func samePort(a, b string) bool {
	_, pa, ea := net.SplitHostPort(a)
	_, pb, eb := net.SplitHostPort(b)
	return ea == nil && eb == nil && pa == pb && pa != "0"
}

// reconcileTLS brings the mTLS listener to newCfg's [tls].addr / [tls].dir.
func (l *Listeners) reconcileTLS(newCfg config.Config) (changed bool, err error) {
	tcfg := newCfg.TLS
	key := tcfg.Addr + "\x00" + tcfg.Dir
	l.mu.Lock()
	oldSrv, oldLn, oldCancel, have := l.TLSServer, l.TLS, l.tlsCancel, l.boundTLS
	l.mu.Unlock()

	if tcfg.Addr == "" {
		if oldLn == nil {
			l.noteState("tls", ListenerState{})
			return false, nil
		}
		l.dropTLS(oldSrv, oldLn, oldCancel)
		l.noteState("tls", ListenerState{})
		log.Info().Msg("reload: mTLS listener closed ([tls].addr is empty)")
		return true, nil
	}
	if oldLn != nil && have == key {
		l.noteState("tls", ListenerState{Configured: tcfg.Addr, Listening: oldLn.Addr().String()})
		return false, nil
	}
	if l.opts.Serve == nil {
		return false, errors.New("this Listeners cannot start a listener after boot (no Serve hook)")
	}
	keep := ListenerState{Configured: tcfg.Addr}
	if oldLn != nil {
		keep.Listening = oldLn.Addr().String()
	}
	fail := func(reason string, err error) (bool, error) {
		if err != nil {
			log.Error().Err(err).Str("addr", tcfg.Addr).Msg("reload: mTLS listener not changed")
		}
		keep.Reason = reason
		l.noteState("tls", keep)
		if err == nil {
			err = errors.New(reason)
		}
		return false, err
	}

	// An unchanged address (only the directory moved) cannot be bound twice:
	// close the old one first and restore it if the new one fails.
	closedOld := false
	if oldLn != nil && oldLn.Addr().String() != "" && sameBoundAddr(oldLn, tcfg.Addr) {
		l.dropTLS(oldSrv, oldLn, oldCancel)
		closedOld = true
	}
	tctx, cancel := context.WithCancel(l.opts.Base)
	srv, ln, err := OpenTLSServer(tctx, tcfg, l.opts.KeyPath, l.HTTP)
	if err != nil || srv == nil {
		cancel()
		reason := ListenerReasonNoCertificate
		if err != nil {
			reason = ListenerReasonTLSFailed
			if errors.Is(err, ErrTLSAddrInUse) {
				reason = ListenerReasonAddrInUse
			}
		}
		if closedOld {
			// Put the previous listener back (its own directory and address).
			l.mu.Lock()
			prev := l.cfg.TLS
			if l.boundTLSCfg != nil {
				prev = *l.boundTLSCfg
			}
			l.mu.Unlock()
			pctx, pcancel := context.WithCancel(l.opts.Base)
			psrv, pln, perr := OpenTLSServer(pctx, prev, l.opts.KeyPath, l.HTTP)
			if perr == nil && psrv != nil {
				l.installTLS(psrv, pln, pcancel, prev)
				keep.Listening = pln.Addr().String()
				log.Warn().Str("addr", keep.Listening).Msg("reload: previous mTLS listener re-bound")
			} else {
				pcancel()
				keep.Listening = ""
				log.Error().Err(perr).Msg("reload: previous mTLS listener could not be re-bound")
			}
		}
		return fail(reason, err)
	}
	l.installTLS(srv, ln, cancel, tcfg)
	if oldLn != nil && !closedOld {
		l.dropTLS(oldSrv, oldLn, oldCancel)
	}
	log.Info().Str("tls", "https://"+ln.Addr().String()).Msg("reload: mTLS listener bound")
	return true, nil
}

func sameBoundAddr(ln net.Listener, want string) bool {
	return samePort(ln.Addr().String(), want) && want != ""
}

func (l *Listeners) installTLS(srv *http.Server, ln net.Listener, cancel context.CancelFunc, tcfg config.TLSConfig) {
	l.mu.Lock()
	l.TLSServer, l.TLS, l.tlsCancel = srv, ln, cancel
	l.boundTLS = tcfg.Addr + "\x00" + tcfg.Dir
	c := tcfg
	l.boundTLSCfg = &c
	l.TLSState = ListenerState{Configured: tcfg.Addr, Listening: ln.Addr().String()}
	l.mu.Unlock()
	l.opts.Serve("tls listener", srv, ln)
}

// dropTLS retires and closes the mTLS server and listener, and stops the
// certificate re-check loop that belonged to them.
func (l *Listeners) dropTLS(srv *http.Server, ln net.Listener, cancel context.CancelFunc) {
	l.mu.Lock()
	if l.TLS == ln {
		l.TLSServer, l.TLS, l.tlsCancel, l.boundTLS, l.boundTLSCfg = nil, nil, nil, "", nil
	}
	l.retired[ln] = true
	l.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
	if ln != nil {
		_ = ln.Close()
	}
	if cancel != nil {
		cancel()
	}
}

// trackedListener remembers the connections it accepted so that retiring the
// listener ends them too. The plaintext http.Server is shared with the local
// socket and cannot be Closed for one listener's sake; without this, an SSE
// stream on a port that was switched off would live on.
type trackedListener struct {
	net.Listener
	mu     sync.Mutex
	conns  map[*trackedConn]struct{}
	closed bool
}

func newTrackedListener(ln net.Listener) *trackedListener {
	return &trackedListener{Listener: ln, conns: map[*trackedConn]struct{}{}}
}

type trackedConn struct {
	net.Conn
	t *trackedListener
}

func (c *trackedConn) Close() error {
	c.t.mu.Lock()
	delete(c.t.conns, c)
	c.t.mu.Unlock()
	return c.Conn.Close()
}

func (t *trackedListener) Accept() (net.Conn, error) {
	c, err := t.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := &trackedConn{Conn: c, t: t}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = c.Close()
		return nil, net.ErrClosed
	}
	t.conns[tc] = struct{}{}
	t.mu.Unlock()
	return tc, nil
}

// closeAll closes the listener and every connection it accepted.
func (t *trackedListener) closeAll() {
	t.mu.Lock()
	t.closed = true
	conns := make([]*trackedConn, 0, len(t.conns))
	for c := range t.conns {
		conns = append(conns, c)
	}
	t.mu.Unlock()
	_ = t.Listener.Close()
	for _, c := range conns {
		_ = c.Conn.Close()
	}
}
