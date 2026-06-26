package cmd

import (
	"bytes"
	"testing"

	"knomit/internal/version"
)

func TestVersionCmd_PrintsVersionString(t *testing.T) {
	orig, origCommit := version.Version, version.Commit
	t.Cleanup(func() { version.Version, version.Commit = orig, origCommit })
	version.Version, version.Commit = "0.5.0", "2a7ae9d"

	c := versionCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	if err := c.Execute(); err != nil {
		t.Fatalf("version command: %v", err)
	}

	if got, want := out.String(), "0.5.0.2a7ae9d\n"; got != want {
		t.Errorf("version output = %q, want %q", got, want)
	}
}

func TestRootCmd_HasVersionSubcommand(t *testing.T) {
	root := RootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "version" {
			return
		}
	}
	t.Error("root command missing 'version' subcommand")
}
