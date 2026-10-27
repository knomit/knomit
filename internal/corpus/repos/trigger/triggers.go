// The trigger dispatcher (F07 PR 1b). One actor per writable repo that turns
// advances of THIS machine's agent branch into trigger fires.
//
// The write path does no trigger work. The commit observer callback
// (ri.onCommit, builder.go) runs under the writer's branch lock and only does
// a non-blocking send on a 1-slot channel — the kick — when the branch is the
// agent branch. The channel IS the pending flag: a second kick while one is
// pending merges into it, and the send never blocks. The dispatcher is
// level-triggered: whatever the kicks, a run processes "watermark → current
// head", so coalesced advances are diffed once and nothing is lost.
//
// A run has three phases, and the store is held (Acquire) only in the first
// and the last:
//
//	A  (Acquire) read the head; the ontology at the head → the compiled trigger
//	   set (cached by blob); the watermarks; ONE tree diff per distinct
//	   watermark; glob matching in memory; `change` built ONCE per matched
//	   path (the toucher walk, the signer, verified, the trailer).
//	   release
//	B  (no store) per (trigger, path): `if` then the action (emit, script,
//	   push — one non-blocking send on ri.syncWake — or run, which resolves
//	   the recipe and STARTS it beside the dispatcher, never waiting: see
//	   trigger_recipe.go), ctx checked per path;
//	   timed as the trigger's own work for the statistics and the slow
//	   detector.
//	C  (Acquire) tx1: the buffered runs' fire rows + ONE run row per run;
//	   tx2: watermarks, due marks and the prune. release. Phase C is
//	   WRITE-BEHIND: a run buffers it and it is flushed when no kick is
//	   pending (the writer is quiet), when the buffer reaches its bound, or at
//	   shutdown — see pendingFlush for the measurement that forced this.
//
// So a long `if` never holds a store reference (SwapStore and teardown do not
// drain behind it), and the dispatcher's SQLite writes take the process-wide
// write lock at most twice per flush, never across user code, and never while
// the writer is mid-burst.
//
// `on: due` (F07 PR 2) is a second source of fires inside the SAME run: phase
// A also sweeps the dated facts live at the head whose instant has passed
// (sweepDue), so a due fire shares the phases, the buffer, the statistics and
// the slow detector with the advances. The sweep has no timer of its own: the
// reconcile tick kicks the dispatcher (sync.go), and every run — tick-kicked or
// write-kicked — sweeps. The run's clock is read ONCE, as UTC; it is the one
// clock comparison in F07.
//
// The dispatcher runs under the Serve stage's life, not Sync's: an origin
// attach or detach restarts Sync and must not touch the dispatcher. The due
// sweep is gated on the index being ready (indexReady), and the index job's
// success kicks the dispatcher.
package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"knomit/internal/corpus/repos/events"
	"knomit/internal/engine/store/model"

	"knomit/internal/engine/store/verify"

	"github.com/dop251/goja"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/facts"
	"knomit/internal/platform/crashdump"
)

// maxTriggerLogField caps repo content (trigger names, paths) in the slow
// trigger WARN, the same bound the slow-request log applies to client fields.
const maxTriggerLogField = 256

// TestHooks are test seams. Every field is nil (or zero) in production; a
// test installs them with SetTestHooksForTest (testseams.go), which refuses
// to run outside a test binary.
type TestHooks struct {
	// AfterKick runs between receiving a kick and reading the head ref. It is
	// how a test parks the worker.
	AfterKick func()
	// AfterHeadRead runs right after phase A read the head ref: a commit made
	// here is NOT in this run's range, and its kick must survive the run
	// (KickReceivedBeforeRefRead). A dispatcher that drained the kick channel
	// after reading the ref would swallow it.
	AfterHeadRead func()
	// BeforeTx2 runs between tx1 and tx2 (the crash test panics here).
	BeforeTx2 func()
	// InChange runs inside the change build: knomit's per-path cost, which
	// must not be charged to the trigger. A test advances its Stopwatch here.
	InChange func()
	// Stopwatch is the clock the dispatcher measures DURATIONS by: the run,
	// its diff, the change build, a trigger's own work, a script's host
	// calls and a recipe run's row. nil means time.Now. A test that asserts which side of a split a
	// cost lands on (the trigger's own time against change_ms or host_ms, the
	// slow detector, the p95) installs a clock that moves only when the test
	// moves it, so the assertion is exact whatever the CPU load.
	Stopwatch func() time.Time
	// Now is the run's clock — read ONCE per run, the one clock comparison in
	// F07 (the due sweep's `expires_at <= now`). nil means time.Now().UTC().
	Now func() time.Time
	// DueCandidates lets a test append STALE candidates (a path the index no
	// longer lists) to what the liveness join returned, so the head
	// confirmation is exercised on its own.
	DueCandidates func([]facts.DueCandidate) []facts.DueCandidate
	// Trees wraps the run's tree reader (a test counts blob reads).
	Trees func(facts.TriggerTrees) facts.TriggerTrees
	// ScriptBudget replaces fact.ScriptEvalTimeout (5 s) for a script fire.
	ScriptBudget time.Duration
	// EvalDelay is slept INSIDE the trigger's timed work (before its `if`),
	// per (trigger, path): a trigger that is slow in real wall time whatever
	// the CPU load. The sandbox's `Date.now()` is the run's pinned clock, so
	// an `if` can no longer spin on the wall clock; a spin counted in
	// iterations shrinks under contention and made the detector tests flaky.
	EvalDelay func(trigger, path string) time.Duration
	// FlushGrace, when non-zero, replaces triggerFlushGrace in settle: a long
	// value holds phase C's buffer pending, so every later run meets it and
	// only overlayPending keeps a range from re-firing.
	FlushGrace time.Duration
	// BeforeFlush runs once per flush of phase C's buffer, before its first
	// transaction, with the number of runs the buffer holds: how a test
	// counts flushes against runs (#419: phase C is write-behind, so a burst
	// of runs is one flush, never one per run). It also fires, with 0, for a
	// flush that carries no run rows (only watermarks, due marks or recipe
	// results).
	BeforeFlush func(runs int)
	// SkipFlushOnStop tears the loop down WITHOUT the final flush: a crash
	// as far as the tables are concerned (T20 pins the M2 window with it).
	SkipFlushOnStop bool
}

var (
	triggerHooksMu   sync.Mutex
	triggerTestHooks TestHooks
)

func currentTriggerHooks() TestHooks {
	triggerHooksMu.Lock()
	defer triggerHooksMu.Unlock()
	return triggerTestHooks
}

// Dispatcher is one repo's dispatcher. The repos parent builds it (New) for a
// writable repo with an agent branch, holds it in RepoInstance.triggers, and
// runs it under the Serve stage's life (Run).
type Dispatcher struct {
	repo     string
	branch   string // the agent branch: the only branch that kicks
	identity fact.TriggerIdentity
	slow     time.Duration
	kick     chan struct{}
	cache    fact.TriggerCache
	stats    *triggerStats
	// ontologyRoot is the repo's ontology root (fixed when the instance is
	// built): a fire's root-relative path is the store path under it.
	ontologyRoot string
	// pub publishes the SSE `trigger` event (emit). The repos parent owns the
	// publisher and hands it in here; it is never handed out again.
	pub *events.TaskPublisher
	// acquire is the repo's store lease (RepoInstance.Acquire): every phase
	// that reads or writes the store holds it, and only for that phase.
	acquire func() (*store.Service, func(), error)
	// syncWake is the push action's one non-blocking send on the repo's sync
	// wake (RepoInstance.wakeSync).
	syncWake func()
	// bind puts the repo's binding for the agent branch on a script or
	// recipe host's ctx (repos.WithBinding of NewBindingOfRepo), which the
	// injected tools resolve their branch from.
	bind func(context.Context) context.Context
	// indexReady gates the `on: due` sweep, which reads index tables
	// (branch_facts ⋈ fact_expires): while the repo's index is not ready the
	// sweep is skipped, and the index job's success kicks the dispatcher so it
	// runs then. Bound to the machine's status; nil reads as ready.
	indexReady func() bool

	mu       sync.Mutex
	lastSet  *fact.TriggerSet
	lastHead string
	lastBlob string
	runSeq   int64
	// completedHead is the head the last COMPLETED run read (set when run
	// returns, whatever it did), so a test can wait for "the run for head H
	// is over" rather than sleep.
	completedHead string
	// errLogged holds the name@blob keys already reported at ERROR, so a
	// broken trigger is logged once per blob and not on every advance. Reset
	// when the blob changes (the keys carry the blob, so old ones are dead).
	errLogged map[string]bool
	errBlob   string
	// ontErr is the blob (or head, when there is no ontology) whose failure
	// was already logged.
	ontErr string
	// verified caches the upstream tip's ancestor set (what F09 counts as
	// verified after PR 5), keyed by the tip.
	verified struct {
		tip plumbing.Hash
		set map[plumbing.Hash]bool
	}
	// pending is phase C's write-behind buffer (see pendingFlush).
	pending pendingFlush
	// verifyBlob/verifyOn memoise the F09 mode of the ontology blob at the head.
	verifyBlob string
	verifyOn   bool
	// sc is the `do: script` state (trigger_script.go): the script cache, the
	// injected tools, the rate cap and the WARN-once keys.
	sc scriptState
	// home is this machine's knomit home: <home>/recipes holds the local
	// recipes, and a recipe's child gets it as KNOMIT_HOME.
	home string
	// serverAddr is this server's own address (Manager.ServerAddress, ""
	// until the listeners are bound); a recipe's child gets it as
	// KNOMIT_SERVER. KNOMIT_HOME does not choose the server — this does.
	serverAddr func() string
	// rc is the `do: run` state (trigger_recipe.go): the recipe caches, the
	// concurrency slots and the running recipes' wait group.
	rc recipeState
	// runCtx is the loop's ctx (set by run): every recipe derives from it, so
	// the Serve stage's exit interrupts them and kills their processes.
	runCtx context.Context
	// lateIn is the inbox of recipe RESULT rows, filled by runner goroutines
	// (addLate) and drained into pending.late by the dispatcher goroutine only
	// (drainLate) [M1]. Behind lateMu, not mu.
	lateMu sync.Mutex
	lateIn []facts.TriggerFire
}

