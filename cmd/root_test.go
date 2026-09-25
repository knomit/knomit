package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// An error returned from any command is printed once, by main.go, and by
// nothing else: cobra adds neither its "Error:" line (the error would appear
// twice) nor the usage block (it buries the error under the flag list).
//
// What main.go prints is Execute's error. A command line cobra rejects ends
// with the "Run '<command> --help' for usage." hint; a RunE error does not.
func TestRoot_ErrorsPrintOnceWithTheHintOnlyForUsageMistakes(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
		hint string // "" = no hint
	}{
		// A RunE error. The config load fails on the env overlay, before
		// app.New, so no embedder or repo is ever built.
		{"serve config load fails", map[string]string{"KNOMIT_LLM_CACHE": "notabool"}, []string{"serve"}, ""},
		// An argument error on a SUBCOMMAND. Its parent `grants` once set
		// SilenceUsage itself, which cobra never consulted for `grants add`.
		{"grants add wrong arg count", nil, []string{"grants", "add", "x"}, "Run 'knomit grants add --help' for usage."},
		{"unknown command", nil, []string{"bogus"}, "Run 'knomit --help' for usage."},
		{"unknown flag", nil, []string{"serve", "--prot", "1"}, "Run 'knomit serve --help' for usage."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KNOMIT_HOME", t.TempDir())
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			root := RootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tc.args)
			err := Execute(context.Background(), root)
			if err == nil {
				t.Fatalf("%v: want an error, got none; output:\n%s", tc.args, out.String())
			}
			for _, bad := range []string{"Usage:", "Error:"} {
				if strings.Contains(out.String(), bad) {
					t.Errorf("%v printed %q through cobra; main.go is the only printer. output:\n%s", tc.args, bad, out.String())
				}
			}
			printed := err.Error()
			switch {
			case tc.hint == "" && strings.Contains(printed, "--help"):
				t.Errorf("%v is a RunE error and must carry no usage hint; main.go would print:\n%s", tc.args, printed)
			case tc.hint != "" && !strings.HasSuffix(printed, "\n"+tc.hint):
				t.Errorf("%v: main.go would print:\n%s\nwant it to end with %q", tc.args, printed, tc.hint)
			}
		})
	}
}

// Execute tells a usage mistake from a RunE error by whether the root's
// PersistentPreRun ran. With cobra.EnableTraverseRunHooks off, a subcommand's
// own PersistentPreRun(E) would REPLACE the root's for that subtree, and every
// RunE error there would gain the hint.
func TestRoot_NoSubcommandReplacesTheRunHook(t *testing.T) {
	if cobra.EnableTraverseRunHooks {
		t.Skip("traversing run hooks: a child hook no longer replaces the root's")
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.PersistentPreRun != nil || sub.PersistentPreRunE != nil {
				t.Errorf("%q defines its own PersistentPreRun(E), replacing the root's; Execute would put the usage hint on its RunE errors", sub.CommandPath())
			}
			walk(sub)
		}
	}
	walk(RootCmd())
}
