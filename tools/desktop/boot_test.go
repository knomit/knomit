//go:build desktop

package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"knomit/tools/desktop/internal/lockfile"
)

func TestBootServer_ServesAndWritesLockfile(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "server.json")
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	srv, port, err := bootServer(base, lockPath, "test-ver")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.shutdown()

	// Lockfile records the live port + this process's PID + version.
	info, err := lockfile.Read(lockPath)
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	if info.Port != port {
		t.Errorf("lockfile port: got %d, want %d", info.Port, port)
	}
	if info.Version != "test-ver" {
		t.Errorf("lockfile version: got %q, want %q", info.Version, "test-ver")
	}

	// Server is reachable on the chosen looknomitck port.
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBootServer_ShutdownRemovesLockfile(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "server.json")
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	srv, _, err := bootServer(base, lockPath, "v")
	if err != nil {
		t.Fatal(err)
	}
	srv.shutdown()
	if _, err := lockfile.Read(lockPath); err == nil {
		t.Error("shutdown must remove the lockfile")
	}
}
