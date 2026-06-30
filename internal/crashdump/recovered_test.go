package crashdump

import (
	"os"
	"strings"
	"testing"
)

func TestReportRecovered_WritesBundleViaGlobalReporter(t *testing.T) {
	dir := t.TempDir()
	SetGlobalReporter(New(dir, nil))
	defer SetGlobalReporter(nil)

	ReportRecovered("http", "boom from a handler")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d bundles, want 1", len(entries))
	}
	raw, _ := os.ReadFile(dir + "/" + entries[0].Name())
	if !strings.Contains(string(raw), "boom from a handler") {
		t.Errorf("bundle missing cause:\n%s", raw)
	}
	if !strings.Contains(entries[0].Name(), "http") {
		t.Errorf("bundle filename does not name the component: %s", entries[0].Name())
	}
}

func TestReportRecovered_NoReporterIsNoop(t *testing.T) {
	SetGlobalReporter(nil)
	// Must not panic or error when no reporter is installed.
	ReportRecovered("task", "ignored")
}
