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
//	B  (no store) per (trigger, path): `if` then emit, ctx checked per path;
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
// The dispatcher has its OWN context and wait group. It must not share
// syncCtx: ActivateSync cancels that to restart the reconcile loop and would
// kill the dispatcher whenever an origin is attached.
package repos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/platform/crashdump"
	"knomit/internal/store"
)

// triggerTraceTrailer is the commit trailer the dispatcher READS for
// change.trace. Nothing stamps it before PR 3.
const triggerTraceTrailer = "Knomit-Trace"

// maxTriggerLogField caps repo content (trigger names, paths) in the slow
// trigger WARN, the same bound the slow-request log applies to client fields.
const maxTriggerLogField = 256

// triggerHooks are test seams. Every field is nil (or zero) in production.
type triggerHooks struct {
	// afterKick runs between receiving a kick and reading the head ref. It is
	// how a test parks the worker.
	afterKick func()
	// afterHeadRead runs right after phase A read the head ref: a commit made
	// here is NOT in this run's range, and its kick must survive the run
	// (KickReceivedBeforeRefRead). A dispatcher that drained the kick channel
	// after reading the ref would swallow it.
	afterHeadRead func()
	// beforeTx2 runs between tx1 and tx2 (the crash test panics here).
	beforeTx2 func()
	// changeDelay is added inside the change build: knomit's per-path cost,
	// which must not be charged to the trigger.
	changeDelay time.Duration
	// now is the run's clock — read ONCE per run, the one clock comparison in
	// F07 (the due sweep's `expires_at <= now`). nil means time.Now().UTC().
	now func() time.Time
	// dueCandidates lets a test append STALE candidates (a path the index no
	// longer lists) to what the liveness join returned, so the head
	// confirmation is exercised on its own.
	dueCandidates func([]store.DueCandidate) []store.DueCandidate
	// trees wraps the run's tree reader (a test counts blob reads).
	trees func(store.TriggerTrees) store.TriggerTrees
}

var (
	triggerHooksMu   sync.Mutex
	triggerTestHooks triggerHooks
)

func currentTriggerHooks() triggerHooks {
	triggerHooksMu.Lock()
	defer triggerHooksMu.Unlock()
	return triggerTestHooks
}

// triggerDispatcher is one repo's dispatcher.
type triggerDispatcher struct {
	ri       *RepoInstance
	repo     string
	branch   string // the agent branch: the only branch that kicks
	identity fact.TriggerIdentity
	slow     time.Duration
	kick     chan struct{}
	cache    fact.TriggerCache
	stats    *triggerStats

	cancel context.CancelFunc
	wg     sync.WaitGroup

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
}

// newTriggerDispatcher builds the dispatcher for ri WITHOUT starting it, so
// the kick slot exists from build() (a write during the background heal kicks
// it) while the goroutine starts from activate() after the initial index.
func newTriggerDispatcher(ri *RepoInstance, repo, agentBranch string, signer ssh.Signer, slowMS int) *triggerDispatcher {
	return &triggerDispatcher{
		ri:       ri,
		repo:     repo,
		branch:   agentBranch,
		identity: triggerIdentityFor(agentBranch, signer),
		slow:     time.Duration(slowMS) * time.Millisecond,
		kick:     make(chan struct{}, 1),
		stats:    newTriggerStats(),
	}
}

// triggerIdentityFor is this instance's {agent}, {host} and {fp8}. {agent} is
// the branch minus "agent/", which is also the commit author's name; {fp8} is
// the first 8 hex of sha256 of the signing key (identity.go's fingerprint),
// the SAME key the signer uses — a store with no signer has no {fp8}, and a
// trigger naming it is then invalid on this instance; {host} is the branch
// name without the fingerprint suffix (a legacy agent/<host> branch has no
// suffix, so {host} == {agent} there, and {agent} still names the inbox).
func triggerIdentityFor(agentBranch string, signer ssh.Signer) fact.TriggerIdentity {
	agent := strings.TrimPrefix(agentBranch, "agent/")
	id := fact.TriggerIdentity{Agent: agent, Host: agent}
	if signer != nil {
		h := sha256.Sum256(signer.PublicKey().Marshal())
		id.FP8 = hex.EncodeToString(h[:])[:8]
	}
	if i := strings.LastIndex(agent, "-"); i >= 0 && isHex8(agent[i+1:]) {
		id.Host = agent[:i]
	}
	return id
}

