//go:build !windows

package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"knomit/internal/config"
	"knomit/internal/serveraddr"
)

// The reload contract (Listeners.Apply / Reload): which listeners follow a
// changed configuration, that a failed bind leaves the previous listener
// serving, which keys are reported as needing a restart, and when
// server.json's generation advances. Every address check is a re-bind of the
// address, never an inspection of our own bookkeeping alone.

// reloadRig is an opened, serving listener set whose Serve hook serves the
// listeners Apply binds, as cmd does.
type reloadRig struct {
	f      fixture
	ls     *Listeners
	lock   string
	mu     sync.Mutex
	failed []string // Serve errors that were NOT a retirement
	served map[string]int
}

func sseHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: hello\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // ends when the connection is closed
	})
	return mux
}

// newRig opens f's listeners (boot serving included) and advertises.
func newRig(t *testing.T, f fixture) *reloadRig {
	t.Helper()
	prevLevel := zerolog.GlobalLevel() // a reload applies [log].level to the process
	t.Cleanup(func() { zerolog.SetGlobalLevel(prevLevel) })
	r := &reloadRig{f: f, lock: filepath.Join(f.cfg.Home, "server.json"), served: map[string]int{}}
	var ls *Listeners
	serve := func(what string, srv *http.Server, ln net.Listener) {
		r.mu.Lock()
		r.served[what]++
		r.mu.Unlock()
		go func() {
			err := srv.Serve(ln)
			if err == nil || errors.Is(err, http.ErrServerClosed) || ls.Retired(ln) {
				return
			}
			r.mu.Lock()
			r.failed = append(r.failed, what+": "+err.Error())
			r.mu.Unlock()
		}()
	}
	opened, err := OpenListeners(context.Background(), ListenerOptions{
		Config: f.cfg, Handler: sseHandler(), Base: context.Background(), KeyPath: f.keyPath,
		Version: "9.9.9-test", LockPath: r.lock, Serve: serve,
	})
	if err != nil {
		t.Fatalf("OpenListeners: %v", err)
	}
	ls = opened
	r.ls = ls
	t.Cleanup(ls.Close)
	if err := ls.Advertise(); err != nil {
		t.Fatal(err)
	}
	if ls.TCP != nil {
		serve("listen", ls.HTTP, ls.TCP)
	}
	if ls.Local != nil {
		serve("local", ls.HTTP, ls.Local)
	}
	if ls.TLSServer != nil {
		serve("tls listener", ls.TLSServer, ls.TLS)
	}
	return r
}

func (r *reloadRig) lockInfo(t *testing.T) serveraddr.LockInfo {
	t.Helper()
	info, err := serveraddr.ReadLock(r.lock)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func (r *reloadRig) requireNoServeFailure(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.failed) > 0 {
		t.Fatalf("a listener's Serve failed (a retired listener must read as a clean stop): %v", r.failed)
	}
}

func get(t *testing.T, addr string) string {
	t.Helper()
	c := http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get("http://" + addr + "/ok")
	if err != nil {
		t.Fatalf("GET %s: %v", addr, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func requireBound(t *testing.T, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		ln.Close()
		t.Fatalf("%s is free, but a listener should be bound there", addr)
	}
}

func (f fixture) next(mut func(*config.Config)) config.Config {
	c := f.cfg
	mut(&c)
	return c
}

func TestApply_HTTPOnToOff_DropsTheListenerAndItsStreams(t *testing.T) {
	f := newFixture(t)
	r := newRig(t, f)
	addr := f.cfg.Host + ":" + f.cfg.Port
	if got := get(t, addr); got != "ok" { // positive control: it serves before
		t.Fatalf("before the reload: %q", got)
	}
	// An in-flight SSE stream on the plaintext port.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /stream HTTP/1.1\r\nHost: localhost\r\n\r\n")
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream did not start: %v", err)
		}
		if strings.HasPrefix(line, "data: hello") {
			break
		}
	}

	res := r.ls.Apply(f.next(func(c *config.Config) { c.HTTP = false }))

	if !slices.Equal(res.Applied, []string{"http"}) || len(res.Failed) != 0 {
		t.Fatalf("result %+v", res)
	}
	requireFree(t, addr) // closed AND released, not just marked off
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var rerr error
	for rerr == nil {
		_, rerr = br.ReadByte()
	}
	var ne net.Error
	if errors.As(rerr, &ne) && ne.Timeout() {
		t.Fatalf("the SSE stream on the retired listener is still open (err %v)", rerr)
	}
	// The socket shares the http.Server and must still answer.
	c, err := net.Dial("unix", f.cfg.Socket)
	if err != nil {
		t.Fatalf("local listener was dropped with the TCP one: %v", err)
	}
	c.Close()

	info := r.lockInfo(t)
	if info.Generation != 2 || info.HTTP != "" || info.Port != 0 {
		t.Fatalf("server.json after http off: %+v", info)
	}
	if got := info.Listeners["http"]; got.Reason != ListenerReasonOff || got.Listening != "" {
		t.Fatalf("listeners.http = %+v", got)
	}
	r.requireNoServeFailure(t)
}

