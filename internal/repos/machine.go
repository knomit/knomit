package repos

// The lifecycle machine: ONE per mounted repo, the only thing that starts,
// stops, cancels or marks anything about it (rule 1).
//
// One driver goroutine handles events one at a time and is the only writer of
// lifecycle state. It waits only on foreground Enters (milliseconds, except
// Populate during a create), on a guard's bounded network read, and on
// draining lives it has already cancelled. It NEVER waits for background work
// to complete: a crashed worker is restarted through a waiter goroutine and a
// timer, and the index job reports back with an internal event.
//
// Callers (web handlers, the Manager, Create/Restore) only Send events and
// read Status/Watch. Status is DERIVED from the cursor and the Index result
// and published from exactly one place, publish().

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// DefaultCrashBackoff is how long a crashed worker stage waits, once drained,
// before it is re-entered.
//
// CLASSIFICATION (MN13): a RESOURCE BUDGET, class 2. It asserts nothing about
// any repository; it bounds how often a worker that panics on every run may
// spin (one crashdump per backoff), against how long the repo runs without
// that worker after a one-off panic.
const DefaultCrashBackoff = 5 * time.Second

// DefaultUnmountDrainBound is how long Manager.Close waits for one machine to
// finish unmounting before it gives up on it and logs.
//
// CLASSIFICATION (MN13): a RESOURCE BUDGET, class 2. An unmount drains the
// index job at its next embedding batch and a sync round at its next network
// step; this bounds how long a process shutdown may wait on a remote that has
// stopped answering. It is not a claim about how long either takes.
const DefaultUnmountDrainBound = 30 * time.Second

// Options configure a Machine.
type Options struct {
	// Synchronous is the test harness's mode (formerly DisableBackgroundSync).
	// It is consulted in exactly two places: Serve.Enter starts no experiment
	// sweep, and Sync.Enter runs one inline round and starts no loop. The
	// index job still runs in the background; a test waits for it with Watch.
	Synchronous bool
	// CrashBackoff overrides DefaultCrashBackoff (class-2 budget).
	CrashBackoff time.Duration
	// UnmountDrainBound overrides DefaultUnmountDrainBound (class-2 budget).
	UnmountDrainBound time.Duration
	// Hook is the test seam, nil in production. It is called at the start of
	// every stage Enter (point "enter") and inside the index job after its
	// plan (point "index-job"). A hook that blocks MUST also select on ctx —
	// the life's context — so a held stage still unmounts.
	Hook func(stage StageID, point string, ctx context.Context)
}

func (o Options) crashBackoff() time.Duration {
	if o.CrashBackoff > 0 {
		return o.CrashBackoff
	}
	return DefaultCrashBackoff
}

func (o Options) unmountDrainBound() time.Duration {
	if o.UnmountDrainBound > 0 {
		return o.UnmountDrainBound
	}
	return DefaultUnmountDrainBound
}

// Status is a repo's lifecycle status, derived from the machine and read
// lock-free from an atomic snapshot.
type Status struct {
	// Stage is populate|open|identify|index|serve|sync while a walk is in
	// flight, ready once it finished, else unavailable|closed.
	Stage string
	Index IndexStatus
	Sync  struct {
		Running bool
		Origin  string
	}
	Serve struct{ Running bool }
	// Err is the cause of Unavailable.
	Err error

	open         bool // the store is attached (Open entered)
	indexRunning bool // the index job is in flight
	terminal     terminal
	job          string // the manual rebuild's job id, "" for a heal
	jobFull      bool
	gens         [StageReady]uint64 // each entered stage's life generation, 0 when not entered
}

// IndexRunning reports whether the index job is in flight — what makes
// Attach, Detach and Swap answer ErrIndexing.
func (s Status) IndexRunning() bool { return s.indexRunning }

// IndexStatus is the index half of Status. State is indexing|ready|error.
type IndexStatus struct {
	State  string
	Done   int
	Total  int
	Reason string
	phase  string
}

