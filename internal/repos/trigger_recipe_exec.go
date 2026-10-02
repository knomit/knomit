// `exec` — the one privileged function of the recipe tier (F07 PR 5), and the
// ONLY non-test file in internal/repos that imports os/exec (a structural
// test pins that). A `do: script` host never has it: recipeHostFunctions adds
// it to a recipe's map and nowhere else.
//
//		exec(argv, {stdin, env, cwd, timeout_ms}) → {exit, stdout, stderr, truncated?}
//
//	  - argv only: a non-empty array of strings, run as argv[0] with argv[1:],
//	    never through a shell, never joined or split. argv[0] without a path
//	    separator is resolved against the CHILD's PATH.
//	  - env MERGES over the recipe's base environment (knomit's own plus
//	    KNOMIT_SERVER/HOME/TRACE/CAUSE/RUN); it never replaces it, and a key
//	    it names wins over the base, KNOMIT_SERVER included.
//	  - the whole process GROUP (Unix) or job object (Windows) is killed when
//	    the call's timeout, the recipe's budget or knomit's stop ends it; a
//	    grandchild holding the pipes cannot keep the call alive beyond
//	    recipeWaitDelay. A child that exits normally leaves any grandchild it
//	    detached alive on both OSes (status `spawned`).
//	  - stdout and stderr are each capped at recipeOutputCap; bytes beyond it
//	    are read and discarded (the child never blocks on a full pipe) and the
//	    result says truncated: true.
//	  - a start failure or a timeout THROWS; a non-zero exit is `exit`.
package repos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// recipeWaitDelay bounds how long an ended call waits for its pipes after
	// the child exits or is killed (a grandchild may hold them).
	recipeWaitDelay = 2 * time.Second
	// recipeOutputCap caps stdout and stderr each.
	recipeOutputCap = 1 << 20
)

// recipeExecHooks are test seams; nil in production. afterStart runs right
// after the child is in its kill group: after AssignProcessToJobObject on
// Windows, after Start on Unix (Setpgid holds from the fork). A test's helper
// waits for it before forking a grandchild, so the test never races the
// assignment it does not test [R2-1].
var (
	recipeExecHooksMu sync.Mutex
	recipeExecHooks   struct {
		afterStart func(pid int)
	}
)

func currentAfterStart() func(pid int) {
	recipeExecHooksMu.Lock()
	defer recipeExecHooksMu.Unlock()
	return recipeExecHooks.afterStart
}

// recipeExec is one recipe run's exec: its ctx (the budget, knomit's stop)
// and its base environment, built per call because it waits for the server's
// own address (recipeEnv) and an error there means the child is not started.
type recipeExec struct {
	ctx context.Context
	env func(ctx context.Context) ([]string, error)
}

// capWriter keeps the first max bytes and discards the rest, never failing a
// write (a failed write would make the copying goroutine stop reading, and the
// child would block on a full pipe).
type capWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
			w.truncated = true
		} else {
			w.buf.Write(p)
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}

// call is the `exec` host function.
func (x *recipeExec) call(args []any) (any, error) {
	raw, ok := argAt(args, 0).([]any)
	if !ok || len(raw) == 0 {
		return nil, errors.New("exec(argv, opts?): argv must be a non-empty array of strings")
	}
	argv := make([]string, len(raw))
	for i, a := range raw {
		s, ok := a.(string)
		if !ok {
			return nil, fmt.Errorf("exec: argv[%d] is not a string", i)
		}
		argv[i] = s
	}
	if argv[0] == "" {
		return nil, errors.New("exec: argv[0] is empty")
	}
	opts := objectArg(args, 1)
	over := map[string]string{}
	if e, ok := opts["env"].(map[string]any); ok {
		for k, v := range e {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("exec: env.%s is not a string", k)
			}
			over[k] = s
		}
	} else if opts["env"] != nil {
		return nil, errors.New("exec: env must be an object of strings")
	}
	base, err := x.env(x.ctx)
	if err != nil {
		return nil, err
	}
	env := mergeEnv(base, over)
	ctx := x.ctx
	var timeout time.Duration
	if v, ok := opts["timeout_ms"]; ok && v != nil {
		ms, ok := v.(float64)
		if !ok || ms <= 0 {
			return nil, errors.New("exec: timeout_ms must be a positive number")
		}
		timeout = time.Duration(ms) * time.Millisecond
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	path, err := lookPathIn(argv[0], envValue(env, "PATH"))
	if err != nil {
		return nil, fmt.Errorf("exec: %s: %w", argv[0], err)
	}
	cmd := exec.CommandContext(ctx, path, argv[1:]...)
	cmd.Args[0] = argv[0] // the child sees argv verbatim
	cmd.Env = env
	if cwd, ok := opts["cwd"].(string); ok && cwd != "" {
		cmd.Dir = cwd
	}
	if in, ok := opts["stdin"].(string); ok {
		cmd.Stdin = strings.NewReader(in)
	}
	stdout := &capWriter{max: recipeOutputCap}
	stderr := &capWriter{max: recipeOutputCap}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = recipeWaitDelay
	grp := newProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		grp.close(false)
		return nil, fmt.Errorf("exec: start %s: %w", argv[0], err)
	}
	grp.started(cmd)
	if h := currentAfterStart(); h != nil {
		h(cmd.Process.Pid)
	}
	werr := cmd.Wait()
	clean := ctx.Err() == nil
	grp.close(clean)
	if !clean {
		if x.ctx.Err() != nil {
			return nil, fmt.Errorf("exec: %s cancelled: %w", argv[0], context.Cause(x.ctx))
		}
		return nil, fmt.Errorf("exec: %s timed out after %d ms", argv[0], timeout.Milliseconds())
	}
	if werr != nil && cmd.ProcessState == nil {
		return nil, fmt.Errorf("exec: %s: %w", argv[0], werr)
	}
	out := map[string]any{
		"exit":   cmd.ProcessState.ExitCode(),
		"stdout": stdout.buf.String(),
		"stderr": stderr.buf.String(),
	}
	if stdout.truncated || stderr.truncated {
		out["truncated"] = true
	}
	return out, nil
}

// lookPathIn resolves file against pathList (the CHILD's PATH) when it has no
// path separator; a name with one is used as given.
func lookPathIn(file, pathList string) (string, error) {
	if strings.ContainsAny(file, `/\`) {
		return file, nil
	}
	for _, dir := range filepath.SplitList(pathList) {
		if dir == "" {
			continue
		}
		if p, err := exec.LookPath(filepath.Join(dir, file)); err == nil {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// envKey compares environment names the way the OS does: case-insensitively
// on Windows.
func envKey(k string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(k)
	}
	return k
}

// mergeEnv returns base with every key of over set (replacing any entry of the
// same name), base's order kept and the new keys appended sorted.
func mergeEnv(base []string, over map[string]string) []string {
	out := make([]string, 0, len(base)+len(over))
	seen := map[string]bool{}
	idx := map[string]string{}
	for k := range over {
		idx[envKey(k)] = k
	}
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if orig, ok := idx[envKey(k)]; ok {
			if !seen[envKey(k)] {
				out = append(out, orig+"="+over[orig])
				seen[envKey(k)] = true
			}
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(over))
	for k := range over {
		if !seen[envKey(k)] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+over[k])
	}
	return out
}

// envValue is the value of key in env ("" when absent), the last one winning.
func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && envKey(k) == envKey(key) {
			v = val
		}
	}
	return v
}
