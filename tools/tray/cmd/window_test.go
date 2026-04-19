package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"knomit/tools/tray/internal/lockfile"
)

func TestResolveURL_ExplicitURLWins(t *testing.T) {
	got, err := resolveURL("http://explicit.example", "/nonexistent/lockfile")
	if err != nil {
		t.Fatalf("resolveURL: %v", err)
	}
	if got != "http://explicit.example" {
		t.Errorf("resolveURL = %q, want explicit URL", got)
	}
}

func TestResolveURL_FromLockfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	if err := lockfile.Write(path, lockfile.Info{PID: 1, Port: 45678, Version: "0"}); err != nil {
		t.Fatalf("seed lockfile: %v", err)
	}
	got, err := resolveURL("", path)
	if err != nil {
		t.Fatalf("resolveURL: %v", err)
	}
	want := "http://127.0.0.1:45678"
	if got != want {
		t.Errorf("resolveURL = %q, want %q", got, want)
	}
}

func TestResolveURL_NoURLNoLockfile(t *testing.T) {
	_, err := resolveURL("", filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatal("resolveURL = nil error, want error")
	}
	if !strings.Contains(err.Error(), "no --url given") {
		t.Errorf("error message = %q, want to mention --url", err.Error())
	}
}
