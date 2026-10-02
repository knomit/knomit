package repos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/serverkey"
	"knomit/internal/store"
)

// ---- Fixture (F07 PR 5, `do: run`)
//
// The script fixture (stub tools, a temp home) plus recipes: a LOCAL recipe
// is a file in <home>/recipes, a REPO recipe a commit of
// .knomit/recipes/<name>.js on MAIN (user ruling D1: "recipes run only from
// main"). Results arrive asynchronously, as a second fire-log row with the
// same run id; the tests read the TABLE through the store.

// runTrig renders a `do: run` entry.
func runTrig(name, on, match, recipe string) string {
	e := fmt.Sprintf("      - name: %s\n        on: %s\n        do: run\n        recipe: %s\n", name, on, recipe)
	if match != "" {
		e += fmt.Sprintf("        match: %q\n", match)
	}
	return e
}

func newRunRepo(t *testing.T, entries ...string) (*RepoInstance, string) {
	t.Helper()
	_, ri, _ := newScriptRepo(t, 0, entries...)
	return ri, ri.triggers.home
}

func putLocalRecipe(t *testing.T, home, name, src string) string {
	t.Helper()
	dir := filepath.Join(home, "recipes")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	p := filepath.Join(dir, name+".js")
	require.NoError(t, os.WriteFile(p, []byte(src), 0o600))
	return p
}

func localRev(src string) string {
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:])[:12]
}

// putMainRecipe commits the recipe onto MAIN (as a merge through the gate, or
// an edit in the forge's web editor, would put it there) and returns the
// file's blob hash there.
func putMainRecipe(t *testing.T, ri *RepoInstance, name, src string) string {
	t.Helper()
	svc := testService(t, ri)
	upstream := svc.UpstreamBranch()
	_, err := svc.Facts().WriteFact(context.Background(), upstream, fact.TriggerRecipePath(name), src, "recipe: "+name, "updated")
	require.NoError(t, err)
	tip, err := svc.Triggers().UpstreamTip(context.Background(), upstream)
	require.NoError(t, err)
	blob, _, err := svc.Triggers().RecipeAt(context.Background(), tip, name)
	require.NoError(t, err)
	return blob
}

