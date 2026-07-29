package homelock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// holderEnv makes the test binary re-enter itself as a process that takes the
// lock and then hangs, so the stale-lock guarantee can be tested against a real
// child that is really killed. Simulating it in-process by closing a descriptor
// would test our own close path, not the kernel's cleanup on process death —
// and the kernel's cleanup is the entire claim.
const (
	holderEnv   = "KNOMIT_TEST_HOMELOCK_HOLD"
	readyEnvKey = "KNOMIT_TEST_HOMELOCK_READY"
)

func TestMain(m *testing.M) {
	if home := os.Getenv(holderEnv); home != "" {
		if _, err := Acquire(home); err != nil {
			os.Exit(1)
		}
		// Announce that the lock is held, then wait to be killed. A bare
		// `select {}` would be reaped by Go's deadlock detector — the child would
		// die on its own and release the lock, quietly turning this into a test
		// of nothing.
		if ready := os.Getenv(readyEnvKey); ready != "" {
			_ = os.WriteFile(ready, []byte("held"), 0o644)
		}
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestAcquireAndRelease(t *testing.T) {
	home := t.TempDir()
	l, err := Acquire(home)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, LockFile)); err != nil {
		t.Errorf("lock file missing: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Releasing must actually free it, not merely stop reporting.
	l2, err := Acquire(home)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	_ = l2.Release()
}

// TestAcquireRefusesAHeldHome is the property `knomit restore` depends on.
// flock is bound to the open file description, not the process, so a second
// open of the same path conflicts even here.
func TestAcquireRefusesAHeldHome(t *testing.T) {
	home := t.TempDir()
	l, err := Acquire(home)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	_, err = Acquire(home)
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire = %v, want ErrHeld", err)
	}
	// The message must name the holder, or an operator has nothing to act on.
	if !strings.Contains(err.Error(), "pid") {
		t.Errorf("error %q does not name the holding process", err)
	}
}

// TestAcquireSucceedsAfterTheHolderIsKilled is the reason this is an advisory
// lock and not a PID file. Recovery is exactly when `knomit restore` is needed,
// so a lock that survives the crash it is meant to help you recover from would
// be worse than no lock at all.
func TestAcquireSucceedsAfterTheHolderIsKilled(t *testing.T) {
	home := t.TempDir()
	ready := filepath.Join(t.TempDir(), "ready")

	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), holderEnv+"="+home, readyEnvKey+"="+ready)
	if err := child.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	defer func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	}()

	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the holder child never took the lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := Acquire(home); !errors.Is(err, ErrHeld) {
		t.Fatalf("Acquire while a live child holds it = %v, want ErrHeld", err)
	}

	// SIGKILL: no handler of ours runs, so only the kernel can release this.
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill holder: %v", err)
	}
	if _, err := child.Process.Wait(); err != nil {
		t.Fatalf("reap holder: %v", err)
	}

	l, err := Acquire(home)
	if err != nil {
		t.Fatalf("Acquire after the holder was killed = %v; a stale lock would block the recovery "+
			"this command exists for", err)
	}
	_ = l.Release()
}

// TestReleaseIsNilSafe: callers defer it unconditionally, including on the path
// where Acquire failed.
func TestReleaseIsNilSafe(t *testing.T) {
	var l *Lock
	if err := l.Release(); err != nil {
		t.Errorf("(*Lock)(nil).Release() = %v, want nil", err)
	}
	if l.Path() != "" {
		t.Errorf("(*Lock)(nil).Path() = %q, want empty", l.Path())
	}
}
