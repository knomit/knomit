//go:build desktop

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"knomit/internal/domain/modelparams"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/config"
	"knomit/internal/foundation/serveraddr"
)

// externalServer stands in for a `knomit serve` the user started: it answers
// on the socket, and answer decides whether a version request gets a 200.
func externalServer(t *testing.T, socket, full string, answer func() bool) {
	t.Helper()
	ln, cleanup, err := auth.ListenLocal(socket)
	if err != nil {
		t.Fatal(err)
	}
	inner := versionHandler(full)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answer != nil && !answer() {
			http.Error(w, "booting", http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); cleanup() })
}

// Attach: a knomit already answers on the socket, so nothing is spawned; the
// UI is pointed at it; quitting leaves it running; "Restart server" refuses,
// naming its pid; and a different major.minor is warned about.
func TestSupervisor_AttachesWhenTheSocketAnswers(t *testing.T) {
	e, s := newFakeEnv(t, "ready")
	logs := captureLog(t)
	externalServer(t, e.socket, "0.9.1.cli", nil)
	if err := serveraddr.WriteLock(filepath.Join(e.home, "server.json"), serveraddr.LockInfo{PID: 4242, Version: "0.9.1.cli", Schema: serveraddr.LockSchemaV2}); err != nil {
		t.Fatal(err)
	}
	s.start(context.Background())
	waitReady(t, s)

	if got := e.spawns(t); len(got) != 0 {
		t.Fatalf("spawned %d server(s) although one answered on the socket", len(got))
	}
	if !s.isAttached() || e.up.get() != e.socket {
		t.Fatalf("attached=%v upstream=%q, want attached to %q", s.isAttached(), e.up.get(), e.socket)
	}
	if !logs.hasWarn("different major.minor", "server_version", "0.9.1.cli") {
		t.Errorf("no version-skew WARN for a 0.9 server under a 0.5 desktop:\n%s", logs.String())
	}
	err := s.restartServer(context.Background())
	var att errAttachedServer
	if !errors.As(err, &att) || att.pid != 4242 {
		t.Fatalf("restartServer while attached = %v, want the attached refusal naming pid 4242", err)
	}

	s.stop()
	if _, err := s.probe(context.Background()); err != nil {
		t.Fatalf("the attached server stopped answering after the desktop quit: %v", err)
	}
}

// Spawn: nothing answers, so the bundled binary is started as `knomit serve
// --supervised --log-file <server log>`, with this home and none of the five
// forbidden variables; quitting closes its stdin and it stops by itself.
func TestSupervisor_SpawnsThenStopsTheServerByClosingStdin(t *testing.T) {
	e, s := newFakeEnv(t, "ready")
	s.start(context.Background())
	waitReady(t, s)

	sp := e.spawns(t)
	if len(sp) != 1 {
		t.Fatalf("spawns = %d, want 1", len(sp))
	}
	want := filepath.Join(e.bundle, serverExecName+exeSuffix)
	if runtime.GOOS != "windows" {
		want, _ = filepath.EvalSymlinks(want) // the fake reports its resolved executable
	}
	if sp[0].Exe != want {
		t.Errorf("spawned %q, want the bundle's %q", sp[0].Exe, want)
	}
	if wantArgs := []string{"serve", "--supervised", "--log-file", e.logFile}; !slices.Equal(sp[0].Args, wantArgs) {
		t.Errorf("args = %v, want %v", sp[0].Args, wantArgs)
	}
	if sp[0].Home != e.home {
		t.Errorf("child KNOMIT_HOME = %q, want the desktop's home %q", sp[0].Home, e.home)
	}
	if len(sp[0].Forbidden) != 0 {
		t.Errorf("the supervisor added %v to the server's environment; each beats knomit.toml on reload", sp[0].Forbidden)
	}
	if s.isAttached() || e.up.get() != e.socket {
		t.Errorf("attached=%v upstream=%q after a spawn", s.isAttached(), e.up.get())
	}

	start := time.Now()
	s.stop()
	if took := time.Since(start); took >= s.quitGrace {
		t.Errorf("stop took %s: the server was killed, not stopped by stdin EOF", took)
	}
	if !strings.Contains(e.events(), "eof ") {
		t.Errorf("the server never saw stdin close: events %q", e.events())
	}
	waitGone(t, sp[0].PID, 5*time.Second)
}

