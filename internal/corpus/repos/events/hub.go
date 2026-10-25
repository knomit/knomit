package events

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/ysmood/goob"

	"knomit/internal/engine/store/merge"
	"knomit/internal/engine/store/remotesync"
	"knomit/internal/platform/crashdump"
)

// TaskEvent is the event payload broadcast to SSE clients via TaskHub.
// Status progresses through: "running" → "done" | "error".
// Phase and Message carry provider-specific detail (e.g. "raptor-depth 2/3").
type TaskEvent struct {
	Op      string `json:"op"`     // operation name, e.g. "synth", "sync"
	ID      string `json:"id"`     // unique task ID, e.g. "synth-3"
	Status  string `json:"status"` // "running", "done", or "error"
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
	Repo    string `json:"repo,omitempty"` // repository name for log context
}

// StatusEvent is broadcast when HEAD changes. SSE clients use it to refresh.
type StatusEvent struct {
	Head string `json:"head"`
}

// TaskHub manages async tasks with per-op single-flight control and pub/sub
// broadcasting to SSE clients.
//
// Design:
//   - At most one task per operation (op) runs at a time. A second Start
//     for the same op returns an error (the handler converts this to 409).
//   - Events are broadcast via goob.Observable to all subscribers.
//   - Subscribe returns a snapshot of recent events so reconnecting clients
//     can catch up without replaying the full history.
//   - Panic recovery is built into the goroutine wrapper to prevent a
//     crashing task from tearing down the server.
type TaskHub struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	ob       *goob.Observable
	active   map[string]TaskEvent          // op → latest running event
	lastDone map[string]TaskEvent          // op → last terminal event
	cancels  map[string]context.CancelFunc // taskID → cancel
	counter  int
}

// TaskPublisher is the publishing half of a TaskHub: the only way to put an
// event on the hub's streams, record a task the hub did not Start, cancel every
// task, or shut the hub down. NewTaskHub returns it beside the hub, and the
// repo instance that owns the hub keeps it in an unexported field; consumers
// hold the *TaskHub, which can subscribe, read and start tasks but cannot
// announce anything. TestEventPublishers_NotExposed keeps it that way.
type TaskPublisher struct {
	h *TaskHub
}

// NewTaskHub creates a TaskHub and its publisher. The provided context is the
// parent for all task goroutines.
func NewTaskHub(ctx context.Context) (*TaskHub, *TaskPublisher) {
	taskCtx, taskCancel := context.WithCancel(ctx)
	h := &TaskHub{
		ctx:      taskCtx,
		cancel:   taskCancel,
		ob:       goob.New(taskCtx),
		active:   make(map[string]TaskEvent),
		lastDone: make(map[string]TaskEvent),
		cancels:  make(map[string]context.CancelFunc),
	}
	return h, &TaskPublisher{h: h}
}

// Start launches a background goroutine for op. Returns the task ID
// (e.g. "synth-1") or an error if a task for this op is already running.
// The fn callback receives a cancellable context and an emit function
// for publishing progress; fn MUST emit a terminal event (done/error)
// before returning.
func (h *TaskHub) Start(op string, fn func(ctx context.Context, emit func(TaskEvent))) (string, error) {
	h.mu.Lock()

	if existing, running := h.active[op]; running {
		h.mu.Unlock()
		return "", fmt.Errorf("%s is already running (%s)", op, existing.ID)
	}

	h.counter++
	id := fmt.Sprintf("%s-%d", op, h.counter)

	taskCtx, taskCancel := context.WithCancel(h.ctx)
	h.cancels[id] = taskCancel
	// Mark as active before launching goroutine to prevent race with concurrent Start.
	h.active[op] = TaskEvent{Op: op, ID: id, Status: "running"}

	h.mu.Unlock()

	var terminated bool
	var termMu sync.Mutex

	emit := func(ev TaskEvent) {
		termMu.Lock()
		if terminated {
			termMu.Unlock()
			return
		}
		ev.Op = op
		ev.ID = id

		isTerminal := ev.Status == "done" || ev.Status == "error"
		if isTerminal {
			terminated = true
		}
		termMu.Unlock()

		// A terminal is recorded BEFORE it is published (PublishTask's
		// order): a Subscribe in between then finds it in lastDone. Published
		// first, a Subscribe in that window saw the task still running and
		// never received its end, so a per-job stream hung (PR #623 F1).
		if isTerminal {
			h.mu.Lock()
			delete(h.active, op)
			delete(h.cancels, id)
			h.lastDone[op] = ev
			h.mu.Unlock()
		}

		h.emit(ev)

		if isTerminal {
			taskCancel()
		}
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				crashdump.ReportRecovered("task:"+op, r)
				emit(TaskEvent{Status: "error", Message: fmt.Sprintf("panic: %v", r)})
			}
		}()
		fn(taskCtx, emit)
	}()

	return id, nil
}