// rowsOf is a trigger's rows, OLDEST first.
func rowsOf(t *testing.T, ri *RepoInstance, trigger string) []store.TriggerFire {
	t.Helper()
	rows := firesOf(t, ri, trigger)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

// waitRows waits until trigger has n rows in the TABLE and returns them.
func waitRows(t *testing.T, ri *RepoInstance, trigger string, n int) []store.TriggerFire {
	t.Helper()
	var rows []store.TriggerFire
	require.Eventually(t, func() bool {
		rows = rowsOf(t, ri, trigger)
		return len(rows) >= n
	}, 30*time.Second, 10*time.Millisecond, "trigger %s never reached %d row(s): %+v", trigger, n, rows)
	return rows
}

// ---- T1: unbound

// UnboundIsCountedNoOp: no recipe on main and none on this machine — the
// `do: run` fire is counted `unbound`, writes NO row and starts nothing; the
// bookmark still moves. Sabotage: write a row for unbound (red: a row).
func TestRun_UnboundIsCountedNoOp(t *testing.T) {
	ri, _ := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"))
	h := write(t, ri, "kb/tasks/in/a.md")
	require.Empty(t, firesOf(t, ri, "w"), "unbound writes no row")
	st := ri.triggers.stats.view("w")
	require.Equal(t, int64(1), st.Unbound)
	require.Equal(t, int64(1), st.Evaluations)
	require.Equal(t, int64(0), st.Fires, "an unbound fire started nothing")
	require.Equal(t, h, watermarks(t, ri)["w"], "the bookmark moves past it")
}

// ---- T2: argv verbatim, no shell

// LocalArgvVerbatimNoShell [F3]: a local recipe's exec argv reaches the child
// element for element — a space, shell metacharacters, a glob — and the
// child's parent is knomit itself (no intermediate shell). The fire is
// `started` with recipe_source=local and recipe_rev = sha256[:12] of the
// file; the result row is `done`. Sabotage: run through `sh -c` with the argv
// joined (red: argv split, ppid differs).
func TestRun_LocalArgvVerbatimNoShell(t *testing.T) {
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"))
	dir := t.TempDir()
	argv := []string{helperExe(t), "a b", "$(x);|&", "*", `"q"`}
	src := fmt.Sprintf(`var r = knomit.exec([%s, %s, %s, %s, %s], {env: %s});
({status: r.exit === 0 ? "done" : "error", message: "exit " + r.exit});`,
		jsString(argv[0]), jsString(argv[1]), jsString(argv[2]), jsString(argv[3]), jsString(argv[4]), helperEnv(dir))
	putLocalRecipe(t, home, "worker", src)
	write(t, ri, "kb/tasks/in/a.md")

	rep := waitHelperReports(t, dir, "child", 1)[0]
	require.Equal(t, argv, rep.Args, "argv arrives verbatim: never joined, split or globbed")
	require.Equal(t, os.Getpid(), rep.Ppid, "knomit starts the program itself: no shell in between")

	rows := waitRows(t, ri, "w", 2)
	require.Equal(t, store.TriggerOutcomeStarted, rows[0].Outcome)
	require.Equal(t, store.TriggerOutcomeDone, rows[1].Outcome, rows[1].Error)
	for _, r := range rows {
		require.Equal(t, store.RecipeSourceLocal, r.RecipeSource)
		require.Equal(t, localRev(src), r.RecipeRev)
	}
}

// ---- T3′, T4′, T5′: where the recipe comes from (ruling D1)

// MainWinsOverLocal: a recipe of the same name on main and on this machine —
// the MAIN one runs (recipe_source=repo, recipe_rev = its blob at main's tip).
// Sabotage: local first (red: the local one runs).
func TestRun_MainWinsOverLocal(t *testing.T) {
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"))
	putLocalRecipe(t, home, "worker", `({status: "done", message: "local"});`)
	blob := putMainRecipe(t, ri, "worker", `({status: "done", message: "main"});`)
	write(t, ri, "kb/tasks/in/a.md")
	rows := waitRows(t, ri, "w", 2)
	require.Equal(t, "main", rows[1].Error, "the recipe on main wins")
	require.Equal(t, store.RecipeSourceRepo, rows[0].RecipeSource)
	require.Equal(t, blob, rows[0].RecipeRev)
	require.Equal(t, blob, rows[1].RecipeRev)
}

// AgentBranchRecipeNeverRuns [N6 replaced]: a recipe committed on the AGENT
// branch but NOT on main is never used — the local one runs; without a local
// one the fire is unbound. And a repo with no main branch at all (a zero
// upstream tip) has no repo tier: its local recipe runs. Sabotage: read the
// recipe at the agent-branch head (red: the agent-branch recipe runs);
// treat a missing main as an error (red: recipe-error instead of local).
func TestRun_AgentBranchRecipeNeverRuns(t *testing.T) {
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"))
	_, err := testService(t, ri).Facts().WriteFact(context.Background(), trigAgent, fact.TriggerRecipePath("worker"),
		`({status: "done", message: "agent-branch"});`, "recipe on the agent branch", "updated")
	require.NoError(t, err)

	write(t, ri, "kb/tasks/in/unbound.md")
	require.Empty(t, firesOf(t, ri, "w"), "a recipe on the agent branch only is not a recipe: unbound")
	require.Equal(t, int64(1), ri.triggers.stats.view("w").Unbound)

	putLocalRecipe(t, home, "worker", `({status: "done", message: "local"});`)
	write(t, ri, "kb/tasks/in/local.md")
	rows := waitRows(t, ri, "w", 2)
	require.Equal(t, "local", rows[1].Error, "the agent branch never shadows the local recipe")
	require.Equal(t, store.RecipeSourceLocal, rows[1].RecipeSource)

	// No upstream branch: the repo tier is skipped, the local recipe runs.
	svc := testService(t, ri)
	require.NoError(t, svc.TestingDeleteRef("refs/heads/"+svc.UpstreamBranch()))
	tip, err := svc.Triggers().UpstreamTip(context.Background(), svc.UpstreamBranch())
	require.NoError(t, err)
	require.True(t, tip.IsZero(), "the fixture must really have no main")
	write(t, ri, "kb/tasks/in/no-main.md")
	rows = waitRows(t, ri, "w", 4)
	require.Equal(t, store.TriggerOutcomeStarted, rows[2].Outcome, rows[2].Error)
	require.Equal(t, "local", rows[3].Error)
}

// BrokenMainRecipeIsErrorNoFallback [D7]: a main recipe that does not compile
// is a `recipe-error` — the local recipe of the same name does NOT run — and
// the ERROR is logged once per blob, not per fire. Sabotage: fall back to the
// local recipe (red: a started row and the local result).
func TestRun_BrokenMainRecipeIsErrorNoFallback(t *testing.T) {
	logs := captureLogs(t, zerolog.ErrorLevel)
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"))
	putLocalRecipe(t, home, "worker", `({status: "done", message: "local"});`)
	blob := putMainRecipe(t, ri, "worker", `this is not ( javascript`)
	write(t, ri, "kb/tasks/in/a.md")
	write(t, ri, "kb/tasks/in/b.md")
	rows := rowsOf(t, ri, "w")
	require.Len(t, rows, 2, "one row per fire, and no result rows: nothing started")
	for _, r := range rows {
		require.Equal(t, store.TriggerOutcomeRecipeError, r.Outcome)
		require.Contains(t, r.Error, "does not compile")
		require.Equal(t, store.RecipeSourceRepo, r.RecipeSource)
		require.Equal(t, blob, r.RecipeRev)
		require.Empty(t, r.RunID, "nothing started, no run id")
	}
	require.Equal(t, 1, countLines(logs, "invalid recipe skipped"), "logged once per blob")
}

// ---- T6: the sandbox has no exec

// NoExecInSandbox [F2]: a `do: script` fire sees no exec anywhere — not on
// knomit, not as a global — while a recipe (the positive control) does.
// Sabotage: add exec to scriptHost.functions() (red).
func TestScript_NoExecInSandbox(t *testing.T) {
	_, ri, _ := newScriptRepo(t, 0, scriptTrig("s", "learn", "tasks/in/**", "probe"),
		runTrig("r", "learn", "tasks/rec/**", "probe"))
	putScript(t, ri, "probe", `knomit.emit({t: [typeof knomit.exec, typeof exec, typeof globalThis.exec]});`)
	putLocalRecipe(t, ri.triggers.home, "probe", `({status: "done", message: [typeof knomit.exec, typeof exec].join(",")});`)
	sink := subscribePayloads(t, ri, "s")
	write(t, ri, "kb/tasks/in/a.md")
	require.Equal(t, []any{"undefined", "undefined", "undefined"}, sink.wait(t, 1)[0]["t"])
	write(t, ri, "kb/tasks/rec/a.md")
	rows := waitRows(t, ri, "r", 2)
	require.Equal(t, "function,undefined", rows[1].Error, "a recipe has knomit.exec (and still no global exec)")
}

// OSExecOnlyInExecFile [F2 (a)]: os/exec is imported by no non-test file of
// internal/repos except the exec file and its OS twins. Sabotage: import
// os/exec in trigger_script.go (red).
func TestRecipe_OSExecOnlyInExecFile(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var importers []string
	walked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		walked++
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		require.NoError(t, err, f)
		for _, imp := range af.Imports {
			if imp.Path.Value == `"os/exec"` {
				importers = append(importers, f)
			}
		}
	}
	require.Greater(t, walked, 20, "the guard must actually have walked the package")
	sort.Strings(importers)
	require.Equal(t, []string{"trigger_recipe_exec.go", "trigger_recipe_exec_unix.go", "trigger_recipe_exec_windows.go"}, importers)
}

// ---- T7, T7b: process groups

