package crashdump

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardWritesReportAndRepanics(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, NewRingWriter(0))

	repanicked := false
	func() {
		// Registered FIRST → runs LAST → catches Guard's re-panic.
		defer func() {
			if rec := recover(); rec != nil {
				repanicked = true
			}
		}()
		// Registered SECOND → runs FIRST → recovers, writes report, re-panics.
		defer r.Guard("worker")
		panic("kaboom")
	}()

	if !repanicked {
		t.Fatal("Guard swallowed the panic; it must re-panic so GOTRACEBACK=crash fires")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read crash dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d crash reports, want 1", len(entries))
	}

	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var rep Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rep.Component != "worker" {
		t.Errorf("Component = %q, want worker", rep.Component)
	}
	if !strings.Contains(rep.Cause, "kaboom") {
		t.Errorf("Cause = %q, want it to mention kaboom", rep.Cause)
	}
	if !strings.Contains(rep.Stack, "crashdump.TestGuardWritesReportAndRepanics") {
		t.Errorf("Stack does not capture the panicking goroutine: %q", rep.Stack)
	}
}

func TestGuardNoPanicNoReport(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, NewRingWriter(0))

	func() {
		defer r.Guard("worker")
		// no panic
	}()

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("Guard wrote %d reports on a clean return, want 0", len(entries))
	}
}
