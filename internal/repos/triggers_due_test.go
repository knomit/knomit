package repos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	knomitfact "knomit/internal/fact"
	"knomit/internal/store"
)

// ---- F07 PR 2: `on: due`.
//
// The fixture is the 1b one (newTriggerRepo: agent branch "agent/test",
// background sync off, triggers installed by committing the ontology) plus a
// FAKE CLOCK installed through the `now` hook: the dispatcher reads its clock
// once per run, so a test moves time by moving the clock and kicking a run,
// never by sleeping past a real date.

// fakeClock is the run clock the tests control. UTC, whole seconds.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Now().UTC().Truncate(time.Second)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t.UTC().Truncate(time.Second)
}

// dueHooks installs the fake clock (and any extra hooks) for the test.
func dueHooks(t *testing.T, clock *fakeClock, extra triggerHooks) {
	t.Helper()
	extra.now = clock.now
	setHooks(t, extra)
}

// newDueRepo boots a repo with the given triggers and a fake clock.
func newDueRepo(t *testing.T, entries ...string) (*Manager, *RepoInstance, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	dueHooks(t, clock, triggerHooks{})
	m, ri := newTriggerRepo(t, entries...)
	return m, ri, clock
}

// datedBody is factBody with an `expires` line.
func datedBody(title, expires string) string {
	return "---\ntype: hypothesis\nconfidence: 0.8\nsources: 1\nexpires: \"" + expires + "\"\n---\n# " + title + "\n\nbody\n"
}

// writeDated writes a dated fact on branch and returns the commit. It does
// NOT wait for the dispatcher (writeOn's shape); see writeDatedAndWait.
func writeDated(t *testing.T, ri *RepoInstance, branch, path string, expires time.Time) string {
	t.Helper()
	r, err := testService(t, ri).Facts().WriteFact(context.Background(), branch, path,
		datedBody(path, expires.UTC().Format(time.RFC3339)), "learn: "+path, "learn")
	require.NoError(t, err)
	return r.CommitHash
}

func writeDatedAndWait(t *testing.T, ri *RepoInstance, path string, expires time.Time) string {
	t.Helper()
	h := writeDated(t, ri, trigAgent, path, expires)
	waitTriggerHead(t, ri, h)
	return h
}

// kickAndWait delivers one kick (a tick, as far as the dispatcher can tell)
// and waits for a run to complete AND flush.
func kickAndWait(t *testing.T, ri *RepoInstance) {
	t.Helper()
	_, seq := ri.triggers.runSequence()
	ri.triggers.triggerKick()
	require.Eventually(t, func() bool {
		_, s := ri.triggers.runSequence()
		return s > seq && ri.triggers.flushed()
	}, 20*time.Second, 10*time.Millisecond, "no run completed and flushed after the kick")
}

// dueFiresOf is the DUE fire rows of one trigger, newest first.
func dueFiresOf(t *testing.T, ri *RepoInstance, name string) []store.TriggerFire {
	t.Helper()
	var out []store.TriggerFire
	for _, f := range firesOf(t, ri, name) {
		if f.Episode == "due" {
			out = append(out, f)
		}
	}
	return out
}

func dueMarks(t *testing.T, ri *RepoInstance) map[store.DueKey]int64 {
	t.Helper()
	m, err := testService(t, ri).Triggers().DueMarks(context.Background(), trigAgent)
	require.NoError(t, err)
	return m
}

// dueCandidates is the sweep's join at instant `at`, as path → expires_at.
func dueCandidates(t *testing.T, ri *RepoInstance, branch string, at time.Time) map[string]int64 {
	t.Helper()
	rows, err := testService(t, ri).Triggers().DueCandidates(context.Background(), branch, at.Unix())
	require.NoError(t, err)
	out := map[string]int64{}
	for _, r := range rows {
		out[r.Path] = r.ExpiresAt
	}
	return out
}

// farFuture is "every dated fact is a candidate": the join without the clock.
var farFuture = time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)

// ---- T1–T9 [M4] A fact removed or replaced before its date never fires.