func TestApply_HTTPOffToOn_Binds(t *testing.T) {
	f := newFixture(t)
	free := freeAddr(t)
	f.cfg.HTTP = false
	r := newRig(t, f)
	if s := r.ls.Snapshot(); s.TCPAddr != "" || s.HTTP.Reason != ListenerReasonOff {
		t.Fatalf("boot with http = false: %+v", s)
	}
	host, port, _ := net.SplitHostPort(free)

	res := r.ls.Apply(f.next(func(c *config.Config) { c.HTTP, c.Host, c.Port = true, host, port }))

	if !slices.Equal(res.Applied, []string{"http"}) || len(res.Failed) != 0 {
		t.Fatalf("result %+v", res)
	}
	if got := get(t, free); got != "ok" {
		t.Fatalf("new listener answered %q", got)
	}
	info := r.lockInfo(t)
	if info.Generation != 2 || info.HTTP != "http://"+free || info.Listeners["http"].Listening != free {
		t.Fatalf("server.json after http on: %+v", info)
	}
	r.requireNoServeFailure(t)
}

func TestApply_PortChangeRebinds(t *testing.T) {
	f := newFixture(t)
	r := newRig(t, f)
	oldAddr := f.cfg.Host + ":" + f.cfg.Port
	newAddr := freeAddr(t)
	_, port, _ := net.SplitHostPort(newAddr)

	res := r.ls.Apply(f.next(func(c *config.Config) { c.Port = port }))

	if !slices.Equal(res.Applied, []string{"http"}) {
		t.Fatalf("result %+v", res)
	}
	requireFree(t, oldAddr)
	if got := get(t, newAddr); got != "ok" {
		t.Fatalf("new port answered %q", got)
	}
	if info := r.lockInfo(t); info.Port == 0 || info.HTTP != "http://"+newAddr {
		t.Fatalf("server.json: %+v", info)
	}
	r.requireNoServeFailure(t)
}

// A host change on the SAME port collides with the socket still open there;
// the old listener is closed, the new one bound.
func TestApply_HostChangeOnTheSamePortRebinds(t *testing.T) {
	f := newFixture(t)
	r := newRig(t, f)
	addr := f.cfg.Host + ":" + f.cfg.Port

	res := r.ls.Apply(f.next(func(c *config.Config) { c.Host = "localhost" }))

	if !slices.Equal(res.Applied, []string{"http"}) || len(res.Failed) != 0 {
		t.Fatalf("result %+v", res)
	}
	if s := r.ls.Snapshot(); s.HTTP.Configured != "localhost:"+f.cfg.Port || s.TCPAddr == "" {
		t.Fatalf("state after host change: %+v", s)
	}
	if got := get(t, addr); got != "ok" { // localhost binds 127.0.0.1 here
		t.Fatalf("answered %q", got)
	}
	r.requireNoServeFailure(t)
}

func TestApply_BindFailureKeepsThePreviousListener(t *testing.T) {
	f := newFixture(t)
	r := newRig(t, f)
	oldAddr := f.cfg.Host + ":" + f.cfg.Port
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, heldPort, _ := net.SplitHostPort(held.Addr().String())

	res := r.ls.Apply(f.next(func(c *config.Config) { c.Port = heldPort }))

	if len(res.Applied) != 0 || res.Failed["http"] == "" {
		t.Fatalf("result %+v", res)
	}
	if got := get(t, oldAddr); got != "ok" {
		t.Fatalf("the previous listener stopped serving: %q", got)
	}
	s := r.ls.Snapshot()
	if s.TCPAddr != oldAddr || s.HTTP.Reason != ListenerReasonAddrInUse || s.HTTP.Configured != "127.0.0.1:"+heldPort {
		t.Fatalf("state after the failed rebind: %+v", s)
	}
	info := r.lockInfo(t)
	if info.Port == 0 || info.Listeners["http"].Reason != ListenerReasonAddrInUse || info.Listeners["http"].Listening != oldAddr {
		t.Fatalf("server.json must say the old listener still serves and why: %+v", info)
	}
	r.requireNoServeFailure(t)

	// Releasing the port and reloading again recovers.
	held.Close()
	res = r.ls.Apply(f.next(func(c *config.Config) { c.Port = heldPort }))
	if !slices.Equal(res.Applied, []string{"http"}) || len(res.Failed) != 0 {
		t.Fatalf("retry after the port was released: %+v", res)
	}
	if got := get(t, "127.0.0.1:"+heldPort); got != "ok" {
		t.Fatalf("answered %q", got)
	}
}

