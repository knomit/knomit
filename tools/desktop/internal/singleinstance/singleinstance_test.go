package singleinstance_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"knomit/internal/serveraddr"
	"knomit/tools/desktop/internal/singleinstance"
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
	must(t, serveraddr.WriteLock(path, serveraddr.LockInfo{PID: 99999, Port: 10001, Version: "0.1.0"}))

	if err := singleinstance.Acquire(path); err != nil {
		t.Errorf("Acquire(stale) = %v, want nil (stale treated as free)", err)
	}
}

// Regression: a corrupt/unparseable lockfile records no live instance, so
// Acquire must proceed (return nil) — bootServer overwrites it. Previously the
// raw parse error propagated and the caller misreported "already running",
// bricking startup.
func TestAcquire_CorruptFile_ReturnsNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	if err := os.WriteFile(path, []byte("not json{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := singleinstance.Acquire(path); err != nil {
		t.Errorf("Acquire(corrupt) = %v, want nil (corrupt treated as free)", err)
	}
}

func TestAcquire_LivePID_ReturnsErrAlreadyRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	must(t, serveraddr.WriteLock(path, serveraddr.LockInfo{PID: os.Getpid(), Port: 10002, Version: "0.1.0"}))

	err := singleinstance.Acquire(path)
	if !errors.Is(err, singleinstance.ErrAlreadyRunning) {
		t.Errorf("Acquire(live) = %v, want ErrAlreadyRunning", err)
	}
}

// `knomit serve` writes server.json for its own home too (schema 2), so a
// live pid in it is a SERVER, not another desktop. Counting it would make
// the desktop refuse to start beside a running `knomit serve` with a message
// about itself. The positive control is the previous test: the same live pid
// in a file with no schema blocks.
func TestAcquire_LiveServerFileIsNotAnotherDesktop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	must(t, serveraddr.WriteLock(path, serveraddr.LockInfo{PID: os.Getpid(), Port: 10003, Version: "0.1.0", Schema: 2}))
	if err := singleinstance.Acquire(path); err != nil {
		t.Errorf("Acquire(live knomit serve) = %v, want nil", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