// RemovedBeforeItsDateNeverFires: a `due` trigger and a fact due in an hour;
// the fact is removed or replaced by each path the F03/F07 design names; the
// clock jumps past the date; a run. TWO assertions per path: (1) the sweep's
// JOIN has no row for the removed VERSION — the store-level truth the index's
// maintenance gives; (2) no fire for it — what the head confirmation gives
// even when fed a stale candidate (the dueCandidates hook appends the removed
// path with its old instant on every run, so the confirmation is exercised).
// Two sabotages, each reddening exactly one assertion: drop the branch_facts
// DELETE in searchIndex.delete → (1) is red (the join lists the path) while
// (2) stays green because the confirmation skips the absent blob — which is
// why (1) must be asserted; skip the head confirmation → (2) is red (the
// stale candidate fires). The last sub-case is named for what it proves: a
// rebuild removes nothing; a fact removed before it stays absent after it.
func TestDue_RemovedBeforeItsDateNeverFires(t *testing.T) {
	const path = "kb/tasks/gone.md"
	cases := []struct {
		name string
		// remove removes or replaces path on the agent branch and returns the
		// expires_at the join may still show (0 for none) — for a REPLACED
		// fact, the NEW version's instant. base is the head before the fact.
		remove func(t *testing.T, ri *RepoInstance, base string, due time.Time) int64
	}{
		{"retract", func(t *testing.T, ri *RepoInstance, _ string, _ time.Time) int64 {
			waitTriggerHead(t, ri, deleteOn(t, ri, trigAgent, path)) // knomit_retract → DeleteFact
			return 0
		}},
		{"delete", func(t *testing.T, ri *RepoInstance, _ string, _ time.Time) int64 {
			// REST DELETE reaches the same DeleteFact; the message differs.
			h, err := testService(t, ri).Facts().DeleteFact(context.Background(), trigAgent, path, "delete: "+path)
			require.NoError(t, err)
			waitTriggerHead(t, ri, h)
			return 0
		}},
		{"batch", func(t *testing.T, ri *RepoInstance, _ string, _ time.Time) int64 {
			h, _, err := testService(t, ri).Facts().BatchWriteFacts(context.Background(), trigAgent,
				map[string]string{"kb/other/kept.md": factBody("kept")}, []string{path}, "batch", "learn")
			require.NoError(t, err)
			waitTriggerHead(t, ri, h)
			return 0
		}},
		{"update_clear", func(t *testing.T, ri *RepoInstance, _ string, _ time.Time) int64 {
			waitTriggerHead(t, ri, updateOn(t, ri, trigAgent, path)) // undated body: expires cleared
			return 0
		}},
		{"update_change", func(t *testing.T, ri *RepoInstance, _ string, due time.Time) int64 {
			writeDatedAndWait(t, ri, path, due.Add(3*time.Hour))
			return due.Add(3 * time.Hour).Unix()
		}},
		{"merge_delete", func(t *testing.T, ri *RepoInstance, _ string, _ time.Time) int64 {
			svc := testService(t, ri)
			ctx := context.Background()
			_, err := svc.AdvanceLocalUpstream(ctx, trigAgent, "main") // main holds the fact too
			require.NoError(t, err)
			_, err = svc.Facts().DeleteFact(ctx, "main", path, "peer retract")
			require.NoError(t, err)
			require.NoError(t, svc.Branches().MergeBranch(ctx, "main", trigAgent, store.StrategyRemoteWins))
			waitTriggerHead(t, ri, head(t, ri))
			return 0
		}},
		{"merge_rewrite", func(t *testing.T, ri *RepoInstance, _ string, due time.Time) int64 {
			svc := testService(t, ri)
			ctx := context.Background()
			_, err := svc.AdvanceLocalUpstream(ctx, trigAgent, "main")
			require.NoError(t, err)
			writeDated(t, ri, "main", path, due.Add(3*time.Hour))
			require.NoError(t, svc.Branches().MergeBranch(ctx, "main", trigAgent, store.StrategyRemoteWins))
			waitTriggerHead(t, ri, head(t, ri))
			return due.Add(3 * time.Hour).Unix()
		}},
		{"rewind", func(t *testing.T, ri *RepoInstance, base string, _ time.Time) int64 {
			// The branch goes back to before the fact and a new commit lands on
			// top: the path is scrubbed from the new history (the 1b rewind).
			svc := testService(t, ri)
			require.NoError(t, svc.TestingSetRef("refs/heads/"+trigAgent, base))
			write(t, ri, "kb/tasks/after-rewind.md")
			return 0
		}},
		{"removed_then_rebuilt", func(t *testing.T, ri *RepoInstance, _ string, _ time.Time) int64 {
			waitTriggerHead(t, ri, deleteOn(t, ri, trigAgent, path))
			require.NoError(t, testService(t, ri).IndexManager().Rebuild(context.Background(), trigAgent, nil))
			return 0
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ri, clock := newDueRepo(t, trig("due", "due", "", ""))
			due := clock.now().Add(time.Hour)
			// Every run is also fed the removed version as a STALE candidate.
			dueHooks(t, clock, triggerHooks{dueCandidates: func(c []store.DueCandidate) []store.DueCandidate {
				return append(c, store.DueCandidate{Path: path, ExpiresAt: due.Unix()})
			}})
			base := head(t, ri)
			writeDatedAndWait(t, ri, path, due)
			require.Equal(t, due.Unix(), dueCandidates(t, ri, trigAgent, farFuture)[path], "fixture: live and dated")

			keep := tc.remove(t, ri, base, due)
			clock.add(2 * time.Hour)
			kickAndWait(t, ri)

			// (1) the join.
			got, listed := dueCandidates(t, ri, trigAgent, farFuture)[path]
			if keep == 0 {
				require.False(t, listed, "%s: the removed fact is still in the sweep's join (expires_at %d)", tc.name, got)
			} else {
				require.Equal(t, keep, got, "%s: the join must show the NEW version's instant only", tc.name)
			}
			// (2) no fire for the old version.
			for _, f := range dueFiresOf(t, ri, "due") {
				require.NotEqual(t, path, f.Path, "%s: a removed fact fired: %+v", tc.name, f)
			}
			if keep != 0 {
				// The replacement fires exactly once, for ITS instant, when it passes.
				clock.set(time.Unix(keep, 0).Add(time.Minute))
				kickAndWait(t, ri)
				rows := dueFiresOf(t, ri, "due")
				require.Len(t, rows, 1, "%s: the new version fires once", tc.name)
				require.Equal(t, path, rows[0].Path)
				require.Equal(t, keep, dueMarks(t, ri)[store.DueKey{Trigger: "due", Path: path}], "the mark holds the new instant")
			}
		})
	}
}

// ---- T7 [M5] Fires once across ticks, including a run INSIDE the flush window.

