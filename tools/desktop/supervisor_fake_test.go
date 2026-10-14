//go:build desktop

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/auth"
	"knomit/internal/serveraddr"
)

// The supervisor is tested against a FAKE `knomit`: this test binary itself,
// re-executed with KNOMIT_FAKE_SERVER set (TestMain below). It behaves like
// `knomit serve --supervised` as far as the supervisor can tell — it answers
// GET /api/v1/version on the local socket/pipe and stops on stdin EOF — and
// can instead exit 2, exit 3, crash, or ignore stdin, per spawn. Every spawn is
// recorded (pid, resolved executable, args, KNOMIT_HOME, which forbidden
// variables it saw) in FAKE_STATE/spawns.jsonl, and every stdin EOF in
// FAKE_STATE/events.

func TestMain(m *testing.M) {
	if os.Getenv("KNOMIT_FAKE_PARENT") != "" {
		os.Exit(fakeParentMain())
	}
	if os.Getenv("KNOMIT_FAKE_SERVER") != "" {
		os.Exit(fakeServerMain())
	}
	os.Exit(m.Run())
}

// fakeParentMain stands in for a desktop that is about to be SIGKILLed: it
// starts FAKE_BIN exactly as runChild does (childEnv, a stdin pipe it holds,
// configureChild, startChild — Pdeathsig on Linux, the Job Object on Windows),
// prints the child's pid, and waits to be killed.
func fakeParentMain() int {
	_ = os.Unsetenv("KNOMIT_FAKE_PARENT") // the child is a server, not another parent
	cmd := exec.Command(os.Getenv("FAKE_BIN"), "serve", "--supervised")
	cmd.Env = childEnv(os.Environ(), os.Getenv("FAKE_HOME"))
	pr, pw, err := os.Pipe()
	if err != nil {
		return 5
	}
	cmd.Stdin = pr
	configureChild(cmd)
	proc, err := startChild(cmd)
	if err != nil {
		fmt.Println("ERR", err)
		return 6
	}
	_ = pr.Close()
	fmt.Printf("CHILD %d\n", cmd.Process.Pid)
	go func() { _ = proc.wait() }()
	time.Sleep(2 * time.Minute)
	_ = pw.Close()
	return 0
}

// spawnRecord is one line of spawns.jsonl.
type spawnRecord struct {
	PID       int      `json:"pid"`
	Exe       string   `json:"exe"`
	Args      []string `json:"args"`
	Home      string   `json:"home"`
	Forbidden []string `json:"forbidden"`
}