// A server that ignores stdin EOF is killed once the grace runs out.
func TestSupervisor_QuitKillsAServerThatIgnoresStdin(t *testing.T) {
	e, s := newFakeEnv(t, "ignore-stdin")
	s.quitGrace = 400 * time.Millisecond
	s.start(context.Background())
	waitReady(t, s)
	pid := e.spawns(t)[0].PID

	start := time.Now()
	s.stop()
	took := time.Since(start)
	if took < s.quitGrace {
		t.Errorf("stop returned after %s, before the %s grace: the server was not given its chance", took, s.quitGrace)
	}
	if !strings.Contains(e.events(), "eof-ignored") {
		t.Errorf("stdin was not closed before the kill: events %q", e.events())
	}
	waitGone(t, pid, 5*time.Second)
}

// Exit 2 is a configuration or trust failure: no retry, a failed status that
// carries the server's own words.
func TestSupervisor_Exit2FailsWithoutRetry(t *testing.T) {
	e, s := newFakeEnv(t, "exit2")
	s.start(context.Background())
	st := waitFailed(t, s)
	if !strings.Contains(st.Error, "bad toml at line 3") {
		t.Errorf("failed status %q does not carry the server's stderr", st.Error)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(e.spawns(t)); n != 1 {
		t.Errorf("spawns = %d after exit 2, want exactly 1 (no retry)", n)
	}
	if d := e.backoffs(); len(d) != 0 {
		t.Errorf("backoff waits %v after exit 2, want none", d)
	}
}

// Crashes restart with 1, 2, 4, 8 s backoff; the fifth crash inside ten
// minutes stops the supervisor with the crash's tail.
func TestSupervisor_CrashBacksOffThenGivesUp(t *testing.T) {
	e, s := newFakeEnv(t, "crash")
	s.start(context.Background())
	st := waitFailed(t, s)
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}; !slices.Equal(e.backoffs(), want) {
		t.Errorf("backoff waits %v, want %v", e.backoffs(), want)
	}
	if n := len(e.spawns(t)); n != 5 {
		t.Errorf("spawns = %d, want 5 (gave up on the fifth crash)", n)
	}
	if !strings.Contains(st.Error, "panic: boom") {
		t.Errorf("failed status %q does not carry the crash's stderr", st.Error)
	}
}

// One crash, then a healthy server: the UI recovers after one backoff.
func TestSupervisor_RecoversAfterACrash(t *testing.T) {
	e, s := newFakeEnv(t, "crash,ready")
	s.start(context.Background())
	waitReady(t, s)
	if n := len(e.spawns(t)); n != 2 {
		t.Errorf("spawns = %d, want 2", n)
	}
	if want := []time.Duration{time.Second}; !slices.Equal(e.backoffs(), want) {
		t.Errorf("backoff waits %v, want %v", e.backoffs(), want)
	}
}