func TestApply_HTTPOffWithoutALocalListenerIsRefused(t *testing.T) {
	f := newFixture(t)
	r := newRig(t, f)
	r.ls.mu.Lock()
	r.ls.Local = nil // as if this process never bound the socket
	r.ls.mu.Unlock()
	addr := f.cfg.Host + ":" + f.cfg.Port

	res := r.ls.Apply(f.next(func(c *config.Config) { c.HTTP = false }))

	if res.Failed["http"] == "" || len(res.Applied) != 0 {
		t.Fatalf("result %+v", res)
	}
	if got := get(t, addr); got != "ok" {
		t.Fatal("the only listener was dropped")
	}
	if s := r.ls.Snapshot(); s.HTTP.Reason != ListenerReasonNoRoute {
		t.Fatalf("state %+v", s)
	}
}

func TestApply_TLSAddAndDrop(t *testing.T) {
	f := newFixture(t)
	f.withCert(t) // certificate installed, [tls].addr set
	tlsAddr := f.cfg.TLS.Addr
	f.cfg.TLS.Addr = "" // booted with the listener off
	r := newRig(t, f)
	if s := r.ls.Snapshot(); s.TLSAddr != "" {
		t.Fatalf("boot: %+v", s)
	}

	// The bundle was installed after boot (fe172776): reload starts it.
	res := r.ls.Apply(f.next(func(c *config.Config) { c.TLS.Addr = tlsAddr }))
	if !slices.Equal(res.Applied, []string{"tls"}) || len(res.Failed) != 0 {
		t.Fatalf("add: %+v", res)
	}
	requireBound(t, tlsAddr)
	if info := r.lockInfo(t); info.TLS != tlsAddr || info.Listeners["tls"].Listening != tlsAddr || info.Generation != 2 {
		t.Fatalf("server.json after tls add: %+v", info)
	}

	res = r.ls.Apply(f.next(func(c *config.Config) { c.TLS.Addr = "" }))
	if !slices.Equal(res.Applied, []string{"tls"}) {
		t.Fatalf("drop: %+v", res)
	}
	requireFree(t, tlsAddr)
	if info := r.lockInfo(t); info.TLS != "" || info.Generation != 3 {
		t.Fatalf("server.json after tls drop: %+v", info)
	}
	r.requireNoServeFailure(t)
}

func TestApply_TLSAddrWithoutACertificateIsReported(t *testing.T) {
	f := newFixture(t) // no certificate installed
	r := newRig(t, f)
	tlsAddr := freeAddr(t)

	res := r.ls.Apply(f.next(func(c *config.Config) { c.TLS.Addr = tlsAddr }))

	if res.Failed["tls"] == "" || len(res.Applied) != 0 {
		t.Fatalf("result %+v", res)
	}
	requireFree(t, tlsAddr)
	if got := r.lockInfo(t).Listeners["tls"]; got.Reason != ListenerReasonNoCertificate || got.Configured != tlsAddr || got.Listening != "" {
		t.Fatalf("listeners.tls = %+v", got)
	}
	// What GET /fleet's host.listening reads (PR #528 B1).
	if l, why := r.ls.TLSListening(); l != "" || why != ListenerReasonNoCertificate {
		t.Fatalf("TLSListening = %q, %q; want not listening, no_certificate", l, why)
	}
}