// TimeoutKillsProcessGroup [H5] [M3]: the child forks a grandchild that holds
// the stdout pipe — ONLY after the go-signal (the exec hook, called once the
// child is in its kill group) [R2-1] — and sleeps; exec's timeout_ms ends it:
// exec throws "timed out" well within timeout + recipeWaitDelay, and the
// grandchild is dead. Runs on every OS (windows-2025 included). Sabotage:
// drop the group kill (Setpgid/Cancel, or the job object) — the grandchild
// survives (red).
func TestRun_TimeoutKillsProcessGroup(t *testing.T) {
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"))
	dir := t.TempDir()
	installGoSignal(t, dir)
	putLocalRecipe(t, home, "worker", fmt.Sprintf(`try {
  knomit.exec([%s], {env: %s, timeout_ms: 3000});
  ({status: "done", message: "not killed"});
} catch (e) { ({status: "unreachable", message: e.message}); }`,
		jsString(helperExe(t)), helperEnv(dir, "HELPER_WAIT_GO", "1", "HELPER_FORK", "hold", "HELPER_SLEEP_MS", "60000")))
	write(t, ri, "kb/tasks/in/a.md")

	gc := waitHelperReports(t, dir, "grandchild", 1)[0]
	t.Cleanup(func() { killProcess(gc.Pid) })
	rows := waitRows(t, ri, "w", 2)
	require.Equal(t, store.TriggerOutcomeUnreachable, rows[1].Outcome)
	require.Contains(t, rows[1].Error, "timed out after 3000 ms")
	require.Less(t, rows[1].DurationMS, int64(3000+recipeWaitDelay.Milliseconds()+5000),
		"exec returned within its timeout, the wait delay and a margin: the pipes did not keep it alive")
	require.Eventually(t, func() bool { return !processAlive(gc.Pid) }, 10*time.Second, 20*time.Millisecond,
		"the grandchild (pid %d) survived the timeout: the process group / job was not killed", gc.Pid)
}

// CleanExitLeavesDetachedGrandchild [M3]: a child that starts a grandchild
// with its stdio redirected away (after the go-signal, so it is inside the
// kill group) and exits 0 — exec returns exit 0, and the grandchild is STILL
// alive afterwards on every OS (status `spawned` works the same everywhere).
// Sabotage: leave kill-on-close set when closing the job after a clean exit
// (Windows) or signal the group after a clean exit (Unix) — red.
func TestRun_CleanExitLeavesDetachedGrandchild(t *testing.T) {
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"))
	dir := t.TempDir()
	installGoSignal(t, dir)
	putLocalRecipe(t, home, "worker", fmt.Sprintf(`var r = knomit.exec([%s], {env: %s});
({status: r.exit === 0 ? "spawned" : "error", message: "exit " + r.exit});`,
		jsString(helperExe(t)), helperEnv(dir, "HELPER_WAIT_GO", "1", "HELPER_FORK", "detach")))
	write(t, ri, "kb/tasks/in/a.md")

	rows := waitRows(t, ri, "w", 2)
	require.Equal(t, store.TriggerOutcomeSpawned, rows[1].Outcome, rows[1].Error)
	gcs := helperReports(t, dir, "grandchild")
	require.Len(t, gcs, 1, "the child must really have started a grandchild")
	gc := gcs[0]
	t.Cleanup(func() { killProcess(gc.Pid) })
	for i := 0; i < 10; i++ { // alive now, and still alive a second later
		require.True(t, processAlive(gc.Pid), "the detached grandchild (pid %d) was killed after a clean exit", gc.Pid)
		time.Sleep(100 * time.Millisecond)
	}
}

// ---- T8: concurrency

// ConcurrencyCap [M4] [D6]: header `concurrent: 1`; a second fire while the
// first recipe's program blocks is a `busy` row that starts nothing (one
// helper alive); once the first ends, a third fire starts again. Sabotage: no
// slot (red: two helpers); a blocking wait for the slot (red: no busy row).
func TestRun_ConcurrencyCap(t *testing.T) {
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"))
	dir := t.TempDir()
	block := filepath.Join(t.TempDir(), "release")
	putLocalRecipe(t, home, "worker", fmt.Sprintf(`// knomit: {"concurrent": 1}
var r = knomit.exec([%s], {env: %s});
({status: "done", message: "exit " + r.exit});`, jsString(helperExe(t)), helperEnv(dir, "HELPER_BLOCK_FILE", block)))
	write(t, ri, "kb/tasks/in/a.md")
	waitHelperReports(t, dir, "child", 1)
	write(t, ri, "kb/tasks/in/b.md")
	rows := rowsOf(t, ri, "w")
	require.Equal(t, []string{store.TriggerOutcomeStarted, store.TriggerOutcomeBusy}, outcomesOf(rows))
	require.Empty(t, rows[1].RunID, "a busy fire started nothing and has no run id")
	time.Sleep(200 * time.Millisecond)
	require.Len(t, helperReports(t, dir, "child"), 1, "the busy fire started no process")

	require.NoError(t, os.WriteFile(block, nil, 0o600))
	waitRows(t, ri, "w", 3) // the first recipe's result
	write(t, ri, "kb/tasks/in/c.md")
	waitHelperReports(t, dir, "child", 2)
	rows = waitRows(t, ri, "w", 5)
	require.Equal(t, store.TriggerOutcomeStarted, rows[3].Outcome, "the slot is free again")
	require.Equal(t, int64(1), ri.triggers.stats.view("w").Busy)
}

// ---- T9: the dispatcher never waits

