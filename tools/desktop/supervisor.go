//go:build desktop

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/foundation/serveraddr"
)

// The desktop does not contain a knomit server. It runs the bundled `knomit`
// binary as `knomit serve --supervised` and hosts the UI over that process's
// local socket / named pipe (uiproxy.go, uistreams.go). This file decides
// WHICH server the UI talks to and keeps it running:
//
//   - attach: a knomit already answers on this home's socket (a `knomit
//     serve` the user started, or a desktop that is quitting). It is not ours:
//     never stopped on quit, never restarted; "Restart server" refuses.
//   - spawn: nothing answers, so start the bundled binary (never one found on
//     PATH: serverbin.go), wait for it to answer, and restart it when it dies.
//
// The exit codes of `knomit serve --supervised` (cmd/serve.go) drive the
// restart policy: 2 is a configuration or trust failure no restart can fix,
// 3 means another process holds the socket (attach to it), anything else is
// a crash. The decision record is kb/decisions/serve/supervised-mode.

// The documented exit codes of `knomit serve --supervised` (cmd/serve.go
// ExitSupervisedConfig and ExitSupervisedHeld; the desktop cannot import cmd).
const (
	exitServerConfig = 2
	exitServerHeld   = 3
)

// forbiddenChildEnv are the variables the supervisor must never ADD to the
// server's environment. Each beats knomit.toml on boot AND on every reload, so
// a value the desktop injected would silently defeat Settings edits (kb gotcha
// serve/reload, 2ab87f74). The user's own environment passes through
// unchanged, which keeps Settings' "overridden by environment" readout (read
// from the desktop's env) true of the child.
var forbiddenChildEnv = []string{"KNOMIT_PORT", "KNOMIT_HTTP", "KNOMIT_HOST", "KNOMIT_TLS_ADDR", "KNOMIT_LOG_LEVEL"}

// childEnv is the server's environment: the desktop's own, with KNOMIT_HOME
// pinned to the data root the desktop resolved, so the child cannot land on a
// different home than the one whose socket the UI dials.
func childEnv(parent []string, home string) []string {
	out := make([]string, 0, len(parent)+1)
	for _, kv := range parent {
		k, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "KNOMIT_HOME") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "KNOMIT_HOME="+home)
}

// supervisorConfig is everything the supervisor needs from the outside. Every
// field is a value or a function so the tests can drive it without a display,
// a bundle or a model cache.
type supervisorConfig struct {
	// socket is cfg.Socket: where the server listens and the UI dials.
	socket string
	// home is cfg.Home, passed to the child as KNOMIT_HOME.
	home string
	// lockPath is this home's server.json, read to name an attached server's
	// pid and to tell our own child's answer from another server's.
	lockPath string
	// logFile is passed as --log-file: the server owns the configured log file,
	// so Reveal and Settings name the file being written.
	logFile string
	// desktopVersion is version.String() of this desktop build.
	desktopVersion string
	// serverBin resolves the bundled server (serverBinaryPath).
	serverBin func() (string, error)
	// environ is the desktop's environment (os.Environ).
	environ func() []string
	// installTools is the desktop's own link step (kb, knomit-okf into
	// <home>/bin): the installing-tools phase. Best-effort.
	installTools func()
	// modelsCached reports whether the embedding model is on disk; false makes
	// the spawn phase read "downloading-models" instead of "starting-engine".
	modelsCached func() bool
	// up is where the UI's API traffic goes; set to socket once a server
	// answers there.
	up *uiUpstream
	// onChange is told of every status transition (tray badge, fatal dialog).
	// Called without the supervisor's lock held, from the supervisor's
	// goroutine; it must not block.
	onChange func(bootStatus)
}

// supervisor is the attach-or-spawn state machine. The zero value is not
// usable; call newSupervisor.
type supervisor struct {
	cfg supervisorConfig

	// Timing and policy. Production values from newSupervisor; tests shorten.
	pollEvery   time.Duration // readiness probe interval while a child boots
	probeWait   time.Duration // one probe's timeout
	attachEvery time.Duration // health probe interval while attached
	attachLost  int           // consecutive failed probes before an attached server counts as gone
	heldWait    time.Duration // after exit 3: how long to look for the holder to answer
	quitGrace   time.Duration // stdin closed -> kill
	policy      restartPolicy
	backoff     func(context.Context, time.Duration) error // waits out a restart delay; tests record it
	now         func() time.Time

	client *http.Client

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu         sync.Mutex
	phase      bootPhase
	ready      bool
	readyGen   int // bumped each time a server starts answering
	failed     error
	attached   bool
	child      *serverChild
	restarting bool // the running child is being stopped by restartServer
	stopping   bool
	stopOnce   sync.Once
}

