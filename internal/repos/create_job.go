package repos

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/segmentio/ksuid"
)

// DefaultCreateTimeout bounds a detached create's total wall-clock.
//
// CLASSIFICATION (MN13): an OPERATIONAL BOUND, not a corpus property. It
// measures nothing about any repository's content or distribution — it is the
// wall-clock ceiling on one operation, set so a create that has stopped making
// progress cannot hold a name reservation forever. Deps.CreateTimeout
// overrides it, which is how the timeout path is testable in milliseconds
// rather than in half-hours.
//
// Deliberately far LOOSER than cfg.Git.NetworkTimeout (120s default), which
// already bounds each individual git network operation. This is the backstop
// for everything that timeout does not cover — a large clone's local indexing,
// an ontology write, a wedged SQLite handle — so it must not fire on a
// legitimately slow but progressing create.
const DefaultCreateTimeout = 30 * time.Minute

// createDrainTimeout bounds how long Close waits for in-flight creates AFTER
// cancelling them.
//
// CLASSIFICATION (MN13): an OPERATIONAL BOUND, not a corpus property. It is a
// backstop on shutdown latency, not a measure of anything. The cancel above is
// what makes the wait short in the normal case — a create notices at its next
// step boundary — so this only fires when a create is wedged inside work that
// does not observe cancellation, and there the right answer is to stop waiting
// and let the process exit rather than hang shutdown forever.
//
// The cost of it firing is the hazard it exists to prevent, so it is generous
// rather than tight: a create that outlives it may still touch a closing
// handle, which is exactly the use-after-close being fixed.
const createDrainTimeout = 30 * time.Second

// CreateJobTTL is how long a FINISHED job stays readable before it is reaped.
//
// CLASSIFICATION (MN13): an OPERATIONAL BOUND on retention, not a corpus
// property. It exists to outlive a realistic client reconnect window: the
// whole point of a detached create is that the client which started it may be
// gone when it ends, so the terminal outcome has to still be there when that
// client (or another) comes back to ask. An hour covers a closed laptop lid.
// It is deliberately NOT a durability guarantee — the job record is a
// courtesy, and the authoritative answer to "does this repo exist" is always
// the registry.
const CreateJobTTL = time.Hour

// The PHASES a create passes through, as distinct from its finer-grained Step.
//
// A phase says what KIND of thing is happening, and therefore how a client
// should draw it; a step says which one. They are separate because the
// drawable answer is not derivable from the step name without the client
// knowing every step there is — and the set of steps is ours to change.
//
// PhaseTransfer is the one that matters: it is the phase with NO percent.
// Nothing on the wire knows how many bytes a clone will move, so a create in
// transfer reports Indeterminate with the remote's own progress line as its
// message, and the UI draws an indeterminate bar. Inventing a number here was
// the original sin — the incident this work comes from is a wizard that sat at
// "40%" for minutes because 40 was a constant.
const (
	PhaseValidate = "validate"
	PhaseTransfer = "transfer"
	PhaseRegister = "register"
	PhaseIndex    = "index"
	PhaseDone     = "done"
)

// The index states a create job can report, mirroring RepoInstance.IndexStatus.
// Empty means the job never reached indexing (it failed earlier, or is still
// before it).
const (
	IndexStateIndexing = "indexing"
	IndexStateReady    = "ready"
	IndexStateError    = "error"
)

// CreateState is the lifecycle state of a detached create.
//
// Terminal states are CreateDone and CreateFailed. There is no separate
// "timed out" state: a create that exceeds its deadline is a create that
// failed, and its error says so — see CreateStatus.TimedOut, which is derived
// from that error rather than tracked alongside it, so the two cannot
// disagree.
type CreateState string