// DispatcherNotBlockedByLongProcess [F4]: a recipe whose program runs for
// many seconds; a later write matching an emit trigger fires (its row and its
// run complete) within a second. Sabotage: run the recipe inline in phase B
// (red: the emit waits for the program).
func TestRun_DispatcherNotBlockedByLongProcess(t *testing.T) {
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"), trig("e", "learn", "tasks/other/**", ""))
	dir := t.TempDir()
	block := filepath.Join(t.TempDir(), "release")
	t.Cleanup(func() { _ = os.WriteFile(block, nil, 0o600) })
	putLocalRecipe(t, home, "worker", fmt.Sprintf(`var r = knomit.exec([%s], {env: %s});
({status: "done"});`, jsString(helperExe(t)), helperEnv(dir, "HELPER_BLOCK_FILE", block)))
	writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
	waitHelperReports(t, dir, "child", 1)
	t0 := time.Now()
	h := writeOn(t, ri, trigAgent, "kb/tasks/other/b.md")
	waitTriggerHeadFor(t, ri, h, 10*time.Second)
	require.Less(t, time.Since(t0), 3*time.Second, "the dispatcher went on while the recipe's program ran")
	require.Len(t, firesOf(t, ri, "e"), 1)
	require.Len(t, rowsOf(t, ri, "w"), 1, "the recipe is still running: only its started row")
}

// ---- T10, T21: knomit.run, the run id

// ScriptGetsStartedAndResultIsLogged [D5] [M1]: knomit.run("worker", {k: 1})
// answers at once {ok: true, status: "started", id, source: "local"}; the
// recipe saw payload.k === 1 and task_path == the firing path; its result
// (`delivered`) reaches the trigger_fires TABLE with the same run id and NO
// further commit to the repo after the fire. Sabotage: return the stub (red);
// leave `late` out of pendingFlush.empty() (red: the row never reaches the
// table).
func TestRun_ScriptGetsStartedAndResultIsLogged(t *testing.T) {
	_, ri, _ := newScriptRepo(t, 0, scriptTrig("s", "learn", "tasks/in/**", "caller"))
	dir := t.TempDir()
	block := filepath.Join(t.TempDir(), "release")
	t.Cleanup(func() { _ = os.WriteFile(block, nil, 0o600) })
	// The recipe's program blocks until the fire's own phase C is FLUSHED, so
	// its result row cannot ride that flush: it must be flushed on its own.
	putLocalRecipe(t, ri.triggers.home, "worker", fmt.Sprintf(`knomit.exec([%s], {env: %s});
({status: "delivered", message: "k=" + payload.k + " path=" + task_path});`, jsString(helperExe(t)), helperEnv(dir, "HELPER_BLOCK_FILE", block)))
	putScript(t, ri, "caller", `knomit.emit(knomit.run("worker", {k: 1}));`)
	sink := subscribePayloads(t, ri, "s")
	fired := writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")

	got := sink.wait(t, 1)[0]
	id, _ := got["id"].(string)
	require.Equal(t, map[string]any{"ok": true, "status": "started", "id": id, "source": "local"}, got)
	require.Regexp(t, `^run-[0-9a-f]{32}$`, id)

	waitHelperReports(t, dir, "child", 1)
	waitTriggerHead(t, ri, fired) // the fire is fully flushed; the buffer is empty
	require.Len(t, rowsOf(t, ri, "s"), 2, "ran + started are in the table; the recipe still runs")
	require.NoError(t, os.WriteFile(block, nil, 0o600))
	rows := waitRows(t, ri, "s", 3)
	require.Equal(t, []string{store.TriggerOutcomeRan, store.TriggerOutcomeStarted, store.TriggerOutcomeDelivered}, outcomesOf(rows))
	require.Equal(t, id, rows[1].RunID)
	require.Equal(t, id, rows[2].RunID, "the result row carries the same run id")
	require.Equal(t, "k=1 path=kb/tasks/in/a.md", rows[2].Error)
	require.Equal(t, fired, head(t, ri), "no commit was needed to flush the result row")
}

// RunIDCorrelates [D5]: two triggers in the same story (same task, same
// trace) each knomit.run the recipe: two DIFFERENT ids of the form
// run-<32 hex>; per id exactly two rows (started, then the result), both with
// that id and the story's trace; the recipe saw run.id == the id, and the
// child's env KNOMIT_RUN == the id and KNOMIT_TRACE == the trace. A `do: run`
// fire's two rows share one id as well. Sabotage: mint a second id for the
// result row (red: one row per id); use the trace as the id (red: the ids
// collide); drop run_id from the insert (red: the lookup finds nothing).
func TestRun_RunIDCorrelates(t *testing.T) {
	_, ri, _ := newScriptRepo(t, 0,
		scriptTrig("ta", "learn", "tasks/in/**", "caller"), scriptTrig("tb", "learn", "tasks/in/**", "caller"),
		runTrig("direct", "learn", "tasks/in/**", "worker"))
	dir := t.TempDir()
	putLocalRecipe(t, ri.triggers.home, "worker", fmt.Sprintf(`// knomit: {"concurrent": 3}
var r = knomit.exec([%s, run.id], {env: %s});
({status: "done", message: run.id});`, jsString(helperExe(t)), helperEnv(dir)))
	putScript(t, ri, "caller", `knomit.run("worker");`)
	writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")

	reports := waitHelperReports(t, dir, "child", 3)
	byID := map[string]helperReport{}
	for _, r := range reports {
		byID[r.Args[1]] = r
	}
	var ids []string
	for _, name := range []string{"ta", "tb", "direct"} {
		rows := waitRows(t, ri, name, 2)
		start := rows[0]
		if name != "direct" {
			start = rows[1] // after the script's own `ran` row
			rows = waitRows(t, ri, name, 3)
		}
		require.Equal(t, store.TriggerOutcomeStarted, start.Outcome, name)
		id := start.RunID
		require.Regexp(t, `^run-[0-9a-f]{32}$`, id)
		ids = append(ids, id)
		byRun, err := ri.TriggerFiresByRun(context.Background(), id)
		require.NoError(t, err)
		require.Len(t, byRun, 2, "%s: exactly the started row and the result row", name)
		require.Equal(t, []string{store.TriggerOutcomeStarted, store.TriggerOutcomeDone}, outcomesOf(byRun))
		for _, r := range byRun {
			require.Equal(t, id, r.RunID)
			require.Equal(t, start.Trace, r.Trace, "both rows carry the story's trace")
		}
		require.Equal(t, id, byRun[1].Error, "the recipe saw run.id")
		rep, ok := byID[id]
		require.True(t, ok, "a helper was started with this run id")
		v, _ := envOf(rep.Env, "KNOMIT_RUN")
		require.Equal(t, id, v)
		v, _ = envOf(rep.Env, "KNOMIT_TRACE")
		require.Equal(t, start.Trace, v)
	}
	require.Len(t, ids, 3)
	require.NotEqual(t, ids[0], ids[1], "one story, two runs: two ids")
	require.NotEqual(t, ids[0], ids[2])
	require.NotEqual(t, ids[1], ids[2])
}

// ---- T11, T20: stop, restart, crash

// StopKillsAndRecords [F5] [D4]: stop() while a recipe's program runs returns
// within the wait delay and a margin, the program is dead, and a
// `recipe-error` "stopped" row is in the TABLE; after a restart nothing
// re-runs (the helper count stays 1). Sabotage: no kill at stop (red: stop
// hangs / the helper lives); hold the bookmark until the result (red: a
// re-run after restart).
func TestRun_StopKillsAndRecords(t *testing.T) {
	home := t.TempDir()
	deps := Deps{Cfg: config.Config{Home: home, OntologyRoot: "kb"}, AgentBranch: trigAgent,
		KeyPath: filepath.Join(home, "agent.key"), DisableBackgroundSync: true, ScriptTools: &stubTools{}}
	m := New(context.Background(), deps)
	ri := bootRepo(t, m)
	setOntology(t, ri, triggerOntology("", runTrig("w", "learn", "tasks/in/**", "worker")))
	dir := t.TempDir()
	putLocalRecipe(t, home, "worker", fmt.Sprintf(`var r = knomit.exec([%s], {env: %s});
({status: "done"});`, jsString(helperExe(t)), helperEnv(dir, "HELPER_SLEEP_MS", "60000")))
	write(t, ri, "kb/tasks/in/a.md")
	child := waitHelperReports(t, dir, "child", 1)[0]
	t.Cleanup(func() { killProcess(child.Pid) })

	t0 := time.Now()
	ri.triggers.stop()
	require.Less(t, time.Since(t0), recipeWaitDelay+5*time.Second, "stop() waits for the killed recipe, not for its program")
	require.Eventually(t, func() bool { return !processAlive(child.Pid) }, 10*time.Second, 20*time.Millisecond, "the program survived stop()")
	rows := rowsOf(t, ri, "w")
	require.Equal(t, []string{store.TriggerOutcomeStarted, store.TriggerOutcomeRecipeError}, outcomesOf(rows), "the stopped row is in the TABLE after stop()")
	require.Contains(t, rows[1].Error, "stopped")
	require.Equal(t, rows[0].RunID, rows[1].RunID)
	require.NoError(t, m.Close())

	m2 := New(context.Background(), deps)
	require.NoError(t, m2.Start())
	t.Cleanup(func() { _ = m2.Close() })
	ri2 := m2.Get(testRepoName)
	require.NotNil(t, ri2)
	waitTriggerHead(t, ri2, head(t, ri2))
	time.Sleep(300 * time.Millisecond)
	require.Len(t, helperReports(t, dir, "child"), 1, "a stopped recipe is never re-run (at-most-once after the flush)")
}

// CrashBeforeFlushReRunsOnce [M2]: pins the NAMED window, it does not close
// it. With phase C held pending (flushGrace 1 h) and the loop torn down
// WITHOUT its final flush (a crash, as the tables see it), a restart re-fires
// the range and starts the recipe a SECOND time; with the flush done first,
// the count stays 1. Sabotage: store the bookmark at `started` (flush at
// once; red: count 1 in the crash case — the stated window would be wrong).
func TestRun_CrashBeforeFlushReRunsOnce(t *testing.T) {
	for _, crash := range []bool{true, false} {
		t.Run(fmt.Sprintf("crash=%v", crash), func(t *testing.T) {
			home := t.TempDir()
			deps := Deps{Cfg: config.Config{Home: home, OntologyRoot: "kb"}, AgentBranch: trigAgent,
				KeyPath: filepath.Join(home, "agent.key"), DisableBackgroundSync: true, ScriptTools: &stubTools{}}
			m := New(context.Background(), deps)
			ri := bootRepo(t, m)
			setOntology(t, ri, triggerOntology("", runTrig("w", "learn", "tasks/in/**", "worker")))
			dir := t.TempDir()
			putLocalRecipe(t, home, "worker", fmt.Sprintf(`knomit.exec([%s], {env: %s}); ({status: "done"});`,
				jsString(helperExe(t)), helperEnv(dir)))
			if crash {
				setHooks(t, triggerHooks{flushGrace: time.Hour, skipFlushOnStop: true})
			}
			h := writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
			waitHelperReports(t, dir, "child", 1)
			if !crash {
				waitTriggerHead(t, ri, h)
				waitRows(t, ri, "w", 2)
			} else {
				require.Eventually(t, func() bool { got, _ := ri.triggers.runSequence(); return got == h }, 20*time.Second, 10*time.Millisecond)
				require.NotEqual(t, h, watermarks(t, ri)["w"], "the fire's phase C is still pending (not flushed)")
			}
			require.NoError(t, m.Close())
			setHooks(t, triggerHooks{})

			m2 := New(context.Background(), deps)
			require.NoError(t, m2.Start())
			t.Cleanup(func() { _ = m2.Close() })
			ri2 := m2.Get(testRepoName)
			require.NotNil(t, ri2)
			waitTriggerHead(t, ri2, h)
			want := 1
			if crash {
				want = 2
				waitHelperReports(t, dir, "child", 2)
			}
			time.Sleep(300 * time.Millisecond)
			require.Len(t, helperReports(t, dir, "child"), want)
		})
	}
}

// ---- T12: the sample recipe

// SampleRecipe_ArgvHasPathNotBody [F3]: the shipped
// examples/recipes/claude-session.js, with a fake `claude` (the helper) on
// PATH: argv is exactly [claude, --model, sonnet, --mcp-config, <this repo's
// MCP config>, --allowedTools, mcp__knomit-repo-<repo>, -p, <prompt>]; the
// prompt names task_path; a sentinel in the task BODY is absent from argv,
// stdin and env. Sabotage: interpolate fact.body into the prompt (red).
func TestSampleRecipe_ArgvHasPathNotBody(t *testing.T) {
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "claude-session"))
	src, err := os.ReadFile(filepath.Join("..", "..", "examples", "recipes", "claude-session.js"))
	require.NoError(t, err)
	putLocalRecipe(t, home, "claude-session", string(src))

	bin := t.TempDir()
	fake := filepath.Join(bin, "claude")
	if runtime.GOOS == "windows" {
		fake += ".exe"
	}
	// A COPY, never a hard link: on Windows a link to the running test binary
	// shares its lock, and TempDir's cleanup cannot delete it.
	data, err := os.ReadFile(helperExe(t))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(fake, data, 0o755))
	dir := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(recipeHelperEnv, dir)

	const sentinel = "SENTINEL-BODY-7f3a"
	path := "kb/tasks/in/task.md"
	_, err = testService(t, ri).Facts().WriteFact(context.Background(), trigAgent, path,
		"---\ntype: observation\nconfidence: 0.8\nsources: 1\n---\n# Task\n\n"+sentinel+"\n", "learn: task", "learn")
	require.NoError(t, err)

	rep := waitHelperReports(t, dir, "child", 1)[0]
	mcp := recipeMCP(testRepoName)
	key := serverkey.ServerKey(testRepoName, "")
	require.Len(t, rep.Args, 9, "%q", rep.Args)
	require.Equal(t, []string{"claude", "--model", "sonnet", "--mcp-config", mcp["config"].(string), "--allowedTools", "mcp__" + key, "-p"}, rep.Args[:8])
	require.Contains(t, rep.Args[8], path, "the prompt names the task by path")
	require.JSONEq(t, `{"mcpServers":{"`+key+`":{"command":"kb","args":["--repo","`+testRepoName+`"]}}}`, rep.Args[4],
		"the MCP config is the one kb claude init writes")
	for _, a := range rep.Args {
		require.NotContains(t, a, sentinel, "the task body never enters argv")
	}
	require.NotContains(t, rep.Stdin, sentinel)
	for _, kv := range rep.Env {
		require.NotContains(t, kv, sentinel, "the task body never enters the environment")
	}
	rows := waitRows(t, ri, "w", 2)
	require.Equal(t, store.TriggerOutcomeDone, rows[1].Outcome, rows[1].Error)

	// #349: the prompt hands the session its trace as a JSON literal — the
	// fire's trace (a plain id here: the firing commit), the firing commit and
	// the run id — to pass on every write.
	require.Equal(t, map[string]string{"Knomit-Trace": rows[0].Trace, "Knomit-Cause": rows[0].Trace, "Knomit-Run": rows[0].RunID},
		promptTrace(t, rep.Args[8]))

	// A trace that is not a plain id (a toucher's Knomit-Trace is copied
	// forward verbatim, and a task's entity is author text) stays OUT of the
	// prompt; the cause and the run still tie the session's writes to the fire.
	weird := writeMsg(t, ri, trigAgent, "kb/tasks/in/other.md", "learn: other\n\nKnomit-Trace: a story <with> spaces\n")
	// Match the second fire by its path and the child by its run id: neither
	// the report files nor the rows are guaranteed to come back in start order.
	reports := waitHelperReports(t, dir, "child", 2)
	waitRows(t, ri, "w", 4)
	var second store.TriggerFire
	for _, r := range rowsOf(t, ri, "w") {
		if r.Path == "kb/tasks/in/other.md" && r.Outcome == store.TriggerOutcomeStarted {
			second = r
		}
	}
	require.NotEmpty(t, second.RunID, "fixture: the second fire started")
	require.Equal(t, "a story <with> spaces", second.Trace, "fixture: the fire's trace is the copied-forward value")
	var prompt2 string
	for _, r := range reports {
		if strings.Contains(r.Args[8], second.RunID) {
			prompt2 = r.Args[8]
		}
	}
	require.NotEmpty(t, prompt2, "no child was started with run %s", second.RunID)
	require.Equal(t, map[string]string{"Knomit-Cause": weird, "Knomit-Run": second.RunID}, promptTrace(t, prompt2))
	require.NotContains(t, prompt2, "a story")
}

// promptTrace extracts the JSON trace object a sample recipe put in its
// prompt (the last {...} in it).
func promptTrace(t *testing.T, prompt string) map[string]string {
	t.Helper()
	i, j := strings.LastIndex(prompt, "{"), strings.LastIndex(prompt, "}")
	require.True(t, i >= 0 && j > i, "no trace object in the prompt: %q", prompt)
	var out map[string]string
	require.NoError(t, json.Unmarshal([]byte(prompt[i:j+1]), &out), prompt)
	return out
}

// ---- T13: the recipe name

// RecipeNameValidated [H7]: knomit.run("../x") throws (the fire is a
// script-error) and starts nothing, even with a file that name would reach.
// (The ontology half — `recipe: ../x` is invalid — is in internal/fact.)
// Sabotage: remove the check (red: the fire is `ran`).
func TestRun_RecipeNameValidatedAtCall(t *testing.T) {
	_, ri, _ := newScriptRepo(t, 0, scriptTrig("s", "learn", "tasks/in/**", "caller"))
	putLocalRecipe(t, ri.triggers.home, "x", `({status: "done"});`)
	require.NoError(t, os.WriteFile(filepath.Join(ri.triggers.home, "x.js"), []byte(`({status: "done"});`), 0o600))
	putScript(t, ri, "caller", `knomit.run("../x");`)
	write(t, ri, "kb/tasks/in/a.md")
	rows := rowsOf(t, ri, "s")
	require.Equal(t, []string{store.TriggerOutcomeScriptError}, outcomesOf(rows))
	require.Contains(t, rows[0].Error, "kebab-case recipe name")
}

// ---- T14: nowhere without a dispatcher

// NotOnReadOnlyOrSubscribed: a read-only server and a subscription build no
// dispatcher, so nothing can resolve or start a recipe there, even with a
// local recipe present. Sabotage: build the dispatcher unconditionally (red).
func TestRun_NotOnReadOnlyOrSubscribed(t *testing.T) {
	home := t.TempDir()
	putLocalRecipe(t, home, "worker", `({status: "done"});`)
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb", ReadOnly: true},
		AgentBranch: trigAgent, KeyPath: filepath.Join(home, "agent.key"), DisableBackgroundSync: true,
	})
	t.Cleanup(func() { _ = m.Close() })
	ri := bootRepo(t, m)
	require.Nil(t, ri.triggers, "a read-only server runs no dispatcher and so no recipe")
	rows, err := ri.TriggerFiresByRun(context.Background(), "run-00000000000000000000000000000000")
	require.NoError(t, err)
	require.Empty(t, rows)

	_, sub, _ := buildSubscription(t)
	require.Nil(t, sub.triggers, "a subscription runs no dispatcher and so no recipe")
}

// ---- T15: reload

// ReloadLocalAndMain: a changed local file (same size, new mtime) runs the new
// code on the next fire; a new blob on main does too, with the new blob as
// recipe_rev. Sabotage: cache by name only (red: v1 again).
func TestRun_ReloadLocalAndMain(t *testing.T) {
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"))
	p := putLocalRecipe(t, home, "worker", `({status: "done", message: "v1"});`)
	write(t, ri, "kb/tasks/in/a.md")
	require.Equal(t, "v1", waitRows(t, ri, "w", 2)[1].Error)

	require.NoError(t, os.WriteFile(p, []byte(`({status: "done", message: "v2"});`), 0o600))
	later := time.Now().Add(5 * time.Second)
	require.NoError(t, os.Chtimes(p, later, later))
	write(t, ri, "kb/tasks/in/b.md")
	require.Equal(t, "v2", waitRows(t, ri, "w", 4)[3].Error, "the edited local recipe runs")

	b1 := putMainRecipe(t, ri, "worker", `({status: "done", message: "m1"});`)
	write(t, ri, "kb/tasks/in/c.md")
	rows := waitRows(t, ri, "w", 6)
	require.Equal(t, "m1", rows[5].Error)
	require.Equal(t, b1, rows[5].RecipeRev)
	b2 := putMainRecipe(t, ri, "worker", `({status: "done", message: "m2"});`)
	require.NotEqual(t, b1, b2)
	write(t, ri, "kb/tasks/in/d.md")
	rows = waitRows(t, ri, "w", 8)
	require.Equal(t, "m2", rows[7].Error, "a new blob on main recompiles")
	require.Equal(t, b2, rows[7].RecipeRev)
}

// ---- T17: the environment

// EnvMergedWithTrace [N1]: the child sees knomit's own environment (PATH
// inherited), KNOMIT_HOME (this server's home), KNOMIT_TRACE / KNOMIT_CAUSE
// (the fire's trace and firing commit), KNOMIT_RUN, and the recipe's env
// overriding an inherited variable. Sabotage: replace the environment with
// the recipe's (red: PATH missing).
func TestRun_EnvMergedWithTrace(t *testing.T) {
	t.Setenv("KNOMIT_T17_BASE", "inherited")
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"))
	dir := t.TempDir()
	putLocalRecipe(t, home, "worker", fmt.Sprintf(`knomit.exec([%s], {env: %s}); ({status: "done"});`,
		jsString(helperExe(t)), helperEnv(dir, "KNOMIT_T17_BASE", "override")))
	fired := write(t, ri, "kb/tasks/in/a.md")
	rep := waitHelperReports(t, dir, "child", 1)[0]
	rows := waitRows(t, ri, "w", 2)
	get := func(k string) string {
		v, ok := envOf(rep.Env, k)
		require.True(t, ok, "the child has no %s", k)
		return v
	}
	require.Equal(t, os.Getenv("PATH"), get("PATH"), "the environment is merged, never replaced")
	require.Equal(t, home, get("KNOMIT_HOME"))
	require.Equal(t, rows[0].Trace, get("KNOMIT_TRACE"))
	require.Equal(t, fired, get("KNOMIT_CAUSE"))
	require.Equal(t, rows[0].RunID, get("KNOMIT_RUN"))
	require.Equal(t, "override", get("KNOMIT_T17_BASE"), "the recipe's env wins over the inherited value")
}

// runEnvChild fires one recipe that execs the helper with the recipe env
// extra (key/value pairs), on a repo whose Manager knows addr as its own
// address ("" leaves it unset), and returns the child's environment.
func runEnvChild(t *testing.T, addr string, extra ...string) []string {
	t.Helper()
	m, ri, _ := newScriptRepo(t, 0, runTrig("w", "learn", "tasks/in/**", "worker"))
	if addr != "" {
		m.SetServerAddress(addr)
	}
	dir := t.TempDir()
	putLocalRecipe(t, ri.triggers.home, "worker", fmt.Sprintf(`knomit.exec([%s], {env: %s}); ({status: "done"});`,
		jsString(helperExe(t)), helperEnv(dir, extra...)))
	write(t, ri, "kb/tasks/in/a.md")
	_ = waitRows(t, ri, "w", 2)
	return waitHelperReports(t, dir, "child", 1)[0].Env
}

// The recipe child's KNOMIT_SERVER is the server's OWN address, exactly as the
// listener-binding code recorded it — that is what makes the `kb` a recipe
// starts reach THIS server rather than whichever one the desktop lockfile or
// the default names. An inherited value is overwritten. Sabotage: drop the
// KNOMIT_SERVER line from recipeEnv (red: the inherited value survives).
func TestRun_EnvCarriesTheServersOwnAddress(t *testing.T) {
	t.Setenv("KNOMIT_SERVER", "http://inherited.invalid:1")
	const own = "unix:///srv/knomit/knomit.sock"
	got, ok := envOf(runEnvChild(t, own), "KNOMIT_SERVER")
	require.True(t, ok, "the child has no KNOMIT_SERVER")
	require.Equal(t, own, got)
}

// A recipe's `exec` env names a different server and wins. Sabotage: merge
// the recipe env UNDER the base (red: the server's own address).
func TestRun_EnvRecipeOverridesKnomitServer(t *testing.T) {
	env := runEnvChild(t, "unix:///srv/knomit/knomit.sock", "KNOMIT_SERVER", "http://127.0.0.1:19310")
	got, ok := envOf(env, "KNOMIT_SERVER")
	require.True(t, ok)
	require.Equal(t, "http://127.0.0.1:19310", got, "the recipe's env wins")
}

// Before the server knows its address the child gets NO KNOMIT_SERVER: an
// inherited one names whatever server knomit's own launcher pointed at, and
// passing it on would send the recipe's `kb` there. Sabotage: skip
// withoutEnv (red: the inherited value reaches the child).
func TestRun_EnvNoAddressDropsAnInheritedKnomitServer(t *testing.T) {
	t.Setenv("KNOMIT_SERVER", "http://inherited.invalid:1")
	_, ok := envOf(runEnvChild(t, ""), "KNOMIT_SERVER")
	require.False(t, ok, "an inherited KNOMIT_SERVER reached the child although this server has no address yet")
}

// ---- T18: a recipe's writes

// RecipeWritesStampedAndGuarded: knomit.learn from a recipe commits with the
// fire's trailers (trace, cause, trigger) plus Knomit-Run = the `started`
// row's run id (#349, T11), and does not re-fire its own trigger on that path
// (`self-caused`). Sabotage: build the recipe host without WithTrailers (red:
// no trailers, a second start); drop `Run: j.id` (red: no Knomit-Run).
func TestRun_RecipeWritesStampedAndGuarded(t *testing.T) {
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"))
	putLocalRecipe(t, home, "worker", `knomit.learn({topic: "tasks", category: "in", title: "From recipe"}); ({status: "done"});`)
	fired := writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
	rows := waitRows(t, ri, "w", 2)
	require.Equal(t, store.TriggerOutcomeDone, rows[1].Outcome, rows[1].Error)
	settle(t, ri)
	commits := commitsAfter(t, ri, fired)
	require.Len(t, commits, 1, "the recipe made one commit")
	tr := trailersOf(t, ri, commits[0])
	require.Regexp(t, `^run-[0-9a-f]{32}$`, rows[0].RunID)
	require.Equal(t, store.Trailers{Trace: rows[0].Trace, Cause: fired, Trigger: "w", Run: rows[0].RunID}, tr)
	require.True(t, strings.HasSuffix(commitMessage(t, ri, commits[0]), "\nKnomit-Trigger: w\nKnomit-Run: "+rows[0].RunID+"\n"),
		"Knomit-Run is the last line, after Knomit-Trigger")
	require.Equal(t, int64(1), ri.triggers.stats.view("w").SelfCaused, "its own write does not re-fire it")
	require.Len(t, rowsOf(t, ri, "w"), 2, "no second start")
}

// RecipeLearnTraceRefused [#349 T6, N1, recipe tier]: a recipe's
// knomit.learn with `trace` in its options throws (the recipe host shares the
// script host's learn) and nothing is committed; the recipe sees the reason.
// Sabotage: remove the host's opts.trace check (red: a commit lands, the
// message is empty).
func TestRun_RecipeLearnTraceRefused(t *testing.T) {
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"))
	putLocalRecipe(t, home, "worker", `var m = "";
try { knomit.learn({topic: "tasks", category: "out", title: "From recipe"}, {trace: {"Knomit-Trace": "mine"}}); } catch (e) { m = e.message; }
({status: "done", message: m});`)
	fired := writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
	rows := waitRows(t, ri, "w", 2)
	require.Equal(t, store.TriggerOutcomeDone, rows[1].Outcome)
	require.Contains(t, rows[1].Error, "opts.trace is refused")
	settle(t, ri)
	require.Empty(t, commitsAfter(t, ri, fired), "nothing was committed")
}

// ---- T19: output cap

// OutputCap: a program writing 3 MiB to stdout — exec returns 1 MiB of it,
// truncated: true, and the program exits (it never blocked on the pipe).
// Sabotage: stop reading at the cap (red: the program blocks until the
// timeout).
func TestRun_OutputCap(t *testing.T) {
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"))
	dir := t.TempDir()
	putLocalRecipe(t, home, "worker", fmt.Sprintf(`try {
  var r = knomit.exec([%s], {env: %s, timeout_ms: 20000});
  ({status: "done", message: r.stdout.length + ":" + r.truncated + ":" + r.exit});
} catch (e) { ({status: "unreachable", message: e.message}); }`,
		jsString(helperExe(t)), helperEnv(dir, "HELPER_WRITE_BYTES", strconv.Itoa(3<<20))))
	write(t, ri, "kb/tasks/in/a.md")
	rows := waitRows(t, ri, "w", 2)
	require.Equal(t, store.TriggerOutcomeDone, rows[1].Outcome, rows[1].Error)
	require.Equal(t, fmt.Sprintf("%d:true:0", recipeOutputCap), rows[1].Error)
}

// ---- Result shapes

// ResultShapes: a recipe's completion value decides its result row — a known
// status is that outcome (`error` is recipe-error with the message), a
// recipe's own id is kept as id=<value>, anything else is an invalid result,
// a throw is recipe-error, and running past the header's timeout_ms is
// recipe-timeout. Sabotage: accept any status (red).
func TestRun_ResultShapes(t *testing.T) {
	cases := []struct{ src, outcome, msg string }{
		{`({status: "unreachable", message: "no session", id: "s-1"});`, store.TriggerOutcomeUnreachable, "no session id=s-1"},
		{`({status: "error", message: "boom"});`, store.TriggerOutcomeRecipeError, "boom"},
		{`({status: "maybe"});`, store.TriggerOutcomeRecipeError, "invalid result"},
		{`42;`, store.TriggerOutcomeRecipeError, "invalid result"},
		{`throw new Error("thrown");`, store.TriggerOutcomeRecipeError, "thrown"},
		{"// knomit: {\"timeout_ms\": 50}\nwhile (true) {}", store.TriggerOutcomeRecipeTimeout, "exceeded"},
		{"// knomit: {\"bogus\": 1}\n({status: \"done\"});", store.TriggerOutcomeRecipeError, "does not compile"},
	}
	ri, home := newRunRepo(t, runTrig("w", "learn", "tasks/in/**", "worker"))
	n := 0
	for i, c := range cases {
		putLocalRecipe(t, home, "worker", c.src)
		p := filepath.Join(home, "recipes", "worker.js")
		later := time.Now().Add(time.Duration(i+1) * time.Second)
		require.NoError(t, os.Chtimes(p, later, later))
		write(t, ri, fmt.Sprintf("kb/tasks/in/c%d.md", i))
		if c.msg == "does not compile" {
			n++
			rows := waitRows(t, ri, "w", n)
			require.Equal(t, c.outcome, rows[n-1].Outcome, c.src)
			require.Contains(t, rows[n-1].Error, c.msg, c.src)
			continue
		}
		n += 2
		rows := waitRows(t, ri, "w", n)
		require.Equal(t, c.outcome, rows[n-1].Outcome, c.src)
		require.Contains(t, rows[n-1].Error, c.msg, c.src)
	}
}