func TestApply_TLSRebindMovesTheListenerAndKeepsTheOldOnFailure(t *testing.T) {
	f := newFixture(t)
	f.withCert(t)
	r := newRig(t, f)
	oldAddr := f.cfg.TLS.Addr
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	res := r.ls.Apply(f.next(func(c *config.Config) { c.TLS.Addr = held.Addr().String() }))
	if res.Failed["tls"] == "" {
		t.Fatalf("a held tls address must fail: %+v", res)
	}
	requireBound(t, oldAddr) // still serving on the old address
	if got := r.lockInfo(t).Listeners["tls"]; got.Reason != ListenerReasonAddrInUse || got.Listening != oldAddr {
		t.Fatalf("listeners.tls = %+v", got)
	}

	newAddr := freeAddr(t)
	res = r.ls.Apply(f.next(func(c *config.Config) { c.TLS.Addr = newAddr }))
	if !slices.Equal(res.Applied, []string{"tls"}) {
		t.Fatalf("rebind: %+v", res)
	}
	requireFree(t, oldAddr)
	requireBound(t, newAddr)
	if l, why := r.ls.TLSListening(); l != newAddr || why != "" {
		t.Fatalf("TLSListening = %q, %q; want %s", l, why, newAddr)
	}
	r.requireNoServeFailure(t)
}

func TestApply_LogLevelIsLive(t *testing.T) {
	prev := zerolog.GlobalLevel()
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })
	f := newFixture(t)
	r := newRig(t, f)
	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	res := r.ls.Apply(f.next(func(c *config.Config) { c.Log.Level = "debug" }))
	if !slices.Equal(res.Applied, []string{"log.level"}) || len(res.RestartNeeded) != 0 {
		t.Fatalf("result %+v", res)
	}
	if zerolog.GlobalLevel() != zerolog.DebugLevel {
		t.Fatalf("level = %v", zerolog.GlobalLevel())
	}
	// Same level again: nothing to apply.
	res = r.ls.Apply(f.next(func(c *config.Config) { c.Log.Level = "debug" }))
	if len(res.Applied) != 0 {
		t.Fatalf("an unchanged level was reported applied: %+v", res)
	}
	res = r.ls.Apply(f.next(func(c *config.Config) { c.Log.Level = "nonsense" }))
	if res.Failed["log.level"] == "" || zerolog.GlobalLevel() != zerolog.DebugLevel {
		t.Fatalf("a bad level must be refused and leave the level alone: %+v / %v", res, zerolog.GlobalLevel())
	}
}

func TestApply_RestartNeededKeysAreListedExactly(t *testing.T) {
	prev := zerolog.GlobalLevel()
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })
	f := newFixture(t)
	r := newRig(t, f)
	addr := f.cfg.Host + ":" + f.cfg.Port

	// Positive control: reloadable changes are NOT restart-needed.
	res := r.ls.Apply(f.next(func(c *config.Config) { c.Log.Level = "warn" }))
	if len(res.RestartNeeded) != 0 {
		t.Fatalf("log.level listed as restart-only: %v", res.RestartNeeded)
	}

	res = r.ls.Apply(f.next(func(c *config.Config) {
		c.Log.Level = "warn"
		c.Socket = c.Socket + ".other"
		c.Auth.Require = true
		c.Auth.LoopbackHosts = []string{"kb.example"}
		c.OAuth.Issuer = "https://issuer.example"
		c.Runtime.Addr = "127.0.0.1:1"
		c.LLM.APIKey = "secret-value"
	}))
	want := []string{"auth.loopback_hosts", "auth.require", "llm.api_key", "oauth.issuer", "runtime.addr", "socket"}
	if !slices.Equal(res.RestartNeeded, want) {
		t.Fatalf("restart_needed = %v\nwant            %v", res.RestartNeeded, want)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("a restart-only change applied something: %+v", res)
	}
	info := r.lockInfo(t)
	if !slices.Equal(info.RestartNeeded, want) {
		t.Fatalf("server.json restart_needed = %v", info.RestartNeeded)
	}
	raw, _ := os.ReadFile(r.lock)
	if strings.Contains(string(raw), "secret-value") {
		t.Fatal("a config VALUE reached server.json; only key names may")
	}
	if got := get(t, addr); got != "ok" { // and nothing was disturbed
		t.Fatalf("answered %q", got)
	}

	// The list is relative to the running config, so it persists until
	// the restart: reloading the same file again still reports it.
	res = r.ls.Apply(f.next(func(c *config.Config) { c.Log.Level = "warn"; c.Socket = c.Socket + ".other" }))
	if !slices.Equal(res.RestartNeeded, []string{"socket"}) {
		t.Fatalf("second reload: %v", res.RestartNeeded)
	}
	// And undoing the change clears it.
	res = r.ls.Apply(f.next(func(c *config.Config) { c.Log.Level = "warn" }))
	if len(res.RestartNeeded) != 0 || len(r.lockInfo(t).RestartNeeded) != 0 {
		t.Fatalf("restart_needed not cleared: %v", res.RestartNeeded)
	}
}