// New builds a repo's dispatcher WITHOUT starting it, so the kick slot exists
// from the instance's construction (a write during the index job kicks it)
// while Run starts under the Serve stage's life. Its due sweep is gated on the
// repo's index being ready (indexReady).
// ratePerMinute is [triggers].script_rate_per_minute; tools the injected
// in-process MCP tool set of `do: script` (nil on a build with none); home
// the knomit home whose recipes/ folder holds this machine's recipes.
//
// identity is this instance's {agent} and {host}, from its PERSISTED
// identity (instance_identity.agent_id and the host label stored with it),
// handed in by the Manager. Nothing here derives either from the agent
// branch's name (D18): a hostname that happens to end in "-<8 hex>" is not
// trimmed, and an opaque id is not split.
//
// The last four parameters are the dispatcher's only ways back into its repo
// instance, each a func the repos parent binds to that instance: acquire is
// the store lease, syncWake the push action's wake of the sync loop,
// indexReady the due sweep's gate, and bind the binding a script's or a
// recipe's injected tools resolve the agent branch from. pub is the repo's
// task publisher (the `trigger` SSE event), owned by the parent and kept here
// unexported.
//
// The second result is the dispatcher's run loop, its lifetime under the
// Serve stage's life. It is a capability, not a method: the parent keeps it
// in an unexported field and is the only one that starts it, because a
// second loop on one dispatcher would consume the same kicks and fire the
// same watermark→head range twice. Callers holding the *Dispatcher
// (RepoInstance.Triggers) can kick it and read its report, nothing more.
// Only the repos parent may call New (test/archtest
// TestTriggerNew_OnlyFromRepos).
func New(repo, agentBranch, ontologyRoot string, identity fact.TriggerIdentity, slowMS, ratePerMinute int,
	tools ScriptTools, home string, serverAddr func() string, pub *events.TaskPublisher,
	acquire func() (*store.Service, func(), error), syncWake func(), indexReady func() bool,
	bind func(context.Context) context.Context) (*Dispatcher, func(context.Context)) {
	if serverAddr == nil {
		serverAddr = func() string { return "" }
	}
	d := &Dispatcher{
		repo:         repo,
		branch:       agentBranch,
		ontologyRoot: ontologyRoot,
		identity:     identity,
		pub:          pub,
		acquire:      acquire,
		syncWake:     syncWake,
		bind:         bind,
		indexReady:   indexReady,
		slow:         time.Duration(slowMS) * time.Millisecond,
		kick:         make(chan struct{}, 1),
		stats:        newTriggerStats(),
		sc:           newScriptState(tools, ratePerMinute),
		home:         home,
		serverAddr:   serverAddr,
		rc:           newRecipeState(),
	}
	return d, d.run
}

// indexIsReady reports whether the due sweep may read the index now.
func (d *Dispatcher) indexIsReady() bool {
	return d.indexReady == nil || d.indexReady()
}

// Kick is the observer's whole job for triggers: record "the agent branch
// advanced" and return. O(1), never blocks, never allocates. A nil
// dispatcher (a read-only server, a subscription: the repo has none) ignores
// it, so ri.onCommit and the other kick sources call it without a check.
func (d *Dispatcher) Kick() {
	if d == nil {
		return
	}
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// wakeSync is how phase B and the script host reach the push action: ask
// this machine's sync loop for a round now (F07 PR 4), a non-blocking send
// that never waits and carries nothing.
func (d *Dispatcher) wakeSync() { d.syncWake() }

// run is the dispatcher's lifetime, under the Serve stage's life. It kicks
// itself once so advances made while the repo was not served fire now (the
// watermark makes that at-least-once), then receives kicks until ctx ends.
// The kick is RECEIVED before the head ref is read (inside runOnce): the
// reverse order would swallow the kick of a commit that lands between the ref
// read and the drain, the lost-wake-up shape.
//
// On ctx's end it waits for every running recipe to be killed and to hand in
// its `stopped` row, then flushes what is buffered — while the store is still
// attached, because the Serve stage is drained before Open closes the store.
func (d *Dispatcher) run(ctx context.Context) {
	d.runCtx = ctx
	d.Kick()
	for {
		select {
		case <-ctx.Done():
			// The recipes derive from ctx: each is interrupted, its process
			// group or job killed, and it ends within recipeWaitDelay of the
			// kill. Wait for them FIRST, so their `stopped` rows reach the
			// late inbox before the final flush reads it.
			d.rc.wg.Wait()
			if currentTriggerHooks().SkipFlushOnStop {
				return
			}
			d.flushOnStop()
			return
		case <-d.kick:
			d.safeRun(ctx)
			d.settle(ctx)
		}
	}
}

// triggerFlushGrace is how long the writer must be silent before phase C is
// flushed. A run usually finishes while the NEXT write is still in flight
// (its kick is not pending yet), so "no kick pending" alone still put the
// flush on top of that write. Waiting for a short silence puts it in the gap
// after a burst instead. Emits are not delayed by this: they happened in the
// run. Only the tables lag, by at most this long plus the flush.
const triggerFlushGrace = 20 * time.Millisecond

// settle waits for the writer to go quiet, then flushes. A kick that arrives
// meanwhile is consumed and its run happens right away; the buffer keeps
// growing until a gap (or a bound, see maybeFlush) flushes it.
func (d *Dispatcher) settle(ctx context.Context) {
	for {
		d.mu.Lock()
		empty := d.pending.empty()
		d.mu.Unlock()
		if empty {
			return
		}
		grace := triggerFlushGrace
		if g := currentTriggerHooks().FlushGrace; g > 0 {
			grace = g
		}
		timer := time.NewTimer(grace)
		select {
		case <-ctx.Done():
			timer.Stop()
			return // loop's ctx.Done flushes on stop
		case <-d.kick:
			timer.Stop()
			d.safeRun(ctx)
		case <-timer.C:
			d.flush(ctx)
			return
		}
	}
}

// safeRun is one run with panic recovery: a panic (or a test hook that panics
// between tx1 and tx2) must not take the actor down; the range simply re-fires
// on the next run or after a restart, which is the at-least-once contract.
func (d *Dispatcher) safeRun(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			crashdump.ReportRecovered("triggers:"+d.repo, r)
			log.Error().Str("repo", d.repo).Str("branch", d.branch).Interface("panic", r).
				Msg("trigger dispatcher: run panicked; the range will be re-run")
			// Whatever was buffered is suspect: the tables are the truth
			// again, and the unflushed range re-fires.
			d.resetPending()
		}
		d.mu.Lock()
		d.runSeq++
		d.mu.Unlock()
	}()
	if h := currentTriggerHooks().AfterKick; h != nil {
		h()
	}
	d.runOnce(ctx)
}

