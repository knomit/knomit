package crashdump

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReporterWriteProducesBundle(t *testing.T) {
	dir := t.TempDir()
	ring := NewRingWriter(10)
	ring.Write([]byte("log line one\n"))
	ring.Write([]byte("log line two\n"))

	r := New(dir, ring)
	path, err := r.Write("indexer", "boom: nil map write", []byte("goroutine 1 [running]:\nmain.boom()"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if filepath.Dir(path) != dir {
		t.Fatalf("report written to %s, want under %s", path, dir)
	}
	if !strings.Contains(filepath.Base(path), "indexer") {
		t.Fatalf("filename %q does not name the component", filepath.Base(path))
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var rep Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}

	if rep.Component != "indexer" {
		t.Errorf("Component = %q, want indexer", rep.Component)
	}
	if rep.Cause != "boom: nil map write" {
		t.Errorf("Cause = %q", rep.Cause)
	}
	if !strings.Contains(rep.Stack, "main.boom()") {
		t.Errorf("Stack missing faulting frame: %q", rep.Stack)
	}
	if rep.GoroutineDump == "" {
		t.Error("GoroutineDump is empty; want all-goroutine dump")
	}
	if rep.MemStats.Sys == 0 {
		t.Error("MemStats not captured")
	}
	wantLogs := []string{"log line one", "log line two"}
	if strings.Join(rep.RecentLogs, "|") != strings.Join(wantLogs, "|") {
		t.Errorf("RecentLogs = %v, want %v", rep.RecentLogs, wantLogs)
	}
	if rep.Timestamp == "" {
		t.Error("Timestamp is empty")
	}
}

func TestReporterWriteCreatesDir(t *testing.T) {
	// The crashes directory should be created on demand — a crash must not be
	// lost because the directory did not exist yet.
	dir := filepath.Join(t.TempDir(), "nested", "crashes")
	r := New(dir, NewRingWriter(0))
	path, err := r.Write("main", "panic", []byte("stack"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("report file not created: %v", err)
	}
}
