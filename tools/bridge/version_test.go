package main

import (
	"bytes"
	"testing"

	"knomit/internal/version"
)

func TestRunVersion_PrintsAndHandles(t *testing.T) {
	orig, origCommit := version.Version, version.Commit
	t.Cleanup(func() { version.Version, version.Commit = orig, origCommit })
	version.Version, version.Commit = "0.5.0", "2a7ae9d"

	var out bytes.Buffer
	handled := runVersion([]string{"version"}, &out)

	if !handled {
		t.Fatal("runVersion should handle the 'version' subcommand")
	}
	if got, want := out.String(), "0.5.0.2a7ae9d\n"; got != want {
		t.Errorf("version output = %q, want %q", got, want)
	}
}

func TestRunVersion_IgnoresOtherArgs(t *testing.T) {
	var out bytes.Buffer
	if runVersion([]string{"claude", "init"}, &out) {
		t.Error("runVersion should not handle non-version args")
	}
	if out.Len() != 0 {
		t.Errorf("runVersion wrote %q for non-version args", out.String())
	}
}

func TestRunVersion_EmptyArgs(t *testing.T) {
	var out bytes.Buffer
	if runVersion(nil, &out) {
		t.Error("runVersion should not handle empty args")
	}
}