// pendingFire is one (trigger, path) evaluation phase A prepared for phase B.
type pendingFire struct {
	trig      *fact.CompiledTrigger
	repoPath  string
	episode   string
	nonlinear bool
	commit    string
	source    string
	// trace is the fire's DERIVED trace (deriveTrace): the toucher's
	// Knomit-Trace, else the task id, else the commit — never empty.
	trace string
	// byTrigger is the toucher's Knomit-Trigger: the trigger whose script
	// made the commit, "" for any other write. The loop guard compares it to
	// the trigger being fired (tree episodes only).
	byTrigger string
	change    map[string]any
	factMap   map[string]any // nil when the fact is absent or unparseable
	parseable bool
}

// runState is everything one run accumulates across the phases.
type runState struct {
	svc       *store.Service // identity of the store generation phase A read; never dereferenced after release
	head      string
	newWM     map[string]string
	del       []string
	pending   []pendingFire
	paths     int
	nonlinear bool
	rangeFrom string
	diffMS    int64
	changeMS  int64
	started   time.Time
	// now is the run's clock (UTC), read once; the due sweep compares
	// expires_at against it at whole seconds and stamps the marks with it.
	now time.Time
	// dueMarks is one mark per due (trigger, path) EVALUATION this run
	// prepared — whatever `if` will say — written in phase C with the
	// watermarks. len(dueMarks) is the run's due evaluation count.
	dueMarks []facts.DueMark
	// dueRetry is the due (trigger, path) evaluations phase B DROPPED for
	// capacity — outcome rate-limited (the script cap) or busy (a `do: run`
	// recipe at its `concurrent` limit). Their marks are NOT buffered, so the
	// next sweep (the next tick or write) finds them unmarked and fires them
	// again (F08 M3). A due fire is a STATE re-derived every sweep, so this
	// is a re-evaluation, not a replay; learn/update/retract fires stay
	// dropped for good. A script error or timeout keeps its mark: it is
	// usually deterministic and would otherwise re-fire every tick for the
	// life of the fact.
	dueRetry map[facts.DueKey]bool
	// duePaths is the number of distinct due paths evaluated (into the run
	// row's paths next to the diff rows).
	duePaths int
}

// fireRange is the range a fire of this run belongs to, as its row will
// carry it (a run with no advance has range_from = range_to = head). A
// recipe's late result row copies it.
func (rs *runState) fireRange() (from, to string) {
	if rs.rangeFrom == "" {
		return rs.head, rs.head
	}
	return rs.rangeFrom, rs.head
}

// didWork reports whether the run evaluated anything: diff rows in its range
// or due evaluations. A run that did neither writes no run row and is not
// counted — an idle 30 s tick on a repo whose due facts are all processed must
// not put 2,880 rows a day into a 10,000-row log.
func (rs *runState) didWork() bool { return rs.paths > 0 || len(rs.dueMarks) > 0 }

// pendingFlush is phase C's write-behind buffer: the runs (with their fire
// rows) and the watermark moves that have been EVALUATED AND EMITTED but not
// yet written to the two tables.
//
// WHY A BUFFER. Phase C's two transactions take SQLite's process-wide write
// lock, the same lock every fact write needs several times per commit, and a
// writer that meets a held lock sleeps in the busy handler's millisecond
// steps. Flushing after every run therefore cost a busy writer 5–7 ms per
// write (measured: 10.2 → 17.1 ms median with 50 matching triggers), while
// the kick alone cost nothing. So the run buffers its phase C and flushes
// when the writer is QUIET — no kick pending — or when the buffer reaches its
// bound, or at shutdown. Under a burst the fire rows and watermarks of several
// runs land in ONE tx1 and ONE tx2 after the burst; a lone write flushes in
// its own run. Measured: 5.4 → 6.5 ms median.
//
// WHAT DOES NOT CHANGE. Emits still happen per run, immediately. Each run
// still produces exactly one run row. The stored watermark moves only after
// its rows are logged (tx1 before tx2). Phase A overlays the buffered
// watermarks on the stored ones, so a buffered advance is never diffed twice.
// A crash before the flush loses the buffered log rows and re-fires the range
// on restart — the at-least-once contract the proposal states for a crash
// before tx1 — and a failed or panicking flush drops the buffer for the same
// reason: what the tables say is then the truth, and the range re-fires.
type pendingFlush struct {
	svc  *store.Service // the store generation the buffer was built against
	runs []facts.TriggerRun
	rows int
	wm   map[string]string
	del  map[string]bool
	// due is the buffered due marks, keyed so a re-arm within one buffer keeps
	// the latest instant. They ride tx2 with the watermarks.
	due map[facts.DueKey]facts.DueMark
	// late is recipe RESULT rows (F07 PR 5), moved here from the late inbox
	// by the dispatcher goroutine. They carry no bookmark and are written by
	// RecordTriggerResults with no run row. empty() counts them, so a result
	// with no new commit is still flushed [M1]. A failed flush, a flush of
	// another store generation, or a crash loses them — the fire itself was
	// already logged as `started` (or re-fires, M2's window).
	late []facts.TriggerFire
}

// maxBufferedRuns bounds the write-behind buffer by run count; the fire rows
// are bounded by TriggerFireRetention. Either bound flushes regardless of
// pending kicks.
const maxBufferedRuns = 256

func (p *pendingFlush) empty() bool {
	return len(p.runs) == 0 && len(p.wm) == 0 && len(p.del) == 0 && len(p.due) == 0 && len(p.late) == 0
}

// clock is the run's one clock read: UTC (all times are UTC) at whole seconds
// — the granularity fact_expires.expires_at and F03's `expired` filter use.
func (d *Dispatcher) clock() time.Time {
	if h := currentTriggerHooks().Now; h != nil {
		return h().UTC().Truncate(time.Second)
	}
	return time.Now().UTC().Truncate(time.Second)
}

// stopwatch is the clock durations are measured by (TestHooks.Stopwatch).
func stopwatch() time.Time {
	if h := currentTriggerHooks().Stopwatch; h != nil {
		return h()
	}
	return time.Now()
}

func (d *Dispatcher) runOnce(ctx context.Context) {
	rs := &runState{newWM: map[string]string{}, started: stopwatch()}
	defer func() {
		d.mu.Lock()
		d.completedHead = rs.head
		d.mu.Unlock()
	}()

	// ---- Phase A: with the store.
	svc, release, err := d.acquire()
	if err != nil {
		return // closed or mid-swap: the next commit kicks again
	}
	rs.svc = svc
	rs.now = d.clock()
	// Recipe results that arrived since the last run join the buffer first:
	// phase A may find nothing to do and return early, but they are then
	// already buffered, and settle flushes them after the grace.
	d.drainLate(svc)
	ok := d.phaseA(ctx, svc, rs)
	release()
	if !ok {
		d.maybeFlush(ctx)
		return
	}

	// ---- Phase B: no store.
	rows, evaluated, fires, aborted := d.phaseB(ctx, rs)
	if aborted {
		return // ctx cancelled: nothing is recorded, the range re-fires
	}

	// ---- Phase C: buffered; written in at most two short transactions per
	// flush, when the writer is quiet.
	durationMS := stopwatch().Sub(rs.started).Milliseconds()
	if rs.rangeFrom == "" {
		rs.rangeFrom = rs.head // a run with no advance (a tick's sweep): range_from = range_to = head
	}
	paths := rs.paths + rs.duePaths
	if rs.didWork() {
		d.stats.recordRun(LastRun{
			RangeFrom: rs.rangeFrom, RangeTo: rs.head, Nonlinear: rs.nonlinear, Paths: paths,
			Evaluated: evaluated, Fires: fires, DurationMS: durationMS, DiffMS: rs.diffMS, ChangeMS: rs.changeMS,
		})
	}
	d.buffer(rs, facts.TriggerRun{
		Branch: d.branch, RangeFrom: rs.rangeFrom, RangeTo: rs.head, Nonlinear: rs.nonlinear,
		Paths: paths, Evaluated: evaluated, Fires: fires,
		DurationMS: durationMS, DiffMS: rs.diffMS, ChangeMS: rs.changeMS, Rows: rows,
	})
	d.maybeFlush(ctx)
}