func isHex8(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// start launches the actor with its own context derived from parent. It kicks
// itself once so advances made while the server was down fire on restart (the
// watermark makes that at-least-once).
func (d *triggerDispatcher) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	d.cancel = cancel
	d.wg.Add(1)
	d.triggerKick()
	go d.loop(ctx)
}

// stop cancels the actor and waits for the current run to notice (phase B
// checks ctx per path, so it returns within one evaluation).
func (d *triggerDispatcher) stop() {
	if d.cancel != nil {
		d.cancel()
	}
	d.wg.Wait()
}

// triggerKick is the observer's whole job for triggers: record "the agent
// branch advanced" and return. O(1), never blocks, never allocates.
func (d *triggerDispatcher) triggerKick() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// triggerKick on the instance is what ri.onCommit calls; nil when this repo
// has no dispatcher (read-only server, subscription).
func (ri *RepoInstance) triggerKick() {
	if d := ri.triggers; d != nil {
		d.triggerKick()
	}
}

// loop receives kicks. The kick is RECEIVED before the head ref is read
// (inside run): the reverse order would swallow the kick of a commit that
// lands between the ref read and the drain, the lost-wake-up shape.
func (d *triggerDispatcher) loop(ctx context.Context) {
	defer d.wg.Done()
	for {
		select {
		case <-ctx.Done():
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
func (d *triggerDispatcher) settle(ctx context.Context) {
	for {
		d.mu.Lock()
		empty := d.pending.empty()
		d.mu.Unlock()
		if empty {
			return
		}
		timer := time.NewTimer(triggerFlushGrace)
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
func (d *triggerDispatcher) safeRun(ctx context.Context) {
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
	if h := currentTriggerHooks().afterKick; h != nil {
		h()
	}
	d.run(ctx)
}

// pendingFire is one (trigger, path) evaluation phase A prepared for phase B.
type pendingFire struct {
	trig      *fact.CompiledTrigger
	repoPath  string
	episode   string
	nonlinear bool
	commit    string
	source    string
	trace     string
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
	dueMarks []store.DueMark
	// duePaths is the number of distinct due paths evaluated (into the run
	// row's paths next to the diff rows).
	duePaths int
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
	runs []store.TriggerRun
	rows int
	wm   map[string]string
	del  map[string]bool
	// due is the buffered due marks, keyed so a re-arm within one buffer keeps
	// the latest instant. They ride tx2 with the watermarks.
	due map[store.DueKey]store.DueMark
}

// maxBufferedRuns bounds the write-behind buffer by run count; the fire rows
// are bounded by TriggerFireRetention. Either bound flushes regardless of
// pending kicks.
const maxBufferedRuns = 256

func (p *pendingFlush) empty() bool {
	return len(p.runs) == 0 && len(p.wm) == 0 && len(p.del) == 0 && len(p.due) == 0
}

// clock is the run's one clock read: UTC (all times are UTC) at whole seconds
// — the granularity fact_expires.expires_at and F03's `expired` filter use.
func (d *triggerDispatcher) clock() time.Time {
	if h := currentTriggerHooks().now; h != nil {
		return h().UTC().Truncate(time.Second)
	}
	return time.Now().UTC().Truncate(time.Second)
}

func (d *triggerDispatcher) run(ctx context.Context) {
	rs := &runState{newWM: map[string]string{}, started: time.Now()}
	defer func() {
		d.mu.Lock()
		d.completedHead = rs.head
		d.mu.Unlock()
	}()

	// ---- Phase A: with the store.
	svc, release, err := d.ri.Acquire()
	if err != nil {
		return // closed or mid-swap: the next commit kicks again
	}
	rs.svc = svc
	rs.now = d.clock()
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
	durationMS := time.Since(rs.started).Milliseconds()
	if rs.rangeFrom == "" {
		rs.rangeFrom = rs.head // a run with no advance (a tick's sweep): range_from = range_to = head
	}
	paths := rs.paths + rs.duePaths
	if rs.didWork() {
		d.stats.recordRun(TriggerLastRun{
			RangeFrom: rs.rangeFrom, RangeTo: rs.head, Nonlinear: rs.nonlinear, Paths: paths,
			Evaluated: evaluated, Fires: fires, DurationMS: durationMS, DiffMS: rs.diffMS, ChangeMS: rs.changeMS,
		})
	}
	d.buffer(rs, store.TriggerRun{
		Branch: d.branch, RangeFrom: rs.rangeFrom, RangeTo: rs.head, Nonlinear: rs.nonlinear,
		Paths: paths, Evaluated: evaluated, Fires: fires,
		DurationMS: durationMS, DiffMS: rs.diffMS, ChangeMS: rs.changeMS, Rows: rows,
	})
	d.maybeFlush(ctx)
}

// buffer merges one run's phase C into the write-behind buffer.
func (d *triggerDispatcher) buffer(rs *runState, run store.TriggerRun) {
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
		p.wm, p.del, p.due = map[string]string{}, map[string]bool{}, map[store.DueKey]store.DueMark{}
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
		p.due[store.DueKey{Trigger: m.Trigger, Path: m.Path}] = m
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
func (d *triggerDispatcher) maybeFlush(ctx context.Context) {
	d.mu.Lock()
	full := !d.pending.empty() &&
		(d.pending.rows >= store.TriggerFireRetention || len(d.pending.runs) >= maxBufferedRuns)
	d.mu.Unlock()
	if full {
		d.flush(ctx)
	}
}

// flushed reports whether nothing is buffered (tests wait on it).
func (d *triggerDispatcher) flushed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pending.empty()
}

// flush writes the buffer: tx1 (the runs' fire rows and run rows), then tx2
// (the watermarks and the prune). Any failure — or a panic — drops the
// buffer: the stored tables are then the truth and the unflushed range
// re-fires (at-least-once).
func (d *triggerDispatcher) flush(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			crashdump.ReportRecovered("triggers:"+d.repo, r)
			log.Error().Str("repo", d.repo).Str("branch", d.branch).Interface("panic", r).
				Msg("trigger dispatcher: flush panicked; the range will be re-run")
			d.resetPending()
		}
	}()
	svc, release, err := d.ri.Acquire()
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
	wm := make(map[string]string, len(p.wm))
	for k, v := range p.wm {
		wm[k] = v
	}
	del := make([]string, 0, len(p.del))
	for n := range p.del {
		del = append(del, n)
	}
	sort.Strings(del)
	due := make([]store.DueMark, 0, len(p.due))
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

	tr := svc.Triggers()
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
	if h := currentTriggerHooks().beforeTx2; h != nil {
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
func (d *triggerDispatcher) resetPending() {
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
func (d *triggerDispatcher) overlayPending(svc *store.Service, wms map[string]string, marks map[store.DueKey]int64) {
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
// it closes the store.
func (d *triggerDispatcher) flushOnStop() {
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
func (d *triggerDispatcher) phaseA(ctx context.Context, svc *store.Service, rs *runState) bool {
	tr := svc.Triggers()
	headStr, err := svc.Branches().HeadCommit(ctx, d.branch)
	if err != nil {
		log.Warn().Err(err).Str("repo", d.repo).Str("branch", d.branch).Msg("trigger dispatcher: head unreadable")
		return false
	}
	head := plumbing.NewHash(headStr)
	rs.head = headStr
	if h := currentTriggerHooks().afterHeadRead; h != nil {
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
		verifiedSet = d.verifiedBelow(ctx, tr, svc.UpstreamBranch())
	}
	instanceFP := svc.SignerFingerprint()
	root := d.ri.ontologyRoot
	if root == "" {
		root = "kb"
	}
	// One tree reader and one commit-meta cache for the whole run: every
	// matched path of an advance reads the same few commits, and decoding a
	// tree (or verifying a signature) once per path instead of once per
	// commit is what made a large advance take minutes.
	trees := tr.TreeReader()
	if h := currentTriggerHooks().trees; h != nil {
		trees = h(trees)
	}
	cr := &changeReader{trees: trees, tr: tr, meta: map[plumbing.Hash]commitMeta{}, instanceFP: instanceFP}

	if !d.advance(ctx, tr, rs, cr, head, byW, root, verifyOn, verifiedSet) {
		return false
	}
	if len(dueTrigs) > 0 && !d.sweepDue(ctx, tr, rs, cr, head, root, dueTrigs, verifyOn, verifiedSet) {
		return false
	}
	return len(byW) > 0 || len(rs.dueMarks) > 0 || len(rs.newWM) > 0 || len(rs.del) > 0
}

// advance is the tree-episode half of phase A: one diff per distinct
// watermark, glob matching, `change` built once per matched path. It returns
// false only when ctx is cancelled.
func (d *triggerDispatcher) advance(ctx context.Context, tr store.TriggerIndex, rs *runState, cr *changeReader, head plumbing.Hash,
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
		t0 := time.Now()
		rows, err := tr.DiffFacts(ctx, wh, head)
		rs.diffMS += time.Since(t0).Milliseconds()
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
			c0 := time.Now()
			pf := d.buildChange(ctx, cr, head, wh, row.Path, episode, !linear, verifyOn, verifiedSet)
			rs.changeMS += time.Since(c0).Milliseconds()
			for _, ct := range matched {
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
func (d *triggerDispatcher) sweepDue(ctx context.Context, tr store.TriggerIndex, rs *runState, cr *changeReader, head plumbing.Hash,
	root string, trigs []*fact.CompiledTrigger, verifyOn bool, verifiedSet map[plumbing.Hash]bool) bool {
	nowUnix := rs.now.Unix()
	cands, err := tr.DueCandidates(ctx, d.branch, nowUnix)
	if err != nil {
		log.Warn().Err(err).Str("repo", d.repo).Msg("trigger dispatcher: due candidates unreadable; the sweep will re-run")
		return true
	}
	if h := currentTriggerHooks().dueCandidates; h != nil {
		cands = h(cands)
	}
	// Survivors of the cheap filters, ONE entry per path (the join lists a
	// path once — UNIQUE(branch_id, path) — but the filters must not depend on
	// it: two candidate rows for one path, e.g. a stale one, must not become
	// two fires).
	var paths []string
	hits := map[string][]*fact.CompiledTrigger{}
	var marks map[store.DueKey]int64
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
			if at, done := marks[store.DueKey{Trigger: ct.Name, Path: c.Path}]; done && at == c.ExpiresAt {
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
			if done, has := marks[store.DueKey{Trigger: ct.Name, Path: path}]; has && done == *at {
				continue
			}
			fire = append(fire, ct)
		}
		if len(fire) == 0 {
			continue
		}
		c0 := time.Now()
		pf := d.buildChange(ctx, cr, head, head, path, fact.TriggerOnDue, false, verifyOn, verifiedSet)
		rs.changeMS += time.Since(c0).Milliseconds()
		rs.duePaths++
		for _, ct := range fire {
			p := pf
			p.trig = ct
			rs.pending = append(rs.pending, p)
			rs.dueMarks = append(rs.dueMarks, store.DueMark{Trigger: ct.Name, Path: path, ExpiresAt: *at, FiredAt: nowUnix})
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
func (d *triggerDispatcher) verifyModeOn(blob string, data []byte, readable bool) bool {
	if !readable {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.verifyBlob == blob {
		return d.verifyOn
	}
	on := false
	if vs, err := fact.ReadVerifySettings(data); err == nil && vs.Valid && vs.Mode != store.VerifyOff {
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
func (d *triggerDispatcher) verifiedBelow(ctx context.Context, tr store.TriggerIndex, upstream string) map[plumbing.Hash]bool {
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
	trees      store.TriggerTrees
	tr         store.TriggerIndex
	meta       map[plumbing.Hash]commitMeta
	instanceFP string
}

// commitMeta is what one commit contributes to `change`.
type commitMeta struct {
	ok     bool
	kind   string
	id     string
	fp     string
	trace  string
	source string
}

func (cr *changeReader) metaOf(ctx context.Context, commit plumbing.Hash) commitMeta {
	if m, ok := cr.meta[commit]; ok {
		return m
	}
	m := commitMeta{source: "merged"}
	if info, err := cr.tr.CommitInfo(ctx, commit); err == nil {
		m.ok = true
		m.id = info.AuthorName
		m.trace = store.TrailerValue(info.Message, triggerTraceTrailer)
		m.kind = "human"
		switch {
		case strings.HasSuffix(strings.ToLower(info.AuthorEmail), "@agents.knomit.io"):
			m.kind = "agent"
		case info.AuthorName == "" && info.AuthorEmail == "":
			m.kind = "unknown"
		}
		// fp only when the SSHSIG verifies over the payload; never from the
		// embedded key alone. `source` is decided by the SIGNER, not the
		// author text: an experiment's commits are authored exp/<name> yet
		// signed by this instance, so they are local.
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
func (d *triggerDispatcher) buildChange(ctx context.Context, cr *changeReader, head, wm plumbing.Hash, repoPath, episode string,
	nonlinear, verifyOn bool, verifiedSet map[plumbing.Hash]bool) pendingFire {
	if delay := currentTriggerHooks().changeDelay; delay > 0 {
		time.Sleep(delay)
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
	pf.commit, pf.source, pf.trace = commit.String(), source, trace

	// fact: at the head, or at the watermark for a retract (the path is gone
	// at the head). before: the version at the WATERMARK — this machine's
	// last-processed version, not the toucher's parent, which could be an
	// intermediate version this machine never saw. A due fire has no before.
	factAt := head
	if episode == fact.TriggerOnRetract {
		factAt = wm
	}
	pf.factMap, pf.parseable = factGlobal(ctx, cr.trees, factAt, repoPath)
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
func factGlobal(ctx context.Context, trees store.TriggerTrees, commit plumbing.Hash, repoPath string) (map[string]any, bool) {
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

// phaseB evaluates every pending fire OUTSIDE the store: `if`, then emit. It
// returns the rows to log (every outcome but if-false), the evaluation and
// fire counts, and whether ctx cancelled the run.
func (d *triggerDispatcher) phaseB(ctx context.Context, rs *runState) (rows []store.TriggerFire, evaluated, fires int, aborted bool) {
	agent := map[string]any{"id": d.identity.Agent, "host": d.identity.Host, "fp8": d.identity.FP8, "branch": d.branch}
	for _, p := range rs.pending {
		if ctx.Err() != nil {
			return nil, 0, 0, true
		}
		var factGlobal any
		if p.factMap != nil {
			factGlobal = p.factMap
		}
		globals := map[string]any{"fact": factGlobal, "agent": agent, "change": p.change}

		// The trigger's OWN work starts here: its `if` and its action.
		t0 := time.Now()
		pass, err := p.trig.EvalIf(globals)
		outcome, errText := "", ""
		switch {
		case err != nil:
			var interrupted *goja.InterruptedError
			if errors.As(err, &interrupted) {
				outcome = store.TriggerOutcomeIfTimeout
			} else {
				outcome = store.TriggerOutcomeIfError
			}
			errText = err.Error()
		case !pass:
			outcome = store.TriggerOutcomeIfFalse
		default:
			outcome = store.TriggerOutcomeEmitted
			if !p.parseable {
				outcome = store.TriggerOutcomeUnparseable
			}
			d.emit(p)
		}
		elapsed := time.Since(t0)
		evaluated++
		if outcome == store.TriggerOutcomeEmitted || outcome == store.TriggerOutcomeUnparseable {
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
				Msg("slow trigger")
		}
		if outcome != store.TriggerOutcomeIfFalse {
			rows = append(rows, store.TriggerFire{
				Trigger: p.trig.Name, Path: p.repoPath, Episode: p.episode, Source: p.source,
				Commit: p.commit, Trace: p.trace, Outcome: outcome, Error: errText, Nonlinear: p.nonlinear,
			})
		}
	}
	return rows, evaluated, fires, false
}

// emit is the `emit` action: a log line and the SSE `trigger` event on the
// branch stream, published straight to the hub (no debounce).
func (d *triggerDispatcher) emit(p pendingFire) {
	log.Info().
		Str("repo", d.repo).
		Str("branch", d.branch).
		Str("trigger", capForLog(p.trig.Name)).
		Str("episode", p.episode).
		Str("source", p.source).
		Str("path", capForLog(p.repoPath)).
		Str("commit", shortHash(p.commit)).
		Str("trace", capForLog(p.trace)).
		Msg("trigger fired")
	if hub := d.ri.hub; hub != nil {
		hub.broadcastTrigger(TriggerEvent{
			Branch: d.branch, Trigger: p.trig.Name, Path: p.repoPath, Episode: p.episode,
			Source: p.source, Commit: p.commit, Trace: p.trace,
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
	case store.ChangeAdded:
		return fact.TriggerOnLearn
	case store.ChangeDeleted:
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
func (d *triggerDispatcher) recordSet(set *fact.TriggerSet, head, blob string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastSet, d.lastHead, d.lastBlob = set, head, blob
}

func (d *triggerDispatcher) lastCompiledSet() *fact.TriggerSet {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastSet
}

// logInvalidOnce logs each INVALID trigger at ERROR once per (name, blob).
// Unsupported triggers are not errors and are not logged.
func (d *triggerDispatcher) logInvalidOnce(set *fact.TriggerSet) {
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
func (d *triggerDispatcher) logOntologyErrorOnce(key string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ontErr == key {
		return
	}
	d.ontErr = key
	log.Error().Err(err).Str("repo", d.repo).Str("branch", d.branch).Str("blob", key).
		Msg("triggers: ontology at head unusable; keeping the last good trigger set")
}

func (d *triggerDispatcher) clearOntologyError() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ontErr = ""
}

// runSequence is how many runs have completed (any outcome) and the head the
// last completed run read. Tests wait on it.
func (d *triggerDispatcher) runSequence() (completedHead string, seq int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.completedHead, d.runSeq
}

// ---- The report behind GET …/branches/{branch}/triggers.

// TriggerReport is the trigger resource of one repo: the ONLY statistics
// surface (user ruling D-d: no stats block, no CLI, no Prometheus counters).
type TriggerReport struct {
	Enabled  bool                `json:"enabled"`
	Reason   string              `json:"reason,omitempty"`
	Branch   string              `json:"branch"`
	Head     string              `json:"head,omitempty"`
	Blob     string              `json:"ontology_blob,omitempty"`
	Runs     TriggerRunStats     `json:"runs"`
	Triggers []TriggerView       `json:"triggers"`
	Fires    []store.TriggerFire `json:"fires"`
}

// TriggerView is one declared trigger as the endpoint shows it. State is
// active | invalid | unsupported | frozen, where frozen is an invalid trigger
// that holds a bookmark (its fires wait for the fix).
type TriggerView struct {
	Name      string           `json:"name"`
	Node      string           `json:"node"`
	Match     string           `json:"match,omitempty"`
	On        []string         `json:"on"`
	Do        string           `json:"do"`
	State     string           `json:"state"`
	Error     string           `json:"error,omitempty"`
	Watermark string           `json:"watermark,omitempty"`
	Stats     TriggerStatsView `json:"stats"`
}

// TriggerReport renders the repo's trigger resource. logN is how many recent
// fire rows to include (0 for none).
func (ri *RepoInstance) TriggerReport(ctx context.Context, logN int) (TriggerReport, error) {
	rep := TriggerReport{Branch: ri.agentBranch, Triggers: []TriggerView{}, Fires: []store.TriggerFire{}}
	d := ri.triggers
	if d == nil {
		switch {
		case ri.subscribed || ri.agentBranch == "":
			rep.Reason = "subscription: no agent branch to observe"
		default:
			rep.Reason = "read-only server"
		}
		return rep, nil
	}
	rep.Enabled = true
	rep.Runs = d.stats.runView()

	var wms map[string]string
	err := ri.WithRead(func(svc *store.Service) {
		var werr error
		wms, werr = svc.Triggers().TriggerWatermarks(ctx, d.branch)
		if werr != nil {
			return
		}
		d.overlayPending(svc, wms, nil)
		if logN > 0 {
			rep.Fires, werr = svc.Triggers().RecentTriggerFires(ctx, d.branch, logN)
		}
		if werr != nil {
			log.Warn().Err(werr).Str("repo", ri.Name()).Msg("trigger report: fire log unreadable")
		}
	})
	if err != nil {
		return rep, err
	}

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
		v := TriggerView{Name: st.Name, Node: st.Node, Match: st.Match, On: st.On, Do: st.Do, State: st.State, Error: st.Error}
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
		v.Stats = d.stats.view(st.Name)
		rep.Triggers = append(rep.Triggers, v)
	}
	sort.SliceStable(rep.Triggers, func(i, j int) bool { return rep.Triggers[i].Name < rep.Triggers[j].Name })
	return rep, nil
}

// String renders the identity for logs.
func (d *triggerDispatcher) String() string {
	return fmt.Sprintf("triggers(%s@%s)", d.repo, d.branch)
}