// emit broadcasts an event to all subscribers and updates internal state.
func (h *TaskHub) emit(ev TaskEvent) {
	// Skip noisy progress logs (running + indexing phase).
	if !(ev.Status == "running" && ev.Phase == "indexing") {
		l := log.Debug().
			Str("op", ev.Op).
			Str("id", ev.ID).
			Str("status", ev.Status)
		if ev.Repo != "" {
			l = l.Str("repo", ev.Repo)
		}
		if ev.Phase != "" {
			l = l.Str("phase", ev.Phase)
		}
		if ev.Message != "" {
			l = l.Str("message", ev.Message)
		}
		l.Msg("task event")
	}

	h.mu.Lock()
	if ev.Status == "running" {
		h.active[ev.Op] = ev
	}
	h.mu.Unlock()

	h.ob.Publish(ev)
}

// Subscribe atomically creates a subscription and returns the current snapshot
// of active and last-completed tasks. The snapshot allows reconnecting SSE
// clients to render the correct UI state immediately.
func (h *TaskHub) Subscribe(ctx context.Context) (goob.Events, []TaskEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	events := h.ob.Subscribe(ctx)

	var snapshot []TaskEvent
	for _, ev := range h.active {
		snapshot = append(snapshot, ev)
	}
	for _, ev := range h.lastDone {
		// Only include if not currently active for this op
		if _, running := h.active[ev.Op]; !running {
			snapshot = append(snapshot, ev)
		}
	}

	return events, snapshot
}

// BroadcastIndex publishes an index-state change to this repo's SSE
// subscribers. Called only from Machine.publishIndex, which is the
// chokepoint every index-state change goes through.
func (p *TaskPublisher) BroadcastIndex(ev IndexEvent) {
	h := p.h
	h.ob.Publish(ev)
}

// BroadcastStatus publishes a HEAD change to all SSE subscribers.
func (p *TaskPublisher) BroadcastStatus(head string) {
	h := p.h
	h.ob.Publish(StatusEvent{Head: head})
}

// SyncEvent is broadcast after a remote sync attempt. On success, Main and
// Agent carry the reconcile outcome for the main and agent branches
// respectively (the same shapes store.Sync returns). On error, Error is
// set and Main/Agent are nil.
type SyncEvent struct {
	Remote string                          `json:"remote"`
	Status string                          `json:"status"` // "sync_ok" or "sync_error"
	Main   *remotesync.MainReconcileResult `json:"main,omitempty"`
	Agent  *merge.AgentReconcileResult     `json:"agent,omitempty"`
	Error  string                          `json:"error,omitempty"`
}

// BroadcastSyncOK publishes a successful sync event carrying the full
// SyncResult so frontends can render which side of the reconcile actually
// changed (main fast-forward / rewound, agent merge / ff / rebase / noop)
// rather than collapsing both into a single merge-commit hash.
func (p *TaskPublisher) BroadcastSyncOK(remote string, result remotesync.SyncResult) {
	h := p.h
	main := result.Main
	agent := result.Agent
	h.ob.Publish(SyncEvent{
		Remote: remote,
		Status: "sync_ok",
		Main:   &main,
		Agent:  &agent,
	})
}