// buffer merges one run's phase C into the write-behind buffer.
func (d *Dispatcher) buffer(rs *runState, run facts.TriggerRun) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := &d.pending
	if p.svc != rs.svc {
		// A different store generation (SwapStore): what was buffered
		// describes the old store. Drop it; the tables are the truth.
		*p = pendingFlush{}
	}
	p.svc = rs.svc
	if p.wm == nil {
		p.wm, p.del, p.due = map[string]string{}, map[string]bool{}, map[facts.DueKey]facts.DueMark{}
	}
	if rs.didWork() {
		p.runs = append(p.runs, run)
		p.rows += len(run.Rows)
	}
	for name, h := range rs.newWM {
		p.wm[name] = h
		delete(p.del, name)
	}
	for _, m := range rs.dueMarks {
		k := facts.DueKey{Trigger: m.Trigger, Path: m.Path}
		if rs.dueRetry[k] {
			continue // dropped for capacity: unmarked, retried next sweep (M3)
		}
		p.due[k] = m
	}
	for _, name := range rs.del {
		p.del[name] = true
		delete(p.wm, name)
		for k := range p.due {
			if k.Trigger == name {
				delete(p.due, k) // the name is gone: its marks go with its bookmark
			}
		}
	}
}

// maybeFlush flushes the buffer when it has reached a bound; otherwise the
// flush waits for the writer to go quiet (settle).
func (d *Dispatcher) maybeFlush(ctx context.Context) {
	d.mu.Lock()
	full := !d.pending.empty() &&
		(d.pending.rows >= facts.TriggerFireRetention || len(d.pending.runs) >= maxBufferedRuns)
	d.mu.Unlock()
	if full {
		d.flush(ctx)
	}
}

// flushed reports whether nothing is buffered (tests wait on it).
func (d *Dispatcher) flushed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pending.empty()
}

// flush writes the buffer: tx1 (the runs' fire rows and run rows), then tx2
// (the watermarks and the prune). Any failure — or a panic — drops the
// buffer: the stored tables are then the truth and the unflushed range
// re-fires (at-least-once).
func (d *Dispatcher) flush(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			crashdump.ReportRecovered("triggers:"+d.repo, r)
			log.Error().Str("repo", d.repo).Str("branch", d.branch).Interface("panic", r).
				Msg("trigger dispatcher: flush panicked; the range will be re-run")
			d.resetPending()
		}
	}()
	svc, release, err := d.acquire()
	if err != nil {
		return // closed or mid-swap: retried by the next run, or dropped on the swap
	}
	defer release()
	d.mu.Lock()
	p := d.pending
	if p.svc != svc {
		d.pending = pendingFlush{}
		d.mu.Unlock()
		return
	}
	runs := p.runs
	late := p.late
	wm := make(map[string]string, len(p.wm))
	for k, v := range p.wm {
		wm[k] = v
	}
	del := make([]string, 0, len(p.del))
	for n := range p.del {
		del = append(del, n)
	}
	sort.Strings(del)
	due := make([]facts.DueMark, 0, len(p.due))
	for _, m := range p.due {
		due = append(due, m)
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].Trigger != due[j].Trigger {
			return due[i].Trigger < due[j].Trigger
		}
		return due[i].Path < due[j].Path
	})
	d.mu.Unlock()

	if h := currentTriggerHooks().BeforeFlush; h != nil {
		h(len(runs))
	}
	tr := svc.FactsRepo()
	if len(runs) > 0 {
		if _, err := tr.RecordTriggerRuns(ctx, runs); err != nil {
			log.Warn().Err(err).Str("repo", d.repo).Msg("trigger dispatcher: fire log write failed; the range will be re-run")
			d.resetPending()
			return
		}
		d.mu.Lock()
		d.pending.runs, d.pending.rows = nil, 0 // durable now; a retry must not log them twice
		d.mu.Unlock()
	}
	if len(late) > 0 {
		if err := tr.RecordTriggerResults(ctx, late); err != nil {
			log.Warn().Err(err).Str("repo", d.repo).Msg("trigger dispatcher: recipe result rows lost (fire log write failed)")
			d.resetPending()
			return
		}
		d.mu.Lock()
		d.pending.late = nil
		d.mu.Unlock()
	}
	if h := currentTriggerHooks().BeforeTx2; h != nil {
		h()
	}
	if len(wm) > 0 || len(del) > 0 || len(due) > 0 {
		if err := tr.AdvanceTriggerWatermarks(ctx, d.branch, wm, del, due); err != nil {
			log.Warn().Err(err).Str("repo", d.repo).Msg("trigger dispatcher: watermark write failed; the range will be re-run")
			d.resetPending()
			return
		}
	}
	d.resetPending()
}

// resetPending drops the write-behind buffer.
func (d *Dispatcher) resetPending() {
	d.mu.Lock()
	d.pending = pendingFlush{}
	d.mu.Unlock()
}

// overlayPending applies the buffered watermark moves (and, when marks is not
// nil, the buffered due marks) on top of the stored ones, so a run reads the
// bookkeeping as it WILL be once flushed: it never diffs a buffered advance a
// second time and never re-evaluates a due fact whose mark is still in the
// buffer — a run kicked inside the 20 ms flush grace sees the mark. A buffer
// from another store generation is dropped first. Either map may be nil.
func (d *Dispatcher) overlayPending(svc *store.Service, wms map[string]string, marks map[facts.DueKey]int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pending.svc != svc && !d.pending.empty() {
		d.pending = pendingFlush{}
		return
	}
	if wms != nil {
		for n, h := range d.pending.wm {
			wms[n] = h
		}
	}
	if marks != nil {
		for k, m := range d.pending.due {
			marks[k] = m.ExpiresAt
		}
	}
	for n := range d.pending.del {
		delete(wms, n)
		for k := range marks {
			if k.Trigger == n {
				delete(marks, k)
			}
		}
	}
}