// Transition is one published change of a machine's status.
type Transition struct {
	Status Status
}

// terminal is the machine's end state, if any.
type terminal int

const (
	termNone terminal = iota
	termUnavailable
	termClosed
)

// verdict is the index job's result: nil error = ready.
type verdict struct{ err error }

// indexProgress is the job's last progress report.
type indexProgress struct {
	phase       string
	done, total int
}

// internalEvent is posted by background work under a Life (and by the
// driver's own waiters and timers). Never blocks, never dropped while the
// machine runs.
type internalEvent interface{ isInternal() }

type indexDone struct {
	gen uint64
	err error
}
type indexProgressed struct{ gen uint64 }
type crashed struct {
	stage StageID
	gen   uint64
}
type drained struct {
	stage StageID
	gen   uint64
}
type reenter struct {
	stage StageID
	gen   uint64
}

func (indexDone) isInternal()       {}
func (indexProgressed) isInternal() {}
func (crashed) isInternal()         {}
func (drained) isInternal()         {}
func (reenter) isInternal()         {}

// mailbox is the internal event queue: unbounded, so a post never blocks the
// worker that makes it and never drops an indexDone.
type mailbox struct {
	mu     sync.Mutex
	items  []internalEvent
	notify chan struct{}
	closed bool
}

func newMailbox() *mailbox { return &mailbox{notify: make(chan struct{}, 1)} }

func (b *mailbox) post(e internalEvent) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.items = append(b.items, e)
	b.mu.Unlock()
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

func (b *mailbox) take() []internalEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := b.items
	b.items = nil
	return items
}

func (b *mailbox) close() {
	b.mu.Lock()
	b.closed = true
	b.items = nil
	b.mu.Unlock()
}

// Machine drives one repo through its stages.
type Machine struct {
	r          *RepoInstance
	root       context.Context
	rootCancel context.CancelFunc
	external   chan *MachineEvent
	inbox      *mailbox
	opts       Options

	// Driver-owned state: read and written only on the driver goroutine.
	lives      [StageReady]*Life
	entering   StageID // the stage whose Enter is running, StageReady when none
	pending    [StageReady]uint64
	timers     [StageReady]*time.Timer
	nextGen    uint64
	term       terminal
	termErr    error
	mountSpec  MountSpec
	indexSpec  indexSpec
	jobSeq     int
	jobID      string // the current manual rebuild's job id
	closing    bool   // set before an Unmount's exits: Open.Exit marks the instance closed
	pendingBak string // a swap's backup, deleted by the next successful Open
	lastPub    Status
	pub        indexPublisher

	progressPending atomic.Bool
	status          atomic.Pointer[Status]
	watchers        broadcaster
	done            chan struct{}
}

// newMachine builds the machine for r and starts its driver. It owns r from
// here on: r.machine is set before the driver runs.
func newMachine(parent context.Context, r *RepoInstance, opts Options) *Machine {
	root, cancel := context.WithCancel(parent)
	m := &Machine{
		r:          r,
		root:       root,
		rootCancel: cancel,
		external:   make(chan *MachineEvent),
		inbox:      newMailbox(),
		opts:       opts,
		entering:   StagePopulate,
		done:       make(chan struct{}),
	}
	r.machine = m
	s := m.computeStatus()
	m.status.Store(&s)
	m.lastPub = s
	go m.drive()
	return m
}

