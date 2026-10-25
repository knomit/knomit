package trigger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The exec layer of the recipe tier, driven directly: recipeExec.call starts
// the test binary itself as the child (recipeHelperEnv set), which reports
// what it was given. The process-group, timeout and boot-window tests run in
// the repos parent, through a real fire.

const recipeHelperEnv = "KNOMIT_TRIGGER_EXEC_HELPER"

// helperOut is what the helper child prints on stdout.
type helperOut struct {
	Args   []string `json:"args"`
	Server string   `json:"server"`
	Keep   string   `json:"keep"`
	Stdin  string   `json:"stdin"`
}

// maybeRunRecipeHelper turns this process into the helper child when the
// marker is set: mode "echo" prints a helperOut, writes "to stderr" on stderr
// and exits with $KNOMIT_TRIGGER_EXEC_EXIT; mode "flood" writes more than
// recipeOutputCap bytes to stdout and exits 0. Called first thing in TestMain.
func maybeRunRecipeHelper() {
	switch os.Getenv(recipeHelperEnv) {
	case "":
		return
	case "flood":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", recipeOutputCap+4096))
		os.Exit(0)
	default:
		in, _ := io.ReadAll(os.Stdin)
		b, _ := json.Marshal(helperOut{Args: os.Args, Server: os.Getenv("KNOMIT_SERVER"), Keep: os.Getenv("KNOMIT_KEEP"), Stdin: string(in)})
		_, _ = os.Stdout.Write(b)
		_, _ = os.Stderr.WriteString("to stderr")
		code, _ := strconv.Atoi(os.Getenv("KNOMIT_TRIGGER_EXEC_EXIT"))
		os.Exit(code)
	}
}

// helperExec is a recipeExec whose base environment is this process's plus
// KNOMIT_SERVER and KNOMIT_KEEP, as recipeEnv's base carries knomit's own.
func helperExec(t *testing.T) (*recipeExec, string) {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	base := append(os.Environ(), "KNOMIT_SERVER=base", "KNOMIT_KEEP=kept")
	return &recipeExec{ctx: context.Background(), env: func(context.Context) ([]string, error) { return base, nil }}, self
}

// ArgvEnvStdinExit: argv reaches the child verbatim (an argument with a space
// stays one argument, argv[0] is what the recipe passed), the recipe's env
// MERGES over the base (its KNOMIT_SERVER wins, the base's other keys
// survive), stdin is the child's stdin, and the result carries the exit code,
// stdout and stderr. Sabotage: merge the base over the recipe's env in
// mergeEnv's caller (red: server is "base"), or replace the base with it (red:
// keep is empty).
func TestRecipeExec_ArgvEnvStdinExit(t *testing.T) {
	x, self := helperExec(t)
	res, err := x.call([]any{
		[]any{self, "one arg", "two"},
		map[string]any{"stdin": "fed", "env": map[string]any{
			recipeHelperEnv: "echo", "KNOMIT_SERVER": "recipe", "KNOMIT_TRIGGER_EXEC_EXIT": "3",
		}},
	})
	require.NoError(t, err)
	out := res.(map[string]any)
	require.Equal(t, 3, out["exit"], "a non-zero exit is reported, not thrown")
	require.Equal(t, "to stderr", out["stderr"])
	require.NotContains(t, out, "truncated")
	var got helperOut
	require.NoError(t, json.Unmarshal([]byte(out["stdout"].(string)), &got), "%v", out["stdout"])
	require.Equal(t, []string{self, "one arg", "two"}, got.Args)
	require.Equal(t, "recipe", got.Server, "the recipe's env wins over the base")
	require.Equal(t, "kept", got.Keep, "the recipe's env merges over the base, it does not replace it")
	require.Equal(t, "fed", got.Stdin)
}

// OutputCapTruncates: stdout beyond recipeOutputCap is read and discarded (the
// child exits instead of blocking on a full pipe) and the result says
// truncated. Sabotage: drop the cap in capWriter.Write (red: the length).
func TestRecipeExec_OutputCapTruncates(t *testing.T) {
	x, self := helperExec(t)
	res, err := x.call([]any{[]any{self}, map[string]any{"env": map[string]any{recipeHelperEnv: "flood"}}})
	require.NoError(t, err)
	out := res.(map[string]any)
	require.Equal(t, 0, out["exit"])
	require.Equal(t, true, out["truncated"])
	require.Len(t, out["stdout"].(string), recipeOutputCap, fmt.Sprintf("stdout is capped at %d bytes", recipeOutputCap))
}

// ArgvMustBeStrings: exec refuses before starting anything when argv is not a
// non-empty array of strings.
func TestRecipeExec_ArgvMustBeStrings(t *testing.T) {
	x, _ := helperExec(t)
	for _, argv := range []any{nil, []any{}, []any{1}, []any{""}} {
		_, err := x.call([]any{argv})
		require.Error(t, err, "%v", argv)
	}
}