// flushOnStop writes what is buffered when the actor stops (a clean
// shutdown), so a restart does not re-fire runs this process already
// emitted. The store is still attached: shutdown stops the dispatcher before
// it closes the store. Recipe results still in the late inbox (the killed
// recipes' `stopped` rows) are drained into the buffer first.
func (d *Dispatcher) flushOnStop() {
	if svc, release, err := d.acquire(); err == nil {
		d.drainLate(svc)
		release()
	}
	d.mu.Lock()
	empty := d.pending.empty()
	d.mu.Unlock()
	if empty {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.flush(ctx)
}

// phaseA reads everything the run needs and prepares the pending fires. It
// returns false when there is nothing to do (or the run should stop).
func (d *Dispatcher) phaseA(ctx context.Context, svc *store.Service, rs *runState) bool {
	tr := svc.FactsRepo()
	headStr, err := svc.HeadCommit(ctx, d.branch)
	if err != nil {
		log.Warn().Err(err).Str("repo", d.repo).Str("branch", d.branch).Msg("trigger dispatcher: head unreadable")
		return false
	}
	head := plumbing.NewHash(headStr)
	rs.head = headStr
	if h := currentTriggerHooks().AfterHeadRead; h != nil {
		h()
	}

	// The trigger set at THIS head, cached by the ontology blob (D-b).
	_, blob, data, oerr := tr.OntologyAtCommit(ctx, head)
	var set *fact.TriggerSet
	if oerr != nil {
		d.logOntologyErrorOnce(headStr, oerr)
		set = d.lastCompiledSet()
	} else {
		set, err = d.cache.Get(blob, data, d.identity)
		if err != nil {
			d.logOntologyErrorOnce(blob, err) // last good set is returned alongside
		} else {
			d.clearOntologyError()
		}
	}
	if set == nil {
		return false
	}
	d.recordSet(set, headStr, blob)
	d.logInvalidOnce(set)
	d.loadScripts(ctx, tr, head, set)

	wms, err := tr.TriggerWatermarks(ctx, d.branch)
	if err != nil {
		log.Warn().Err(err).Str("repo", d.repo).Msg("trigger dispatcher: watermarks unreadable")
		return false
	}
	d.overlayPending(svc, wms, nil)

	// Per declared name: first appearance, freeze, unsupported, or advance.
	active := map[string]*fact.CompiledTrigger{}
	for _, ct := range set.Active {
		active[ct.Name] = ct
	}
	states := nameStates(set)
	byW := map[string][]*fact.CompiledTrigger{}
	for name := range set.Declared {
		wm, has := wms[name]
		switch states[name] {
		case fact.TriggerActive:
			if !has {
				rs.newWM[name] = headStr // first appearance: no back-fill
				continue
			}
			if wm == headStr {
				continue
			}
			byW[wm] = append(byW[wm], active[name])
		case fact.TriggerUnsupported:
			// No bookmark until a version acts on it: it starts at the head
			// of the advance where it becomes active, never back-filling.
			if has {
				rs.del = append(rs.del, name)
			}
		default: // invalid: frozen. A first appearance while invalid starts here.
			if !has {
				rs.newWM[name] = headStr
			}
		}
	}
	for name := range wms {
		if !set.Declared[name] {
			rs.del = append(rs.del, name) // the NAME is gone: forget it
		}
	}
	sort.Strings(rs.del)
	// The `on: due` triggers sweep on EVERY run (D4: tick-kicked or
	// write-kicked), first-appearance ones included — a due fact is a state,
	// so a trigger that just came alive processes the currently overdue set
	// once (D2 (a), maintainer ruling 2026-09-28).
	var dueTrigs []*fact.CompiledTrigger
	for _, ct := range set.Active {
		if states[ct.Name] == fact.TriggerActive && ct.OnEpisode(fact.TriggerOnDue) {
			dueTrigs = append(dueTrigs, ct)
		}
	}
	if len(byW) == 0 && len(dueTrigs) == 0 {
		return len(rs.newWM) > 0 || len(rs.del) > 0
	}

	// `verified` needs the mode at the head and the anchor's history. The
	// mode is read from the ontology blob, once per blob.
	verifyOn := d.verifyModeOn(blob, data, oerr == nil)
	var verifiedSet map[plumbing.Hash]bool
	if verifyOn {
		remote := svc.Remote()
		verifiedSet = d.verifiedBelow(ctx, tr, remote.UpstreamBranch())
	}
	instanceFP := tr.SignerFingerprint()
	root := d.ontologyRoot
	if root == "" {
		root = "kb"
	}
	// One tree reader and one commit-meta cache for the whole run: every
	// matched path of an advance reads the same few commits, and decoding a
	// tree (or verifying a signature) once per path instead of once per
	// commit is what made a large advance take minutes.
	trees := tr.TreeReader()
	if h := currentTriggerHooks().Trees; h != nil {
		trees = h(trees)
	}
	cr := &changeReader{trees: trees, tr: tr, meta: map[plumbing.Hash]commitMeta{}, instanceFP: instanceFP}

	if !d.advance(ctx, svc, tr, rs, cr, head, byW, root, verifyOn, verifiedSet) {
		return false
	}
	// The due sweep reads the index (branch_facts ⋈ fact_expires), so it waits
	// for the index to be ready; the index job's success kicks this
	// dispatcher. Due fires are late during indexing, never wrong: the
	// advance above reads git alone and runs regardless.
	if len(dueTrigs) > 0 && d.indexIsReady() && !d.sweepDue(ctx, tr, rs, cr, head, root, dueTrigs, verifyOn, verifiedSet) {
		return false
	}
	return len(byW) > 0 || len(rs.dueMarks) > 0 || len(rs.newWM) > 0 || len(rs.del) > 0
}

// advance is the tree-episode half of phase A: one diff per distinct
// watermark, glob matching, `change` built once per matched path. It returns
// false only when ctx is cancelled.
func (d *Dispatcher) advance(ctx context.Context, svc *store.Service, tr *facts.Repo, rs *runState, cr *changeReader, head plumbing.Hash,
	byW map[string][]*fact.CompiledTrigger, root string, verifyOn bool, verifiedSet map[plumbing.Hash]bool) bool {
	headStr := head.String()
	// Normally every trigger shares one watermark, so this is ONE diff.
	ws := make([]string, 0, len(byW))
	for w := range byW {
		ws = append(ws, w)
	}
	sort.Strings(ws)
	for _, w := range ws {
		trigs := byW[w]
		wh := plumbing.NewHash(w)
		linear, err := tr.IsAncestor(ctx, wh, head)
		if err != nil {
			// The watermark's commit is gone (garbage-collected): reset to the
			// head, fire nothing, say so.
			log.Warn().Err(err).Str("repo", d.repo).Str("watermark", w).
				Msg("trigger dispatcher: watermark commit unreadable; resetting to head")
			for _, ct := range trigs {
				rs.newWM[ct.Name] = headStr
			}
			continue
		}
		t0 := stopwatch()
		rows, err := svc.DiffFacts(ctx, wh, head)
		rs.diffMS += stopwatch().Sub(t0).Milliseconds()
		if err != nil {
			log.Warn().Err(err).Str("repo", d.repo).Msg("trigger dispatcher: diff failed; the range will be re-run")
			continue
		}
		if rs.rangeFrom == "" || w < rs.rangeFrom {
			rs.rangeFrom = w
		}
		if !linear {
			rs.nonlinear = true
		}
		rs.paths += len(rows)
		for _, row := range rows {
			if err := ctx.Err(); err != nil {
				return false
			}
			episode := episodeOf(row.Change)
			rel := strings.TrimPrefix(row.Path, root+"/")
			var matched []*fact.CompiledTrigger
			for _, ct := range trigs {
				if ct.OnEpisode(episode) && ct.Matches(rel) {
					matched = append(matched, ct)
				}
			}
			if len(matched) == 0 {
				continue
			}
			// change is built ONCE per matched path and shared by every
			// trigger that matched it; its cost is knomit's (change_ms), not
			// the trigger's.
			c0 := stopwatch()
			pf := d.buildChange(ctx, cr, head, wh, row.Path, episode, !linear, verifyOn, verifiedSet)
			rs.changeMS += stopwatch().Sub(c0).Milliseconds()
			for _, ct := range matched {
				// The loop guard (F07: "a write made by a trigger does not
				// re-fire the same trigger on the same path"), on the
				// commit rather than the advance: a script's write is a NEW
				// commit and therefore the NEXT advance, so the toucher's
				// Knomit-Trigger is what identifies it — here, after a
				// restart, and on every other machine (B's copy of T does
				// not re-run on a write A's T made). A human's later edit
				// carries no trailer and fires; another trigger's write
				// carries ITS name and fires this one (chains across
				// triggers are allowed). Tree episodes only: a due fire is
				// caused by time, never `self-caused` (sweepDue has no such
				// check), so a script that re-arms its own due trigger by
				// changing `expires` fires again at the new instant.
				if pf.byTrigger != "" && pf.byTrigger == ct.Name {
					d.stats.recordSelfCaused(ct.Name)
					continue
				}
				p := pf
				p.trig = ct
				rs.pending = append(rs.pending, p)
			}
		}
		for _, ct := range trigs {
			rs.newWM[ct.Name] = headStr
		}
	}
	return true
}

// sweepDue is the `on: due` half of phase A (F07 PR 2). Candidates are the
// dated facts LIVE on the agent branch (the index's branch_facts ⋈
// fact_expires) whose instant is at or before the run's clock; then, in this
// order — cheap filters first, so an already-processed or unmatched due fact
// never costs a blob read on any run:
//
//  1. the glob of the ACTIVE `due` triggers (none → drop the row);
//  2. the marks: a trigger that has processed this path at this very instant
//     drops out (a DIFFERENT instant is a re-arm: the fact's `expires`
//     changed); the marks are read once, only when something matched, with
//     the buffered marks overlaid;
//  3. the head's tree is the truth: the blob is read and parsed, and a path
//     absent, unparseable, undated or not yet due at the head is skipped —
//     this closes the window between a writer moving the ref and the index
//     Sync, and is what makes "a removed fact never fires" hold by
//     construction rather than by index timing;
//  4. `change` once per path (episode due, source due, commit = the toucher
//     at the head, before null), one pendingFire and one mark per surviving
//     trigger. The mark records the HEAD's instant, stamped with the run's
//     clock.
//
// It returns false only when ctx is cancelled. Nothing else happens on
// expiry: the sweep emits and marks; it never writes a fact.
func (d *Dispatcher) sweepDue(ctx context.Context, tr *facts.Repo, rs *runState, cr *changeReader, head plumbing.Hash,
	root string, trigs []*fact.CompiledTrigger, verifyOn bool, verifiedSet map[plumbing.Hash]bool) bool {
	nowUnix := rs.now.Unix()
	cands, err := tr.DueCandidates(ctx, d.branch, nowUnix)
	if err != nil {
		log.Warn().Err(err).Str("repo", d.repo).Msg("trigger dispatcher: due candidates unreadable; the sweep will re-run")
		return true
	}
	if h := currentTriggerHooks().DueCandidates; h != nil {
		cands = h(cands)
	}
	// Survivors of the cheap filters, ONE entry per path (the join lists a
	// path once — UNIQUE(branch_id, path) — but the filters must not depend on
	// it: two candidate rows for one path, e.g. a stale one, must not become
	// two fires).
	var paths []string
	hits := map[string][]*fact.CompiledTrigger{}
	var marks map[facts.DueKey]int64
	for _, c := range cands {
		rel := strings.TrimPrefix(c.Path, root+"/")
		var matched []*fact.CompiledTrigger
		for _, ct := range trigs {
			if ct.Matches(rel) {
				matched = append(matched, ct)
			}
		}
		if len(matched) == 0 {
			continue
		}
		if marks == nil {
			marks, err = tr.DueMarks(ctx, d.branch)
			if err != nil {
				log.Warn().Err(err).Str("repo", d.repo).Msg("trigger dispatcher: due marks unreadable; the sweep will re-run")
				return true
			}
			d.overlayPending(rs.svc, nil, marks)
		}
		for _, ct := range matched {
			if at, done := marks[facts.DueKey{Trigger: ct.Name, Path: c.Path}]; done && at == c.ExpiresAt {
				continue // processed at this instant, whatever `if` said
			}
			if _, seen := hits[c.Path]; !seen {
				paths = append(paths, c.Path)
			}
			hits[c.Path] = appendTrigger(hits[c.Path], ct)
		}
	}
	for _, path := range paths {
		if ctx.Err() != nil {
			return false
		}
		content, ok, err := cr.trees.BlobAt(ctx, head, path)
		if err != nil || !ok {
			continue // not at the head: the index is behind the ref, or the path is gone
		}
		f, err := fact.ParseFact(path, content)
		if err != nil {
			continue
		}
		at := fact.ExpiresUnix(f.Expires)
		if at == nil || *at > nowUnix {
			continue // the head's version is undated or not yet due
		}
		// The HEAD's instant is the one that counts: a trigger that processed
		// this path at THIS instant is done, whatever instant the candidate
		// row carried (the index may be behind the ref).
		var fire []*fact.CompiledTrigger
		for _, ct := range hits[path] {
			if done, has := marks[facts.DueKey{Trigger: ct.Name, Path: path}]; has && done == *at {
				continue
			}
			fire = append(fire, ct)
		}
		if len(fire) == 0 {
			continue
		}
		c0 := stopwatch()
		pf := d.buildChange(ctx, cr, head, head, path, fact.TriggerOnDue, false, verifyOn, verifiedSet)
		rs.changeMS += stopwatch().Sub(c0).Milliseconds()
		rs.duePaths++
		for _, ct := range fire {
			p := pf
			p.trig = ct
			rs.pending = append(rs.pending, p)
			rs.dueMarks = append(rs.dueMarks, facts.DueMark{Trigger: ct.Name, Path: path, ExpiresAt: *at, FiredAt: nowUnix})
		}
	}
	return true
}

// appendTrigger appends ct unless the same trigger is already listed.
func appendTrigger(list []*fact.CompiledTrigger, ct *fact.CompiledTrigger) []*fact.CompiledTrigger {
	for _, have := range list {
		if have == ct {
			return list
		}
	}
	return append(list, ct)
}

// verifyModeOn reports whether the ontology blob at the head turns F09 on
// (`verify_signatures: log|enforce`), parsing each distinct blob once.
func (d *Dispatcher) verifyModeOn(blob string, data []byte, readable bool) bool {
	if !readable {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.verifyBlob == blob {
		return d.verifyOn
	}
	on := false
	if vs, err := fact.ReadVerifySettings(data); err == nil && vs.Valid && vs.Mode != verify.VerifyOff {
		on = true
	}
	d.verifyBlob, d.verifyOn = blob, on
	return on
}

// verifiedBelow is the set of commits F09 counts as verified: everything
// reachable from the local upstream's tip. Since F09 PR 5 verification runs
// once at the acceptance gate that advances main, and main is trusted
// afterwards; there is no anchor ref and no fold cache to consult, so the
// dispatcher walks the tip's history ONCE per run and reuses the set while
// the tip does not move (a restart re-walks once). Your own unmerged writes
// are therefore verified:false until they come back through main.
func (d *Dispatcher) verifiedBelow(ctx context.Context, tr *facts.Repo, upstream string) map[plumbing.Hash]bool {
	tip, err := tr.UpstreamTip(ctx, upstream)
	if err != nil || tip == plumbing.ZeroHash {
		return nil
	}
	d.mu.Lock()
	if d.verified.tip == tip && d.verified.set != nil {
		set := d.verified.set
		d.mu.Unlock()
		return set
	}
	d.mu.Unlock()
	set, err := tr.AncestorSet(ctx, tip)
	if err != nil {
		log.Warn().Err(err).Str("repo", d.repo).Msg("trigger dispatcher: upstream history walk failed; verified reads false this run")
		return nil
	}
	d.mu.Lock()
	d.verified.tip, d.verified.set = tip, set
	d.mu.Unlock()
	return set
}

// changeReader is the per-run state buildChange reads through: the tree
// reader (decoded trees cached) and, per commit, the author, trailer and
// verified signer, read once however many paths the commit touched.
type changeReader struct {
	trees      facts.TriggerTrees
	tr         *facts.Repo
	meta       map[plumbing.Hash]commitMeta
	instanceFP string
}

// commitMeta is what one commit contributes to `change`: the author, the
// three trace trailers (one message read, three scans of its last
// paragraph) and the verified signer.
type commitMeta struct {
	ok      bool
	kind    string
	id      string
	fp      string
	trace   string // Knomit-Trace
	cause   string // Knomit-Cause
	trigger string // Knomit-Trigger
	source  string
}

func (cr *changeReader) metaOf(ctx context.Context, commit plumbing.Hash) commitMeta {
	if m, ok := cr.meta[commit]; ok {
		return m
	}
	m := commitMeta{source: "merged"}
	if info, err := cr.tr.CommitInfo(ctx, commit); err == nil {
		m.ok = true
		m.id = info.AuthorName
		m.trace = model.TrailerValue(info.Message, model.TrailerTrace)
		m.cause = model.TrailerValue(info.Message, model.TrailerCause)
		m.trigger = model.TrailerValue(info.Message, model.TrailerTrigger)
		m.kind = "human"
		switch {
		case strings.HasSuffix(info.AuthorEmail, "@agents.knomit.io"):
			m.kind = "agent"
		case info.AuthorName == "" && info.AuthorEmail == "":
			m.kind = "unknown"
		}
		// fp only when the SSHSIG verifies over the payload; never from the
		// embedded key alone. `source` is decided by the SIGNER, not the
		// author text. (An experiment's commits are authored by the agent
		// that owns the experiment and signed by this instance, #394; before
		// that fix they were authored exp/<name>, and history keeps such
		// commits — the signer, not the author, still makes them local.)
		if signer, err := cr.tr.CommitSignerOf(ctx, commit); err == nil {
			m.fp = signer.Fingerprint
		}
		if m.fp != "" && m.fp == cr.instanceFP {
			m.source = "local"
		}
	}
	cr.meta[commit] = m
	return m
}

// buildChange builds the `change` global for one matched path: the ORIGINAL
// commit via the treesame walk, its verified signer, `source`, `verified`,
// the trace trailer, the fact at the head (at the watermark for a retract) and
// `before` at the watermark. For a due fire (episode due, wm == head) there is
// no advance: `source` is "due", `before` is null, and `commit` is the commit
// that introduced the content the path carries at the head.
func (d *Dispatcher) buildChange(ctx context.Context, cr *changeReader, head, wm plumbing.Hash, repoPath, episode string,
	nonlinear, verifyOn bool, verifiedSet map[plumbing.Hash]bool) pendingFire {
	if h := currentTriggerHooks().InChange; h != nil {
		h()
	}
	pf := pendingFire{repoPath: repoPath, episode: episode, nonlinear: nonlinear}
	author := map[string]any{"kind": "unknown", "id": "", "fp": "", "verified": false}
	commit, source, trace := head, "merged", ""
	due := episode == fact.TriggerOnDue

	// A retract in a NONLINEAR advance (a rewind replay) has no originating
	// commit in the new history, and the same-absent-blob walk would run to
	// the root: take the fallback directly.
	found := false
	if !(nonlinear && episode == fact.TriggerOnRetract) {
		res, err := cr.trees.Toucher(ctx, head, repoPath)
		if err == nil && res.Found {
			commit, found = res.Commit, true
		} else if err != nil {
			log.Warn().Err(err).Str("repo", d.repo).Str("path", repoPath).Msg("trigger dispatcher: toucher walk failed; using the head")
		}
	}
	if found {
		if m := cr.metaOf(ctx, commit); m.ok {
			source, trace = m.source, m.trace
			pf.byTrigger = m.trigger
			author = map[string]any{
				"kind":     m.kind,
				"id":       m.id,
				"fp":       m.fp,
				"verified": verifyOn && verifiedSet != nil && verifiedSet[commit],
			}
		}
	}
	if due {
		source = fact.TriggerOnDue // the design fixes it; author.fp still says who wrote the fact
	}
	pf.commit, pf.source = commit.String(), source

	// fact: at the head, or at the watermark for a retract (the path is gone
	// at the head). before: the version at the WATERMARK — this machine's
	// last-processed version, not the toucher's parent, which could be an
	// intermediate version this machine never saw. A due fire has no before.
	factAt := head
	if episode == fact.TriggerOnRetract {
		factAt = wm
	}
	pf.factMap, pf.parseable = factGlobal(ctx, cr.trees, factAt, repoPath)
	// The fire's trace is derived from the trailer, the fact and the commit
	// (deriveTrace) — never empty since PR 3 — and is what a script's writes
	// carry forward as Knomit-Trace.
	trace = deriveTrace(trace, pf.factMap, pf.commit)
	pf.trace = trace
	var before any
	if episode != fact.TriggerOnLearn && !due {
		if m, ok := factGlobal(ctx, cr.trees, wm, repoPath); ok {
			before = m
		}
	}
	pf.change = map[string]any{
		"episode": episode,
		"source":  source,
		"commit":  commit.String(),
		"trace":   trace,
		"path":    repoPath,
		"author":  author,
		"before":  before,
	}
	return pf
}

// factGlobal reads and parses the fact at commit; ok is false when absent or
// unparseable (the `if` then sees fact === null).
func factGlobal(ctx context.Context, trees facts.TriggerTrees, commit plumbing.Hash, repoPath string) (map[string]any, bool) {
	content, ok, err := trees.BlobAt(ctx, commit, repoPath)
	if err != nil || !ok {
		return nil, false
	}
	f, err := fact.ParseFact(repoPath, content)
	if err != nil {
		return nil, false
	}
	return fact.FactGlobal(f), true
}

// phaseB evaluates every pending fire OUTSIDE the store: `if`, then the
// action (emit, or the script — trigger_script.go). It returns the rows to
// log (every outcome but if-false), the evaluation and fire counts, and
// whether ctx cancelled the run.
func (d *Dispatcher) phaseB(ctx context.Context, rs *runState) (rows []facts.TriggerFire, evaluated, fires int, aborted bool) {
	agent := map[string]any{"id": d.identity.Agent, "host": d.identity.Host, "branch": d.branch}
	for _, p := range rs.pending {
		if ctx.Err() != nil {
			return nil, 0, 0, true
		}
		var factGlobal any
		if p.factMap != nil {
			factGlobal = p.factMap
		}
		globals := map[string]any{"fact": factGlobal, "agent": agent, "change": p.change}

		// The trigger's OWN work starts here: its `if` and its action. The
		// time a script spends INSIDE host calls (a write, a text query) is
		// knomit's, like change_ms, and is taken out of the trigger's own
		// duration before the statistics and the slow detector see it.
		t0 := stopwatch()
		if h := currentTriggerHooks().EvalDelay; h != nil {
			time.Sleep(h(p.trig.Name, p.repoPath))
		}
		pass, err := p.trig.EvalIf(globals, rs.now)
		outcome, errText := "", ""
		var hostMS time.Duration
		var run runStart
		var extra []facts.TriggerFire
		switch {
		case err != nil:
			var interrupted *goja.InterruptedError
			if errors.As(err, &interrupted) {
				outcome = facts.TriggerOutcomeIfTimeout
			} else {
				outcome = facts.TriggerOutcomeIfError
			}
			errText = err.Error()
		case !pass:
			outcome = facts.TriggerOutcomeIfFalse
		case p.trig.Do == fact.TriggerDoScript:
			outcome, errText, hostMS, extra = d.runScript(ctx, rs, p, globals)
			if ctx.Err() != nil {
				return nil, 0, 0, true // the script was interrupted by the dispatcher's cancel
			}
		case p.trig.Do == fact.TriggerDoPush:
			// The push action: a wake for this machine's sync loop, nothing
			// else — no SSE event, no commit, no trailer (fire log only). It
			// kicks on an unparseable fact too: what needs pushing is the
			// commit, not the fact. Its OWN case, never the emit arm below.
			d.wakeSync()
			outcome = facts.TriggerOutcomeKicked
			log.Debug().Str("repo", d.repo).Str("trigger", capForLog(p.trig.Name)).
				Str("path", capForLog(p.repoPath)).Str("commit", shortHash(p.commit)).Msg("trigger kicked sync")
		case p.trig.Do == fact.TriggerDoRun:
			// The run action: resolve the recipe (main's tip, else this
			// machine's) and START it beside the dispatcher — a slot taken
			// without blocking, a run id minted, a goroutine. Never waits:
			// the result is a later row with the same run id. Its OWN case,
			// never the emit arm below.
			rf, rt := rs.fireRange()
			run = d.startRecipe(p, p.trig.Recipe, nil, globals, rf, rt)
			outcome, errText = run.outcome, run.errText
			if outcome == "" {
				outcome = outcomeUnbound
			}
		default:
			outcome = facts.TriggerOutcomeEmitted
			if !p.parseable {
				outcome = facts.TriggerOutcomeUnparseable
			}
			d.emit(p, nil)
		}
		if p.episode == fact.TriggerOnDue &&
			(outcome == facts.TriggerOutcomeRateLimited || outcome == facts.TriggerOutcomeBusy) {
			if rs.dueRetry == nil {
				rs.dueRetry = map[facts.DueKey]bool{}
			}
			rs.dueRetry[facts.DueKey{Trigger: p.trig.Name, Path: p.repoPath}] = true
		}
		elapsed := stopwatch().Sub(t0) - hostMS
		evaluated++
		if outcome == facts.TriggerOutcomeEmitted || outcome == facts.TriggerOutcomeUnparseable || outcome == facts.TriggerOutcomeRan ||
			outcome == facts.TriggerOutcomeKicked || outcome == facts.TriggerOutcomeStarted {
			fires++
		}
		slow := d.slow > 0 && elapsed > d.slow
		d.stats.record(p.trig.Name, outcome, elapsed, slow)
		if slow {
			log.Warn().
				Str("repo", d.repo).
				Str("branch", d.branch).
				Str("trigger", capForLog(p.trig.Name)).
				Str("path", capForLog(p.repoPath)).
				Str("episode", p.episode).
				Str("commit", shortHash(p.commit)).
				Str("outcome", outcome).
				Dur("elapsed", elapsed).
				Dur("threshold", d.slow).
				Int64("change_ms", rs.changeMS).
				Int64("host_ms", hostMS.Milliseconds()).
				Msg("slow trigger")
		}
		if outcome != facts.TriggerOutcomeIfFalse && outcome != outcomeUnbound {
			rows = append(rows, facts.TriggerFire{
				Trigger: p.trig.Name, Path: p.repoPath, Episode: p.episode, Source: p.source,
				Commit: p.commit, Trace: p.trace, Outcome: outcome, Error: errText, Nonlinear: p.nonlinear,
				RecipeSource: run.source, RecipeRev: run.rev, RunID: run.id,
			})
		}
		// knomit.run from a script: its start rows, after the script's own.
		rows = append(rows, extra...)
	}
	return rows, evaluated, fires, false
}

// emit is the `emit` action, and knomit.emit(payload) from a script: a log
// line and the SSE `trigger` event on the branch stream, published straight
// to the hub (no debounce). payload is nil for the action.
func (d *Dispatcher) emit(p pendingFire, payload json.RawMessage) {
	ev := log.Info().
		Str("repo", d.repo).
		Str("branch", d.branch).
		Str("trigger", capForLog(p.trig.Name)).
		Str("episode", p.episode).
		Str("source", p.source).
		Str("path", capForLog(p.repoPath)).
		Str("commit", shortHash(p.commit)).
		Str("trace", capForLog(p.trace))
	if payload != nil {
		ev = ev.Str("payload", capForLog(string(payload)))
	}
	ev.Msg("trigger fired")
	if pub := d.pub; pub != nil {
		pub.BroadcastTrigger(events.TriggerEvent{
			Branch: d.branch, Trigger: p.trig.Name, Path: p.repoPath, Episode: p.episode,
			Source: p.source, Commit: p.commit, Trace: p.trace, Payload: payload,
		})
	}
}

// nameStates folds the per-declaration states into one state per NAME: active
// when the winning declaration compiled; else unsupported when the winner is
// unsupported; else invalid (a duplicate's loser is invalid, so a duplicated
// active name stays active).
func nameStates(set *fact.TriggerSet) map[string]string {
	out := map[string]string{}
	for _, st := range set.States {
		if st.Name == "" {
			continue
		}
		cur, has := out[st.Name]
		switch {
		case st.State == fact.TriggerActive:
			out[st.Name] = fact.TriggerActive
		case st.State == fact.TriggerUnsupported && (!has || cur == fact.TriggerInvalid):
			out[st.Name] = fact.TriggerUnsupported
		case !has:
			out[st.Name] = fact.TriggerInvalid
		}
	}
	return out
}

func episodeOf(change string) string {
	switch change {
	case model.ChangeAdded:
		return fact.TriggerOnLearn
	case model.ChangeDeleted:
		return fact.TriggerOnRetract
	default:
		return fact.TriggerOnUpdate
	}
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func capForLog(s string) string {
	if len(s) <= maxTriggerLogField {
		return s
	}
	return s[:maxTriggerLogField] + "...[truncated]"
}

// recordSet remembers the last compiled set for the report.
func (d *Dispatcher) recordSet(set *fact.TriggerSet, head, blob string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastSet, d.lastHead, d.lastBlob = set, head, blob
}

func (d *Dispatcher) lastCompiledSet() *fact.TriggerSet {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastSet
}

// logInvalidOnce logs each INVALID trigger at ERROR once per (name, blob).
// Unsupported triggers are not errors and are not logged.
func (d *Dispatcher) logInvalidOnce(set *fact.TriggerSet) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.errBlob != set.Blob {
		d.errBlob, d.errLogged = set.Blob, map[string]bool{}
	}
	for _, st := range set.States {
		if st.State != fact.TriggerInvalid || d.errLogged[st.Key] {
			continue
		}
		d.errLogged[st.Key] = true
		name := st.Name
		if name == "" {
			name = "(unnamed)"
		}
		log.Error().
			Str("repo", d.repo).
			Str("branch", d.branch).
			Str("trigger", capForLog(name)).
			Str("topic", st.Node).
			Str("blob", set.Blob).
			Str("error", st.Error).
			Msg("invalid trigger skipped")
	}
}

// logOntologyErrorOnce reports an ontology at the head that cannot be read or
// parsed, once per blob (or head), keeping the last good trigger set.
func (d *Dispatcher) logOntologyErrorOnce(key string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ontErr == key {
		return
	}
	d.ontErr = key
	log.Error().Err(err).Str("repo", d.repo).Str("branch", d.branch).Str("blob", key).
		Msg("triggers: ontology at head unusable; keeping the last good trigger set")
}

func (d *Dispatcher) clearOntologyError() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ontErr = ""
}

// runSequence is how many runs have completed (any outcome) and the head the
// last completed run read. Tests wait on it.
func (d *Dispatcher) runSequence() (completedHead string, seq int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.completedHead, d.runSeq
}

// ---- The report behind GET …/branches/{branch}/triggers.

// Report is the trigger resource of one repo: the ONLY statistics surface
// (user ruling D-d: no stats block, no CLI, no Prometheus counters).
type Report struct {
	Enabled  bool                `json:"enabled"`
	Reason   string              `json:"reason,omitempty"`
	Branch   string              `json:"branch"`
	Head     string              `json:"head,omitempty"`
	Blob     string              `json:"ontology_blob,omitempty"`
	Runs     RunStats            `json:"runs"`
	Triggers []View              `json:"triggers"`
	Fires    []facts.TriggerFire `json:"fires"`
}

// View is one declared trigger as the endpoint shows it. State is
// active | invalid | unsupported | frozen, where frozen is an invalid trigger
// that holds a bookmark (its fires wait for the fix). A `do: script` trigger
// whose script is missing or does not compile at the head shows `invalid`
// with the error, though its bookmark keeps advancing (every fire is a
// `script-error` row): fixing the file needs no catch-up.
type View struct {
	Name      string    `json:"name"`
	Node      string    `json:"node"`
	Match     string    `json:"match,omitempty"`
	On        []string  `json:"on"`
	Do        string    `json:"do"`
	Script    string    `json:"script,omitempty"`
	JS        string    `json:"js,omitempty"`
	Recipe    string    `json:"recipe,omitempty"`
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
	Watermark string    `json:"watermark,omitempty"`
	Stats     StatsView `json:"stats"`
}

// Report renders the repo's trigger resource. logN is how many recent fire
// rows to include (0 for none). repoName is the repo's CURRENT display name,
// for the log (a rename keeps the dispatcher, which was built with the old
// one). A repo without a dispatcher has a disabled report, which the repos
// parent renders (RepoInstance.TriggerReport): only it knows why.
func (d *Dispatcher) Report(ctx context.Context, logN int, repoName string) (Report, error) {
	rep := Report{Branch: d.branch, Triggers: []View{}, Fires: []facts.TriggerFire{}}
	rep.Enabled = true
	rep.Runs = d.stats.runView()

	// The store is held for the reads only (released on the way out, a panic
	// included), not for the rendering below.
	svc, release, err := d.acquire()
	if err != nil {
		return rep, err
	}
	var wms map[string]string
	func() {
		defer release()
		var werr error
		wms, werr = svc.FactsRepo().TriggerWatermarks(ctx, d.branch)
		if werr != nil {
			return
		}
		d.overlayPending(svc, wms, nil)
		if logN > 0 {
			rep.Fires, werr = svc.FactsRepo().RecentTriggerFires(ctx, d.branch, logN)
		}
		if werr != nil {
			log.Warn().Err(werr).Str("repo", repoName).Msg("trigger report: fire log unreadable")
		}
	}()

	d.mu.Lock()
	set, head, blob := d.lastSet, d.lastHead, d.lastBlob
	d.mu.Unlock()
	rep.Head, rep.Blob = head, blob
	if set == nil {
		return rep, nil
	}
	active := map[string]*fact.CompiledTrigger{}
	for _, ct := range set.Active {
		active[ct.Name] = ct
	}
	for _, st := range set.States {
		v := View{Name: st.Name, Node: st.Node, Match: st.Match, On: st.On, Do: st.Do, Script: st.Script, JS: st.JS, Recipe: st.Recipe,
			State: st.State, Error: st.Error}
		if v.On == nil {
			v.On = []string{}
		}
		if ct := active[st.Name]; ct != nil && st.State == fact.TriggerActive {
			v.Match = ct.Match
		}
		if wm, has := wms[st.Name]; has {
			v.Watermark = wm
			if st.State == fact.TriggerInvalid {
				v.State = "frozen"
			}
		}
		// The script's compile state overlays an ACTIVE script trigger: the
		// entry is fine, the file is not (R5: state invalid with the error).
		if st.State == fact.TriggerActive && st.Do == fact.TriggerDoScript {
			if e := d.scriptError(st.Name); e != "" {
				v.State, v.Error = fact.TriggerInvalid, e
			}
		}
		v.Stats = d.stats.view(st.Name)
		rep.Triggers = append(rep.Triggers, v)
	}
	sort.SliceStable(rep.Triggers, func(i, j int) bool { return rep.Triggers[i].Name < rep.Triggers[j].Name })
	return rep, nil
}

// FiresByRun returns the fire-log rows of one recipe run on the observed agent
// branch, oldest first: the `started` row, then the result row once the
// recipe has finished and its row is flushed (F07 PR 5, D5). A nil dispatcher
// (the repo has none) has none.
func (d *Dispatcher) FiresByRun(ctx context.Context, id string) ([]facts.TriggerFire, error) {
	if d == nil {
		return []facts.TriggerFire{}, nil
	}
	svc, release, err := d.acquire()
	if err != nil {
		return nil, err
	}
	defer release()
	return svc.FactsRepo().TriggerFiresByRun(ctx, d.branch, id)
}

// String renders the identity for logs.
func (d *Dispatcher) String() string {
	return fmt.Sprintf("triggers(%s@%s)", d.repo, d.branch)
}
