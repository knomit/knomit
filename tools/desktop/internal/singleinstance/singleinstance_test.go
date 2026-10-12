package singleinstance_test

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knomit/tools/desktop/internal/singleinstance"
)

func TestAcquire_SecondHolderIsRefusedUntilRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop.lock")
	first, err := singleinstance.Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if _, err := singleinstance.Acquire(path); !errors.Is(err, singleinstance.ErrAlreadyRunning) {
		t.Fatalf("second Acquire while held = %v, want ErrAlreadyRunning", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("second Release (idempotent) = %v", err)
	}
	again, err := singleinstance.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after Release = %v, want nil", err)
	}
	_ = again.Release()
}

// Two data roots, two locks: a desktop on a scratch KNOMIT_HOME runs beside
// the user's (the path is derived from the home by the caller).
func TestAcquire_DifferentPathsDoNotConflict(t *testing.T) {
	a, err := singleinstance.Acquire(filepath.Join(t.TempDir(), "desktop.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := singleinstance.Acquire(filepath.Join(t.TempDir(), "desktop.lock"))
	if err != nil {
		t.Fatalf("Acquire of another home's lock = %v, want nil", err)
	}
	_ = b.Release()
}

// A file left behind (a desktop that crashed, an old build's leftovers) is not
// a holder: only a live lock is.
func TestAcquire_LeftoverFileIsNotAHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop.lock")
	if err := os.WriteFile(path, []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := singleinstance.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire over a leftover file = %v, want nil", err)
	}
	_ = l.Release()
}

// The property the PID-in-server.json scheme could not give: the OS frees the
// lock when the holder dies, even by SIGKILL, with nothing to clean up.
func TestAcquire_LockDiesWithItsProcess(t *testing.T) {
	if os.Getenv("SINGLEINSTANCE_HOLD") != "" {
		return // the helper's own run of this test: see holdHelper
	}
	path := filepath.Join(t.TempDir(), "desktop.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHoldHelper$")
	cmd.Env = append(os.Environ(), "SINGLEINSTANCE_HOLD="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	held := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "HELD") {
				held <- sc.Text()
				return
			}
		}
		held <- ""
	}()
	select {
	case line := <-held:
		if line == "" {
			t.Fatal("helper exited without taking the lock")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("helper did not take the lock")
	}
	if _, err := singleinstance.Acquire(path); !errors.Is(err, singleinstance.ErrAlreadyRunning) {
		t.Fatalf("Acquire while another PROCESS holds the lock = %v, want ErrAlreadyRunning", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	l, err := singleinstance.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after the holder was killed = %v, want nil (the OS releases it)", err)
	}
	_ = l.Release()
}

// TestHoldHelper is the child process of TestAcquire_LockDiesWithItsProcess:
// it takes the lock, says so, and waits to be killed.
func TestHoldHelper(t *testing.T) {
	path := os.Getenv("SINGLEINSTANCE_HOLD")
	if path == "" {
		t.Skip("helper process only")
	}
	if _, err := singleinstance.Acquire(path); err != nil {
		os.Stdout.WriteString("ERR " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("HELD\n")
	time.Sleep(time.Minute)
	os.Exit(0)
}

func TestMutexNameIsPerPathAndCaseInsensitive(t *testing.T) {
	a := singleinstance.MutexName(`C:\Users\x\AppData\Local\knomit\desktop.lock`)
	b := singleinstance.MutexName(`c:\users\X\appdata\local\KNOMIT\desktop.lock`)
	c := singleinstance.MutexName(`C:\scratch\desktop.lock`)
	if a != b {
		t.Errorf("case variants differ: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("two homes share a mutex: %q", a)
	}
	if !strings.HasPrefix(a, `Local\knomit-desktop-`) {
		t.Errorf("name %q is not session-local", a)
	}
}