// FiresOnceAcrossTicks: run 1 after the date fires; run 2 is kicked while
// run 1 is still parked, so it starts as soon as run 1 ends — BEFORE the 20 ms
// flush grace — and must see the mark through the write-behind overlay; run 3
// after the flush reads it from the table. One fire, one mark, one row on the
// endpoint. Two sabotages, each red on its own: drop the mark write → 3
// fires; drop the overlay only (overlayPending ignores pending marks) → 2
// fires (the in-window run re-fires; the post-flush run does not).
func TestDue_FiresOnceAcrossTicks(t *testing.T) {
	_, ri, clock := newDueRepo(t, trig("due", "due", "", ""))
	writeDatedAndWait(t, ri, "kb/tasks/x.md", clock.now().Add(time.Hour))
	clock.add(2 * time.Hour)

	// Park run 1 between its kick and its ref read; queue run 2 meanwhile.
	gate := make(chan struct{})
	var parked sync.Once
	dueHooks(t, clock, triggerHooks{afterKick: func() {
		parked.Do(func() { <-gate })
	}})
	_, seq := ri.triggers.runSequence()
	ri.triggers.triggerKick()
	time.Sleep(50 * time.Millisecond) // run 1 is parked; this kick is pending behind it
	ri.triggers.triggerKick()
	close(gate)
	require.Eventually(t, func() bool {
		_, s := ri.triggers.runSequence()
		return s >= seq+2 && ri.triggers.flushed()
	}, 20*time.Second, 10*time.Millisecond, "runs 1 and 2 must complete and flush")
	require.Len(t, dueFiresOf(t, ri, "due"), 1, "run 2 (inside the flush window) must not re-fire")

	kickAndWait(t, ri) // run 3, after the flush
	require.Len(t, dueFiresOf(t, ri, "due"), 1)
	require.Len(t, dueMarks(t, ri), 1)
	rep, err := ri.TriggerReport(context.Background(), 50)
	require.NoError(t, err)
	n := 0
	for _, f := range rep.Fires {
		if f.Episode == "due" {
			n++
		}
	}
	require.Equal(t, 1, n, "the endpoint shows one due row")
	require.EqualValues(t, 1, ri.triggers.stats.view("due").Fires)
}

// ---- T8, T9 Re-arm and body edits.

// ChangingExpiresRearms: fire at date A; the author moves the date to B in the
// future: no fire; the clock passes B: exactly one more fire, and the mark
// holds B. Sabotage: compare marks by path only (ignore expires_at).
func TestDue_ChangingExpiresRearms(t *testing.T) {
	_, ri, clock := newDueRepo(t, trig("due", "due", "", ""))
	a := clock.now().Add(time.Hour)
	writeDatedAndWait(t, ri, "kb/tasks/x.md", a)
	clock.add(2 * time.Hour)
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 1)
	require.Equal(t, a.Unix(), dueMarks(t, ri)[store.DueKey{Trigger: "due", Path: "kb/tasks/x.md"}])

	b := clock.now().Add(time.Hour)
	writeDatedAndWait(t, ri, "kb/tasks/x.md", b) // knomit_update sets the new date
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 1, "B is in the future: no fire")

	clock.add(2 * time.Hour)
	kickAndWait(t, ri)
	rows := dueFiresOf(t, ri, "due")
	require.Len(t, rows, 2, "the fact re-armed at B")
	require.Equal(t, b.Unix(), dueMarks(t, ri)[store.DueKey{Trigger: "due", Path: "kb/tasks/x.md"}])
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 2, "once per due instant")
}

// BodyEditDoesNotRefire: after the fire, an edit that keeps `expires` (a body
// change) does not fire again — the mark is keyed by the due instant, not the
// fact version. Sabotage: key the marks by fact_id (the version).
func TestDue_BodyEditDoesNotRefire(t *testing.T) {
	_, ri, clock := newDueRepo(t, trig("due", "due", "", ""))
	a := clock.now().Add(time.Hour)
	writeDatedAndWait(t, ri, "kb/tasks/x.md", a)
	clock.add(2 * time.Hour)
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 1)

	r, err := testService(t, ri).Facts().WriteFact(context.Background(), trigAgent, "kb/tasks/x.md",
		datedBody("kb/tasks/x.md v2 — same date", a.Format(time.RFC3339)), "update", "update")
	require.NoError(t, err)
	waitTriggerHead(t, ri, r.CommitHash)
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 1, "a body-only edit must not re-fire")
	// Cleared then set back to the SAME date: same path + same instant = processed.
	waitTriggerHead(t, ri, updateOn(t, ri, trigAgent, "kb/tasks/x.md"))
	writeDatedAndWait(t, ri, "kb/tasks/x.md", a)
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 1)
}

// ---- T10 Crash between the fire and its mark.

// CrashBeforeMarkRefiresAndLogsBoth: a panic between tx1 and tx2 leaves the
// fire row logged and the mark unwritten; on restart the fact fires again
// (at-least-once, as advances) and the log shows both. Sabotage: write the
// marks in tx1 (before the fire rows), or before the emits.
func TestDue_CrashBeforeMarkRefiresAndLogsBoth(t *testing.T) {
	home := t.TempDir()
	deps := Deps{Cfg: config.Config{Home: home, OntologyRoot: "kb"}, AgentBranch: trigAgent,
		KeyPath: filepath.Join(home, "agent.key"), DisableBackgroundSync: true}
	clock := newFakeClock()
	dueHooks(t, clock, triggerHooks{})
	m := New(context.Background(), deps)
	ri := bootRepo(t, m)
	setOntology(t, ri, triggerOntology("", trig("due", "due", "", "")))
	writeDatedAndWait(t, ri, "kb/tasks/x.md", clock.now().Add(time.Hour))
	clock.add(2 * time.Hour)
	var once sync.Once
	dueHooks(t, clock, triggerHooks{beforeTx2: func() {
		once.Do(func() { panic("crash between tx1 and tx2") })
	}})
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 1, "tx1 committed before the crash")
	require.Empty(t, dueMarks(t, ri), "tx2 never ran: no mark")
	require.NoError(t, m.Close())

	m2 := New(context.Background(), deps)
	require.NoError(t, m2.Start())
	t.Cleanup(func() { _ = m2.Close() })
	ri2 := m2.Get(testRepoName)
	require.NotNil(t, ri2)
	require.Eventually(t, func() bool {
		return len(dueFiresOf(t, ri2, "due")) == 2 && len(dueMarks(t, ri2)) == 1
	}, 20*time.Second, 20*time.Millisecond, "the fact must fire again on restart and be marked")
}

