package repos

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---- The recipe helper process (F07 PR 5)
//
// Processes a recipe starts in these tests are the TEST BINARY re-executed
// with KNOMIT_RECIPE_HELPER=<report dir> in its environment (TestMain hands
// control to recipeHelperMain before anything else), so no shell is needed on
// any OS. The helper records what it was given — argv, env, stdin, its pid
// and its parent's — as JSON in the report dir, then does what the HELPER_*
// variables say:
//
//	HELPER_WAIT_GO=1       wait for <dir>/go-<pid> (the exec test hook writes it
//	                       once the child is in its kill group) before forking
//	HELPER_FORK=hold|detach start a grandchild that holds the stdout/stderr
//	                       pipes (hold) or has its stdio redirected away (detach)
//	HELPER_WRITE_BYTES=n   write n bytes to stdout
//	HELPER_BLOCK_FILE=f    wait until f exists (max 60 s)
//	HELPER_SLEEP_MS=n      sleep
//	HELPER_STDOUT=s        print s to stdout
//	HELPER_EXIT=n          exit code
//
// A grandchild (HELPER_ROLE=grandchild) records itself and sleeps 60 s.
//
// Started under the NAME mktemp or rm (the mission e2e links the test binary
// under those names beside its fake claude), the helper stands in for those
// tools instead: `mktemp -d` makes an empty directory under the report dir,
// prints it and records it (role "mktemp"); `rm -rf <dir>` removes it and
// records the argv (role "rm"). Every report carries the process's working
// directory and how many entries it held at start.

const recipeHelperEnv = "KNOMIT_RECIPE_HELPER"

// helperReport is what one helper process recorded.
type helperReport struct {
	Role  string   `json:"role"`
	Pid   int      `json:"pid"`
	Ppid  int      `json:"ppid"`
	Args  []string `json:"args"`
	Env   []string `json:"env"`
	Stdin string   `json:"stdin"`
	Cwd   string   `json:"cwd"`
	// CwdEntries is how many entries the working directory held at start
	// (-1 when it could not be read).
	CwdEntries int `json:"cwd_entries"`
}

// maybeRunRecipeHelper turns this process into the helper when the env marker
// is set. Called first thing in TestMain.
func maybeRunRecipeHelper() {
	if dir := os.Getenv(recipeHelperEnv); dir != "" {
		os.Exit(recipeHelperMain(dir))
	}
}

