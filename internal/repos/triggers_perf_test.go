package repos

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// The slow `if` in these tests is slowIf (triggers_test.go): a real 90 ms of
// wall time inside the trigger's timed section, whatever the CPU load.

// newSlowRepo boots a repo with slow_trigger_ms set.
func newSlowRepo(t *testing.T, slowMS int, entries ...string) (*Manager, *RepoInstance) {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{Home: home, OntologyRoot: "kb"}
	cfg.Log.SlowTriggerMS = slowMS
	m := New(context.Background(), Deps{Cfg: cfg, AgentBranch: trigAgent,
		KeyPath: filepath.Join(home, "agent.key"), DisableBackgroundSync: true})
	t.Cleanup(func() { _ = m.Close() })
	ri := bootRepo(t, m)
	setOntology(t, ri, triggerOntology("", entries...))
	return m, ri
}

// ---- Slow-trigger detector and statistics

// SlowIfIsReported: with slow_trigger_ms = 40, a trigger whose own work takes
// 90 ms logs exactly one "slow trigger" WARN naming repo, branch, trigger,
// path and elapsed, and its `slow` count is 1; a fast trigger in the same run
// logs nothing. Sabotage: hard-code the threshold, or time the whole run.
func TestDispatch_SlowIfIsReported(t *testing.T) {
	logs := captureLogs(t, zerolog.WarnLevel)
	_, ri := newSlowRepo(t, 40, trig("slow", "learn", "", "true"), trig("fast", "learn", "", "true"))
	slowIf(t, "slow", 90, nil)
	write(t, ri, "kb/tasks/x.md")
	out := logs.String()
	require.Equal(t, 1, strings.Count(out, `"message":"slow trigger"`), "%s", out)
	for _, field := range []string{`"repo":"core"`, `"branch":"agent/test"`, `"trigger":"slow"`, `"path":"kb/tasks/x.md"`, `"elapsed":`, `"threshold":`} {
		require.Contains(t, out, field)
	}
	require.NotContains(t, out, `"trigger":"fast"`)
	require.Equal(t, int64(1), ri.triggers.stats.view("slow").Slow)
	require.Equal(t, int64(0), ri.triggers.stats.view("fast").Slow)
	require.GreaterOrEqual(t, ri.triggers.stats.view("slow").Duration.MaxMS, 80.0)
}

// ChangeCostNotChargedToTrigger [R2-2]: a slow `change` build (the toucher
// walk, here a test hook) with a trivial `if` yields a large change_ms on the
// run, a small trigger duration, and NO "slow trigger" WARN. Sabotage: time
// the change build inside the trigger's timer.
func TestDispatch_ChangeCostNotChargedToTrigger(t *testing.T) {
	logs := captureLogs(t, zerolog.WarnLevel)
	_, ri := newSlowRepo(t, 40, trig("fast", "learn", "", "true"))
	setHooks(t, triggerHooks{changeDelay: 80 * time.Millisecond})
	write(t, ri, "kb/tasks/x.md")
	require.NotContains(t, logs.String(), "slow trigger")
	st := ri.triggers.stats.view("fast")
	require.Equal(t, int64(0), st.Slow)
	require.Less(t, st.Duration.MaxMS, 40.0, "the trigger's own work stays small")
	run := ri.triggers.stats.runView()
	require.NotNil(t, run.Last)
	require.GreaterOrEqual(t, run.Last.ChangeMS, int64(80), "the change build is knomit's cost, on the run")
	rows := runRows(t, ri)
	require.GreaterOrEqual(t, rows[0].ChangeMS, int64(80))
}