// Send delivers an event and waits for its reply, or for ctx. The cheap
// refusals are answered from the status snapshot at once, so a caller is
// refused even while the driver is inside a clone. Unmount cancels the root
// context FIRST, from the caller's goroutine, so a Populate in flight aborts
// and every life is told to stop before the driver drains them.
func (m *Machine) Send(ctx context.Context, e MachineEvent) (Reply, error) {
	if e.kind == evUnmount {
		m.rootCancel()
	} else {
		st := m.Status()
		switch {
		case st.terminal == termClosed:
			return Reply{Status: st}, ErrClosed
		case st.terminal == termUnavailable && e.kind != evMount:
			return Reply{Status: st}, unavailableErr(st.Err)
		case e.kind != evMount && !st.open:
			if st.Stage == "populate" {
				return Reply{Status: st}, errNotOpenPopulating
			}
			return Reply{Status: st}, ErrNotOpen
		case e.exclusiveWithIndex() && st.indexRunning:
			return Reply{Status: st}, ErrIndexing
		}
	}
	e.reply = make(chan reply, 1)
	select {
	case m.external <- &e:
	case <-m.done:
		if e.kind == evUnmount {
			return Reply{Status: m.Status()}, nil
		}
		return Reply{Status: m.Status()}, ErrClosed
	case <-ctx.Done():
		return Reply{Status: m.Status()}, ctx.Err()
	}
	select {
	case r := <-e.reply:
		return r.Reply, r.err
	case <-ctx.Done():
		return Reply{Status: m.Status()}, ctx.Err()
	}
}

// Status is the last published status.
func (m *Machine) Status() Status { return *m.status.Load() }

// Watch streams status transitions until ctx ends or the machine is done. The
// current status is delivered first. A slow reader sees the latest status,
// not every intermediate one.
func (m *Machine) Watch(ctx context.Context) <-chan Transition {
	return m.watchers.subscribe(ctx, m.Status(), m.done)
}

// Done is closed once the machine has unmounted.
func (m *Machine) Done() <-chan struct{} { return m.done }

// post is how lives and the driver's own goroutines reach the driver.
func (m *Machine) post(e internalEvent) { m.inbox.post(e) }

// postProgress coalesces the index job's progress reports into at most one
// pending internal event.
func (m *Machine) postProgress(gen uint64) {
	if m.progressPending.CompareAndSwap(false, true) {
		m.post(indexProgressed{gen: gen})
	}
}

func unavailableErr(cause error) error {
	if cause == nil {
		return ErrUnavailable
	}
	return cause
}

// drive is the driver goroutine.
func (m *Machine) drive() {
	for {
		select {
		case e := <-m.external:
			if m.handleExternal(e) {
				return
			}
		case <-m.inbox.notify:
			for _, ie := range m.inbox.take() {
				m.handleInternal(ie)
			}
		}
	}
}

// handleExternal performs one event's whole rewind or restart synchronously;
// it reports true once the machine has unmounted.
func (m *Machine) handleExternal(e *MachineEvent) bool {
	answer := func(rep Reply, err error) {
		rep.Status = m.Status()
		e.reply <- reply{Reply: rep, err: err}
	}
	if m.term == termClosed {
		answer(Reply{}, ErrClosed)
		return false
	}
	if e.kind == evUnmount {
		m.unmount()
		answer(Reply{}, nil)
		return true
	}
	if m.term == termUnavailable && e.kind != evMount {
		answer(Reply{}, unavailableErr(m.termErr))
		return false
	}
	if e.kind != evMount && m.lives[StageOpen] == nil {
		answer(Reply{}, ErrNotOpen)
		return false
	}
	if e.exclusiveWithIndex() && m.indexRunning() {
		answer(Reply{}, ErrIndexing)
		return false
	}
	switch e.kind {
	case evRebuild:
		if m.indexRunning() && m.indexSpec.full && m.indexSpec.branch == e.branch {
			answer(Reply{JobID: m.jobID, Absorbed: true}, nil)
			return false
		}
	case evCancelIndex:
		if !m.indexRunning() {
			answer(Reply{}, nil)
			return false
		}
	}

	gctx, cancel := guardContext(m.root, m.r)
	err := e.guard(gctx, m.r)
	cancel()
	if err != nil {
		answer(Reply{}, err)
		return false
	}

	switch {
	case e.kind == evCancelIndex:
		m.exitStages(StageIndex)
		m.r.indexVerdict = &verdict{err: ErrIndexCancelled}
	case stages[e.target].worker:
		m.exitStages(e.target)
	default:
		m.exitStages(m.enteredFrom(e.target)...)
		m.term, m.termErr = termNone, nil
		m.indexSpec = indexSpec{}
	}
	var rep Reply
	if e.kind == evRebuild {
		m.jobSeq++
		m.jobID = fmt.Sprintf("rebuild-%d", m.jobSeq)
		rep.JobID = m.jobID
	}
	applyErr := e.apply(m)
	if e.kind != evCancelIndex {
		m.walk(e.target)
	}
	m.publish()
	if applyErr == nil && m.term == termUnavailable {
		applyErr = m.termErr
	}
	answer(rep, applyErr)
	return false
}