func newSupervisor(cfg supervisorConfig) *supervisor {
	probeUp := &uiUpstream{}
	probeUp.set(cfg.socket)
	return &supervisor{
		cfg:         cfg,
		pollEvery:   250 * time.Millisecond,
		probeWait:   2 * time.Second,
		attachEvery: 2 * time.Second,
		attachLost:  3,
		heldWait:    5 * time.Second,
		quitGrace:   10 * time.Second,
		policy:      defaultRestartPolicy(),
		backoff:     sleepCtx,
		now:         time.Now,
		// The same checked dial the UI path uses (newUpstreamTransport ->
		// auth.DialLocal), never TCP: archtest TestDesktopUIPathNeverDialsTCP
		// lists this file. No keep-alive: a probe is one request.
		client: &http.Client{Transport: newUpstreamTransport(probeUp, false)},
		phase:  phaseStarting,
		done:   make(chan struct{}),
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// start runs the supervisor on its own goroutine and returns at once: nothing
// slow may run before application.Run (kb invariant desktop/startup/wails,
// fabd21af).
func (s *supervisor) start(parent context.Context) {
	s.ctx, s.cancel = context.WithCancel(parent)
	go func() {
		defer close(s.done)
		s.run(s.ctx)
	}()
}

// status is the boot status the UI reads (GET /boot/status, /config.js) and
// the /api/ proxy's readiness gate. Non-blocking.
func (s *supervisor) status() bootStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *supervisor) statusLocked() bootStatus {
	switch {
	case s.failed != nil:
		return bootStatus{Phase: string(phaseFailed), Error: s.failed.Error()}
	case s.ready:
		return bootStatus{Ready: true, Phase: string(phaseReady)}
	default:
		return bootStatus{Phase: string(s.phase)}
	}
}

// isAttached reports whether the server the UI talks to was started outside
// the app.
func (s *supervisor) isAttached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attached
}

// update applies f under the lock and tells onChange if the status changed.
func (s *supervisor) update(f func()) {
	s.mu.Lock()
	before := s.statusLocked()
	f()
	after := s.statusLocked()
	s.mu.Unlock()
	if after != before && s.cfg.onChange != nil {
		s.cfg.onChange(after)
	}
}

func (s *supervisor) setPhase(p bootPhase) {
	log.Info().Str("phase", string(p)).Msg("desktop boot phase")
	s.update(func() { s.phase = p; s.ready = false })
}

func (s *supervisor) setReady() {
	s.cfg.up.set(s.cfg.socket)
	s.update(func() { s.ready = true; s.readyGen++ })
}

func (s *supervisor) fail(err error) {
	log.Error().Err(err).Msg("knomit server: giving up")
	s.update(func() { s.failed = err; s.ready = false })
}