// P95MovesWithASlowTrigger: 40 fast evaluations give a p95 near the fast
// duration; after 4 slow ones (~10% of the window) the p95 is at least the
// slow duration; count, min and max are exact. Sabotage: report the mean, or
// leave the slow samples out of the window.
func TestDispatch_P95MovesWithASlowTrigger(t *testing.T) {
	_, ri := newTriggerRepo(t, trig("mixed", "learn", "", "true"))
	slowIf(t, "mixed", 90, func(path string) bool { return strings.Contains(path, "slow") })
	release := parkDispatcher(t, ri)
	var last string
	for i := 0; i < 40; i++ {
		last = writeOn(t, ri, trigAgent, fmt.Sprintf("kb/tasks/fast-%02d.md", i))
	}
	release()
	waitTriggerHead(t, ri, last)
	st := ri.triggers.stats.view("mixed")
	require.Equal(t, int64(40), st.Duration.Count)
	require.Less(t, st.Duration.P95RecentMS, 40.0, "40 fast evaluations: the p95 is fast")

	release = parkDispatcher(t, ri)
	for i := 0; i < 4; i++ {
		last = writeOn(t, ri, trigAgent, fmt.Sprintf("kb/tasks/slow-%d.md", i))
	}
	release()
	waitTriggerHead(t, ri, last)
	st = ri.triggers.stats.view("mixed")
	require.Equal(t, int64(44), st.Duration.Count)
	require.Equal(t, int64(44), st.Evaluations)
	require.Equal(t, int64(44), st.Fires)
	require.GreaterOrEqual(t, st.Duration.P95RecentMS, 85.0, "4 slow of 44 puts the nearest-rank p95 on a slow sample")
	require.GreaterOrEqual(t, st.Duration.MaxMS, 85.0)
	require.Less(t, st.Duration.MinMS, 40.0)
	require.Equal(t, triggerP95Window, st.Duration.P95Window)
}

// SlowTriggerThresholdFromConfig: slow_trigger_ms = 0 disables the detector
// (no WARN for the same slow `if`). The config half (default 50, TOML, env)
// is in internal/config. Sabotage: a constant threshold.
func TestDispatch_SlowTriggerThresholdFromConfig(t *testing.T) {
	logs := captureLogs(t, zerolog.WarnLevel)
	_, ri := newSlowRepo(t, 0, trig("slow", "learn", "", "true"))
	slowIf(t, "slow", 90, nil)
	write(t, ri, "kb/tasks/x.md")
	require.NotContains(t, logs.String(), "slow trigger")
	require.Equal(t, int64(0), ri.triggers.stats.view("slow").Slow)
	require.GreaterOrEqual(t, ri.triggers.stats.view("slow").Duration.MaxMS, 80.0, "the duration is still measured")

	// The default from config.Defaults reaches the dispatcher unchanged.
	home := t.TempDir()
	cfg := config.Defaults()
	cfg.Home = home
	m := New(context.Background(), Deps{Cfg: cfg, AgentBranch: trigAgent,
		KeyPath: filepath.Join(home, "agent.key"), DisableBackgroundSync: true})
	t.Cleanup(func() { _ = m.Close() })
	ri2 := bootRepo(t, m)
	require.Equal(t, 50*time.Millisecond, ri2.triggers.slow)
}

// Tx1Bounded at the dispatcher: one advance with 12,000 matching paths logs
// 10,000 fire rows plus a run row with fires_not_logged = 2000, and the
// statistics count all 12,000. Sabotage: insert every row.
func TestDispatch_Tx1Bounded(t *testing.T) {
	_, ri := newTriggerRepo(t, trig("all", "learn", "", ""))
	files := make(map[string]string, 12000)
	for i := 0; i < 12000; i++ {
		files[fmt.Sprintf("kb/tasks/m/%03d/%05d.md", i%120, i)] = factBody(fmt.Sprintf("m%d", i))
	}
	// The advance is committed WITHOUT the index (TestingCommitFiles): what
	// this test measures is the dispatcher, and indexing 12,000 facts is the
	// store's own cost (minutes). The next ordinary write kicks the run that
	// covers both commits.
	started := time.Now()
	h, err := testService(t, ri).TestingCommitFiles(trigAgent, files, "a merge bringing 12,000 facts")
	require.NoError(t, err)
	t.Logf("commit of 12,000 facts (no index): %s", time.Since(started))
	started = time.Now()
	ri.triggers.triggerKick() // what notifyCommit would have done
	waitTriggerHeadFor(t, ri, h, 2*time.Minute)
	t.Logf("dispatcher run over 12,000 paths: %s (run stats: %+v)", time.Since(started), ri.triggers.stats.runView().Last)
	rows := fires(t, ri)
	// tx1 inserted 10,000 fire rows plus the run row; tx2's prune then kept
	// the newest 10,000 rows of the log, so the run row (newest) and 9,999
	// fire rows remain.
	require.Len(t, rows, store.TriggerFireRetention, "the log holds exactly the retention after the flush")
	require.Equal(t, store.TriggerOutcomeRun, rows[0].Outcome)
	require.Equal(t, 2000, rows[0].FiresNotLogged)
	require.Equal(t, 12000, rows[0].Fires)
	require.Equal(t, 12000, rows[0].Evaluated)
	require.Equal(t, 12000, rows[0].Paths)
	st := ri.triggers.stats.view("all")
	require.Equal(t, int64(12000), st.Fires, "every fire is counted, logged or not")
	require.Equal(t, int64(12000), st.Evaluations)
}