// Exit 3: the socket is held by another server that was still booting at the
// first probe. The supervisor attaches to it instead of retrying the spawn.
func TestSupervisor_Exit3SwitchesToAttach(t *testing.T) {
	e, s := newFakeEnv(t, "exit3")
	// The holder starts answering only once our spawn has been tried AND has
	// exited (the supervisor holds no child). Answering as soon as the spawn
	// was recorded let runChild's readiness probe reach the holder while the
	// doomed child was still alive, marking it ready as our own server before
	// exit 3 was seen; the fake's exit3 lingers so that window is real on
	// every platform, not only on Windows.
	externalServer(t, e.socket, "0.5.3.cli", func() bool {
		if len(e.spawns(t)) == 0 {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.child == nil
	})
	s.start(context.Background())
	waitReady(t, s)
	if !s.isAttached() {
		t.Fatal("ready but not attached after exit 3")
	}
	if n := len(e.spawns(t)); n != 1 {
		t.Errorf("spawns = %d, want 1", n)
	}
	if d := e.backoffs(); len(d) != 0 {
		t.Errorf("exit 3 was treated as a crash: backoff %v", d)
	}
}

// The exit-3 window in production: another server starts answering on the
// socket while our child, which lost the socket to it, is still alive. The
// readiness probe reaches that other server; it must NOT be reported as our
// own server being ready, and "Restart server" must end refused as attached,
// never report a restart that did not happen.
//
// Deterministic: the child (exit3-on-eof) stays alive until its stdin is
// closed, so the window is open for the whole first half of the test, and the
// holder counts the probes it answers. Before the fix the first answered
// probe made the supervisor ready as owner and no second probe came; after
// it, probes keep coming and ready is never set while our child is alive.
func TestSupervisor_HolderAnsweringBeforeExit3IsNotOwnedReady(t *testing.T) {
	e, s := newFakeEnv(t, "exit3-on-eof")
	// The holder's own server.json, as a `knomit serve` holding the socket
	// writes it.
	const holderPID = 4242
	if err := serveraddr.WriteLock(filepath.Join(e.home, "server.json"), serveraddr.LockInfo{PID: holderPID, Version: "0.5.3.cli", Schema: serveraddr.LockSchemaV2}); err != nil {
		t.Fatal(err)
	}
	var answered atomic.Int32
	externalServer(t, e.socket, "0.5.3.cli", func() bool {
		// Booting until our spawn is recorded (so the supervisor spawns),
		// then answering while our child is still alive.
		if len(e.spawns(t)) == 0 {
			return false
		}
		answered.Add(1)
		return true
	})
	s.start(context.Background())

	eventually(t, 30*time.Second, "owned-ready, or three probes answered by the holder", func() bool {
		return s.status().Ready || answered.Load() >= 3
	})
	s.mu.Lock()
	ready, attached, child := s.ready, s.attached, s.child
	s.mu.Unlock()
	if child == nil {
		t.Fatal("our child is gone before the window was examined; the test did not hold it open")
	}
	if ready {
		t.Fatalf("ready (attached=%v) while our own child is alive and the answering server is pid %d: "+
			"owned-ready was reported for a server that is not our child", attached, holderPID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := s.restartServer(ctx)
	var att errAttachedServer
	if !errors.As(err, &att) || att.pid != holderPID {
		t.Fatalf("restartServer in the exit-3 window = %v, want the attached refusal naming pid %d", err, holderPID)
	}
	if !s.isAttached() || !s.status().Ready {
		t.Fatalf("after the window: attached=%v ready=%v, want attached and ready", s.isAttached(), s.status().Ready)
	}
	if n := len(e.spawns(t)); n != 1 {
		t.Errorf("spawns = %d, want 1 (the holder is attached to, never replaced)", n)
	}
	if d := e.backoffs(); len(d) != 0 {
		t.Errorf("a backoff was waited out: %v", d)
	}
}

// "Restart server": the running child is stopped by stdin and replaced at
// once, with no backoff, and the call returns when the new one answers.
func TestSupervisor_RestartServerReplacesTheChild(t *testing.T) {
	e, s := newFakeEnv(t, "ready")
	s.start(context.Background())
	waitReady(t, s)
	first := e.spawns(t)[0].PID

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.restartServer(ctx); err != nil {
		t.Fatalf("restartServer: %v", err)
	}
	sp := e.spawns(t)
	if len(sp) != 2 || sp[1].PID == first {
		t.Fatalf("spawns %+v, want a second, different server", sp)
	}
	if !s.status().Ready {
		t.Error("restartServer returned before the replacement answered")
	}
	if d := e.backoffs(); len(d) != 0 {
		t.Errorf("a requested restart waited out a backoff %v", d)
	}
	if !strings.Contains(e.events(), "eof ") {
		t.Errorf("the old server was not stopped through stdin: events %q", e.events())
	}
	waitGone(t, first, 5*time.Second)
}

// The server is the bundle-relative binary, never one found on PATH: with a
// decoy `knomit` first on PATH the bundle's is spawned, and with the bundle's
// missing the supervisor fails ("bundle is incomplete") and the decoy never
// runs.
func TestSupervisor_SpawnsTheBundleBinaryNeverPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the decoy is a shell script")
	}
	decoyDir := t.TempDir()
	marker := filepath.Join(decoyDir, "decoy-ran")
	script := "#!/bin/sh\ntouch '" + marker + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(decoyDir, "knomit"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", decoyDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	t.Run("bundled", func(t *testing.T) {
		e, s := newFakeEnv(t, "ready")
		s.start(context.Background())
		waitReady(t, s)
		want, _ := filepath.EvalSymlinks(filepath.Join(e.bundle, "knomit"))
		if got := e.spawns(t)[0].Exe; got != want {
			t.Errorf("spawned %q, want the bundle's %q", got, want)
		}
	})
	t.Run("missing", func(t *testing.T) {
		e, s := newFakeEnv(t, "ready")
		if err := os.Remove(filepath.Join(e.bundle, "knomit")); err != nil {
			t.Fatal(err)
		}
		s.start(context.Background())
		st := waitFailed(t, s)
		if !strings.Contains(st.Error, "bundle is incomplete") {
			t.Errorf("failed status %q does not say the bundle is incomplete", st.Error)
		}
	})
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the PATH decoy ran")
	}
}

