// Package supervisor manages the lifecycle of a child `knomit serve` process.
package supervisor

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/tools/tray/internal/netutil"
)

// State represents the lifecycle state of the supervised child process.
type State int

const (
	StateStopped State = iota
	StateRunning
	StateCrashed
)

// String returns a human-readable name for the state.
func (s State) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StateCrashed:
		return "crashed"
	default:
		return "stopped"
	}
}

// Config holds configuration for a Supervisor.
type Config struct {
	Binary  string // path to `knomit` executable
	Port    int    // 0 => supervisor picks a free port via netutil.PickPort
	LogsDir string // directory where serve.log is written
}

// Supervisor spawns and monitors a single child `knomit serve` process.
// It does not auto-restart on crash; callers observe StateCrashed via OnStateChange.
type Supervisor struct {
	cfg Config

	mu       sync.Mutex
	cmd      *exec.Cmd
	port     int
	state    State
	lastErr  error
	logFile  *os.File
	onChange func(State)
	// exited is closed by the wait goroutine once cmd.Wait() returns.
	// Stop selects on it instead of calling cmd.Wait() a second time.
	exited chan struct{}
}

// New creates a Supervisor with the given config. Call Start to launch the child.
func New(cfg Config) *Supervisor {
	return &Supervisor{cfg: cfg, state: StateStopped}
}

// Start launches the child process. Returns an error if already running or if
// the child cannot be started.
func (s *Supervisor) Start() error {
	s.mu.Lock()

	if s.state == StateRunning {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: already running")
	}

	port := s.cfg.Port
	if port == 0 {
		p, err := netutil.PickPort()
		if err != nil {
			s.mu.Unlock()
			return err
		}
		port = p
	}

	if err := os.MkdirAll(s.cfg.LogsDir, 0o755); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("mkdir logs: %w", err)
	}
	logPath := filepath.Join(s.cfg.LogsDir, "serve.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("open log: %w", err)
	}

	// Always pass "serve" as the first argument so the invocation matches the
	// real knomit CLI (`knomit serve --port N --host 127.0.0.1`).
	// The test's fake binary is written to strip argv[1] when it equals
	// "serve" before calling flag.CommandLine.Parse.
	args := []string{"serve", "--port", strconv.Itoa(port), "--host", "127.0.0.1"}

	cmd := exec.Command(s.cfg.Binary, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		s.mu.Unlock()
		return fmt.Errorf("start %s: %w", s.cfg.Binary, err)
	}

	s.cmd = cmd
	s.port = port
	s.logFile = logFile
	s.exited = make(chan struct{})
	fire := s.setStateLocked(StateRunning, nil)
	exited := s.exited
	s.mu.Unlock()

	// Fire the callback after releasing the lock so that accessors are safe to
	// call from within the callback.
	if fire != nil {
		fire()
	}

	go s.wait(cmd, exited)
	return nil
}

// wait blocks until the child exits, closes the exited channel, then
// transitions state accordingly.  cmd.Wait() is called OUTSIDE the mutex so
// that Stop (which holds no lock while selecting on exited) does not deadlock.
func (s *Supervisor) wait(cmd *exec.Cmd, exited chan struct{}) {
	err := cmd.Wait()
	close(exited)

	s.mu.Lock()
	if s.logFile != nil {
		s.logFile.Close()
		s.logFile = nil
	}
	// Stop() pre-sets state to StateStopped before sending SIGTERM, so if
	// we see StateStopped here we know Stop() already handled the transition.
	if s.state == StateStopped {
		s.mu.Unlock()
		return
	}
	var fire func()
	if err != nil {
		log.Error().Err(err).Msg("knomit serve exited with error")
		fire = s.setStateLocked(StateCrashed, err)
	} else {
		fire = s.setStateLocked(StateStopped, nil)
	}
	s.mu.Unlock()

	if fire != nil {
		fire()
	}
}

// Stop sends SIGTERM to the child's process group, waits up to timeout, then
// sends SIGKILL if the child has not exited.
func (s *Supervisor) Stop(timeout time.Duration) error {
	s.mu.Lock()
	cmd := s.cmd
	if cmd == nil || s.state != StateRunning {
		s.mu.Unlock()
		return nil
	}
	// Pre-set state to Stopped before releasing the lock so that the wait
	// goroutine (which re-acquires the lock after cmd.Wait returns) sees
	// StateStopped and does not transition to StateCrashed.
	fire := s.setStateLocked(StateStopped, nil)
	exited := s.exited
	s.mu.Unlock()

	// Fire callback after releasing the lock so accessors are safe to call.
	if fire != nil {
		fire()
	}

	pgid, pgidErr := syscall.Getpgid(cmd.Process.Pid)
	if pgidErr != nil || pgid <= 0 {
		// Fallback: signal only the process itself.
		_ = cmd.Process.Signal(syscall.SIGTERM)
	} else {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-exited:
		return nil
	case <-timer.C:
		if pgidErr == nil && pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = cmd.Process.Kill()
		}
		<-exited
		return fmt.Errorf("supervisor: serve did not exit within %s; sent SIGKILL", timeout)
	}
}

// State returns the current lifecycle state.
func (s *Supervisor) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Port returns the TCP port the child is listening on (or was configured to use).
func (s *Supervisor) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// URL returns "http://127.0.0.1:<port>" when a port is known, otherwise "".
func (s *Supervisor) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.port == 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", s.port)
}

// LastError returns the error that caused a transition to StateCrashed, or nil.
func (s *Supervisor) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// OnStateChange registers a callback invoked on every state transition.
// The callback is invoked WITHOUT the supervisor's internal mutex held, so it
// is safe to call any supervisor accessor (Port, URL, State, PID, LastError)
// from within the callback.
func (s *Supervisor) OnStateChange(fn func(State)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// setStateLocked mutates the state fields and returns a function that fires
// the OnStateChange callback (or nil). The caller MUST invoke the returned
// function AFTER releasing the mutex, not before.
// Caller must hold s.mu.
func (s *Supervisor) setStateLocked(next State, err error) func() {
	s.state = next
	s.lastErr = err
	if s.onChange == nil {
		return nil
	}
	cb := s.onChange
	return func() { cb(next) }
}

// PID returns the child's process ID, or 0 if not running.
func (s *Supervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// WaitForHealthy polls the child's TCP port until a connection succeeds or
// timeout elapses. Use after Start() to know when the server is ready.
func (s *Supervisor) WaitForHealthy(timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", s.Port())
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("serve not healthy at %s within %s", addr, timeout)
}
