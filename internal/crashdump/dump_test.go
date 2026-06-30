package crashdump

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDumpGoroutinesWritesFile(t *testing.T) {
	dir := t.TempDir()

	path, err := DumpGoroutines(dir)
	if err != nil {
		t.Fatalf("DumpGoroutines: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("dump written to %s, want under %s", path, dir)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "goroutine ") {
		t.Errorf("dump does not look like a goroutine trace:\n%s", body)
	}
	if !strings.Contains(body, "crashdump.TestDumpGoroutinesWritesFile") {
		t.Errorf("dump does not include the calling goroutine")
	}
}

func TestDumpGoroutinesCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dumps")
	if _, err := DumpGoroutines(dir); err != nil {
		t.Fatalf("DumpGoroutines into missing dir: %v", err)
	}
}
