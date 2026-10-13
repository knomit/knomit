//go:build unix

package cmd

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"knomit/internal/config"
	"knomit/internal/serveraddr"
)

// A running `knomit serve` re-reads its configuration on SIGHUP and `knomit
// reload`: the whole RunE is the subject (the boot loader, the trigger, the
// listener set and server.json), because the contract is what a user sees.

func writeToml(t *testing.T, home, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "knomit.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *serveHarness) waitGeneration(t *testing.T, gen int) serveraddr.LockInfo {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := serveraddr.ReadLock(h.lock); err == nil && info.Generation >= gen {
			return info
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("server.json never reached generation %d", gen)
	return serveraddr.LockInfo{}
}

func hup(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
}

func httpOK(addr string) bool {
	c := http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get("http://" + addr + "/api/v1/version")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// The manual scenario of the brief, as a test: start with http = false, turn
// it on in the file, SIGHUP, and the port opens with generation 2; a bogus
// value keeps the server running and the generation where it was.
func TestServe_SIGHUPAppliesTheListenerConfigWithoutARestart(t *testing.T) {
	home := isolateServe(t)
	port := os.Getenv("KNOMIT_PORT")
	addr := "127.0.0.1:" + port
	writeToml(t, home, "http = false\n")
	h := startServe(t, home)
	info := h.waitServing()
	if info.Generation != 1 || info.HTTP != "" || !info.Reload {
		t.Fatalf("boot server.json = %+v (want generation 1, no http, reload true)", info)
	}
	if httpOK(addr) {
		t.Fatal("http = false but the port answers")
	}

	writeToml(t, home, "http = true\n")
	hup(t)
	info = h.waitGeneration(t, 2)
	if info.HTTP != "http://"+addr || info.Port == 0 {
		t.Fatalf("after reload server.json = %+v", info)
	}
	if !httpOK(addr) {
		t.Fatal("the port was not opened by the reload")
	}
	// One signal, one reload: the generation does not run on.
	time.Sleep(300 * time.Millisecond)
	if cur, _ := serveraddr.ReadLock(h.lock); cur.Generation != 2 {
		t.Fatalf("one SIGHUP produced generation %d", cur.Generation)
	}

	// A configuration that does not parse: ERROR, nothing changes.
	writeToml(t, home, "http = [this is not toml\n")
	hup(t)
	time.Sleep(500 * time.Millisecond)
	if cur, _ := serveraddr.ReadLock(h.lock); cur.Generation != 2 || cur.HTTP != "http://"+addr {
		t.Fatalf("a parse error changed server.json: %+v", cur)
	}
	if !httpOK(addr) {
		t.Fatal("a parse error stopped the server")
	}
	// A value that parses but does not validate is refused the same way.
	writeToml(t, home, "http = false\n[log]\nlevel = \"nonsense\"\n")
	hup(t)
	time.Sleep(500 * time.Millisecond)
	if cur, _ := serveraddr.ReadLock(h.lock); cur.Generation != 2 {
		t.Fatalf("a refused configuration advanced the generation: %+v", cur)
	}

	// Positive control: the next good file is applied by the same path.
	writeToml(t, home, "http = false\n")
	hup(t)
	info = h.waitGeneration(t, 3)
	if info.HTTP != "" {
		t.Fatalf("http = false was not applied: %+v", info)
	}
	if httpOK(addr) {
		t.Fatal("the port still answers after http = false")
	}
	h.cancel()
	if err := h.wait(15 * time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// installReloadSignal runs the callback exactly once per signal, one at a
// time (go test -race), and not at all after stop.
func TestInstallReloadSignal_OncePerSignal(t *testing.T) {
	var n atomic.Int32
	var running atomic.Int32
	var overlapped atomic.Bool
	got := make(chan struct{}, 8)
	stop, err := installReloadSignal(t.TempDir(), func() {
		if running.Add(1) > 1 {
			overlapped.Store(true)
		}
		n.Add(1)
		time.Sleep(20 * time.Millisecond)
		running.Add(-1)
		got <- struct{}{}
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		hup(t)
		select {
		case <-got:
		case <-time.After(5 * time.Second):
			t.Fatalf("signal %d never reached the callback", i)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n.Load() != 3 || overlapped.Load() {
		t.Fatalf("callback ran %d times for 3 signals (overlap %v)", n.Load(), overlapped.Load())
	}
	stop()
}

// `knomit reload` against a live server: it signals, waits for the
// generation, and prints what happened.
func TestReloadCmd_SignalsAndWaitsForTheGeneration(t *testing.T) {
	home := isolateServe(t)
	port := os.Getenv("KNOMIT_PORT")
	writeToml(t, home, "http = false\n")
	h := startServe(t, home)
	h.waitServing()

	writeToml(t, home, "http = true\n")
	var out bytes.Buffer
	if err := runReload(context.Background(), &out, 10*time.Second); err != nil {
		t.Fatalf("knomit reload: %v", err)
	}
	if !strings.Contains(out.String(), "reloaded (generation 2)") || !strings.Contains(out.String(), "127.0.0.1:"+port) {
		t.Fatalf("output:\n%s", out.String())
	}
	if !httpOK("127.0.0.1:" + port) {
		t.Fatal("reload did not open the port")
	}
	h.cancel()
	_ = h.wait(15 * time.Second)
}

// Never signal what is not a reload-capable server: SIGHUP's default action
// ends a process.
func TestReloadCmd_RefusesWhatItCannotVouchFor(t *testing.T) {
	home := isolateServe(t)
	lock := config.LockfilePathFor(home)
	pid := os.Getpid()

	// A file from a server that never said it listens.
	if err := serveraddr.WriteLock(lock, serveraddr.LockInfo{PID: pid, Schema: serveraddr.LockSchemaV2, Home: home, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	err := runReload(context.Background(), io.Discard, 0)
	if err == nil || !strings.Contains(err.Error(), "does not listen for reload") {
		t.Fatalf("a server without reload: %v", err)
	}

	// Says it listens, names a socket nothing answers on.
	dead := filepath.Join(t.TempDir(), "dead.sock")
	if err := serveraddr.WriteLock(lock, serveraddr.LockInfo{PID: pid, Schema: serveraddr.LockSchemaV2, Home: home, Reload: true, Socket: dead, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	err = runReload(context.Background(), io.Discard, 0)
	if err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("a stale file: %v", err)
	}

	// The other data root's file.
	if err := serveraddr.WriteLock(lock, serveraddr.LockInfo{PID: pid, Home: "/somewhere/else", Reload: true}); err != nil {
		t.Fatal(err)
	}
	if err := runReload(context.Background(), io.Discard, 0); err == nil || !strings.Contains(err.Error(), "belongs to the data root") {
		t.Fatalf("another home's file: %v", err)
	}

	// No server at all.
	os.Remove(lock)
	if err := runReload(context.Background(), io.Discard, 0); err == nil || !strings.Contains(err.Error(), "no running knomit serve") {
		t.Fatalf("no server: %v", err)
	}

	// Positive control for the unix sender: a dead pid is refused, not signalled.
	if err := serveraddr.SignalReload(home, 1<<22+12345); err == nil {
		t.Fatal("signalling a pid that does not exist must fail")
	}
}

// `knomit reload` exits non-zero when the reload completed but a listener did
// not come up, and shows a configuration error instead of timing out.
func TestReloadCmd_ExitStatusReflectsWhatHappened(t *testing.T) {
	home := isolateServe(t)
	port := os.Getenv("KNOMIT_PORT") // env wins over toml, on reload as at boot
	_ = home
	h := startServe(t, home)
	h.waitServing()

	// Bind failure: the generation advances (processed), the status is an error.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, heldPort, _ := net.SplitHostPort(held.Addr().String())
	t.Setenv("KNOMIT_PORT", heldPort)
	var out bytes.Buffer
	err = runReload(context.Background(), &out, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "addr_in_use") {
		t.Fatalf("a held port must be an error naming it, got %v", err)
	}
	if !strings.Contains(out.String(), "reloaded (generation 2)") {
		t.Fatalf("the outcome must still be printed:\n%s", out.String())
	}
	if !httpOK("127.0.0.1:" + port) {
		t.Fatal("the previous listener must keep serving")
	}

	// Config error: refused, shown, generation unchanged; twice in a row.
	t.Setenv("KNOMIT_PORT", port)
	writeToml(t, home, "http = [nope\n")
	for i := 0; i < 2; i++ {
		start := time.Now()
		err = runReload(context.Background(), io.Discard, 10*time.Second)
		if err == nil || !strings.Contains(err.Error(), "reload refused") || !strings.Contains(err.Error(), "toml") {
			t.Fatalf("round %d: a config error must be shown, got %v", i, err)
		}
		if time.Since(start) > 4*time.Second {
			t.Fatalf("round %d took %v: it waited for a timeout instead of the error", i, time.Since(start))
		}
		cur, _ := serveraddr.ReadLock(h.lock)
		if cur.Generation != 2 || cur.ReloadError == "" {
			t.Fatalf("round %d: server.json %+v", i, cur)
		}
	}

	// A good config succeeds (exit 0) and clears the error.
	writeToml(t, home, "\n")
	if err := runReload(context.Background(), io.Discard, 10*time.Second); err != nil {
		t.Fatalf("good reload: %v", err)
	}
	if cur, _ := serveraddr.ReadLock(h.lock); cur.Generation != 3 || cur.ReloadError != "" {
		t.Fatalf("after a good reload: %+v", cur)
	}
	h.cancel()
	_ = h.wait(15 * time.Second)
}
