package repos

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// ---- F08 PR A, M3: a due fire dropped for CAPACITY is retried.
//
// A due mark means "this trigger processed this path at this instant". A fire
// the dispatcher DROPPED because a brake was on — the script rate cap
// (rate-limited) or a recipe at its `concurrent` limit (busy) — was not
// processed, so its mark is not written and the next sweep fires it again.
// A script that threw or ran out of budget WAS processed: its mark stays.

// writeOverdue commits n facts under kb/tasks/due/ in ONE commit, all due an
// hour before the clock, and waits for the dispatcher to process that head.
func writeOverdue(t *testing.T, ri *RepoInstance, clock *fakeClock, n int) []string {
	t.Helper()
	past := clock.now().Add(-time.Hour).Format(time.RFC3339)
	files := map[string]string{}
	var paths []string
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("kb/tasks/due/f%d.md", i)
		files[p] = datedBody(p, past)
		paths = append(paths, p)
	}
	h, _, err := testService(t, ri).Facts().BatchWriteFacts(context.Background(), trigAgent, files, nil, "learn: overdue", "learn")
	require.NoError(t, err)
	waitTriggerHead(t, ri, h)
	settle(t, ri)
	return paths
}

// outcomesByPath counts one trigger's due rows per (path, outcome).
func outcomesByPath(t *testing.T, ri *RepoInstance, name string) map[string]map[string]int {
	t.Helper()
	out := map[string]map[string]int{}
	for _, f := range dueFiresOf(t, ri, name) {
		if out[f.Path] == nil {
			out[f.Path] = map[string]int{}
		}
		out[f.Path][f.Outcome]++
	}
	return out
}

// T-A9 RateLimitedRetriedNextSweep: three overdue facts, a due script capped
// at ONE run a minute. Sweep 1: one `ran`, two `rate-limited`, ONE mark. Each
// later sweep a minute on runs exactly one more, until every fact has exactly
// one `ran` row and three marks; a further sweep adds nothing. Sabotage: keep
// the mark on rate-limited (the old code) → the second sweep runs nothing →
// red; drop the mark on every outcome → the fact that ran runs again → red.
func TestDue_RateLimitedRetriedNextSweep(t *testing.T) {
	clock := newFakeClock()
	dueHooks(t, clock, triggerHooks{})
	_, ri, _ := newScriptRepo(t, 1, scriptTrig("exp", "due", "tasks/due/**", "noop"))
	putScript(t, ri, "noop", `knomit.emit({p: change.path});`)
	settle(t, ri)
	paths := writeOverdue(t, ri, clock, 3)

	ranCount := func() int {
		n := 0
		for _, byOutcome := range outcomesByPath(t, ri, "exp") {
			n += byOutcome[store.TriggerOutcomeRan]
		}
		return n
	}
	require.Equal(t, 1, ranCount(), "sweep 1: the cap lets one run through")
	require.Len(t, dueMarks(t, ri), 1, "only the fact that RAN is marked")

	for sweep := 2; sweep <= 3; sweep++ {
		clock.add(61 * time.Second)
		kickAndWait(t, ri)
		require.Equal(t, sweep, ranCount(), "sweep %d retries one dropped fire", sweep)
		require.Len(t, dueMarks(t, ri), sweep)
	}
	clock.add(61 * time.Second)
	kickAndWait(t, ri)
	byPath := outcomesByPath(t, ri, "exp")
	for _, p := range paths {
		require.Equal(t, 1, byPath[p][store.TriggerOutcomeRan], "%s ran exactly once: %v", p, byPath)
	}
	require.Len(t, dueMarks(t, ri), 3)
	require.Equal(t, 3, ranCount(), "nothing runs twice once every fact is marked")
}

// T-A10 BusyRecipeRetried: a due `do: run` recipe with `concurrent: 1`; two
// overdue facts in one sweep → the first `started`, the second `busy`. Once
// the first recipe's program ends, the next sweep starts the second. Sabotage:
// keep the mark on busy → the second fact never starts → red.
func TestDue_BusyRecipeRetried(t *testing.T) {
	clock := newFakeClock()
	dueHooks(t, clock, triggerHooks{})
	ri, home := newRunRepo(t, runTrig("w", "due", "tasks/due/**", "worker"))
	dir := t.TempDir()
	block := filepath.Join(t.TempDir(), "release")
	t.Cleanup(func() { _ = os.WriteFile(block, nil, 0o600) })
	putLocalRecipe(t, home, "worker", fmt.Sprintf(`// knomit: {"concurrent": 1}
var r = knomit.exec([%s], {env: %s});
({status: "done", message: "exit " + r.exit});`, jsString(helperExe(t)), helperEnv(dir, "HELPER_BLOCK_FILE", block)))
	settle(t, ri)
	paths := writeOverdue(t, ri, clock, 2)
	waitHelperReports(t, dir, "child", 1)

	counts := func() (started, busy map[string]int) {
		started, busy = map[string]int{}, map[string]int{}
		for _, f := range dueFiresOf(t, ri, "w") {
			switch f.Outcome {
			case store.TriggerOutcomeStarted:
				started[f.Path]++
			case store.TriggerOutcomeBusy:
				busy[f.Path]++
			}
		}
		return
	}
	started, busy := counts()
	require.Len(t, started, 1, "one recipe started")
	require.Len(t, busy, 1, "the other fire was busy")
	require.Len(t, dueMarks(t, ri), 1, "the busy fire is not marked")

	require.NoError(t, os.WriteFile(block, nil, 0o600))
	waitRows(t, ri, "w", 3) // started, busy, and the first recipe's result
	kickAndWait(t, ri)
	waitHelperReports(t, dir, "child", 2)
	started, _ = counts()
	for _, p := range paths {
		require.Equal(t, 1, started[p], "%s started exactly once: %v", p, started)
	}
	require.Len(t, dueMarks(t, ri), 2)
}

// T-A11 ScriptErrorNotRetried [N5]: a throwing due script is one
// `script-error`, a looping one (budget 50 ms) one `script-timeout`; both are
// marked, and two more sweeps a minute apart add NO row for either. Sabotage:
// unmark errors and timeouts too → a new row every sweep → red.
func TestDue_ScriptErrorNotRetried(t *testing.T) {
	clock := newFakeClock()
	dueHooks(t, clock, triggerHooks{scriptBudget: 50 * time.Millisecond})
	_, ri, _ := newScriptRepo(t, 0,
		scriptTrig("throws", "due", "tasks/due/**", "throws"),
		scriptTrig("loops", "due", "tasks/due/**", "loops"))
	putScript(t, ri, "throws", `throw new Error("deterministic");`)
	putScript(t, ri, "loops", `while (true) {}`)
	settle(t, ri)
	writeOverdue(t, ri, clock, 1)

	want := func() {
		t.Helper()
		th := dueFiresOf(t, ri, "throws")
		require.Len(t, th, 1, "%+v", th)
		require.Equal(t, store.TriggerOutcomeScriptError, th[0].Outcome)
		lo := dueFiresOf(t, ri, "loops")
		require.Len(t, lo, 1, "%+v", lo)
		require.Equal(t, store.TriggerOutcomeScriptTimeout, lo[0].Outcome)
		require.Len(t, dueMarks(t, ri), 2, "both are marked: processed, not dropped")
	}
	want()
	for i := 0; i < 2; i++ {
		clock.add(61 * time.Second)
		kickAndWait(t, ri)
		want()
	}
}