// A bind host that is a DNS name the Host-header allowlist (built at boot
// from the boot host) does not know is bound but not accepted in Host
// headers until a restart: say so.
func TestApply_ADnsBindHostNeedsARestartForTheHostAllowlist(t *testing.T) {
	f := newFixture(t)
	r := newRig(t, f)
	res := r.ls.Apply(f.next(func(c *config.Config) { c.Host = "kb.example" }))
	if !slices.Contains(res.RestartNeeded, "host") {
		t.Fatalf("restart_needed = %v", res.RestartNeeded)
	}
	// Positive control: an IP literal needs no allowlist entry.
	r.ls.Apply(f.cfg)
	res = r.ls.Apply(f.next(func(c *config.Config) { c.Host = "0.0.0.0" }))
	if slices.Contains(res.RestartNeeded, "host") {
		t.Fatalf("an IP-literal bind host is listed as restart-only: %v", res.RestartNeeded)
	}
}

func TestReload_GenerationAdvancesOnlyWhenTheConfigLoads(t *testing.T) {
	f := newFixture(t)
	r := newRig(t, f)
	before, _ := os.ReadFile(r.lock)
	addr := f.cfg.Host + ":" + f.cfg.Port

	_, err := r.ls.Reload(func() (config.Config, error) { return config.Config{}, errors.New("toml: line 3: bad") })
	if err == nil {
		t.Fatal("a load error must be returned")
	}
	after, _ := os.ReadFile(r.lock)
	if r.ls.Snapshot().Generation != 1 || r.lockInfo(t).Generation != 1 {
		t.Fatalf("a parse error advanced the generation:\n%s\n%s", before, after)
	}
	if !strings.Contains(r.lockInfo(t).ReloadError, "line 3") { // the only change: the recorded reason
		t.Fatalf("the error is not in server.json:\n%s", after)
	}
	if got := get(t, addr); got != "ok" {
		t.Fatal("a parse error disturbed the running listener")
	}

	// Positive control: the same path with a good config advances it.
	res, err := r.ls.Reload(func() (config.Config, error) { return f.cfg, nil })
	if err != nil || res.Generation != 2 || r.lockInfo(t).Generation != 2 {
		t.Fatalf("reload of a good config: %+v %v", res, err)
	}
}

func TestApply_DoesNotOverwriteASuccessorsLockfile(t *testing.T) {
	f := newFixture(t)
	r := newRig(t, f)
	other := serveraddr.LockInfo{PID: os.Getpid() + 1, Port: 7}
	if err := serveraddr.WriteLock(r.lock, other); err != nil {
		t.Fatal(err)
	}
	res := r.ls.Apply(f.next(func(c *config.Config) { c.Log.Level = "warn" }))
	if res.LockErr == nil {
		t.Fatal("rewriting someone else's server.json must be refused")
	}
	if got := r.lockInfo(t); got.PID != other.PID || got.Port != 7 {
		t.Fatalf("a successor's server.json was overwritten: %+v", got)
	}
}

func TestApply_OnAddressWhenTheLocalListenerIsNotOurs(t *testing.T) {
	f := newFixture(t)
	f.cfg.HTTP = false
	var mu sync.Mutex
	var got []string
	r := newRigOpts(t, f, func(o *ListenerOptions) {
		o.OnAddress = func(a string) { mu.Lock(); got = append(got, a); mu.Unlock() }
	})
	r.ls.mu.Lock()
	r.ls.Local = nil
	r.ls.mu.Unlock()
	free := freeAddr(t)
	host, port, _ := net.SplitHostPort(free)
	r.ls.Apply(f.next(func(c *config.Config) { c.HTTP, c.Host, c.Port = true, host, port }))
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != serveraddr.ForTCP(free) {
		t.Fatalf("OnAddress calls = %v", got)
	}
}