// ---- T11 [M3] The sweep runs on the reconcile tick, including a failed one.

// SweepRunsOnReconcileTick: background sync ON, an origin-less repo with a
// 100 ms local reconcile interval; a fact comes due with NO write; the fire
// appears within a few ticks; and the run count grows only while something is
// evaluated (idle ticks write nothing). Sabotage: remove the kick from the
// local loop → never fires; key the run row on candidates instead of
// evaluations → the count keeps growing.
func TestDue_SweepRunsOnReconcileTick(t *testing.T) {
	home := t.TempDir()
	clock := newFakeClock()
	dueHooks(t, clock, triggerHooks{})
	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: home, OntologyRoot: "kb",
			Git: config.GitConfig{LocalReconcileInterval: 100 * time.Millisecond}},
		AgentBranch: trigAgent,
		KeyPath:     filepath.Join(home, "agent.key"),
		// DisableBackgroundSync deliberately NOT set: the tick is the point.
	})
	t.Cleanup(func() { m.Close() })
	ri := bootRepo(t, m)
	setOntology(t, ri, triggerOntology("", trig("due", "due", "", "")))
	writeDatedAndWait(t, ri, "kb/tasks/x.md", clock.now().Add(time.Hour))
	require.Empty(t, dueFiresOf(t, ri, "due"))

	clock.add(2 * time.Hour) // no write follows: only the tick can bring the run
	require.Eventually(t, func() bool { return len(dueFiresOf(t, ri, "due")) == 1 },
		10*time.Second, 20*time.Millisecond, "the tick must kick the sweep")

	runs := ri.triggers.stats.runView().Runs
	time.Sleep(600 * time.Millisecond) // ~6 idle ticks
	require.Equal(t, runs, ri.triggers.stats.runView().Runs, "an idle tick evaluates nothing and is not a run")
	require.Len(t, dueFiresOf(t, ri, "due"), 1)
}

// SweepRunsOnReconcileTick/failed_tick: the local loop's advance fails on
// every tick — the kick is unconditional, so the due fact still fires.
// Sabotage: make the kick conditional on advance() == nil.
func TestDue_SweepRunsOnReconcileTick_FailedLocalTick(t *testing.T) {
	_, ri, clock := newDueRepo(t, trig("due", "due", "", ""))
	writeDatedAndWait(t, ri, "kb/tasks/x.md", clock.now().Add(time.Hour))
	clock.add(2 * time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var advances atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLocalReconcile(ctx, "repo", trigAgent, 20*time.Millisecond, nil,
			func() (bool, error) { return false, nil },
			func() error { advances.Add(1); return errors.New("advance fails every tick") },
			ri.triggers.triggerKick, nil)
	}()
	require.Eventually(t, func() bool { return len(dueFiresOf(t, ri, "due")) == 1 },
		10*time.Second, 20*time.Millisecond, "a failing advance must not silence the sweep")
	require.Greater(t, advances.Load(), int64(0), "fixture: the ticks did run (and fail)")
	cancel()
	<-done
}

// SweepRunsOnReconcileTick/failed_remote_tick: the origin loop's tick fails
// before Sync (auth resolution fails, the :194 return); the deferred kick
// still fires the due fact. Sabotage: move the kick below the early returns.
func TestDue_SweepRunsOnReconcileTick_FailedRemoteTick(t *testing.T) {
	_, ri, clock := newDueRepo(t, trig("due", "due", "", ""))
	writeDatedAndWait(t, ri, "kb/tasks/x.md", clock.now().Add(time.Hour))
	clock.add(2 * time.Hour)
	svc := testService(t, ri)
	svc.SetOrigin(&store.Origin{URL: "https://example.invalid/kb.git", Branch: "main"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var authCalls atomic.Int64
	failingAuth := func(*store.Remote) (transport.AuthMethod, error) {
		authCalls.Add(1)
		return nil, errors.New("no credential")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go runReconcileLoop(ctx, &wg, svc, ri.hub, ri.Name(), trigAgent, failingAuth, "", false, nil, ri.triggers.triggerKick, nil, nil, nil)
	require.Eventually(t, func() bool { return len(dueFiresOf(t, ri, "due")) == 1 },
		10*time.Second, 20*time.Millisecond, "a tick that fails before Sync must still kick the sweep")
	require.Greater(t, authCalls.Load(), int64(0), "fixture: the tick ran and failed at auth resolution")
	cancel()
	wg.Wait()
}

// ---- T12 [N3] One clock read per run; F03's inclusive boundary.

// ClockReadOncePerRun: with the clock hooked, a fact due in 2 s does not fire;
// the clock advances 3 s: it fires, and the mark's fired_at is the hooked
// instant. Boundary: expires_at == now fires; now − 1 s does not. Sabotage:
// compare with time.Now() in the candidate loop (the hook is ignored and the
// first run fires); `<` for `<=` (the boundary case never fires).
func TestDue_ClockReadOncePerRun(t *testing.T) {
	_, ri, clock := newDueRepo(t, trig("due", "due", "", ""))
	soon := clock.now().Add(2 * time.Second)
	writeDatedAndWait(t, ri, "kb/tasks/soon.md", soon)
	kickAndWait(t, ri)
	require.Empty(t, dueFiresOf(t, ri, "due"), "not due yet on the hooked clock")
	clock.add(3 * time.Second)
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 1)
	require.Equal(t, soon.Unix(), dueMarks(t, ri)[store.DueKey{Trigger: "due", Path: "kb/tasks/soon.md"}])

	edge := clock.now().Add(time.Hour)
	writeDatedAndWait(t, ri, "kb/tasks/edge.md", edge)
	clock.set(edge.Add(-time.Second))
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 1, "one second before its instant a fact is not due")
	clock.set(edge)
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 2, "expires_at == now is due (F03's inclusive rule)")
}

