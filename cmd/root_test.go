package cmd

import (
	"strings"
	"testing"
)

// An error returned from any command is printed once, by main.go, and by
// nothing else: cobra adds neither its "Error:" line (the error would appear
// twice) nor the usage block (it buries the error under the flag list).
func TestRoot_ErrorsPrintNothingThroughCobra(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
	}{
		// A RunE error. The config load fails on the env overlay, before
		// app.New, so no embedder or repo is ever built.
		{"serve config load fails", map[string]string{"KNOMIT_LLM_CACHE": "notabool"}, []string{"serve"}},
		// An argument error on a SUBCOMMAND. Its parent `grants` once set
		// SilenceUsage itself, which cobra never consulted for `grants add`.
		{"grants add wrong arg count", nil, []string{"grants", "add", "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KNOMIT_HOME", t.TempDir())
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			out, err := run(t, "", tc.args...)
			if err == nil {
				t.Fatalf("%v: want an error, got none; output:\n%s", tc.args, out)
			}
			for _, bad := range []string{"Usage:", "Error:"} {
				if strings.Contains(out, bad) {
					t.Errorf("%v printed %q through cobra; main.go is the only printer. output:\n%s", tc.args, bad, out)
				}
			}
		})
	}
}