// ---- Write latency

func percentile(d []time.Duration, p float64) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s))*p) - 1
	if i < 0 {
		i = 0
	}
	return s[i]
}

func timedWrites(t *testing.T, ri *RepoInstance, prefix string, n int) []time.Duration {
	t.Helper()
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		s := time.Now()
		writeOn(t, ri, trigAgent, fmt.Sprintf("kb/tasks/%s/%03d.md", prefix, i))
		out = append(out, time.Since(s))
	}
	return out
}

// within reports whether b is within max(1.25×a, a+2ms) of a [M1, M5]. It is
// the bound of the load-bearing sabotage target (_BusyIf: synchronous
// dispatch adds about a second per write, which no bound hides).
func within(a, b time.Duration) bool {
	bound := a + a/4
	if a+2*time.Millisecond > bound {
		bound = a + 2*time.Millisecond
	}
	return b <= bound
}

// withinRace is the -race bound for _BusyIf, max(3×a, a+250ms). Under the race
// detector on a shared CI runner the loaded arm measured 1.25–1.50× the base
// (Race runs 36354285281..36614313171): each busy `if` spins a core to its
// interrupt and the runner is also running other packages, so the writer loses
// CPU although dispatch is asynchronous. The synchronous-dispatch sabotage adds
// about a second per write, which this bound still catches.
func withinRace(a, b time.Duration) bool {
	bound := 3 * a
	if a+250*time.Millisecond > bound {
		bound = a + 250*time.Millisecond
	}
	return b <= bound
}

// withinMedian is the EVERYDAY bound, max(1.5×a, a+3ms) (1b follow-up, applied
// in PR 2): the 1.25× bound failed 2 of 3 runs on a loaded laptop while CI
// never failed it, and a loaded laptop is where the test is run by hand. The
// sabotage that must still redden it is a flush after EVERY run (1b review
// #15: 6.5 → 10.8 ms median, 1.66×).
func withinMedian(a, b time.Duration) bool {
	bound := a + a/2
	if a+3*time.Millisecond > bound {
		bound = a + 3*time.Millisecond
	}
	return b <= bound
}

// abba measures write latency with and without the given trigger entries in
// ALTERNATING blocks (A B B A A B B A …), so both arms see the same repo
// growth. A first-then-second layout does not work: a write's latency on this
// store grows with the repo (about 4.7 ms at 50 facts to 16 ms at 1,000 in
// one measurement), so a set measured after another is slower for that reason
// alone, triggers or no triggers.
func abba(t *testing.T, ri *RepoInstance, entries []string, rounds, perBlock int) (base, loaded []time.Duration) {
	t.Helper()
	// Each switch waits for the dispatcher to drain the previous block: with
	// busy `if`s that backlog is seconds per path.
	switchTo := func(yaml string) {
		r, err := testService(t, ri).Facts().WriteFact(context.Background(), trigAgent, OntologyPath, yaml, "ontology", "updated")
		require.NoError(t, err)
		waitTriggerHeadFor(t, ri, r.CommitHash, 3*time.Minute)
	}
	for r := 0; r < rounds; r++ {
		first := r%2 == 0
		for half := 0; half < 2; half++ {
			withTriggers := (half == 1) == first
			if withTriggers {
				switchTo(triggerOntology("", entries...))
				loaded = append(loaded, timedWrites(t, ri, fmt.Sprintf("l%d%d", r, half), perBlock)...)
			} else {
				switchTo(triggerOntology(""))
				base = append(base, timedWrites(t, ri, fmt.Sprintf("b%d%d", r, half), perBlock)...)
			}
		}
	}
	return base, loaded
}