const (
	// CreateRunning is the only non-terminal state.
	CreateRunning CreateState = "running"
	// CreateDone means the repo exists and is registered.
	CreateDone CreateState = "done"
	// CreateFailed means the repo does NOT exist: Create rolled back its
	// partial database and its registry row before returning. This is the
	// legible terminal state the #67 ruling asks for — "the repo is not
	// there, and here is why" — rather than a registry row pointing at a
	// half-written file.
	CreateFailed CreateState = "failed"
	// CreateCancelled means a caller asked for the create to stop, and nothing
	// it produced survives: a create still running was unwound through its
	// own cleanup(), and one that had already registered a repo had that repo
	// deleted outright — archived and purged in one motion, no archive row
	// left to find. Terminal, like CreateFailed, and like it a promise that
	// the repo is NOT there.
	//
	// It is its own state rather than a flavour of failed because a client
	// renders the two differently: a failure is something to read and retry,
	// a cancellation is the outcome the user asked for.
	CreateCancelled CreateState = "cancelled"
	// CreateCancelling means a cancel has been RECORDED but the repo is not
	// gone yet. Non-terminal, like CreateRunning — and it exists because
	// cancelling is not instantaneous and pretending otherwise is what the
	// UI got wrong.
	//
	// A running create stops at its next STEP BOUNDARY, and the step in
	// flight may be a clone that takes minutes; a finished create's repo has
	// to be archived and purged, which is quick but not free. In both cases
	// the honest report between the click and the outcome is "cancelling",
	// not the last progress line the job happened to emit — a wizard that
	// kept drawing "Reading the remote, 40%" after the user pressed Cancel is
	// the report this state replaces. The user's words: "everything is
	// frozen, stuck in the current stage".
	//
	// CancelCreate NEVER BLOCKS to reach this state's successor. It records
	// the request, returns, and the work of honouring it happens on the
	// job's own goroutine — so the HTTP caller is never held for a transfer
	// it cannot interrupt.
	CreateCancelling CreateState = "cancelling"
)

// ErrCreateCancelled is the error a cancelled job reports through Result. It
// is deliberately NOT context.Canceled: Create returns that for the
// manager's own shutdown as well, and the two must stay distinguishable —
// one is a job the user stopped, the other a process going away.
var ErrCreateCancelled = errors.New("create cancelled")

// ErrCreateFinished refuses to cancel a job that already ended in failure or
// cancellation. There is nothing left to undo: a failed create rolled itself
// back, and a cancelled one already did what cancel does. Dismiss is the
// operation for making that row go away.
var ErrCreateFinished = errors.New("create already finished")

// CreateStatus is an immutable snapshot of a CreateJob, safe to read from any
// goroutine at any time, running or finished. It is what a polling client
// sees.
type CreateStatus struct {
	ID    string
	Name  string
	Mode  string
	State CreateState

	// Step/Message/Pct are the most recent progress report. They are a
	// LATEST-VALUE snapshot, not a stream: a poll arriving between two steps
	// sees the earlier one, and a poll arriving after three sees only the
	// third. Nothing downstream needs the intermediate ones.
	Step    string
	Message string
	Pct     int

	// Phase is the coarse stage Step belongs to — one of the Phase* constants.
	Phase string
	// Indeterminate says the job is doing something whose completion CANNOT be
	// expressed as a percent, so Pct must not be drawn as one. True during
	// transfer, where the only honest report is the remote's own message.
	Indeterminate bool
	// IndexState mirrors RepoInstance.IndexStatus for the repo being created:
	// "" before indexing starts, then indexing | ready | error.
	//
	// An "error" here rides on a job whose State is CreateDone, and that is
	// deliberate: the repo EXISTS and is registered, so failing the job would
	// describe a repo that is not there. The row shows the chip; the log has
	// the cause.
	IndexState string

	// Err is nil unless State is CreateFailed.
	Err error
	// TimedOut reports whether the failure was the create's own deadline
	// expiring rather than a create error.
	TimedOut bool

	StartedAt  time.Time
	FinishedAt time.Time
}