// ---- T13b The sweep is not reachable from the write path (structural).

// SweepNotReachableFromWritePath: the due identifiers appear in the store's
// triggers file, the dispatcher, the migration and tests — nowhere else: not
// in fact_write.go, branch_commit.go or the index. Sabotage: reference
// DueCandidates from fact_write.go.
func TestDue_SweepNotReachableFromWritePath(t *testing.T) {
	allowed := map[string]bool{
		"internal/store/triggers.go": true,
		"internal/repos/triggers.go": true,
	}
	root := filepath.Join("..", "..")
	var offenders []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "dist", ".claude", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if allowed[rel] {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s := string(b)
		for _, id := range []string{"DueCandidates", "DueMarks", "trigger_due_fires", "sweepDue"} {
			if strings.Contains(s, id) {
				offenders = append(offenders, rel+": "+id)
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, offenders, "the due sweep leaked outside the dispatcher and its store surface")
}

// ---- T14, T14b Mixed triggers and activation.

// MixedLearnDue (D3): a `[learn, due]` trigger fires BOTH episodes in one run
// for a fact learned already past its date (two rows, episodes differ); a
// future-dated fact fires learn only, then due when the clock passes; an
// `[update, due]` trigger fires update and due together when the date is
// moved to another past instant (N5). A `[learn]`-only sibling never fires
// due. Sabotage: drop the OnEpisode("due") check in the sweep.
func TestDue_MixedLearnDue(t *testing.T) {
	_, ri, clock := newDueRepo(t,
		trig("mixed", "[learn, due]", "", ""),
		trig("plain", "learn", "", ""),
		trig("upd", "[update, due]", "", ""))
	runsBefore := len(runRows(t, ri))
	writeDatedAndWait(t, ri, "kb/tasks/past.md", clock.now().Add(-time.Hour))
	episodes := func(name, path string) []string {
		var out []string
		for _, f := range firesOf(t, ri, name) {
			if f.Path == path {
				out = append(out, f.Episode)
			}
		}
		return out
	}
	require.ElementsMatch(t, []string{"learn", "due"}, episodes("mixed", "kb/tasks/past.md"), "both episodes, one run")
	require.Equal(t, []string{"learn"}, episodes("plain", "kb/tasks/past.md"), "a learn-only trigger never fires due")
	require.Len(t, runRows(t, ri), runsBefore+1, "one run row for the write that fired both")

	writeDatedAndWait(t, ri, "kb/tasks/future.md", clock.now().Add(time.Hour))
	require.Equal(t, []string{"learn"}, episodes("mixed", "kb/tasks/future.md"))
	clock.add(2 * time.Hour)
	kickAndWait(t, ri)
	require.ElementsMatch(t, []string{"learn", "due"}, episodes("mixed", "kb/tasks/future.md"))

	// [update, due]: `upd` fired due once when the date passed above (no learn
	// side); moving the date to ANOTHER past instant fires update AND due in
	// one run — the re-arm.
	require.Equal(t, []string{"due"}, episodes("upd", "kb/tasks/future.md"))
	writeDatedAndWait(t, ri, "kb/tasks/future.md", clock.now().Add(-30*time.Minute))
	require.Equal(t, []string{"due", "update", "due"}, episodes("upd", "kb/tasks/future.md"), "newest first")
	rows := firesOf(t, ri, "upd")
	require.Equal(t, rows[0].RangeTo, rows[1].RangeTo, "update and the re-armed due are rows of ONE run")
}

// ActivationAlreadyOverdue (D2 (a), maintainer ruling "the feature was never
// live"): facts already past their date when a `due` trigger first appears
// fire ONCE on the first sweep, and only once; the learn side bookmarks W = H
// and nothing before it fires learn. Sabotage: seed marks at first
// appearance (nothing fires).
func TestDue_ActivationAlreadyOverdue(t *testing.T) {
	m := newTestManager(t)
	ri := bootRepo(t, m)
	clock := newFakeClock()
	dueHooks(t, clock, triggerHooks{})
	writeDated(t, ri, trigAgent, "kb/tasks/old-a.md", clock.now().Add(-2*time.Hour))
	writeDated(t, ri, trigAgent, "kb/tasks/old-b.md", clock.now().Add(-time.Hour))
	writeOn(t, ri, trigAgent, "kb/tasks/old-undated.md")
	h := setOntology(t, ri, triggerOntology("", trig("mixed", "[learn, due]", "", "")))
	require.Equal(t, h, watermarks(t, ri)["mixed"], "the learn side bookmarks the head of the activating advance")
	rows := dueFiresOf(t, ri, "mixed")
	require.ElementsMatch(t, []string{"kb/tasks/old-a.md", "kb/tasks/old-b.md"}, pathsOf(rows), "the overdue set fires once")
	require.Empty(t, firesOf(t, ri, "plain"))
	for _, f := range firesOf(t, ri, "mixed") {
		require.Equal(t, "due", f.Episode, "no learn back-fill: %+v", f)
	}
	kickAndWait(t, ri)
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "mixed"), 2, "and only once")
}

// ---- T16 Nothing else happens on expiry.