// /config.js says booting until the spawned server answers on the socket, then
// gives the page's own origin and the stream flag; /api/ answers 503 while
// booting and is proxied to the spawned server's socket once it is up (a POST
// body included: PR 1's ContentLength fix, end to end).
func TestSupervisor_UIBootsUntilTheSpawnedServerAnswers(t *testing.T) {
	_, s := newFakeEnv(t, "slow-ready")
	up := s.cfg.up
	h := apiProxy(configInjectingHandler(testUIFS(), nil, uiStatusFrom(s)),
		func() bool { return s.status().Ready }, up)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	s.start(context.Background())

	if body := get("/config.js").Body.String(); !strings.Contains(body, "__KNOMIT_BOOTING__ = true") || strings.Contains(body, "__KNOMIT_API_BASE__") {
		t.Fatalf("config.js while the server boots:\n%s", body)
	}
	if rec := get("/api/v1/version"); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("/api/ while booting = %d (Retry-After %q), want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}

	waitReady(t, s)
	body := get("/config.js").Body.String()
	if !strings.Contains(body, `__KNOMIT_API_BASE__ = "`+desktopOrigin()+`"`) || strings.Contains(body, "BOOTING") ||
		!strings.Contains(body, `__KNOMIT_EVENT_TRANSPORT__ = "wails-stream"`) {
		t.Fatalf("config.js once the server answers:\n%s", body)
	}
	if rec := get("/api/v1/version"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "0.5.3.fake") {
		t.Fatalf("/api/v1/version through the proxy = %d %q", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/echo", strings.NewReader(`{"x":1}`))
	req.ContentLength = 0 // what a WKWebView custom-scheme POST arrives as
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if b, _ := io.ReadAll(rec.Body); rec.Code != http.StatusOK || string(b) != `{"x":1}` {
		t.Fatalf("POST body through the proxy to the spawned server: %d %q", rec.Code, b)
	}
}

func TestChildEnvPinsTheHomeAndAddsNothingElse(t *testing.T) {
	parent := []string{"PATH=/bin", "KNOMIT_HOME=/elsewhere", "knomit_home=/also-elsewhere", "HOME=/u", "KNOMIT_SOCKET=/s"}
	got := childEnv(parent, "/the/home")
	want := []string{"PATH=/bin", "HOME=/u", "KNOMIT_SOCKET=/s", "KNOMIT_HOME=/the/home"}
	if !slices.Equal(got, want) {
		t.Fatalf("childEnv = %v, want %v", got, want)
	}
	for _, kv := range got {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(forbiddenChildEnv, k) {
			t.Errorf("childEnv added %s", k)
		}
	}
}

func TestRestartPolicy(t *testing.T) {
	p := defaultRestartPolicy()
	t0 := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)

	t.Run("doubles to the cap", func(t *testing.T) {
		h := restartHistory{}
		var got []time.Duration
		for i := 0; i < 9; i++ {
			// crashes 3 minutes apart: never 5 inside 10 minutes
			d, giveUp := h.record(p, t0.Add(time.Duration(i)*3*time.Minute), time.Second)
			if giveUp {
				t.Fatalf("gave up at crash %d with crashes 3 min apart", i+1)
			}
			got = append(got, d)
		}
		want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
		for i := range want {
			want[i] *= time.Second
		}
		if !slices.Equal(got, want) {
			t.Errorf("delays %v, want %v", got, want)
		}
	})
	t.Run("resets after five minutes up", func(t *testing.T) {
		h := restartHistory{}
		h.record(p, t0, time.Second)
		h.record(p, t0.Add(time.Second), time.Second)
		if d, _ := h.record(p, t0.Add(20*time.Minute), 5*time.Minute); d != time.Second {
			t.Errorf("delay after a 5-minute run = %s, want the base 1s", d)
		}
		if d, _ := h.record(p, t0.Add(21*time.Minute), 4*time.Minute); d != 2*time.Second {
			t.Errorf("delay after a 4-minute run = %s, want 2s (no reset below 5 min)", d)
		}
	})
	t.Run("five inside ten minutes gives up", func(t *testing.T) {
		h := restartHistory{}
		for i := 0; i < 4; i++ {
			if _, giveUp := h.record(p, t0.Add(time.Duration(i)*2*time.Minute), time.Second); giveUp {
				t.Fatalf("gave up at crash %d", i+1)
			}
		}
		if _, giveUp := h.record(p, t0.Add(9*time.Minute), time.Second); !giveUp {
			t.Error("did not give up on the fifth crash inside ten minutes")
		}
	})
	t.Run("old crashes leave the window", func(t *testing.T) {
		h := restartHistory{}
		for i := 0; i < 4; i++ {
			h.record(p, t0.Add(time.Duration(i)*time.Minute), time.Second)
		}
		if _, giveUp := h.record(p, t0.Add(12*time.Minute), time.Second); giveUp {
			t.Error("counted crashes older than ten minutes")
		}
	})
}

func TestVersionSkewIsMajorMinor(t *testing.T) {
	for _, c := range []struct {
		a, b string
		skew bool
	}{
		{"0.5.3.6ce780d", "0.5.4.abc", false},
		{"0.5.3.6ce780d", "0.5.3.6ce780d", false},
		{"0.5.3", "0.6.0", true},
		{"1.5.3", "0.5.3", true},
		{"dev", "dev", false},   // identical unparsable: the same build
		{"0.5.3", "dev", false}, // different and unparsable: unknown, no warning
		{"dev", "0.5.3", false},
		{"", "0.5.3", false},
		{"v0.5.3", "0.5.9", false},
	} {
		if got := versionSkew(c.a, c.b); got != c.skew {
			t.Errorf("versionSkew(%q, %q) = %v, want %v", c.a, c.b, got, c.skew)
		}
	}
}

// The updater's Restart stops the spawned server before the swap.
func TestStopServerFirstStopsBeforeTheUpdaterRestarts(t *testing.T) {
	var order []string
	f := &orderedInstaller{order: &order}
	i := stopServerFirst{updateInstaller: f, stop: func() { order = append(order, "stop-server") }}
	if err := installAndRestart(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	if want := []string{"install", "stop-server", "restart"}; !slices.Equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
}

// If the updater's restart fails AFTER the server was stopped for it, nothing
// serves any more: the supervisor must say so (failed status, which drives
// the fatal dialog), not keep reporting the stopped server as ready.
func TestStopServerFirst_AFailedRestartIsAFailedBoot(t *testing.T) {
	e, s := newFakeEnv(t, "ready")
	s.start(context.Background())
	waitReady(t, s)
	pid := e.spawns(t)[0].PID

	boom := errors.New("helper could not relaunch")
	i := stopServerFirst{updateInstaller: &failingRestart{err: boom}, stop: s.stop, failed: s.fail}
	if err := installAndRestart(context.Background(), i); !errors.Is(err, boom) {
		t.Fatalf("installAndRestart = %v, want the restart error", err)
	}
	waitGone(t, pid, 5*time.Second) // positive control: the server WAS stopped for the update
	st := s.status()
	if st.Ready || st.Phase != string(phaseFailed) || !strings.Contains(st.Error, "helper could not relaunch") {
		t.Fatalf("status after a failed update restart = %+v, want failed naming the restart error", st)
	}
}

type failingRestart struct{ err error }

func (f *failingRestart) CheckAndInstall(context.Context) error { return nil }
func (f *failingRestart) Restart(context.Context) error         { return f.err }

type orderedInstaller struct{ order *[]string }

func (o *orderedInstaller) CheckAndInstall(context.Context) error {
	*o.order = append(*o.order, "install")
	return nil
}

func (o *orderedInstaller) Restart(context.Context) error {
	*o.order = append(*o.order, "restart")
	return nil
}

// The boot caption: "downloading-models" only while the configured model's
// artifacts are missing — all three of them, a partial download included — and
// "starting-engine" once they are there or when the model is unknown (the
// server reports that error itself).
func TestModelsCachedPicksTheCaption(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{Home: home}
	cfg.Embeddings.Model = modelparams.DefaultModelID
	if modelsCached(cfg) {
		t.Fatal("a cold home reported the model cached: a first launch would not say it is downloading")
	}
	m, err := modelparams.Lookup(modelparams.DefaultModelID)
	if err != nil {
		t.Fatal(err)
	}
	mp, dp, tp := modelparams.ModelPaths(m, filepath.Join(home, "models"))
	if err := os.MkdirAll(filepath.Dir(mp), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{mp, tp, dp} {
		if p == "" {
			continue
		}
		if i == 2 && modelsCached(cfg) {
			t.Fatal("graph and tokenizer without the weights reported cached")
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !modelsCached(cfg) {
		t.Fatal("a fully populated cache reported a download still needed")
	}
	cfg.Embeddings.Model = "no-such-model"
	if !modelsCached(cfg) {
		t.Error("an unknown model should read as cached (caption only; the server reports the error)")
	}
}

func TestDesktopLockPathIsBesideTheHomesServerJSON(t *testing.T) {
	home := t.TempDir()
	if got, want := desktopLockPath(home), filepath.Join(home, "desktop.lock"); got != want {
		t.Errorf("desktopLockPath(non-default home) = %q, want %q", got, want)
	}
}
