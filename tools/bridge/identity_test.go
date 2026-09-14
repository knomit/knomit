package main

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"knomit/internal/client/sessions"
)

func TestBuildIdentity_DistinctPerProcessStableWithin(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	a := buildIdentity("agent/x", now)
	b := buildIdentity("agent/x", now)
	if a.InstanceID != b.InstanceID || len(a.InstanceID) != 16 {
		t.Fatalf("a=%s b=%s", a.InstanceID, b.InstanceID)
	}
	if a.PID != os.Getpid() || a.ParentPID != os.Getppid() || a.Transport != "stdio" || a.Branch != "agent/x" {
		t.Fatalf("%+v", a)
	}
	// A different start time (i.e. a different process) ⇒ a different id.
	if c := buildIdentity("agent/x", now.Add(time.Second)); c.InstanceID == a.InstanceID {
		t.Fatal("start time must be part of the hash")
	}
	if _, err := sessions.ParseClientHeader(a.Encode()); err != nil {
		t.Fatalf("identity must encode to a parseable header: %v", err)
	}
}

func TestParentAppName_Linux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	// Our own parent is whatever runs the test binary; we only need a
	// non-empty, path-free name.
	if name := parentAppName(os.Getppid()); name == "" {
		t.Fatal("empty parent name")
	}
	// A child we control: `sleep` run from here has us as parent, and its
	// comm is "sleep".
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep binary")
	}
	defer func() { _ = cmd.Process.Kill() }()
	if got := parentAppName(cmd.Process.Pid); got != "sleep" {
		t.Fatalf("got %q want sleep (pid %d)", got, cmd.Process.Pid)
	}
	if parentAppName(0) != "" {
		t.Fatal("pid 0 must yield empty, not error")
	}
}