// NothingElseHappensOnExpiry: after a due fire the fact is still in the tree
// at the head (no commit was made), still returned by the reads, still live in
// the join. The allowlists are the enforcement; this is the documentary check.
func TestDue_NothingElseHappensOnExpiry(t *testing.T) {
	_, ri, clock := newDueRepo(t, trig("due", "due", "", ""))
	h := writeDatedAndWait(t, ri, "kb/tasks/x.md", clock.now().Add(time.Hour))
	clock.add(2 * time.Hour)
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 1)
	require.Equal(t, h, head(t, ri), "no commit was made")
	svc := testService(t, ri)
	_, ok, err := svc.Triggers().BlobAt(context.Background(), plumbing.NewHash(h), "kb/tasks/x.md")
	require.NoError(t, err)
	require.True(t, ok, "the fact is still in the tree")
	got, err := svc.FactQuery().GetByPath(context.Background(), trigAgent, "kb/tasks/x.md")
	require.NoError(t, err)
	require.NotEmpty(t, got.Expires, "still readable, still dated")
	require.Contains(t, dueCandidates(t, ri, trigAgent, farFuture), "kb/tasks/x.md", "still live in the join")
}

// ---- T17, T18 What is never a candidate.

// PrivatePathNeverCandidate: a dated file under a private (dot) path, written
// straight to the tree, is not indexed and so never in the join; a `due`
// trigger fires nothing for it. Sabotage: let isFactPath admit private paths.
func TestDue_PrivatePathNeverCandidate(t *testing.T) {
	_, ri, clock := newDueRepo(t, trig("due", "due", "", ""))
	svc := testService(t, ri)
	past := clock.now().Add(-time.Hour).Format(time.RFC3339)
	h, err := svc.TestingCommitFiles(trigAgent, map[string]string{
		"kb/tasks/.private/x.md": datedBody("private", past),
		"kb/tasks/visible.md":    datedBody("visible", past),
	}, "direct")
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().SyncLocked(context.Background(), trigAgent))
	cands := dueCandidates(t, ri, trigAgent, farFuture)
	require.NotContains(t, cands, "kb/tasks/.private/x.md")
	require.Contains(t, cands, "kb/tasks/visible.md", "fixture: the sibling IS indexed")
	require.Equal(t, h, head(t, ri))
	kickAndWait(t, ri) // TestingCommitFiles skips the observer: a tick brings the run
	require.Equal(t, []string{"kb/tasks/visible.md"}, pathsOf(dueFiresOf(t, ri, "due")))
}

// NotOnReadOnlySubscribedOrExp: a dated fact committed to exp/x is a
// candidate on exp/x's own view and NOT on the agent branch's, so it never
// fires; a read-only server has no dispatcher at all (the join exists, nothing
// sweeps it). Sabotage: drop the branch predicate from the join.
func TestDue_NotOnReadOnlySubscribedOrExp(t *testing.T) {
	_, ri, clock := newDueRepo(t, trig("due", "due", "", ""))
	svc := testService(t, ri)
	ctx := context.Background()
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", trigAgent)
	require.NoError(t, err)
	writeDated(t, ri, "exp/e", "kb/tasks/in-exp.md", clock.now().Add(-time.Hour))
	require.Contains(t, dueCandidates(t, ri, "exp/e", farFuture), "kb/tasks/in-exp.md", "fixture: due on exp/e's own view")
	require.NotContains(t, dueCandidates(t, ri, trigAgent, farFuture), "kb/tasks/in-exp.md")
	kickAndWait(t, ri)
	require.Empty(t, dueFiresOf(t, ri, "due"))

	home := t.TempDir()
	ro := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: home, OntologyRoot: "kb", ReadOnly: true},
		AgentBranch:           trigAgent,
		KeyPath:               filepath.Join(home, "agent.key"),
		DisableBackgroundSync: true,
	})
	t.Cleanup(func() { _ = ro.Close() })
	rri := bootRepo(t, ro)
	require.Nil(t, rri.triggers, "a read-only server runs no dispatcher, so nothing sweeps")
	writeDated(t, rri, trigAgent, "kb/tasks/overdue.md", clock.now().Add(-time.Hour))
	require.Contains(t, dueCandidates(t, rri, trigAgent, farFuture), "kb/tasks/overdue.md", "the join exists; no one reads it")
	require.Empty(t, fires(t, rri))
}

// ---- T19 The change context of a due fire.

// ChangeContext: `change.episode === 'due'`, `source === 'due'`, `before ===
// null`, `commit` is the commit that introduced the fact's content (not the
// head when a later unrelated commit exists), `trace` is that commit's
// Knomit-Trace trailer, `author.verified` is false with verification off;
// the log row has range_from == range_to == the head the run read. The `if`
// is the probe: it is true only when every field is as stated, and the fire
// row carries what the emit saw. Sabotage: commit = head; source = local.
func TestDue_ChangeContext(t *testing.T) {
	m := newTestManager(t)
	ri := bootRepo(t, m)
	clock := newFakeClock()
	dueHooks(t, clock, triggerHooks{})
	svc := testService(t, ri)
	due := clock.now().Add(time.Hour)
	r, err := svc.Facts().WriteFact(context.Background(), trigAgent, "kb/tasks/x.md",
		datedBody("x", due.Format(time.RFC3339)), "learn x\n\nKnomit-Trace: trace-42\n", "learn")
	require.NoError(t, err)
	hA := r.CommitHash
	writeOn(t, ri, trigAgent, "kb/other/unrelated.md") // a later, unrelated commit
	cond := fmt.Sprintf("change.episode === 'due' && change.source === 'due' && change.before === null && "+
		"change.path === 'kb/tasks/x.md' && change.commit === '%s' && change.trace === 'trace-42' && "+
		"change.author.verified === false && fact.expires === '%s'", hA, due.Format(time.RFC3339))
	setOntology(t, ri, triggerOntology("", trig("probe", "due", "", cond), trig("any", "due", "", "")))
	clock.add(2 * time.Hour)
	kickAndWait(t, ri)
	h := head(t, ri)

	probe := dueFiresOf(t, ri, "probe")
	require.Len(t, probe, 1, "the `if` over the change context must be true: %+v", firesOf(t, ri, "any"))
	require.Equal(t, hA, probe[0].Commit)
	require.Equal(t, "trace-42", probe[0].Trace)
	require.Equal(t, "due", probe[0].Source)
	require.Equal(t, h, probe[0].RangeFrom)
	require.Equal(t, h, probe[0].RangeTo)
	require.False(t, probe[0].Nonlinear)
	require.EqualValues(t, 1, ri.triggers.stats.view("probe").Fires)
	require.EqualValues(t, 0, ri.triggers.stats.view("probe").IfFalse)
}

