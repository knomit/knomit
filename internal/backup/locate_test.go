package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubAgent writes an executable file at path so locateAgent can find it. Its
// contents do not matter — locateAgent decides on existence, not on behaviour.
func stubAgent(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })
	return path
}

// TestLocateAgentPrefersTheBinaryBesideTheExecutable pins the rule the Makefile
// and the Dockerfile are built around: both drop knomit-backup into the same
// directory as knomit, and nothing else configures the path. If this search
// step were dropped, every packaged deployment would refuse to boot.
//
// os.Executable() is the test binary here, so the stub goes beside that.
func TestLocateAgentPrefersTheBinaryBesideTheExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	beside := stubAgent(t, filepath.Join(filepath.Dir(exe), agentBinary))
	home := t.TempDir()
	inHome := stubAgent(t, filepath.Join(home, "bin", agentBinary))

	got, err := locateAgent("", home)
	if err != nil {
		t.Fatalf("locateAgent: %v", err)
	}
	if got != beside {
		t.Errorf("locateAgent = %q, want the sibling %q (it found %q instead)", got, beside, inHome)
	}
}

// TestLocateAgentFallsBackToKnomitHomeBin covers the operator who dropped the
// agent beside the data rather than beside the binary.
func TestLocateAgentFallsBackToKnomitHomeBin(t *testing.T) {
	home := t.TempDir()
	want := stubAgent(t, filepath.Join(home, "bin", agentBinary))
	t.Setenv("PATH", t.TempDir())

	got, err := locateAgent("", home)
	if err != nil {
		t.Fatalf("locateAgent: %v", err)
	}
	if got != want {
		t.Errorf("locateAgent = %q, want %q", got, want)
	}
}

// TestLocateAgentFallsBackToPATH covers a system install.
func TestLocateAgentFallsBackToPATH(t *testing.T) {
	dir := t.TempDir()
	want := stubAgent(t, filepath.Join(dir, agentBinary))
	t.Setenv("PATH", dir)

	got, err := locateAgent("", t.TempDir())
	if err != nil {
		t.Fatalf("locateAgent: %v", err)
	}
	if got != want {
		t.Errorf("locateAgent = %q, want %q", got, want)
	}
}

// TestLocateAgentOverrideWinsAndDoesNotFallThrough: an explicit path that is
// wrong must fail rather than quietly resolve to some other binary. Silently
// running a different agent than the one configured is worse than not starting.
func TestLocateAgentOverrideWinsAndDoesNotFallThrough(t *testing.T) {
	dir := t.TempDir()
	onPath := stubAgent(t, filepath.Join(dir, agentBinary))
	t.Setenv("PATH", dir)

	missing := filepath.Join(t.TempDir(), "configured-agent")
	got, err := locateAgent(missing, t.TempDir())
	if err == nil {
		t.Fatalf("locateAgent = %q, want a failure: the override fell through to %q", got, onPath)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the configured path", err)
	}

	want := stubAgent(t, missing)
	got, err = locateAgent(want, t.TempDir())
	if err != nil || got != want {
		t.Fatalf("locateAgent(override) = (%q, %v), want %q", got, err, want)
	}
}

// TestLocateAgentRejectsADirectory: a directory named knomit-backup must not
// be mistaken for the binary, or the failure surfaces as an unhelpful exec
// error at spawn instead of a clear one at boot.
func TestLocateAgentRejectsADirectory(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "bin", agentBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())

	if got, err := locateAgent("", home); err == nil {
		t.Fatalf("locateAgent = %q, want a failure for a directory", got)
	}
}