// CreateJob is a repo create running DETACHED from whatever asked for it.
//
// The ownership boundary is the whole point of this type. The work runs on a
// context derived from the MANAGER's lifetime with the manager's create
// deadline — never on an HTTP request's context — so a client that
// disconnects, or never polls again, changes nothing about whether the repo
// gets created. Callers OBSERVE a job; they do not own it.
//
// Bounded on purpose, and NOT fire-and-forget: the deadline guarantees every
// job reaches a terminal state, so a wedged create surfaces as a failure
// rather than an eternal "creating…".
//
// Progress is a mutex-guarded latest value rather than a channel, and that is
// deliberate. A channel needs a consumer, and the consumer here is exactly the
// thing that is allowed to vanish; a bounded channel with no reader eventually
// blocks its sender, parking the create goroutine forever and reproducing
// 'transient-state-becomes-permanent' — the incident's own motif — in a new
// place. A latest-value field cannot block, so that failure mode is designed
// out rather than guarded against.
type CreateJob struct {
	id   string
	name string
	mode string

	done chan struct{}

	startedAt time.Time

	mu            sync.Mutex
	state         CreateState
	step          string
	message       string
	pct           int
	phase         string
	indeterminate bool
	indexState    string
	ri            *RepoInstance
	err           error
	timedOut      bool
	finishedAt    time.Time

	// cancel ends the create's context. It is the job's OWN cancel, distinct
	// from the manager-wide createsCancel that Close pulls: cancelling one
	// job must not touch its neighbours.
	cancel context.CancelFunc
	// cancelRequested is set by CancelCreate and read by the worker goroutine
	// after Create returns. Create honours cancellation only at its step
	// boundaries, and past m.Add it honours none (mirrorIndexing merely stops
	// narrating), so a cancelled create can still hand back a live repo. This
	// flag is how the worker knows to delete it rather than report it done.
	cancelRequested bool
}

// ID returns the job's identifier, minted at start. It is what a client holds
// on to across a disconnect.
func (j *CreateJob) ID() string { return j.id }

// Done is closed when the job reaches a terminal state.
func (j *CreateJob) Done() <-chan struct{} { return j.done }

// Result returns the terminal outcome, blocking until the job finishes.
// Callers that must not block poll Status instead.
func (j *CreateJob) Result() (*RepoInstance, error) {
	<-j.done
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.ri, j.err
}

// Status snapshots the job without blocking.
func (j *CreateJob) Status() CreateStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.statusLocked()
}

// statusLocked is Status's body for callers that already hold j.mu.
func (j *CreateJob) statusLocked() CreateStatus {
	return CreateStatus{
		ID:            j.id,
		Name:          j.name,
		Mode:          j.mode,
		State:         j.state,
		Step:          j.step,
		Message:       j.message,
		Pct:           j.pct,
		Phase:         j.phase,
		Indeterminate: j.indeterminate,
		IndexState:    j.indexState,
		Err:           j.err,
		TimedOut:      j.timedOut,
		StartedAt:     j.startedAt,
		FinishedAt:    j.finishedAt,
	}
}

// record is the emit callback handed to Create. It overwrites the latest
// progress value and returns immediately — there is no consumer to wait for.
func (j *CreateJob) record(e Event) {
	j.mu.Lock()
	j.step, j.message, j.pct = e.Step, e.Message, e.Pct
	j.phase, j.indeterminate = e.Phase, e.Indeterminate
	// IndexState is STICKY, unlike every other field here: an event that says
	// nothing about the index must not erase what the last one established.
	// The terminal "done" event carries no index state of its own, and the
	// whole point of the mirror is that the final status still says how the
	// index ended up.
	if e.IndexState != "" {
		j.indexState = e.IndexState
	}
	j.mu.Unlock()
}

// finish records the terminal outcome and releases every waiter.
func (j *CreateJob) finish(ri *RepoInstance, err error, timedOut bool) {
	j.mu.Lock()
	j.finishLocked(ri, err, timedOut)
	j.mu.Unlock()
	close(j.done)
}

// finishLocked is finish's body for a caller that already holds j.mu and will
// close j.done itself. ErrCreateCancelled lands the job in CreateCancelled,
// every other error in CreateFailed.
func (j *CreateJob) finishLocked(ri *RepoInstance, err error, timedOut bool) {
	j.ri, j.err, j.timedOut = ri, err, timedOut
	j.finishedAt = time.Now().UTC()
	switch {
	case errors.Is(err, ErrCreateCancelled):
		j.state = CreateCancelled
	case err != nil:
		j.state = CreateFailed
	default:
		j.state = CreateDone
	}
}