// ---- T21 [M1] Processed and unmatched candidates cost nothing.

// MarkedCandidatesCostNothing: 60 due facts, all processed by an earlier run;
// a new run with nothing new reads ZERO blobs, writes NO run row and leaves
// runs.last unchanged; the same for 60 due facts that no `due` trigger's glob
// matches. Sabotage: confirm at the head before the glob/mark filters → blob
// reads = N; key the run row on candidates instead of evaluations → a run
// row appears.
func TestDue_MarkedCandidatesCostNothing(t *testing.T) {
	const n = 60
	_, ri, clock := newDueRepo(t, trig("due", "due", "tasks/**", ""))
	past := clock.now().Add(-time.Hour)
	var last string
	for i := 0; i < n; i++ {
		writeDated(t, ri, trigAgent, fmt.Sprintf("kb/tasks/p%03d.md", i), past)
		last = writeDated(t, ri, trigAgent, fmt.Sprintf("kb/other/u%03d.md", i), past) // no due trigger matches `other`
	}
	waitTriggerHead(t, ri, last)
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), n, "fixture: every matching fact was processed")
	require.Len(t, dueMarks(t, ri), n)

	var blobReads atomic.Int64
	dueHooks(t, clock, triggerHooks{trees: func(inner store.TriggerTrees) store.TriggerTrees {
		return countingTrees{inner: inner, reads: &blobReads}
	}})
	before := ri.triggers.stats.runView()
	runsBefore := len(runRows(t, ri))
	kickAndWait(t, ri)
	kickAndWait(t, ri)
	require.Zero(t, blobReads.Load(), "processed and unmatched candidates never reach the tree")
	require.Len(t, runRows(t, ri), runsBefore, "a run that evaluated nothing writes no run row")
	require.Equal(t, before, ri.triggers.stats.runView(), "runs.last unchanged")
	require.Len(t, dueFiresOf(t, ri, "due"), n)
}

// countingTrees counts BlobAt calls through a TriggerTrees.
type countingTrees struct {
	inner store.TriggerTrees
	reads *atomic.Int64
}

func (c countingTrees) Toucher(ctx context.Context, head plumbing.Hash, path string) (store.TouchResult, error) {
	return c.inner.Toucher(ctx, head, path)
}

func (c countingTrees) BlobAt(ctx context.Context, commit plumbing.Hash, path string) (string, bool, error) {
	c.reads.Add(1)
	return c.inner.BlobAt(ctx, commit, path)
}

// ---- T22 [M2] An `if`-false due evaluation is marked too.

// IfFalseEvaluatedOnce: an `if: false` due trigger and a due fact; three runs;
// ONE evaluation, one if_false, no fire row, one mark. Sabotage: write a mark
// only on `emitted` → 3 evaluations.
func TestDue_IfFalseEvaluatedOnce(t *testing.T) {
	_, ri, clock := newDueRepo(t, trig("never", "due", "", "false"))
	writeDatedAndWait(t, ri, "kb/tasks/x.md", clock.now().Add(-time.Hour))
	kickAndWait(t, ri)
	kickAndWait(t, ri)
	st := ri.triggers.stats.view("never")
	require.EqualValues(t, 1, st.Evaluations, "processed once, whatever `if` said")
	require.EqualValues(t, 1, st.IfFalse)
	require.EqualValues(t, 0, st.Fires)
	require.Empty(t, firesOf(t, ri, "never"))
	require.Len(t, dueMarks(t, ri), 1)
}

// ---- T23 [M6] A rebuild does not re-fire.

// RebuildDoesNotRefire: fire; rebuild the branch's index; run; no fire, the
// marks unchanged. Sabotage: DELETE FROM trigger_due_fires (or INSERT OR
// REPLACE the marks) in rebuildFacts.
func TestDue_RebuildDoesNotRefire(t *testing.T) {
	_, ri, clock := newDueRepo(t, trig("due", "due", "", ""))
	writeDatedAndWait(t, ri, "kb/tasks/x.md", clock.now().Add(time.Hour))
	clock.add(2 * time.Hour)
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 1)
	marks := dueMarks(t, ri)
	require.Len(t, marks, 1)

	require.NoError(t, testService(t, ri).IndexManager().Rebuild(context.Background(), trigAgent, nil))
	require.Contains(t, dueCandidates(t, ri, trigAgent, farFuture), "kb/tasks/x.md", "fixture: the rebuild kept the fact live")
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 1, "a rebuild must not re-arm a processed fact")
	require.Equal(t, marks, dueMarks(t, ri), "the marks are untouched by the rebuild")
}

// ---- T25 [M8] A removed trigger forgets its marks.