// WriteLatencyIndependentOfTriggers: 200 learns with 0 triggers and 200 with
// 50 triggers that all match, in alternating blocks; the median with triggers
// is within max(1.5×, +3 ms) of the median without (withinMedian); the p90 is
// reported, not gated (1b follow-up). The fire log is writing during the
// loaded blocks (two short transactions per run). Sabotage: flush phase C
// after every run (the median moves ~1.66×); or call the dispatcher
// synchronously from ri.onCommit — with the busy-if variant below every write
// gains ≥ 10 × 100 ms.
//
// Under -race the median is LOGGED, not gated: the flush-per-run sabotage
// moves it ~1.66× while race-detector noise on CI already reached 1.50×, so no
// bound separates them there. The test still runs under -race for its race
// coverage; the gate is the non-race run (tests.yml).
func TestDispatch_WriteLatencyIndependentOfTriggers(t *testing.T) {
	m := newTestManager(t)
	ri := bootRepo(t, m)
	entries := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		entries = append(entries, trig(fmt.Sprintf("t%02d", i), "learn", "tasks/**", ""))
	}
	base, loaded := abba(t, ri, entries, 8, 25)
	waitTriggerHead(t, ri, head(t, ri))

	bm, lm := percentile(base, 0.5), percentile(loaded, 0.5)
	b90, l90 := percentile(base, 0.9), percentile(loaded, 0.9)
	t.Logf("median: 0 triggers %s, 50 triggers %s (%.2f×); p90 (report only): %s vs %s", bm, lm, float64(lm)/float64(bm), b90, l90)
	if raceEnabled {
		t.Log("-race: the median is reported, not gated")
	} else {
		require.True(t, withinMedian(bm, lm), "median with 50 triggers (%s) is not within max(1.5×, +3ms) of 0 triggers (%s)", lm, bm)
	}
	require.GreaterOrEqual(t, ri.triggers.stats.view("t00").Fires, int64(100), "the loaded blocks did fire")
}

// The sabotage target [M5]: 10 triggers whose `if` busy-loops to the 100 ms
// interrupt. Asynchronous dispatch leaves the writes untouched; synchronous
// dispatch would add about a second to every write, a difference no bound
// hides. Under -race the bound is withinRace, max(3×, +250ms): the tight one
// failed on CPU contention alone (see withinRace).
// The valid sabotage is "the writer waits for its own run" (triggerKick
// blocks until the run it kicked completes). Running the dispatcher directly
// inside triggerKick is not: onCommit runs under the writer's branch lock
// (builder.go), and the test then stalls instead of measuring latency.
func TestDispatch_WriteLatencyIndependentOfTriggers_BusyIf(t *testing.T) {
	m := newTestManager(t)
	ri := bootRepo(t, m)
	entries := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		entries = append(entries, trig(fmt.Sprintf("b%02d", i), "learn", "tasks/**", "(function(){ while (true) {} })()"))
	}
	base, loaded := abba(t, ri, entries, 2, 20)

	bm, lm := percentile(base, 0.5), percentile(loaded, 0.5)
	t.Logf("median: 0 triggers %s, 10 busy triggers %s (%.2f×)", bm, lm, float64(lm)/float64(bm))
	if raceEnabled {
		require.True(t, withinRace(bm, lm), "median with busy triggers (%s) is not within max(3×, +250ms) of 0 triggers (%s) under -race", lm, bm)
	} else {
		require.True(t, within(bm, lm), "median with busy triggers (%s) is not within max(1.25×, +2ms) of 0 triggers (%s)", lm, bm)
	}
	// Teardown must not wait for the backlog: ctx is checked per evaluation.
	started := time.Now()
	ri.triggers.stop()
	require.Less(t, time.Since(started), 2*time.Second)
}

// ---- Benchmark (numbers go in the PR body; not gated)