// CancelCreate stops a create and removes everything it produced.
//
// A RUNNING job has its context cancelled. Create unwinds at its next step
// boundary through its own cleanup() — the partial database and the
// registry row go — and the job lands in CreateCancelled. Past m.Add there
// is no boundary left that honours the context (mirrorIndexing only stops
// narrating), so the worker goroutine checks cancelRequested when Create
// returns and, if a repo came back anyway, deletes it (DeleteRepo: archive
// and purge in one motion) before finishing the job. Either way the caller
// polls the job to CreateCancelled; this call returns as soon as the request
// is recorded, because a transfer in flight is not interruptible and the
// caller must not be held for it.
//
// A DONE job — the create already finished and its repo is live — is the
// case the user actually hits: "I picked the wrong mode and realised the
// moment it landed". The job flips to CreateCancelling and the delete runs on
// a goroutine, so this call returns at once here too. The repo is deleted
// only if it is the very instance this job registered: a repo of the same
// name created later by someone else is not this job's to remove, and
// answers ErrCreateFinished.
//
// NOTHING HERE BLOCKS, and that is the contract. An earlier version deleted
// synchronously so the caller "learns whether the repo is gone", which sounds
// like candour and reads, through an HTTP handler, as a request that hangs.
// The job is the thing that knows; the caller polls it. That is also why
// every path below returns before the work it authorised has happened.
//
// A CANCELLING job answers nil — IDEMPOTENT, not an error. Pressing a button
// whose own label says "Cancelling…" must not produce a failure, and the
// second request asks for exactly what the first one did.
//
// FAILED and CANCELLED jobs answer ErrCreateFinished: there is nothing to
// undo. An unknown id answers ErrCreateUnknown, indistinguishable from an
// expired one for the same reason CreateJobByID's second result is.
func (m *Manager) CancelCreate(id string) error {
	j, ok := m.CreateJobByID(id)
	if !ok {
		return ErrCreateUnknown
	}
	j.mu.Lock()
	switch j.state {
	case CreateRunning:
		j.cancelRequested = true
		j.state = CreateCancelling
		cancel := j.cancel
		j.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	case CreateCancelling:
		// Already asked, already being honoured. Saying so as an error would
		// make a double-click a failure.
		j.mu.Unlock()
		return nil
	case CreateDone:
		created := j.ri
		if created == nil || m.Get(j.name) != created {
			j.mu.Unlock()
			return ErrCreateFinished
		}
		j.state = CreateCancelling
		j.mu.Unlock()
		// Registered with the drain BEFORE the goroutine starts, exactly as
		// StartCreate does: this delete touches the control database, and
		// Close must not release those handles out from under it.
		m.createWg.Add(1)
		go func() {
			defer m.createWg.Done()
			if derr := m.DeleteRepo(j.name); derr != nil {
				// The one outcome cancel cannot make good on: the repo exists
				// and would not go. Reported as a FAILURE with the reason,
				// never as cancelled — "cancelled" promises the repo is gone.
				log.Error().Err(derr).Str("repo", j.name).Str("create_id", j.id).
					Msg("repo create cancelled after it completed, but the repo could not be deleted")
				j.mu.Lock()
				j.finishLocked(created, fmt.Errorf("cancel: the repo was created but could not be deleted: %w", derr), false)
				j.mu.Unlock()
				return
			}
			j.mu.Lock()
			j.finishLocked(nil, ErrCreateCancelled, false)
			j.mu.Unlock()
			log.Info().Str("repo", j.name).Str("create_id", j.id).
				Msg("repo create cancelled after completion; the repo was deleted")
		}()
		return nil
	default:
		j.mu.Unlock()
		return ErrCreateFinished
	}
}

// createTimeout is the manager's configured create deadline, or the package
// default when unset.
func (m *Manager) createTimeout() time.Duration {
	if d := m.deps.CreateTimeout; d > 0 {
		return d
	}
	return DefaultCreateTimeout
}

// CreateJobByID returns a create job by id. The second result is false for an
// id that never existed AND for one whose job has aged past CreateJobTTL — the
// two are deliberately indistinguishable, because a client cannot act
// differently on them and the registry answers "does this repo exist" either
// way.
func (m *Manager) CreateJobByID(id string) (*CreateJob, bool) {
	m.createJobsMu.Lock()
	defer m.createJobsMu.Unlock()
	j, ok := m.createJobs[id]
	return j, ok
}

// ErrCreateRunning refuses to dismiss a job that has not finished. Dismissal
// is how a FINISHED row leaves the list early; a running create is not a row a
// client gets to discard, because the work is still happening and its outcome
// still has to land somewhere.
var ErrCreateRunning = errors.New("create is still running")

