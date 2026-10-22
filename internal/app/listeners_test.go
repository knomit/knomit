//go:build !windows

package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki/pkitest"
	"knomit/internal/foundation/serveraddr"
)

// The listener set's contract: the order it binds in, that every failure
// leaves every address re-bindable, that http = false binds no TCP, how
// --supervised changes which failures are fatal, and that server.json is
// written last, by Advertise, for its own home only.

// fixture is a config whose every address is free and whose socket path is
// short enough for sun_path.
type fixture struct {
	cfg     config.Config
	keyPath string
	tlsAddr string
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kls") // short: sun_path is ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	cfg.Socket = filepath.Join(dir, "k.sock")
	httpAddr := freeAddr(t)
	host, port, _ := net.SplitHostPort(httpAddr)
	cfg.Host, cfg.Port = host, port
	cfg.TLS.Dir = filepath.Join(cfg.Home, "pki")
	return fixture{cfg: cfg, keyPath: filepath.Join(dir, "no-key"), tlsAddr: freeAddr(t)}
}

// withCert installs an instance certificate (wrapping the key at f.keyPath)
// and turns the TLS listener on at f.tlsAddr.
func (f *fixture) withCert(t *testing.T) {
	t.Helper()
	keyPath, _ := pkitest.NewKey(t)
	fleet := pkitest.New(t)
	fleet.Install(t, fleet.Enroll(t, "serve@agents.knomit.io", keyPath), f.cfg.TLS.Dir)
	f.keyPath = keyPath
	f.cfg.TLS.Addr = f.tlsAddr
}

func (f fixture) open(t *testing.T, supervised bool) (*Listeners, error) {
	t.Helper()
	return OpenListeners(context.Background(), ListenerOptions{
		Config:     f.cfg,
		Handler:    http.NotFoundHandler(),
		Base:       context.Background(),
		KeyPath:    f.keyPath,
		Version:    "9.9.9-test",
		Supervised: supervised,
		LockPath:   filepath.Join(f.cfg.Home, "server.json"),
	})
}

// requireFree asserts addr can be bound right now: the listener a failed
// Open opened was closed, not leaked to the garbage collector.
func requireFree(t *testing.T, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("%s is still bound after the failed Open: %v", addr, err)
	}
	ln.Close()
}

// requireSocketFree asserts the local path can be taken by a successor: the
// listener is closed AND the liveness lock released.
func requireSocketFree(t *testing.T, path string) {
	t.Helper()
	ln, cleanup, err := auth.ListenLocal(path)
	if err != nil || ln == nil {
		t.Fatalf("the local path %s is still held after the failed Open: ln=%v err=%v", path, ln, err)
	}
	ln.Close()
	cleanup()
}

func requireNoLockfile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("server.json exists at %s (stat err %v): something advertised a server that is not serving", path, err)
	}
}