// RemovedTriggerForgetsMarks (twin of RemovedTriggerForgets): fire under x;
// remove x from the ontology: its bookmark AND its marks are gone; re-add x:
// a new trigger, so the still-overdue fact fires once more (D2 (a)).
// Sabotage: keep the marks when the name is absent → no second fire.
func TestDue_RemovedTriggerForgetsMarks(t *testing.T) {
	_, ri, clock := newDueRepo(t, trig("x", "due", "", ""))
	writeDatedAndWait(t, ri, "kb/tasks/x.md", clock.now().Add(-time.Hour))
	require.Len(t, dueFiresOf(t, ri, "x"), 1)
	require.Len(t, dueMarks(t, ri), 1)

	setOntology(t, ri, triggerOntology(""))
	require.NotContains(t, watermarks(t, ri), "x")
	require.Empty(t, dueMarks(t, ri), "the name is gone: its marks go with its bookmark")

	setOntology(t, ri, triggerOntology("", trig("x", "due", "", "")))
	require.Len(t, dueFiresOf(t, ri, "x"), 2, "a re-added name is a new trigger: the overdue fact fires once more")
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "x"), 2)
}

// ---- T26 [N4] SwapStore (documentary).

// SurvivesSwapStore: after a swap to a store that holds no marks (a copy taken
// before the fire), the dispatcher is alive and the overdue fact fires once
// more — the stated at-least-once consequence of the marks being local cache.
func TestDue_SurvivesSwapStore(t *testing.T) {
	m, ri, clock := newDueRepo(t, trig("due", "due", "", ""))
	writeDatedAndWait(t, ri, "kb/tasks/x.md", clock.now().Add(time.Hour))
	require.NoError(t, ri.WithRead(func(svc *store.Service) { require.NoError(t, svc.Checkpoint()) }))
	tmp := filepath.Join(t.TempDir(), "copy.db")
	copyDB(t, m.RepoPath(ri.UID()), tmp) // no marks in this copy

	clock.add(2 * time.Hour)
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 1)
	require.NoError(t, m.SwapStore(ri, tmp))
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 1, "the swapped-in store holds no fire log rows of its own; the fact fires once more into it")
	require.Len(t, dueMarks(t, ri), 1, "and is marked in the new store")
	write(t, ri, "kb/tasks/after.md")
	kickAndWait(t, ri)
	require.Len(t, dueFiresOf(t, ri, "due"), 1, "the dispatcher is alive and does not re-fire")
}

// ---- T27 All times are UTC.

// UTCEverywhere (maintainer ruling 2026-09-28: "all times MUST BE UTC -
// expire, due, everything"): with the process's local zone set to
// America/New_York, an `expires` written with an offset is stored as the same
// instant with an explicit Z, the index holds that instant, the mark's
// fired_at is the run's UTC clock, and every fire-log timestamp the endpoint
// renders is RFC 3339 with a Z. The same scenario under UTC yields identical
// stored values. Sabotage: format with the local zone anywhere (Format without
// .UTC()); store `expires` as written.
func TestDue_UTCEverywhere(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	type observed struct {
		stored, indexed, mark string
	}
	got := map[string]observed{}
	fixed := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for name, loc := range map[string]*time.Location{"utc": time.UTC, "new-york": ny} {
		t.Run(name, func(t *testing.T) {
			orig := time.Local
			time.Local = loc
			t.Cleanup(func() { time.Local = orig })

			_, ri, clock := newDueRepo(t, trig("due", "due", "", ""))
			clock.set(fixed)
			due := fixed.Add(time.Hour)
			// Written with a +02:00 offset, as an agent in Europe might, through
			// the write gate every MCP/REST write passes (SerializeFact).
			offset := due.In(time.FixedZone("x", 2*3600)).Format(time.RFC3339)
			require.Contains(t, offset, "+02:00")
			f, err := knomitfact.ParseFact("kb/tasks/x.md", datedBody("x", offset))
			require.NoError(t, err)
			require.Equal(t, offset, f.Expires, "fixture: the parse keeps the offset; the WRITE normalises")
			content, err := knomitfact.SerializeFact(f)
			require.NoError(t, err)
			r, err := testService(t, ri).Facts().WriteFact(context.Background(), trigAgent, "kb/tasks/x.md", content, "learn", "learn")
			require.NoError(t, err)
			waitTriggerHead(t, ri, r.CommitHash)
			read, err := testService(t, ri).Facts().ReadFact(context.Background(), trigAgent, "kb/tasks/x.md", nil)
			require.NoError(t, err)
			require.Contains(t, read.Content, "expires: \""+due.UTC().Format(time.RFC3339)+"\"", "stored in UTC with a Z")
			require.NotContains(t, read.Content, "+02:00")
			indexed := dueCandidates(t, ri, trigAgent, farFuture)["kb/tasks/x.md"]
			require.Equal(t, due.Unix(), indexed)

			clock.add(2 * time.Hour)
			kickAndWait(t, ri)
			require.Len(t, dueFiresOf(t, ri, "due"), 1)
			rep, err := ri.TriggerReport(context.Background(), 50)
			require.NoError(t, err)
			require.NotEmpty(t, rep.Fires)
			for _, f := range rep.Fires {
				require.True(t, strings.HasSuffix(f.FiredAt, "Z"), "fired_at %q must carry an explicit Z", f.FiredAt)
				_, err := time.Parse(time.RFC3339, f.FiredAt)
				require.NoError(t, err)
			}
			mark := dueMarks(t, ri)[store.DueKey{Trigger: "due", Path: "kb/tasks/x.md"}]
			got[name] = observed{
				stored:  due.UTC().Format(time.RFC3339),
				indexed: fmt.Sprint(indexed),
				mark:    fmt.Sprint(mark),
			}
		})
	}
	require.Equal(t, got["utc"], got["new-york"], "the local zone must not leak into any stored value")
}