// ErrCreateUnknown names a job id the manager does not hold — never started,
// or reaped past CreateJobTTL. Indistinguishable on purpose, exactly as
// CreateJobByID's second result is.
var ErrCreateUnknown = errors.New("no create job with that id")

// CreateJobs returns a snapshot of every job the manager still holds — running
// ones and those that finished within CreateJobTTL — newest first.
//
// This is what makes a detached create RECOVERABLE. Without it, a client that
// lost the id from the 202 (closed the tab, navigated away, restarted the app)
// had no way back to a running or failed create: the single-job endpoint needs
// an id it no longer has, and the repo list cannot show a repo that does not
// exist yet. That is the "no trace of the create anywhere" half of the
// incident.
//
// Newest first because the list is read as "what is happening now", and a
// pending row's place in the UI is decided by the UI, not by this order.
func (m *Manager) CreateJobs() []CreateStatus {
	m.createJobsMu.Lock()
	m.reapCreateJobsLocked(time.Now().UTC())
	out := make([]CreateStatus, 0, len(m.createJobs))
	for _, j := range m.createJobs {
		st := j.Status()
		// CANCELLED JOBS ARE NOT LISTED. The list is what the repo rail draws
		// from, and a cancelled create has nothing left to say there: the
		// repo is gone, the outcome is the one the user asked for, and a row
		// reading "cancelled — dismiss" is the UI telling them what they
		// already know and then asking them to acknowledge it. Their words:
		// "what's the point, I KNOW it was cancelled".
		//
		// FAILED jobs stay listed, and the difference is not arbitrary: a
		// failure carries an error the reader has not seen yet, so its row is
		// the only place that explanation exists.
		//
		// This is a LIST-level omission, not a deletion. The job survives in
		// the map until its TTL and GET /repo-creates/{id} still answers with
		// it — which is what lets the wizard that asked for the cancel poll
		// through to the terminal state instead of falling off a 404.
		if st.State == CreateCancelled {
			continue
		}
		out = append(out, st)
	}
	m.createJobsMu.Unlock()
	// Newest first, then id ascending — a TOTAL order, not merely a primary
	// key, for the same reason ListArchived sorts the way it does (see
	// lifecycle.go): with sort.Slice, which is NOT stable, two jobs whose
	// StartedAt compares equal may come back in either order, and two calls
	// over one unchanged manager are free to disagree.
	//
	// "Compares equal" is not hypothetical here, it is the ordinary case on
	// Windows. StartedAt is time.Now(), whose resolution is nanoseconds on
	// Linux and macOS but the system clock's tick on Windows — coarse enough
	// that two creates started back to back routinely land on the SAME instant,
	// and the faster the machine the likelier. That is how this surfaced: the
	// repo's first Windows CI job failed internal/web's create-list test, which
	// had been passing everywhere else on nothing but clock resolution.
	//
	// NAME THE CALL THAT MAKES THAT TRUE: StartCreate stores
	// `time.Now().UTC()`, and .UTC() STRIPS THE MONOTONIC READING. Equal and
	// After compare the monotonic clock when both operands carry one — QPC on
	// Windows, sub-microsecond — so a StartedAt that kept its monotonic reading
	// would practically never tie and none of the above would hold. It is the
	// .UTC() that puts the coarse wall clock into this comparison. The fix
	// below does not depend on that (the tiebreak is unconditional), but this
	// explanation does: drop the .UTC() in StartCreate and the ties stop,
	// leaving a paragraph that describes something no longer true.
	//
	// The tiebreak is for DETERMINISM, not for recency. Ids are ksuids, which
	// order by a one-SECOND timestamp before their random payload, so two ids
	// minted in the same tick carry no usable ordering between them — there is
	// no direction that would make this "newest first" below a second.
	// Ascending matches the tiebreak in lifecycle.go so the two sorts read the
	// same way.
	sort.SliceStable(out, func(i, k int) bool {
		if !out[i].StartedAt.Equal(out[k].StartedAt) {
			return out[i].StartedAt.After(out[k].StartedAt)
		}
		return out[i].ID < out[k].ID
	})
	return out
}