// newRigOpts is newRig with the options adjusted before opening.
func newRigOpts(t *testing.T, f fixture, adjust func(*ListenerOptions)) *reloadRig {
	t.Helper()
	prevLevel := zerolog.GlobalLevel() // a reload applies [log].level to the process
	t.Cleanup(func() { zerolog.SetGlobalLevel(prevLevel) })
	r := &reloadRig{f: f, lock: filepath.Join(f.cfg.Home, "server.json"), served: map[string]int{}}
	var ls *Listeners
	o := ListenerOptions{
		Config: f.cfg, Handler: sseHandler(), Base: context.Background(), KeyPath: f.keyPath,
		Version: "9.9.9-test", LockPath: r.lock,
		Serve: func(what string, srv *http.Server, ln net.Listener) {
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !ls.Retired(ln) {
					r.mu.Lock()
					r.failed = append(r.failed, err.Error())
					r.mu.Unlock()
				}
			}()
		},
	}
	adjust(&o)
	var err error
	if ls, err = OpenListeners(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	r.ls = ls
	t.Cleanup(ls.Close)
	if err := ls.Advertise(); err != nil {
		t.Fatal(err)
	}
	return r
}

// Apply racing Close and itself must not race (go test -race) and must not
// leave a listener bound after Close returns.
func TestApply_ConcurrentWithCloseLeavesNothingBound(t *testing.T) {
	f := newFixture(t)
	r := newRig(t, f)
	addrs := []string{freeAddr(t), freeAddr(t), freeAddr(t)}
	var wg sync.WaitGroup
	for _, a := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, port, _ := net.SplitHostPort(a)
			r.ls.Apply(f.next(func(c *config.Config) { c.Port = port }))
		}()
	}
	r.ls.Close()
	wg.Wait()
	for _, a := range append(addrs, f.cfg.Host+":"+f.cfg.Port) {
		requireFree(t, a)
	}
}

func TestApply_DiffReachesEveryConfigField(t *testing.T) {
	// changedKeys must see a change in a deep field of a struct the diff
	// recurses into, and must ignore reloadable keys: a guard against the
	// recursion being cut short or the exclusion list drifting.
	a := config.Defaults()
	b := a
	b.Git.Port = "1"
	b.Port = "2"
	b.TLS.Addr = "x:1"
	b.TLS.Dir = "d"
	b.Log.Level = "debug"
	b.Log.File = "f"
	got := changedKeys(a, b)
	if !slices.Equal(got, []string{"git.port", "log.file"}) {
		t.Fatalf("changedKeys = %v", got)
	}
}

func TestReload_ErrorIsRecordedInServerJSONAndClearedByTheNextGoodReload(t *testing.T) {
	f := newFixture(t)
	r := newRig(t, f)
	if _, err := r.ls.Reload(func() (config.Config, error) { return config.Config{}, errors.New("bad toml") }); err == nil {
		t.Fatal("want the error")
	}
	info := r.lockInfo(t)
	if info.Generation != 1 || !strings.Contains(info.ReloadError, "bad toml") {
		t.Fatalf("server.json after a failed load: %+v", info)
	}
	first := info.ReloadError
	time.Sleep(2 * time.Millisecond)
	_, _ = r.ls.Reload(func() (config.Config, error) { return config.Config{}, errors.New("bad toml") })
	if again := r.lockInfo(t).ReloadError; again == first {
		t.Fatal("an identical error must still read as a new one (timestamped)")
	}
	if _, err := r.ls.Reload(func() (config.Config, error) { return f.cfg, nil }); err != nil {
		t.Fatal(err)
	}
	if info := r.lockInfo(t); info.Generation != 2 || info.ReloadError != "" {
		t.Fatalf("after a good reload: %+v", info)
	}
}

// The shipped default bind host is the DNS name "localhost", which the boot
// allowlist holds. Moving off it to an IP literal (the desktop Settings' "All
// networks", then back to "This computer only") loses no name, so it is
// applied live and is NOT listed as needing a restart. A DNS name the
// allowlist lacks still is.
func TestApply_LeavingTheDefaultLocalhostNameNeedsNoRestart(t *testing.T) {
	f := newFixture(t)
	f.cfg.Host = "localhost"
	r := newRig(t, f)
	for _, host := range []string{"0.0.0.0", "127.0.0.1", "localhost", "0.0.0.0"} {
		res := r.ls.Apply(f.next(func(c *config.Config) { c.Host = host }))
		if slices.Contains(res.RestartNeeded, "host") {
			t.Fatalf("host -> %q is listed as restart-only: %v", host, res.RestartNeeded)
		}
	}
	// Positive control: a DNS name the boot allowlist does not know.
	res := r.ls.Apply(f.next(func(c *config.Config) { c.Host = "kb.example" }))
	if !slices.Contains(res.RestartNeeded, "host") {
		t.Fatalf("an unknown DNS bind host must still be listed: %v", res.RestartNeeded)
	}
}