// handleInternal reacts to background work. Stale generations are ignored.
func (m *Machine) handleInternal(ie internalEvent) {
	switch e := ie.(type) {
	case indexDone:
		l := m.lives[StageIndex]
		if l == nil || l.gen != e.gen || m.r.indexVerdict != nil {
			return
		}
		m.r.indexVerdict = &verdict{err: e.err}
		m.publish()
		// The due sweep was skipped while the index was not ready; run it now.
		if e.err == nil {
			m.r.triggerKick()
		}
	case indexProgressed:
		m.progressPending.Store(false)
		if l := m.lives[StageIndex]; l != nil && l.gen == e.gen {
			m.publish()
		}
	case crashed:
		l := m.lives[e.stage]
		if l == nil || l.gen != e.gen || m.pending[e.stage] == e.gen {
			return
		}
		l.cancel()
		m.pending[e.stage] = e.gen
		m.publish()
		// The driver never waits here: a waiter goroutine does, and reports.
		go func() {
			l.wg.Wait()
			m.post(drained{stage: e.stage, gen: e.gen})
		}()
	case drained:
		if m.pending[e.stage] != e.gen || m.term != termNone {
			return
		}
		m.timers[e.stage] = time.AfterFunc(m.opts.crashBackoff(), func() {
			m.post(reenter{stage: e.stage, gen: e.gen})
		})
	case reenter:
		if m.pending[e.stage] != e.gen || m.term != termNone {
			return
		}
		m.pending[e.stage] = 0
		m.timers[e.stage] = nil
		if l := m.lives[e.stage]; l != nil {
			l.wg.Wait() // already drained: returns at once
			stages[e.stage].Exit(m.r)
			m.lives[e.stage] = nil
		}
		m.enter(e.stage)
		m.publish()
	}
}

// indexRunning: the Index stage is entered and its job has not reported.
func (m *Machine) indexRunning() bool {
	return m.lives[StageIndex] != nil && m.r.indexVerdict == nil
}

// enteredFrom lists the entered stages at or above k.
func (m *Machine) enteredFrom(k StageID) []StageID {
	var out []StageID
	for s := k; s < StageReady; s++ {
		if m.lives[s] != nil {
			out = append(out, s)
		}
	}
	return out
}

// exitStages unwinds ks: it cancels EVERY affected life first, then drains and
// exits them newest-first. Cancelling all before draining any is load-bearing:
// lockBranch is a plain RWMutex (not ctx-aware) that the index job holds for a
// whole branch rebuild, so a sync round parked on it is released only once the
// index life is cancelled and the job returns at its next batch boundary.
// Draining newest-first without cancelling Index first would wait on that
// round forever.
func (m *Machine) exitStages(ks ...StageID) {
	for _, k := range ks {
		if l := m.lives[k]; l != nil {
			l.cancel()
		}
	}
	for i := len(ks) - 1; i >= 0; i-- {
		k := ks[i]
		l := m.lives[k]
		if l == nil {
			continue
		}
		l.wg.Wait()
		stages[k].Exit(m.r)
		m.lives[k] = nil
		m.pending[k] = 0
		if t := m.timers[k]; t != nil {
			t.Stop()
			m.timers[k] = nil
		}
	}
}