// DismissCreateJob forgets a FINISHED job, so a failed create's row can leave
// the list before its TTL expires without the user waiting an hour for it.
//
// Refuses a running job (ErrCreateRunning) rather than cancelling it: this is
// a LIST operation, and cancelling a create is a different act with different
// consequences (a half-clone to roll back) that no client asked for by
// dismissing a row.
func (m *Manager) DismissCreateJob(id string) error {
	m.createJobsMu.Lock()
	defer m.createJobsMu.Unlock()
	j, ok := m.createJobs[id]
	if !ok {
		return ErrCreateUnknown
	}
	// Both non-terminal states refuse: a cancelling job is still doing work
	// (a step boundary to reach, or a repo to delete) and its outcome still
	// has to land somewhere, exactly as a running one's does.
	if st := j.Status().State; st == CreateRunning || st == CreateCancelling {
		return ErrCreateRunning
	}
	delete(m.createJobs, id)
	return nil
}

// reapCreateJobsLocked drops FINISHED jobs older than CreateJobTTL. A running
// job is never reaped however long it runs — its own deadline is what bounds
// it, and dropping a live job would lose the outcome a client is waiting for.
//
// Called from the registration path rather than a ticker: the map only grows
// when a create starts, so sweeping there bounds it without another goroutine
// to start, stop, and race against Close. Callers must hold createJobsMu.
func (m *Manager) reapCreateJobsLocked(now time.Time) {
	for id, j := range m.createJobs {
		st := j.Status()
		// Neither NON-TERMINAL state is ever reaped. Cancelling matters as
		// much as running here and is easier to get wrong: a job that reached
		// CreateDone already stamped FinishedAt, so a done→cancelling job
		// carries a timestamp that may already be older than the TTL while
		// its delete goroutine is still working. Testing the state rather
		// than the timestamp is what keeps that job in the map until it has
		// actually finished.
		if st.State == CreateRunning || st.State == CreateCancelling {
			continue
		}
		if now.Sub(st.FinishedAt) > CreateJobTTL {
			delete(m.createJobs, id)
		}
	}
}

