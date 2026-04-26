package singleinstance_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"knomit/tools/tray/internal/lockfile"
	"knomit/tools/tray/internal/singleinstance"
)

func TestAcquire_NoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	if err := singleinstance.Acquire(path); err != nil {
		t.Errorf("Acquire(no file) = %v, want nil", err)
	}
}

func TestAcquire_StalePID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	// 99999 is overwhelmingly likely to be dead; bump higher if flaky on exotic systems.
	must(t, lockfile.Write(path, lockfile.Info{PID: 99999, Port: 10001, Version: "0.1.0"}))

	if err := singleinstance.Acquire(path); err != nil {
		t.Errorf("Acquire(stale) = %v, want nil (stale treated as free)", err)
	}
}

func TestAcquire_LivePID_ReturnsErrAlreadyRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	must(t, lockfile.Write(path, lockfile.Info{PID: os.Getpid(), Port: 10002, Version: "0.1.0"}))

	err := singleinstance.Acquire(path)
	if !errors.Is(err, singleinstance.ErrAlreadyRunning) {
		t.Errorf("Acquire(live) = %v, want ErrAlreadyRunning", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
