package crashdump

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRedirectStderrSubprocess re-execs a child that redirects its stderr to a
// file, then writes to fd 2 and triggers a runtime crash. Both the explicit
// stderr write and the runtime's fatal traceback (which goes straight to fd 2,
// bypassing the logger) must land in the file — that is the whole point: a
// CGO/ONNX-class crash leaves a trace on disk.
func TestRedirectStderrSubprocess(t *testing.T) {
	if os.Getenv("REDIRECT_SUBPROC") == "1" {
		path := os.Getenv("REDIRECT_FILE")
		f, err := RedirectStderr(path)
		if err != nil {
			// Can't use stderr meaningfully; signal via exit code.
			os.Exit(3)
		}
		defer f.Close()
		os.Stderr.WriteString("marker-on-fd2\n")
		os.Exit(0)
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "crash.log")
	cmd := exec.Command(os.Args[0], "-test.run", "TestRedirectStderrSubprocess")
	cmd.Env = append(os.Environ(), "REDIRECT_SUBPROC=1", "REDIRECT_FILE="+file)
	if err := cmd.Run(); err != nil {
		t.Fatalf("subprocess failed: %v", err)
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read redirected stderr file: %v", err)
	}
	if !strings.Contains(string(raw), "marker-on-fd2") {
		t.Errorf("fd-2 write did not land in the file: %q", raw)
	}
}

func TestRedirectStderrCreatesDir(t *testing.T) {
	if os.Getenv("REDIRECT_DIR_SUBPROC") == "1" {
		path := filepath.Join(os.Getenv("REDIRECT_DIR"), "nested", "crash.log")
		if _, err := RedirectStderr(path); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run", "TestRedirectStderrCreatesDir")
	cmd.Env = append(os.Environ(), "REDIRECT_DIR_SUBPROC=1", "REDIRECT_DIR="+dir)
	if err := cmd.Run(); err != nil {
		t.Fatalf("RedirectStderr into a missing dir failed (exit): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nested", "crash.log")); err != nil {
		t.Fatalf("crash-log file/dir not created: %v", err)
	}
}