// StartCreate runs a repo create DETACHED from the caller and returns
// immediately with a handle to observe it.
//
// The work runs on context.WithTimeout(m.ctx, …) — parented to the MANAGER,
// not to the caller. This is the fix for issue #67: the HTTP handler used to
// pass r.Context() into Create, which made a repo's creation the property of
// the request that asked for it. A client that closed its tab cancelled the
// create at its next step boundary, discarding a clone that may already have
// completed; and nothing bounded a create that simply never progressed.
//
// Note the signature: there is NO ctx parameter. That is not an oversight and
// should not be "fixed" — a context parameter here is an invitation to pass
// the request's, which is the bug.
//
// BOUNDED, not fire-and-forget. Every job ends: either Create returns, or the
// deadline expires and Create's own step-boundary checks unwind it through
// cleanup(), removing the partial .db and the registry row. Both outcomes are
// terminal, both are recorded on the job, and both are LOGGED — the log line
// matters precisely because the client that asked may no longer be there to
// read the status.
//
// WHAT THIS MUST NEVER TOUCH: the background index heal. Manager.Add → openOne
// takes no context at all; the builder reads m.ctx, from which indexCtx is
// derived (builder.go). The create deadline is therefore NOT an ancestor of
// indexCtx, and cancelling it — including the defer cancel() below, which
// fires the instant Create returns — cannot stop a heal that is still running.
// Threading this context into openOne would reinstate incident
// kb/incidents/repos/clone-create-index-stuck-indexing: a cancelled heal
// returns without markIndexReady/markIndexFailed and pins IndexStatus at
// 'indexing' forever. Guarded by
// TestStartCreate_JobDeadlineDoesNotPinTheIndexAtIndexing.
func (m *Manager) StartCreate(spec CreateSpec) *CreateJob {
	now := time.Now().UTC()
	j := &CreateJob{
		id:        ksuid.New().String(),
		name:      spec.Name,
		mode:      spec.Mode,
		done:      make(chan struct{}),
		startedAt: now,
		state:     CreateRunning,
	}

	m.createJobsMu.Lock()
	if m.createJobs == nil {
		m.createJobs = make(map[string]*CreateJob)
	}
	m.reapCreateJobsLocked(now)
	m.createJobs[j.id] = j
	m.createJobsMu.Unlock()

	timeout := m.createTimeout()

	// Registered with the drain BEFORE the goroutine starts. Adding inside the
	// goroutine would race Close: a create could be started and not yet counted
	// when Close reads the waitgroup, which is the very gap this closes.
	m.createWg.Add(1)

	// Parented to createsCtx, not m.ctx: same lifetime, plus a cancel that
	// Close owns, so shutdown can wind an in-flight create down instead of
	// waiting out its full deadline. Cancelled on the way out either way so
	// the timer is released rather than left to fire.
	//
	// Minted HERE, before the goroutine, and handed to the job: CancelCreate
	// needs the cancel from the moment StartCreate returns, and a job whose
	// cancel is still nil for a scheduling tick would silently ignore the one
	// request it exists to honour.
	ctx, cancel := context.WithTimeout(m.createsCtx, timeout)
	j.mu.Lock()
	j.cancel = cancel
	j.mu.Unlock()

	go func() {
		defer m.createWg.Done()
		defer cancel()

		ri, err := m.Create(ctx, spec, j.record)

		// A cancel that arrived while Create ran. Decided under j.mu so a
		// CancelCreate racing this very instant lands on one side or the
		// other: either it sees CreateRunning and sets the flag before we read
		// it here, or it sees the terminal state finishLocked writes below and
		// takes the done-job path itself.
		j.mu.Lock()
		cancelled := j.cancelRequested
		if !cancelled {
			timedOut := err != nil && errors.Is(err, context.DeadlineExceeded)
			j.finishLocked(ri, err, timedOut)
			j.mu.Unlock()
			logCreateOutcome(spec, j.id, timeout, err, timedOut)
			close(j.done)
			return
		}
		j.mu.Unlock()

		// Cancelled. A repo that came back anyway got past the last step
		// boundary that honours the context (m.Add and after), so it is live
		// and registered — delete it, the whole of it. An error from Create
		// means its own cleanup() already ran and there is nothing to remove.
		if err == nil && ri != nil {
			if derr := m.DeleteRepo(spec.Name); derr != nil {
				// The one outcome cancel cannot make good on: the repo exists
				// and would not go. Report it as a FAILURE with the reason,
				// never as cancelled — "cancelled" promises the repo is gone.
				log.Error().Err(derr).Str("repo", spec.Name).Str("create_id", j.id).
					Msg("repo create cancelled after it completed, but the repo could not be deleted")
				j.finish(ri, fmt.Errorf("cancel: the repo was created but could not be deleted: %w", derr), false)
				return
			}
		}
		log.Info().Str("repo", spec.Name).Str("mode", spec.Mode).
			Str("create_id", j.id).Msg("repo create cancelled; nothing was kept")
		j.finish(nil, ErrCreateCancelled, false)
	}()

	return j
}

// logCreateOutcome writes the terminal log line for an uncancelled create. The
// line matters precisely because the client that asked may no longer be there
// to read the status.
func logCreateOutcome(spec CreateSpec, id string, timeout time.Duration, err error, timedOut bool) {
	switch {
	case timedOut:
		log.Error().Str("repo", spec.Name).Str("mode", spec.Mode).
			Str("create_id", id).Dur("timeout", timeout).
			Msg("repo create exceeded its deadline; partial repo rolled back")
	case err != nil:
		log.Error().Err(err).Str("repo", spec.Name).Str("mode", spec.Mode).
			Str("create_id", id).Msg("repo create failed; partial repo rolled back")
	default:
		log.Info().Str("repo", spec.Name).Str("mode", spec.Mode).
			Str("create_id", id).Msg("repo create finished")
	}
}

// drainCreates cancels every in-flight detached create and waits for them,
// bounded by createDrainTimeout. Called by Close BEFORE the control.db handles
// are released, so a create still unwinding can complete its own rollback
// against an open database.
//
// Reports whether the drain completed. A false return means at least one create
// is still running and shutdown proceeded anyway — the caller logs it, because
// the alternative is hanging forever on work that is not observing its context.
func (m *Manager) drainCreates() bool {
	if m.createsCancel != nil {
		m.createsCancel()
	}
	done := make(chan struct{})
	go func() {
		m.createWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(createDrainTimeout):
		return false
	}
}