// walk enters every stage from target up that is not entered. A foundation
// Enter error is fatal: the entered stages are unwound and the machine is
// Unavailable(stage, err).
func (m *Machine) walk(target StageID) {
	for k := target; k < StageReady; k++ {
		if m.lives[k] != nil {
			continue
		}
		if err := m.enter(k); err != nil {
			m.exitStages(m.enteredFrom(StagePopulate)...)
			m.term = termUnavailable
			m.termErr = &StageError{Stage: k, Err: err}
			log.Warn().Err(err).Str("repo", m.r.Name()).Str("stage", stageName(k)).
				Msg("lifecycle: repo is unavailable")
			return
		}
	}
}

// enter runs stage k's Enter under a fresh life. A worker's Enter cannot fail;
// a foundation's error is returned with its life drained and discarded.
func (m *Machine) enter(k StageID) error {
	m.nextGen++
	ctx, cancel := context.WithCancel(m.root)
	l := &Life{ctx: ctx, cancel: cancel, gen: m.nextGen, stage: k, post: m.post}
	m.lives[k] = l
	m.entering = k
	m.publish()
	if h := m.opts.Hook; h != nil {
		h(k, "enter", ctx)
	}
	err := stages[k].Enter(ctx, l, m.r)
	m.entering = StageReady
	if err != nil && !stages[k].worker {
		cancel()
		l.wg.Wait()
		m.lives[k] = nil
		return err
	}
	if err != nil {
		log.Warn().Err(err).Str("repo", m.r.Name()).Str("stage", stageName(k)).Msg("lifecycle: worker stage entered with an error")
	}
	return nil
}

// unmount drains every entered stage (the root context is already cancelled,
// and every life derives from it), stops the timers and closes the machine.
// Nothing is published: a closed repo's status is no longer observed, and an
// event announcing it would only reach a chip about to disappear.
func (m *Machine) unmount() {
	m.rootCancel()
	m.closing = true
	m.exitStages(m.enteredFrom(StagePopulate)...)
	for k, t := range m.timers {
		if t != nil {
			t.Stop()
			m.timers[k] = nil
		}
	}
	m.term = termClosed
	if m.r.hub != nil {
		m.r.hub.Shutdown()
	}
	m.inbox.close()
	s := m.computeStatus()
	m.status.Store(&s)
	m.watchers.closeAll(Transition{Status: s})
	close(m.done)
}

// computeStatus derives the status from the machine. The ONE function that
// does so.
func (m *Machine) computeStatus() Status {
	r := m.r
	var s Status
	switch m.term {
	case termClosed:
		s.Stage = "closed"
	case termUnavailable:
		s.Stage = "unavailable"
		s.Err = m.termErr
	default:
		s.Stage = stageName(m.entering)
	}
	s.terminal = m.term
	for k, l := range m.lives {
		if l != nil {
			s.gens[k] = l.gen
		}
	}
	s.open = m.lives[StageOpen] != nil
	s.indexRunning = m.indexRunning()
	switch v := r.indexVerdict; {
	case v == nil:
		s.Index.State = IndexStateIndexing
		if p := r.indexProgress.Load(); p != nil {
			s.Index.Done, s.Index.Total, s.Index.phase = p.done, p.total, p.phase
		}
	case v.err == nil:
		s.Index.State = IndexStateReady
	case errors.Is(v.err, ErrIndexCancelled):
		s.Index.State, s.Index.Reason = IndexStateError, ErrIndexCancelled.Error()
	default:
		s.Index.State, s.Index.Reason = IndexStateError, v.err.Error()
	}
	if m.indexSpec.full && m.lives[StageIndex] != nil {
		s.job, s.jobFull = m.jobID, true
	}
	s.Serve.Running = m.lives[StageServe] != nil && m.pending[StageServe] == 0
	s.Sync.Running = m.lives[StageSync] != nil && m.pending[StageSync] == 0 && !m.opts.Synchronous
	s.Sync.Origin = r.syncOrigin
	return s
}