// probe asks the socket for GET /api/v1/version and returns the server's full
// build version. Any answer but a 200 with that field is "no server".
func (s *supervisor) probe(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.probeWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/api/v1/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /api/v1/version: %s", resp.Status)
	}
	var v struct {
		Full string `json:"full"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || v.Full == "" {
		return "", fmt.Errorf("GET /api/v1/version: not a knomit answer")
	}
	return v.Full, nil
}

// run is the supervisor's loop: attach when a server answers, spawn when none
// does, and apply the restart policy to every unasked-for exit.
func (s *supervisor) run(ctx context.Context) {
	s.setPhase(phaseInstallingTools)
	if s.cfg.installTools != nil {
		s.cfg.installTools()
	}
	var hist restartHistory
	for ctx.Err() == nil {
		if ver, err := s.probe(ctx); err == nil {
			s.attach(ctx, ver)
			continue
		}
		bin, err := s.cfg.serverBin()
		if err != nil {
			s.fail(err)
			return
		}
		if s.cfg.modelsCached == nil || s.cfg.modelsCached() {
			s.setPhase(phaseStartingEngine)
		} else {
			s.setPhase(phaseDownloadingModels)
		}
		ex := s.runChild(ctx, bin)
		switch {
		case ctx.Err() != nil:
			return
		case ex.startErr != nil:
			s.fail(fmt.Errorf("start the knomit server %s: %w", bin, ex.startErr))
			return
		case ex.restarted:
			log.Info().Msg("knomit server restarted on request")
			hist = restartHistory{}
			continue
		case ex.code == exitServerConfig:
			s.fail(fmt.Errorf("the knomit server refused its configuration (exit 2); fix knomit.toml and reopen Knomit:\n%s", tailLines(ex.stderr, 20)))
			return
		case ex.code == exitServerHeld:
			log.Warn().Str("socket", s.cfg.socket).Msg("knomit server: the local socket is held by another process (exit 3); attaching to it")
			if s.waitForHolder(ctx) {
				continue
			}
		}
		delay, giveUp := hist.record(s.policy, s.now(), ex.upFor)
		log.Error().Int("exit_code", ex.code).Dur("up_for", ex.upFor).Str("stderr_tail", ex.stderr).
			Msg("knomit server exited unexpectedly")
		if giveUp {
			s.fail(fmt.Errorf("the knomit server stopped %d times in %s; giving up:\n%s",
				s.policy.maxCrashes, s.policy.window, tailLines(ex.stderr, 20)))
			return
		}
		s.setPhase(phaseStartingEngine)
		log.Warn().Dur("in", delay).Msg("restarting the knomit server")
		if err := s.backoff(ctx, delay); err != nil {
			return
		}
	}
}

// waitForHolder polls the socket after an exit 3 for up to heldWait: the
// holder may still be booting. True when it answered (the loop then attaches).
func (s *supervisor) waitForHolder(ctx context.Context) bool {
	deadline := s.now().Add(s.heldWait)
	for s.now().Before(deadline) {
		if _, err := s.probe(ctx); err == nil {
			return true
		}
		if sleepCtx(ctx, s.pollEvery) != nil {
			return false
		}
	}
	return false
}

// attach adopts a server that already answers on the socket. It returns when
// that server has stopped answering (then the loop looks again and spawns its
// own) or ctx ends. The attached server is never stopped or restarted.
func (s *supervisor) attach(ctx context.Context, ver string) {
	pid := 0
	if info, err := serveraddr.ReadLock(s.cfg.lockPath); err == nil {
		pid = info.PID
	}
	log.Info().Str("socket", s.cfg.socket).Int("pid", pid).Str("server_version", ver).
		Msg("knomit server already running; attaching (it is not stopped on quit)")
	if skew := versionSkew(s.cfg.desktopVersion, ver); skew {
		log.Warn().Str("desktop_version", s.cfg.desktopVersion).Str("server_version", ver).
			Msg("the attached knomit server is a different major.minor version than this desktop; the UI may not match its API")
	}
	s.update(func() { s.attached = true })
	s.setReady()
	misses := 0
	for misses < s.attachLost {
		if sleepCtx(ctx, s.attachEvery) != nil {
			return
		}
		if _, err := s.probe(ctx); err != nil {
			misses++
			continue
		}
		misses = 0
	}
	log.Warn().Str("socket", s.cfg.socket).Msg("the attached knomit server stopped answering")
	s.update(func() { s.attached = false; s.ready = false; s.phase = phaseStarting })
}

// versionSkew reports whether two build versions differ in major.minor.
// "0.5.3.6ce780d" and "0.5.4.abc" do not; "0.5.3" and "0.6.0" do. Identical
// strings are never skew, and a pair where either side is unparsable and they
// differ is "unknown": no warning (the UI shows both versions).
func versionSkew(a, b string) bool {
	mm := func(v string) (string, bool) {
		parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return "", false
		}
		return parts[0] + "." + parts[1], true
	}
	if a == b {
		return false // the same build, parsable or not (a dev build against itself)
	}
	ma, oka := mm(a)
	mb, okb := mm(b)
	if !oka || !okb {
		return false // different and unparsable: unknown, which is not a warning
	}
	return ma != mb
}

// childExit is how one spawned server ended.
type childExit struct {
	startErr  error         // the process could not be started at all
	code      int           // exit code; -1 when killed by a signal
	upFor     time.Duration // start to exit
	stderr    string        // the tail of its stderr
	restarted bool          // restartServer stopped it
}

// serverChild is a running spawned server.
type serverChild struct {
	pid       int
	stdin     *os.File // the write end the desktop holds; closing it is the stop request
	proc      childProc
	exited    chan struct{}
	closeOnce sync.Once
}

func (c *serverChild) closeStdin() {
	c.closeOnce.Do(func() { _ = c.stdin.Close() })
}

// runChild starts the server and blocks until it exits (or ctx ends and stop
// has ended it). It marks the supervisor ready while the child answers.
func (s *supervisor) runChild(ctx context.Context, bin string) childExit {
	if v, err := serverVersion(ctx, bin); err != nil {
		log.Warn().Err(err).Str("bin", bin).Msg("could not read the bundled server's version")
	} else if v != s.cfg.desktopVersion {
		log.Warn().Str("server_version", v).Str("desktop_version", s.cfg.desktopVersion).
			Msg("the bundled knomit server is a different build than this desktop")
	}
	args := []string{"serve", "--supervised"}
	if s.cfg.logFile != "" {
		args = append(args, "--log-file", s.cfg.logFile)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = childEnv(s.cfg.environ(), s.cfg.home)
	pr, pw, err := os.Pipe()
	if err != nil {
		return childExit{startErr: err}
	}
	cmd.Stdin = pr
	cmd.Stdout = nil // discarded: the server logs to stderr and its log file
	ring := newTailBuffer(64 << 10)
	cmd.Stderr = ring
	// A grandchild that inherited stderr must not keep Wait (and so the
	// restart loop) blocked after the server itself exited.
	cmd.WaitDelay = 5 * time.Second
	configureChild(cmd)

	s.mu.Lock()
	stopping := s.stopping
	s.mu.Unlock()
	if stopping {
		_ = pr.Close()
		_ = pw.Close()
		return childExit{}
	}
	started := s.now()
	proc, err := startChild(cmd)
	_ = pr.Close() // the child has its copy
	if err != nil {
		_ = pw.Close()
		return childExit{startErr: err}
	}
	c := &serverChild{pid: cmd.Process.Pid, stdin: pw, proc: proc, exited: make(chan struct{})}
	var waitErr error
	go func() {
		waitErr = proc.wait()
		close(c.exited)
	}()
	// Published under the same lock stop() reads it with: a stop that began
	// while this child was being started did not see it, so it is ended here.
	s.mu.Lock()
	s.child = c
	stopping = s.stopping
	s.mu.Unlock()
	log.Info().Int("pid", c.pid).Str("bin", bin).Strs("args", args).Msg("knomit server started")
	if stopping {
		s.terminate(c)
	}

	// Readiness: the socket answering GET /api/v1/version, polled until the
	// child answers or exits. "knomit ready" in its log means the same thing,
	// but the socket is what the UI will actually use.
	//
	// An answer is our child's only when server.json names its pid. The
	// probe cannot tell who answered: a child that lost the socket to another
	// server (and is about to exit 3) is still alive while that server
	// answers, and taking that answer as ours reported owned-ready and let
	// "Restart server" act on the dying child instead of refusing as attached
	// (kb gotcha desktop/supervisor/readiness-probe-meets-holder). The server
	// writes server.json with its own pid after binding and BEFORE serving
	// anything, a supervised server that cannot write it exits 2, and one that
	// does not hold the socket writes none (cmd/serve.go, Listeners.Advertise),
	// so our child's first answer always finds its own pid there.
	for ready, foreign := false, false; !ready; {
		select {
		case <-c.exited:
			ready = true
			continue
		default:
		}
		if _, err := s.probe(ctx); err == nil {
			if s.answeredByChild(c) {
				log.Info().Int("pid", c.pid).Msg("knomit server is answering on its socket")
				s.setReady()
				ready = true
				continue
			}
			if !foreign {
				foreign = true
				log.Info().Int("pid", c.pid).Msg("a server answers on the socket that is not our child; waiting for our child to answer or exit")
			}
		}
		select {
		case <-c.exited:
			ready = true
		case <-time.After(s.pollEvery):
		}
	}
	<-c.exited
	ex := childExit{code: exitCode(waitErr), upFor: s.now().Sub(started), stderr: ring.String()}
	c.closeStdin()
	s.mu.Lock()
	s.child = nil
	ex.restarted = s.restarting
	s.restarting = false
	s.mu.Unlock()
	return ex
}

// answeredByChild reports whether the server that just answered on the socket
// is c: this home's server.json names c's pid. A missing or unreadable file is
// "not ours yet"; the readiness loop probes again.
func (s *supervisor) answeredByChild(c *serverChild) bool {
	info, err := serveraddr.ReadLock(s.cfg.lockPath)
	return err == nil && info.PID == c.pid
}

// exitCode maps Wait's error to an exit code: 0 for nil, the code for an
// ExitError, -1 for anything else (a signal, a Wait failure).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// terminate asks c to stop by closing its stdin, waits up to quitGrace, then
// kills it, and waits for it to be gone. `knomit serve --supervised` treats
// stdin EOF exactly like SIGTERM and gives its handlers 5 s.
func (s *supervisor) terminate(c *serverChild) {
	c.closeStdin()
	select {
	case <-c.exited:
		return
	case <-time.After(s.quitGrace):
	}
	log.Warn().Int("pid", c.pid).Dur("grace", s.quitGrace).Msg("knomit server did not stop after stdin closed; killing it")
	_ = c.proc.kill()
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
		log.Error().Int("pid", c.pid).Msg("knomit server did not exit after being killed")
	}
}

// stop ends supervision: a spawned server is stopped (stdin closed, killed
// after the grace), an attached one is left alone. Idempotent; bounded by
// quitGrace plus a few seconds.
func (s *supervisor) stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopping = true
		c := s.child
		s.mu.Unlock()
		if s.cancel != nil {
			s.cancel()
		}
		if c != nil {
			s.terminate(c)
		}
		if s.done != nil && s.cancel != nil {
			select {
			case <-s.done:
			case <-time.After(s.quitGrace + 5*time.Second):
				log.Warn().Msg("supervisor did not finish in time")
			}
		}
	})
}

// errAttachedServer is restartServer's answer when the server was started
// outside the app: it is not ours to restart (design Q2 ruling).
type errAttachedServer struct{ pid int }

func (e errAttachedServer) Error() string {
	if e.pid > 0 {
		return fmt.Sprintf("the knomit server was started outside the app (pid %d); restart it where it was started", e.pid)
	}
	return "the knomit server was started outside the app; restart it where it was started"
}

// restartServer stops the spawned server and waits for its replacement to
// answer. The replacement is started at once (no backoff; it does not count
// as a crash). Refused for an attached server and when none is running, and
// answered with the same attached refusal when the server that answers
// afterwards is one the supervisor attached to instead of its own.
func (s *supervisor) restartServer(ctx context.Context) error {
	s.mu.Lock()
	if s.attached {
		s.mu.Unlock()
		pid := 0
		if info, err := serveraddr.ReadLock(s.cfg.lockPath); err == nil {
			pid = info.PID
		}
		return errAttachedServer{pid: pid}
	}
	if s.failed != nil {
		err := s.failed
		s.mu.Unlock()
		return fmt.Errorf("the knomit server is not running (%v); quit and reopen Knomit", err)
	}
	c := s.child
	if c == nil {
		s.mu.Unlock()
		return errors.New("the knomit server is still starting; try again in a moment")
	}
	s.restarting = true
	gen := s.readyGen
	s.mu.Unlock()

	s.terminate(c)
	for {
		s.mu.Lock()
		failed, ok := s.failed, s.readyGen > gen && s.ready
		s.mu.Unlock()
		if failed != nil {
			return failed
		}
		if ok {
			if s.isAttached() {
				// What answers now is a server started outside the app (our
				// child lost the socket to it): nothing of ours was
				// restarted, and that server is not ours to restart.
				pid := 0
				if info, err := serveraddr.ReadLock(s.cfg.lockPath); err == nil {
					pid = info.PID
				}
				return errAttachedServer{pid: pid}
			}
			return nil
		}
		if err := sleepCtx(ctx, 100*time.Millisecond); err != nil {
			return fmt.Errorf("the knomit server has not come back yet: %w", err)
		}
	}
}

// childProc is a started server process, as the platform started it.
type childProc interface {
	wait() error
	kill() error
}

// restartPolicy is the crash-restart rule: backoff base, doubling up to cap,
// reset after a run of at least stable; give up after maxCrashes unasked-for
// exits inside window.
type restartPolicy struct {
	base, cap, stable, window time.Duration
	maxCrashes                int
}

func defaultRestartPolicy() restartPolicy {
	return restartPolicy{base: time.Second, cap: time.Minute, stable: 5 * time.Minute, window: 10 * time.Minute, maxCrashes: 5}
}

// restartHistory is the state the policy keeps between exits.
type restartHistory struct {
	crashes []time.Time
	next    time.Duration
}

// record notes an exit at `at` after a run of upFor and returns how long to
// wait before the next start, or giveUp when the window is full.
func (h *restartHistory) record(p restartPolicy, at time.Time, upFor time.Duration) (delay time.Duration, giveUp bool) {
	if upFor >= p.stable {
		h.next = 0 // it ran long enough to call it healthy: start the backoff over
	}
	keep := h.crashes[:0]
	for _, c := range h.crashes {
		if at.Sub(c) < p.window {
			keep = append(keep, c)
		}
	}
	h.crashes = append(keep, at)
	if len(h.crashes) >= p.maxCrashes {
		return 0, true
	}
	if h.next == 0 {
		h.next = p.base
	} else {
		h.next *= 2
		if h.next > p.cap {
			h.next = p.cap
		}
	}
	return h.next, false
}

// tailBuffer keeps the last max bytes written to it: the child's stderr, kept
// for the desktop log and the failure dialog (a Go traceback is the one thing
// the server cannot put in its own log file).
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// tailLines is the last n lines of s, for a dialog that must stay readable.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