// BenchmarkDispatchAdvance runs one dispatcher run end to end on a fixture of
// about 2,000 facts, for an own-write advance and a merge advance, with 0 and
// 50 triggers. Each iteration resets the watermarks to the pre-advance head
// and runs the dispatcher synchronously.
func BenchmarkDispatchAdvance(b *testing.B) {
	for _, tc := range []struct {
		name     string
		triggers int
		merge    bool
		noIf     bool // triggers without an `if`: matching + change + emit only
	}{
		{"own-write/0", 0, false, false}, {"own-write/50", 50, false, false}, {"own-write/50-no-if", 50, false, true},
		{"merge/0", 0, true, false}, {"merge/50", 50, true, false}, {"merge/50-no-if", 50, true, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			home := b.TempDir()
			m := New(context.Background(), Deps{Cfg: config.Config{Home: home, OntologyRoot: "kb"}, AgentBranch: trigAgent,
				KeyPath: filepath.Join(home, "agent.key"), DisableBackgroundSync: true})
			defer m.Close()
			if err := m.Start(); err != nil {
				b.Fatal(err)
			}
			ri, err := m.Create(context.Background(), CreateSpec{Name: "bench", Mode: "preset"}, nil)
			if err != nil {
				b.Fatal(err)
			}
			svc, release, err := ri.Acquire()
			if err != nil {
				b.Fatal(err)
			}
			defer release()
			ctx := context.Background()
			must := func(err error) {
				if err != nil {
					b.Fatal(err)
				}
			}
			headOf := func() string {
				h, err := svc.Branches().HeadCommit(ctx, trigAgent)
				must(err)
				return h
			}
			waitHead := func(h string) {
				deadline := time.Now().Add(2 * time.Minute)
				for {
					got, _ := ri.triggers.runSequence()
					if got == h && ri.triggers.flushed() {
						return
					}
					if time.Now().After(deadline) {
						b.Fatalf("the dispatcher never completed a run at %s", h)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			writeB := func(branch, path string) {
				_, err := svc.Facts().WriteFact(ctx, branch, path, factBody(path), "learn: "+path, "learn")
				must(err)
			}
			files := map[string]string{}
			for i := 0; i < 2000; i++ {
				files[fmt.Sprintf("kb/tasks/f/%04d.md", i)] = factBody(fmt.Sprintf("f%d", i))
			}
			entries := make([]string, 0, tc.triggers)
			cond := "fact.confidence > 0.5"
			if tc.noIf {
				cond = ""
			}
			for i := 0; i < tc.triggers; i++ {
				entries = append(entries, trig(fmt.Sprintf("t%02d", i), "[learn, update, retract]", "tasks/**", cond))
			}
			files[OntologyPath] = triggerOntology("", entries...)
			// The fixture commit skips the index (TestingCommitFiles): indexing
			// 2,000 facts is the store's cost, not the dispatcher's.
			_, err = svc.TestingCommitFiles(trigAgent, files, "fixture")
			must(err)
			ri.triggers.triggerKick()
			waitHead(headOf())
			w := headOf()
			// The advance: 8 paths (a typical knomit-kb advance), own writes
			// or brought in by a merge from a peer branch.
			src := trigAgent
			if tc.merge {
				src = "peer"
				must(svc.Branches().CreateBranch(ctx, "peer", trigAgent))
			}
			for i := 0; i < 8; i++ {
				writeB(src, fmt.Sprintf("kb/tasks/adv/%d.md", i))
			}
			if tc.merge {
				writeB(trigAgent, "kb/other/mine.md")
				must(svc.Branches().MergeBranch(ctx, "peer", trigAgent, store.StrategyRemoteWins))
			}
			waitHead(headOf())
			d := ri.triggers
			names := map[string]string{}
			for i := 0; i < tc.triggers; i++ {
				names[fmt.Sprintf("t%02d", i)] = w
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				if len(names) > 0 {
					must(svc.Triggers().AdvanceTriggerWatermarks(ctx, trigAgent, names, nil, nil))
				}
				b.StartTimer()
				d.run(ctx)
				d.flush(ctx)
			}
			b.StopTimer()
			if tc.triggers > 0 && d.stats.view("t00").Fires == 0 {
				b.Fatal("the benchmark never fired")
			}
		})
	}
}