// TriggerEvent is F07's `emit` action on the branch SSE stream (event name
// "trigger"). Published straight from the dispatcher (BroadcastTrigger), NOT
// through the debounced commit observer, so a fire reaches a listener within
// the run that produced it and a steady stream of writes cannot starve it.
// Branch is the agent branch the fire happened on; the SSE handler forwards
// the event only to the stream of that branch.
type TriggerEvent struct {
	Branch  string `json:"-"`
	Trigger string `json:"trigger"`
	Path    string `json:"path"`
	Episode string `json:"episode"`
	Source  string `json:"source"`
	Commit  string `json:"commit"`
	Trace   string `json:"trace,omitempty"`
	// Payload is what a script passed to knomit.emit(payload) — any JSON
	// value; absent for the `emit` action. One event shape for both.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// BroadcastTrigger publishes one trigger fire to this repo's SSE subscribers.
func (p *TaskPublisher) BroadcastTrigger(ev TriggerEvent) {
	h := p.h
	h.ob.Publish(ev)
}

// BroadcastSyncError publishes a sync failure event.
func (p *TaskPublisher) BroadcastSyncError(remote, errMsg string) {
	h := p.h
	h.ob.Publish(SyncEvent{
		Remote: remote,
		Status: "sync_error",
		Error:  errMsg,
	})
}

// PushEvent is broadcast after a push attempt.
type PushEvent struct {
	Remote string `json:"remote"`
	Status string `json:"status"` // "push_ok" or "push_error"
	Error  string `json:"error,omitempty"`
}

// BroadcastPushOK publishes a successful push event.
func (p *TaskPublisher) BroadcastPushOK(remote string) {
	h := p.h
	h.ob.Publish(PushEvent{
		Remote: remote,
		Status: "push_ok",
	})
}

// BroadcastPushError publishes a push failure event.
func (p *TaskPublisher) BroadcastPushError(remote, errMsg string) {
	h := p.h
	h.ob.Publish(PushEvent{
		Remote: remote,
		Status: "push_error",
		Error:  errMsg,
	})
}

// Task is the latest event of task id as the hub holds it: its op's running
// task, or its op's last terminal event. ok is false when the hub holds
// neither (an older task of the op, or one it never saw). It is what the job
// routes (GET/DELETE .../synthesis-runs/{id}, .../index-rebuilds/{id} and
// their /events) read: the hub is the only record of a job.
func (h *TaskHub) Task(id string) (TaskEvent, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ev := range h.active {
		if ev.ID == id {
			return ev, true
		}
	}
	for _, ev := range h.lastDone {
		if ev.ID == id {
			return ev, true
		}
	}
	return TaskEvent{}, false
}

// Tasks is every task of op the hub holds: its running task first, then its
// last terminal one. That is at most two — the hub keeps no older history —
// and none when op has never run. It is what the job list routes answer.
func (h *TaskHub) Tasks(op string) []TaskEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []TaskEvent
	if ev, ok := h.active[op]; ok {
		out = append(out, ev)
	}
	if ev, ok := h.lastDone[op]; ok {
		out = append(out, ev)
	}
	return out
}

// Cancel cancels task id's context when id is its op's RUNNING task and the
// hub started it (Start keeps its cancel func), and answers that task's
// latest event. ok is false for any other id: a finished task, an unknown
// one, and a task the hub only records (PublishTask: the lifecycle
// machine's rebuild, which is cancelled through CancelIndex instead). The
// task is still running when Cancel returns; its own terminal event, which
// fn emits once it sees the cancelled context, ends it.
func (h *TaskHub) Cancel(id string) (TaskEvent, bool) {
	h.mu.Lock()
	cancel, started := h.cancels[id]
	var ev TaskEvent
	running := false
	for _, a := range h.active {
		if a.ID == id {
			ev, running = a, true
		}
	}
	h.mu.Unlock()
	if !started || !running {
		return TaskEvent{}, false
	}
	cancel()
	return ev, true
}

// PublishTask records and broadcasts a task event for work the hub did not
// start itself — the lifecycle machine's manual index rebuild — so the jobs
// UI sees it exactly as it sees a Start task: running while it runs, then
// one terminal, and the Subscribe snapshot carries either.
func (p *TaskPublisher) PublishTask(ev TaskEvent) {
	h := p.h
	h.mu.Lock()
	switch ev.Status {
	case "running":
		h.active[ev.Op] = ev
	case "done", "error":
		if cur, ok := h.active[ev.Op]; ok && cur.ID == ev.ID {
			delete(h.active, ev.Op)
		}
		h.lastDone[ev.Op] = ev
	}
	h.mu.Unlock()
	h.ob.Publish(ev)
}

// CancelTasks cancels every running task's context without shutting the hub
// down: the store a task holds is about to be closed (Open.Exit), and the
// drain waits for each task's release. Subscribers stay connected.
func (p *TaskPublisher) CancelTasks() {
	h := p.h
	h.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(h.cancels))
	for _, c := range h.cancels {
		cancels = append(cancels, c)
	}
	h.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// Shutdown cancels all active task contexts.
func (p *TaskPublisher) Shutdown() {
	h := p.h
	h.cancel()
}
