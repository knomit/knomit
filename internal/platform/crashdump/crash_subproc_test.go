package crashdump

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// TestCrashProducesBundleSubprocess re-execs the test binary as a child that
// installs Guard, enables GOTRACEBACK=crash, and panics. It proves the full
// native crash path in a real process: the child must (a) exit non-zero,
// (b) print an all-goroutine traceback to stderr, and (c) leave a crash bundle
// on disk written before the re-panic killed it.
func TestCrashProducesBundleSubprocess(t *testing.T) {
	if os.Getenv("CRASHDUMP_SUBPROC") == "1" {
		dir := os.Getenv("CRASHDUMP_DIR")
		debug.SetTraceback("crash")
		r := New(dir, nil)
		defer r.Guard("subproc")
		panic("subprocess boom")
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run", "TestCrashProducesBundleSubprocess", "-test.v")
	cmd.Env = append(os.Environ(), "CRASHDUMP_SUBPROC=1", "CRASHDUMP_DIR="+dir)
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("subprocess exited 0; expected it to crash.\noutput:\n%s", out)
	}
	body := string(out)
	if !strings.Contains(body, "goroutine ") {
		t.Errorf("stderr missing all-goroutine traceback (GOTRACEBACK=crash):\n%s", body)
	}

	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatalf("read crash dir: %v", rerr)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d crash bundles, want 1", len(entries))
	}
	raw, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if !strings.Contains(string(raw), "subprocess boom") {
		t.Errorf("bundle does not record the panic cause:\n%s", raw)
	}
}