func fakeServerMain() int {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "version" {
		fmt.Println(envOr("FAKE_VERSION", "0.5.3.fake"))
		return 0
	}
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintln(os.Stderr, "fake knomit: unexpected arguments", args)
		return 9
	}
	state := os.Getenv("FAKE_STATE")
	n := countLines(filepath.Join(state, "spawns.jsonl"))
	exe, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	rec := spawnRecord{PID: os.Getpid(), Exe: exe, Args: args, Home: os.Getenv("KNOMIT_HOME")}
	for _, k := range forbiddenChildEnv {
		if _, ok := os.LookupEnv(k); ok {
			rec.Forbidden = append(rec.Forbidden, k)
		}
	}
	b, _ := json.Marshal(rec)
	appendLine(filepath.Join(state, "spawns.jsonl"), string(b))

	modes := strings.Split(envOr("FAKE_MODES", "ready"), ",")
	mode := modes[len(modes)-1]
	if n < len(modes) {
		mode = modes[n]
	}
	switch mode {
	case "exit2":
		fmt.Fprintln(os.Stderr, "Error: load config: bad toml at line 3")
		return 2
	case "exit3":
		fmt.Fprintln(os.Stderr, "Error: local listener: the path is held by another live process")
		// Linger as a real process does between deciding to exit and being
		// reaped (Windows teardown is slow enough that CI hit it): the spawn
		// is recorded but the child is still alive, which is the window in
		// which TestSupervisor_Exit3SwitchesToAttach's holder must NOT be
		// answering yet. Fixed rather than left to the platform, so the
		// test exercises that window everywhere.
		time.Sleep(300 * time.Millisecond)
		return 3
	case "exit3-on-eof":
		// A child that has lost the socket to another server and is on its
		// way out, held there: alive and never answering until the supervisor
		// closes its stdin. The window in which another server answers while
		// our doomed child is still alive stays open for as long as the test
		// needs it, so no sleep decides whether the test sees it.
		fmt.Fprintln(os.Stderr, "Error: local listener: the path is held by another live process")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 3
	case "crash":
		fmt.Fprintln(os.Stderr, "panic: boom\n\ngoroutine 1 [running]:\nmain.main()")
		return 1
	case "slow-ready":
		time.Sleep(600 * time.Millisecond)
	}
	ln, cleanup, err := auth.ListenLocal(os.Getenv("FAKE_SOCKET"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake knomit: listen:", err)
		return 7
	}
	// server.json naming this process, written after the listener is bound
	// and BEFORE anything is served, as `knomit serve` does
	// (Listeners.Advertise runs before serveInto in cmd/serve.go): the
	// supervisor reads it to tell its own child from another server
	// answering on the same socket.
	lock := filepath.Join(os.Getenv("KNOMIT_HOME"), "server.json")
	if err := serveraddr.WriteLock(lock, serveraddr.LockInfo{PID: os.Getpid(), Version: "0.5.3.fake", Schema: serveraddr.LockSchemaV2}); err != nil {
		fmt.Fprintln(os.Stderr, "fake knomit: write server.json:", err)
		return 2
	}
	defer func() { _ = serveraddr.RemoveLockIfOwner(lock, os.Getpid()) }()
	srv := &http.Server{Handler: versionHandler(envOr("FAKE_SERVE_VERSION", "0.5.3.fake"))}
	go func() { _ = srv.Serve(ln) }()
	_, _ = io.Copy(io.Discard, os.Stdin) // the supervisor's lifeline
	if mode == "ignore-stdin" {
		appendLine(filepath.Join(state, "events"), fmt.Sprintf("eof-ignored %d", os.Getpid()))
		select {} // a hung server: only a kill ends it
	}
	appendLine(filepath.Join(state, "events"), fmt.Sprintf("eof %d", os.Getpid()))
	_ = srv.Close()
	cleanup()
	return 0
}

// versionHandler answers GET /api/v1/version the way the server does (the
// `full` field is what the supervisor reads) and 404s everything else except
// /api/v1/echo, which the proxy test uses.
func versionHandler(full string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"full": full, "version": full})
	})
	mux.HandleFunc("POST /api/v1/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write(b)
	})
	return mux
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var appendMu sync.Mutex

func appendLine(path, line string) {
	appendMu.Lock()
	defer appendMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n++
	}
	return n
}

// fakeEnv is one test's world: a state dir the fake writes into, a socket,
// a bundle directory holding knomit-desktop (a stub) and knomit (this test
// binary), and the supervisor wired to them.
type fakeEnv struct {
	state   string
	socket  string
	bundle  string
	home    string
	logFile string
	up      *uiUpstream

	mu     sync.Mutex
	delays []time.Duration
}

func (e *fakeEnv) spawns(t *testing.T) []spawnRecord {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.state, "spawns.jsonl"))
	if err != nil {
		return nil
	}
	var out []spawnRecord
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r spawnRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("spawn record %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func (e *fakeEnv) events() string {
	b, _ := os.ReadFile(filepath.Join(e.state, "events"))
	return string(b)
}

func (e *fakeEnv) backoffs() []time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]time.Duration(nil), e.delays...)
}