// publish is the ONE publish point. It stores the snapshot, wakes watchers,
// and emits the index event (and, for a manual rebuild, the hub task event)
// when the index state changed or its throttled progress moved.
func (m *Machine) publish() {
	s := m.computeStatus()
	m.status.Store(&s)
	m.watchers.send(Transition{Status: s})
	if m.term == termClosed {
		return
	}
	prev := m.lastPub
	m.lastPub = s
	changed := s.Index.State != prev.Index.State || s.Index.Reason != prev.Index.Reason || s.job != prev.job
	moved := s.Index.Done != prev.Index.Done || s.Index.Total != prev.Index.Total
	switch {
	case changed:
		m.publishIndex(s, false)
	case moved && s.Index.State == IndexStateIndexing:
		m.publishIndex(s, true)
	}
	m.publishRebuildTask(prev, s)
}

// publishRebuildTask keeps the jobs UI contract for a manual rebuild — hub
// TaskEvents on op "rebuild", running → done | error — from the same publish
// point, so there is no second single-flight.
func (m *Machine) publishRebuildTask(prev, s Status) {
	hub := m.r.hub
	if hub == nil {
		return
	}
	repo := m.r.Name()
	// A previous rebuild that was still running and is no longer the current
	// job ended without finishing: replaced by another index job, or cancelled.
	if prev.jobFull && prev.job != "" && prev.job != s.job && prev.Index.State == IndexStateIndexing {
		ev := TaskEvent{Op: "rebuild", ID: prev.job, Status: "error", Message: "replaced by another index job", Repo: repo}
		if s.Index.State == IndexStateError && s.Index.Reason == ErrIndexCancelled.Error() {
			ev.Message = ErrIndexCancelled.Error()
		}
		hub.publishTask(ev)
	}
	if !s.jobFull || s.job == "" {
		return
	}
	switch s.Index.State {
	case IndexStateIndexing:
		if prev.job != s.job {
			hub.publishTask(TaskEvent{Op: "rebuild", ID: s.job, Status: "running", Phase: "start", Message: "rebuilding index", Repo: repo})
		} else if s.Index.Done != prev.Index.Done || s.Index.Total != prev.Index.Total {
			hub.publishTask(TaskEvent{Op: "rebuild", ID: s.job, Status: "running", Phase: s.Index.phase,
				Message: fmt.Sprintf("%d/%d", s.Index.Done, s.Index.Total), Repo: repo})
		}
	case IndexStateReady:
		if prev.job != s.job || prev.Index.State != IndexStateReady {
			hub.publishTask(TaskEvent{Op: "rebuild", ID: s.job, Status: "done", Message: "rebuild complete", Repo: repo})
		}
	case IndexStateError:
		if prev.job != s.job || prev.Index.State != IndexStateError {
			hub.publishTask(TaskEvent{Op: "rebuild", ID: s.job, Status: "error", Message: s.Index.Reason, Repo: repo})
		}
	}
}

// broadcaster fans status transitions out to watchers, latest-value-wins.
type broadcaster struct {
	mu     sync.Mutex
	subs   map[chan Transition]struct{}
	closed bool
}

func (b *broadcaster) subscribe(ctx context.Context, current Status, done <-chan struct{}) <-chan Transition {
	ch := make(chan Transition, 1)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		ch <- Transition{Status: current}
		close(ch)
		return ch
	}
	if b.subs == nil {
		b.subs = map[chan Transition]struct{}{}
	}
	b.subs[ch] = struct{}{}
	ch <- Transition{Status: current}
	b.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}()
	return ch
}

// send replaces whatever a watcher has not read yet with t.
func (b *broadcaster) send(t Transition) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case <-ch:
		default:
		}
		ch <- t
	}
}

// closeAll ends every watch with the final transition left readable.
func (b *broadcaster) closeAll(last Transition) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for ch := range b.subs {
		select {
		case <-ch:
		default:
		}
		ch <- last
		delete(b.subs, ch)
		close(ch)
	}
}