func recipeHelperMain(dir string) int {
	role := os.Getenv("HELPER_ROLE")
	if role == "" {
		role = "child"
	}
	rep := helperReport{Role: role, Pid: os.Getpid(), Ppid: os.Getppid(), Args: os.Args, Env: os.Environ(), CwdEntries: -1}
	if wd, err := os.Getwd(); err == nil {
		rep.Cwd = wd
		if ents, err := os.ReadDir(wd); err == nil {
			rep.CwdEntries = len(ents)
		}
	}
	switch strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") {
	case "mktemp":
		made, err := os.MkdirTemp(dir, "session-")
		if err != nil {
			return 96
		}
		rep.Role, rep.Stdin = "mktemp", made
		writeHelperReport(dir, rep)
		fmt.Println(made)
		return 0
	case "rm":
		rep.Role = "rm"
		if len(os.Args) > 1 {
			_ = os.RemoveAll(os.Args[len(os.Args)-1])
		}
		writeHelperReport(dir, rep)
		return 0
	}
	if role == "grandchild" {
		writeHelperReport(dir, rep)
		time.Sleep(60 * time.Second)
		return 0
	}
	if b, err := io.ReadAll(os.Stdin); err == nil {
		rep.Stdin = string(b)
	}
	writeHelperReport(dir, rep)
	if os.Getenv("HELPER_WAIT_GO") == "1" {
		goFile := filepath.Join(dir, fmt.Sprintf("go-%d", os.Getpid()))
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if _, err := os.Stat(goFile); err == nil {
				break
			}
		}
	}
	if mode := os.Getenv("HELPER_FORK"); mode != "" {
		gc := exec.Command(os.Args[0])
		gc.Env = append(os.Environ(), "HELPER_ROLE=grandchild")
		if mode == "hold" {
			gc.Stdout, gc.Stderr = os.Stdout, os.Stderr
		}
		if err := gc.Start(); err != nil {
			return 97
		}
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("forked-%d", gc.Process.Pid)), nil, 0o600)
		// Wait for the grandchild's own report so the test knows its pid.
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("grandchild-%d.json", gc.Process.Pid))); err == nil {
				break
			}
		}
		_ = gc.Process.Release()
	}
	if n, _ := strconv.Atoi(os.Getenv("HELPER_WRITE_BYTES")); n > 0 {
		chunk := []byte(strings.Repeat("x", 64*1024))
		for n > 0 {
			k := min(n, len(chunk))
			if _, err := os.Stdout.Write(chunk[:k]); err != nil {
				return 98
			}
			n -= k
		}
	}
	if f := os.Getenv("HELPER_BLOCK_FILE"); f != "" {
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(f); err == nil {
				break
			}
		}
	}
	if ms, _ := strconv.Atoi(os.Getenv("HELPER_SLEEP_MS")); ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	if s := os.Getenv("HELPER_STDOUT"); s != "" {
		fmt.Print(s)
	}
	code, _ := strconv.Atoi(os.Getenv("HELPER_EXIT"))
	return code
}

func writeHelperReport(dir string, rep helperReport) {
	b, _ := json.Marshal(rep)
	tmp := filepath.Join(dir, fmt.Sprintf(".%s-%d.tmp", rep.Role, rep.Pid))
	_ = os.WriteFile(tmp, b, 0o600)
	_ = os.Rename(tmp, filepath.Join(dir, fmt.Sprintf("%s-%d.json", rep.Role, rep.Pid)))
}

// helperReports reads every report of one role in dir.
func helperReports(t *testing.T, dir, role string) []helperReport {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, role+"-*.json"))
	require.NoError(t, err)
	var out []helperReport
	for _, m := range matches {
		b, err := os.ReadFile(m)
		require.NoError(t, err)
		var r helperReport
		require.NoError(t, json.Unmarshal(b, &r), m)
		out = append(out, r)
	}
	return out
}

// waitHelperReports waits until dir holds n reports of role and returns them.
func waitHelperReports(t *testing.T, dir, role string, n int) []helperReport {
	t.Helper()
	var got []helperReport
	require.Eventually(t, func() bool {
		got = helperReports(t, dir, role)
		return len(got) >= n
	}, 30*time.Second, 10*time.Millisecond, "expected %d %s report(s) in %s", n, role, dir)
	return got
}

// helperExe is the test binary, which is the helper.
func helperExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	return exe
}

// helperEnv renders the env object a recipe passes to exec: the marker plus
// the behaviour variables.
func helperEnv(dir string, kv ...string) string {
	m := map[string]string{recipeHelperEnv: dir}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// jsString is a JavaScript string literal (JSON is a subset).
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// installGoSignal makes the exec test hook write <dir>/go-<pid> once the child
// is in its kill group [R2-1], for the test's duration.
func installGoSignal(t *testing.T, dir string) {
	t.Helper()
	recipeExecHooksMu.Lock()
	recipeExecHooks.afterStart = func(pid int) {
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("go-%d", pid)), []byte{1}, 0o600)
	}
	recipeExecHooksMu.Unlock()
	t.Cleanup(func() {
		recipeExecHooksMu.Lock()
		recipeExecHooks.afterStart = nil
		recipeExecHooksMu.Unlock()
	})
}

// envOf reads one variable out of a helper's recorded environment.
func envOf(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && envKey(k) == envKey(key) {
			return v, true
		}
	}
	return "", false
}
