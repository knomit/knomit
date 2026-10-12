package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"knomit/internal/platform/version"
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

// The desktop reads `knomit version` from STDOUT (tools/desktop/serverbin.go
// serverVersion). The test above cannot see which stream the line goes to:
// SetOut redirects cobra's Println too, which otherwise writes to STDERR. So
// this one runs `knomit version` through the real root command in a child
// process with nothing redirected, and reads the process's real stdout and
// stderr separately — the same view the desktop has.
func TestVersionCmd_PrintsToStdoutInARealProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestVersionCmdHelper$")
	cmd.Env = append(os.Environ(), "KNOMIT_VERSION_HELPER=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), version.String()+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q (stderr %q): the desktop's version check reads stdout", got, want, stderr.String())
	}
	if strings.Contains(stderr.String(), version.String()) {
		t.Errorf("the version also went to stderr: %q", stderr.String())
	}
}

// TestVersionCmdHelper is the child process of the test above: the real root
// command, real os.Stdout/os.Stderr, args `version`.
func TestVersionCmdHelper(t *testing.T) {
	if os.Getenv("KNOMIT_VERSION_HELPER") == "" {
		t.Skip("helper process only")
	}
	root := RootCmd()
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		os.Exit(2)
	}
	os.Exit(0) // before the testing framework prints PASS to stdout
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