// testSocket is a local listener path the test owns: a short unix socket path
// (sun_path is 104 bytes on macOS, and t.TempDir() there is long) or a named
// pipe on Windows.
func testSocket(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`\\.\pipe\knomit-desktop-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	}
	dir, err := os.MkdirTemp("", "kd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// newFakeEnv builds the world and a supervisor over it, not yet started. The
// FAKE_* variables are set in THIS process's environment (t.Setenv), so the
// child inherits them through the supervisor's os.Environ — the same path the
// user's environment takes — and so does the `knomit version` probe.
func newFakeEnv(t *testing.T, modes string) (*fakeEnv, *supervisor) {
	t.Helper()
	captureLog(t)
	e := &fakeEnv{state: t.TempDir(), socket: testSocket(t), bundle: bundleDir(t), home: t.TempDir(), up: &uiUpstream{}}
	e.logFile = filepath.Join(e.state, "knomit.log")
	if err := os.WriteFile(filepath.Join(e.bundle, "knomit-desktop"+exeSuffix), []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkTestBinary(t, filepath.Join(e.bundle, serverExecName+exeSuffix))
	t.Setenv("KNOMIT_FAKE_SERVER", "1")
	t.Setenv("FAKE_STATE", e.state)
	t.Setenv("FAKE_SOCKET", e.socket)
	t.Setenv("FAKE_MODES", modes)
	// The five variables the supervisor must never ADD: absent from the
	// parent, so any the child sees came from the supervisor.
	for _, k := range forbiddenChildEnv {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	// A different home in the parent's env: the child must get cfg.Home.
	t.Setenv("KNOMIT_HOME", filepath.Join(e.state, "not-this-home"))

	s := newSupervisor(supervisorConfig{
		socket:         e.socket,
		home:           e.home,
		lockPath:       filepath.Join(e.home, "server.json"),
		logFile:        e.logFile,
		desktopVersion: "0.5.3.test",
		serverBin:      func() (string, error) { return serverBinaryFor(filepath.Join(e.bundle, "knomit-desktop"+exeSuffix)) },
		environ:        os.Environ,
		up:             e.up,
	})
	s.pollEvery = 20 * time.Millisecond
	s.probeWait = time.Second
	s.attachEvery = 50 * time.Millisecond
	s.heldWait = 3 * time.Second
	s.quitGrace = 3 * time.Second
	s.backoff = func(ctx context.Context, d time.Duration) error {
		e.mu.Lock()
		e.delays = append(e.delays, d)
		e.mu.Unlock()
		return sleepCtx(ctx, time.Millisecond)
	}
	t.Cleanup(s.stop)
	return e, s
}

// linkTestBinary puts this test binary at dst: a symlink on unix (the locator
// resolves the DESKTOP's symlinks, not the server's), a copy on Windows.
func linkTestBinary(t *testing.T, dst string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(self, dst); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

func waitReady(t *testing.T, s *supervisor) {
	t.Helper()
	eventually(t, 30*time.Second, "the supervisor to report ready", func() bool { return s.status().Ready })
}

func waitFailed(t *testing.T, s *supervisor) bootStatus {
	t.Helper()
	eventually(t, 30*time.Second, "the supervisor to report failed", func() bool { return s.status().Phase == string(phaseFailed) })
	return s.status()
}

// waitGone waits for pid to stop running (platform probe in
// supervisor_alive_*_test.go).
func waitGone(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	eventually(t, d, "the server process to be gone", func() bool { return !processAlive(pid) })
}

// eventually polls cond every 20 ms for up to d.
func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

// syncLog collects zerolog output for assertions; captureLog installs it as
// the global logger for one test.
type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncLog) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncLog) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// hasWarn reports whether one WARN line's message contains msg and its field
// key equals want EXACTLY. The line is parsed, not substring-matched: zerolog
// writes JSON, which escapes a Windows path's backslashes.
func (s *syncLog) hasWarn(msg, key, want string) bool {
	for _, line := range strings.Split(s.String(), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		m, _ := ev["message"].(string)
		if ev["level"] == "warn" && strings.Contains(m, msg) && (key == "" || ev[key] == want) {
			return true
		}
	}
	return false
}

func captureLog(t *testing.T) *syncLog {
	t.Helper()
	buf := &syncLog{}
	prev := log.Logger
	log.Logger = zerolog.New(buf)
	t.Cleanup(func() { log.Logger = prev })
	return buf
}