// Every fallible step undoes ALL before it. Each row fails at a different
// step and then asserts all three addresses are re-bindable and no
// server.json was written. Sabotage: dropping the TLS close on the lockfile
// failure path turns the "lockfile" row red; opening TLS before the require
// check without a close turns "require" red.
func TestOpen_EveryFailureReleasesEveryAddress(t *testing.T) {
	t.Run("tls address held (plain serve refuses)", func(t *testing.T) {
		f := newFixture(t)
		f.withCert(t)
		held, err := net.Listen("tcp", f.tlsAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		ls, err := f.open(t, false)
		if ls != nil || !errors.Is(err, ErrTLSAddrInUse) {
			t.Fatalf("Open = %v, %v; want ErrTLSAddrInUse", ls, err)
		}
		var ce *ListenerConfigError
		if errors.As(err, &ce) {
			t.Fatalf("a held address is an availability failure, not a ListenerConfigError: %v", err)
		}
		requireFree(t, f.cfg.Host+":"+f.cfg.Port)
		requireSocketFree(t, f.cfg.Socket)
		requireNoLockfile(t, filepath.Join(f.cfg.Home, "server.json"))
	})

	t.Run("tls trust failure: a certificate for another key", func(t *testing.T) {
		f := newFixture(t)
		f.withCert(t)
		f.keyPath, _ = pkitest.NewKey(t) // not the key the certificate wraps
		ls, err := f.open(t, false)
		var ce *ListenerConfigError
		if ls != nil || !errors.As(err, &ce) || errors.Is(err, ErrTLSAddrInUse) {
			t.Fatalf("Open = %v, %v; want a *ListenerConfigError that is not an in-use error", ls, err)
		}
		requireFree(t, f.cfg.Host+":"+f.cfg.Port)
		requireFree(t, f.tlsAddr)
		requireSocketFree(t, f.cfg.Socket)
	})

	t.Run("require refusal: the local path is held", func(t *testing.T) {
		f := newFixture(t)
		f.withCert(t)
		f.cfg.Auth.Require = true
		owner, release, err := auth.ListenLocal(f.cfg.Socket)
		if err != nil || owner == nil {
			t.Fatalf("owner: %v", err)
		}
		defer release()
		ls, err := f.open(t, false)
		if ls != nil || !errors.Is(err, auth.ErrSocketInUse) || !strings.Contains(err.Error(), "[auth].require = true") {
			t.Fatalf("Open = %v, %v; want the require refusal wrapping ErrSocketInUse", ls, err)
		}
		requireFree(t, f.cfg.Host+":"+f.cfg.Port)
		// The TLS listener is opened AFTER the require check, so a refusal
		// has no network-facing port to release.
		requireFree(t, f.tlsAddr)
	})

	t.Run("plaintext address held", func(t *testing.T) {
		f := newFixture(t)
		held, err := net.Listen("tcp", f.cfg.Host+":"+f.cfg.Port)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		ls, err := f.open(t, false)
		if ls != nil || !IsAddrInUse(err) || !strings.Contains(err.Error(), f.cfg.Port) {
			t.Fatalf("Open = %v, %v; want an address-in-use error naming the address", ls, err)
		}
		requireSocketFree(t, f.cfg.Socket)
	})

	t.Run("server.json cannot be written", func(t *testing.T) {
		f := newFixture(t)
		f.withCert(t)
		blocker := filepath.Join(f.cfg.Home, "file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ls, err := OpenListeners(context.Background(), ListenerOptions{
			Config: f.cfg, Handler: http.NotFoundHandler(), Base: context.Background(), KeyPath: f.keyPath,
			LockPath: filepath.Join(blocker, "server.json"), // a directory below a regular file
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if ls.TLS == nil || ls.TCP == nil || ls.Local == nil {
			t.Fatalf("fixture did not bind all three: tcp=%v local=%v tls=%v", ls.TCP, ls.Local, ls.TLS)
		}
		if err := ls.Advertise(); err == nil || !strings.Contains(err.Error(), "write lockfile") {
			t.Fatalf("Advertise = %v, want the lockfile failure", err)
		}
		ls.Close()
		requireFree(t, f.cfg.Host+":"+f.cfg.Port)
		requireFree(t, f.tlsAddr)
		requireSocketFree(t, f.cfg.Socket)
	})
}

// http = false binds NO TCP listener: the port stays free WHILE the server
// runs, the KNOMIT_SERVER address is the socket, and server.json says so
// (port 0, http "").
func TestOpen_HTTPFalseBindsNoTCP(t *testing.T) {
	f := newFixture(t)
	f.cfg.HTTP = false
	lock := filepath.Join(f.cfg.Home, "server.json")
	ls, err := f.open(t, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ls.Close()
	if ls.TCP != nil {
		t.Fatalf("http = false still bound %v", ls.TCP.Addr())
	}
	requireFree(t, f.cfg.Host+":"+f.cfg.Port) // free while the server is up
	if ls.Local == nil || ls.Address != "unix://"+f.cfg.Socket {
		t.Fatalf("Local=%v Address=%q, want the socket", ls.Local, ls.Address)
	}
	if ls.HTTPState.Reason != ListenerReasonOff {
		t.Fatalf("HTTPState = %+v", ls.HTTPState)
	}
	if err := ls.Advertise(); err != nil {
		t.Fatal(err)
	}
	info, err := serveraddr.ReadLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	if info.Port != 0 || info.HTTP != "" || info.Socket != f.cfg.Socket || info.Address != "unix://"+f.cfg.Socket {
		t.Fatalf("server.json = %+v, want port 0, http \"\", the socket", info)
	}
}

// http = false with nothing else bound serves nothing: refused, not a
// healthy-looking process.
func TestOpen_HTTPFalseAndNoLocalListenerIsRefused(t *testing.T) {
	f := newFixture(t)
	f.cfg.HTTP = false
	owner, release, err := auth.ListenLocal(f.cfg.Socket)
	if err != nil || owner == nil {
		t.Fatalf("owner: %v", err)
	}
	defer release()
	ls, err := f.open(t, false)
	if ls != nil || err == nil || !strings.Contains(err.Error(), "nothing to serve on") {
		t.Fatalf("Open = %v, %v; want the nothing-to-serve refusal", ls, err)
	}
}

// server.json is written by Advertise and by nothing before it, with the v2
// fields, and Close removes it only while it is still this process's.
func TestAdvertise_WritesV2LastAndCloseRemovesOnlyItsOwn(t *testing.T) {
	f := newFixture(t)
	f.withCert(t)
	lock := filepath.Join(f.cfg.Home, "server.json")
	ls, err := f.open(t, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	requireNoLockfile(t, lock) // every listener is bound; still no advertisement
	if err := ls.Advertise(); err != nil {
		t.Fatal(err)
	}
	info, err := serveraddr.ReadLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(f.cfg.Port)
	want := serveraddr.LockInfo{
		PID: os.Getpid(), Port: port, Version: "9.9.9-test", Schema: serveraddr.LockSchemaV2, Home: f.cfg.Home,
		HTTP: "http://" + f.cfg.Host + ":" + f.cfg.Port, Address: "unix://" + f.cfg.Socket,
		Socket: f.cfg.Socket, TLS: f.tlsAddr, Generation: 1,
		Listeners: map[string]serveraddr.LockListener{
			"http": {Configured: f.cfg.Host + ":" + f.cfg.Port, Listening: f.cfg.Host + ":" + f.cfg.Port},
			"tls":  {Configured: f.tlsAddr, Listening: f.tlsAddr},
		},
	}
	if !reflect.DeepEqual(info, want) {
		t.Fatalf("server.json = %+v\nwant          %+v", info, want)
	}

	// Another server has since written its own file: ours must not delete it.
	if err := serveraddr.WriteLock(lock, serveraddr.LockInfo{PID: os.Getpid() + 1, Port: 1}); err != nil {
		t.Fatal(err)
	}
	ls.Close()
	if got, err := serveraddr.ReadLock(lock); err != nil || got.PID != os.Getpid()+1 {
		t.Fatalf("Close removed a successor's server.json: %+v %v", got, err)
	}
	requireFree(t, f.cfg.Host+":"+f.cfg.Port)
	requireFree(t, f.tlsAddr)
	requireSocketFree(t, f.cfg.Socket)
}

func TestClose_RemovesItsOwnLockfile(t *testing.T) {
	f := newFixture(t)
	lock := filepath.Join(f.cfg.Home, "server.json")
	ls, err := f.open(t, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ls.Advertise(); err != nil {
		t.Fatal(err)
	}
	ls.Close()
	ls.Close() // idempotent
	requireNoLockfile(t, lock)
}

// The discovery file is keyed by home. Two servers with different data roots
// write different files, and neither overwrites the other's; the one that
// does not hold its home's local listener (another process does) writes
// nothing at all.
func TestAdvertise_DoesNotClobberAnotherHomesFile(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("HOME", stateRoot)
	t.Setenv("XDG_STATE_HOME", filepath.Join(stateRoot, "state"))
	def, err := config.DefaultHome()
	if err != nil {
		t.Fatal(err)
	}

	open := func(home string) *Listeners {
		f := newFixture(t)
		f.cfg.Home = home
		ls, err := OpenListeners(context.Background(), ListenerOptions{
			Config: f.cfg, Handler: http.NotFoundHandler(), Base: context.Background(), Version: "v",
		}) // LockPath empty: the home's own file
		if err != nil {
			t.Fatalf("Open(%s): %v", home, err)
		}
		t.Cleanup(ls.Close)
		return ls
	}
	a := open(def)
	b := open(t.TempDir())
	if a.LockPath() == b.LockPath() {
		t.Fatalf("two homes share one lockfile: %s", a.LockPath())
	}
	if err := a.Advertise(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(a.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Advertise(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(a.LockPath())
	if err != nil || string(after) != string(before) {
		t.Fatalf("the other home's Advertise changed the default home's server.json:\nbefore %s\nafter  %s (%v)", before, after, err)
	}
	bi, err := serveraddr.ReadLock(b.LockPath())
	if err != nil || bi.Home == "" || bi.Home == def {
		t.Fatalf("home b's file = %+v, %v", bi, err)
	}
	if !strings.HasPrefix(a.LockPath(), filepath.Join(stateRoot)) || filepath.Base(a.LockPath()) != "server.json" {
		t.Fatalf("the default home's file moved: %s", a.LockPath())
	}
}

func TestAdvertise_SkippedWhenAnotherProcessHoldsTheLocalListener(t *testing.T) {
	f := newFixture(t)
	owner, release, err := auth.ListenLocal(f.cfg.Socket)
	if err != nil || owner == nil {
		t.Fatalf("owner: %v", err)
	}
	defer release()
	ls, err := f.open(t, false)
	if err != nil {
		t.Fatalf("Open: %v (a held socket serves TCP only, unsupervised)", err)
	}
	defer ls.Close()
	if ls.Local != nil || ls.TCP == nil {
		t.Fatalf("want TCP only: local=%v tcp=%v", ls.Local, ls.TCP)
	}
	if ls.Address != "http://"+f.cfg.Host+":"+f.cfg.Port {
		t.Fatalf("Address = %q: a held socket must never be named", ls.Address)
	}
	if err := ls.Advertise(); err != nil {
		t.Fatal(err)
	}
	requireNoLockfile(t, filepath.Join(f.cfg.Home, "server.json"))
}

// --supervised: the local listener is mandatory, an address that is only
// HELD is a warning, a trust failure is not.
func TestOpen_Supervised(t *testing.T) {
	t.Run("socket held: ErrSocketInUse, bare, nothing left bound", func(t *testing.T) {
		f := newFixture(t)
		owner, release, err := auth.ListenLocal(f.cfg.Socket)
		if err != nil || owner == nil {
			t.Fatalf("owner: %v", err)
		}
		defer release()
		ls, err := f.open(t, true)
		var ce *ListenerConfigError
		if ls != nil || !errors.Is(err, auth.ErrSocketInUse) || errors.As(err, &ce) {
			t.Fatalf("Open = %v, %v; want ErrSocketInUse that is NOT a ListenerConfigError (the supervisor attaches)", ls, err)
		}
		requireFree(t, f.cfg.Host+":"+f.cfg.Port)
	})

	t.Run("socket held with require: still exit-3 shaped, not the require refusal's code", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.Auth.Require = true
		owner, release, err := auth.ListenLocal(f.cfg.Socket)
		if err != nil || owner == nil {
			t.Fatalf("owner: %v", err)
		}
		defer release()
		_, err = f.open(t, true)
		var ce *ListenerConfigError
		if !errors.Is(err, auth.ErrSocketInUse) || errors.As(err, &ce) {
			t.Fatalf("Open = %v; want a bare ErrSocketInUse", err)
		}
	})

	t.Run("plaintext address held: warn and serve without it", func(t *testing.T) {
		f := newFixture(t)
		held, err := net.Listen("tcp", f.cfg.Host+":"+f.cfg.Port)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		logs := captureLog(t)
		ls, err := f.open(t, true)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer ls.Close()
		if ls.TCP != nil || ls.Local == nil || ls.HTTPState.Reason != ListenerReasonAddrInUse {
			t.Fatalf("tcp=%v local=%v state=%+v", ls.TCP, ls.Local, ls.HTTPState)
		}
		if ls.Address != "unix://"+f.cfg.Socket {
			t.Fatalf("Address = %q", ls.Address)
		}
		if out := logs.String(); !strings.Contains(out, `"level":"warn"`) || !strings.Contains(out, f.cfg.Port) {
			t.Fatalf("want a WARN naming the address; got:\n%s", out)
		}
		if err := ls.Advertise(); err != nil {
			t.Fatal(err)
		}
		info, _ := serveraddr.ReadLock(filepath.Join(f.cfg.Home, "server.json"))
		if info.Port != 0 || info.HTTP != "" {
			t.Fatalf("server.json advertises a TCP listener that is not ours: %+v", info)
		}
	})

	t.Run("tls address held: warn and serve without it", func(t *testing.T) {
		f := newFixture(t)
		f.withCert(t)
		held, err := net.Listen("tcp", f.tlsAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		ls, err := f.open(t, true)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer ls.Close()
		if ls.TLS != nil || ls.TCP == nil || ls.Local == nil {
			t.Fatalf("tcp=%v local=%v tls=%v", ls.TCP, ls.Local, ls.TLS)
		}
		if ls.TLSState.Reason != ListenerReasonAddrInUse || ls.TLSState.Configured != f.tlsAddr {
			t.Fatalf("TLSState = %+v", ls.TLSState)
		}
	})

	t.Run("tls trust failure stays fatal", func(t *testing.T) {
		f := newFixture(t)
		f.withCert(t)
		f.keyPath, _ = pkitest.NewKey(t)
		_, err := f.open(t, true)
		var ce *ListenerConfigError
		if !errors.As(err, &ce) {
			t.Fatalf("Open = %v, want a *ListenerConfigError", err)
		}
	})

	t.Run("a socket path that cannot be a socket is a configuration error", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.Socket = "/tmp/" + strings.Repeat("l", 120) + "/knomit.sock"
		_, err := f.open(t, true)
		var ce *ListenerConfigError
		if !errors.As(err, &ce) || !errors.Is(err, auth.ErrPathTooLong) {
			t.Fatalf("Open = %v, want a *ListenerConfigError wrapping ErrPathTooLong", err)
		}
	})

	t.Run("the same overlong path unsupervised still serves TCP only", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.Socket = "/tmp/" + strings.Repeat("l", 120) + "/knomit.sock"
		ls, err := f.open(t, false)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer ls.Close()
		if ls.Local != nil || ls.TCP == nil {
			t.Fatalf("local=%v tcp=%v", ls.Local, ls.TCP)
		}
	})
}

func TestListenTCP_BindFailureIsAnError(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	ln, err := listenTCP(busy.Addr().String())
	if err == nil {
		ln.Close()
		t.Fatal("binding a taken address succeeded")
	}
	if !strings.Contains(err.Error(), busy.Addr().String()) {
		t.Fatalf("error must name the address: %v", err)
	}
}
