package events

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/ysmood/goob"

	"knomit/internal/engine/store/merge"
	"knomit/internal/engine/store/remotesync"
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

// TaskHub is a repo's event hub: pub/sub broadcasting to SSE clients, plus the
// record of each op's running task and its last terminal one.
//
// Design:
//   - The hub runs no work itself. Tasks are recorded through
//     TaskPublisher.PublishTask by whoever runs them (the lifecycle machine's
//     index rebuild); the hub holds an op's latest running event and its last
//     terminal event, nothing older.
//   - Events are broadcast via goob.Observable to all subscribers.
//   - Subscribe returns a snapshot of recent events so reconnecting clients
//     can catch up without replaying the full history.
type TaskHub struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	ob       *goob.Observable
	active   map[string]TaskEvent // op → latest running event
	lastDone map[string]TaskEvent // op → last terminal event
}

// TaskPublisher is the publishing half of a TaskHub: the only way to put an
// event on the hub's streams, record a task, or shut the hub down. NewTaskHub
// returns it beside the hub, and the repo instance that owns the hub keeps it
// in an unexported field; consumers hold the *TaskHub, which can subscribe and
// read but cannot announce anything. TestEventPublishers_NotExposed keeps it
// that way.
type TaskPublisher struct {
	h *TaskHub
}

// NewTaskHub creates a TaskHub and its publisher. The provided context is the
// parent of the hub's observable: cancelling it, or Shutdown, closes every
// subscription.
func NewTaskHub(ctx context.Context) (*TaskHub, *TaskPublisher) {
	hubCtx, hubCancel := context.WithCancel(ctx)
	h := &TaskHub{
		cancel:   hubCancel,
		ob:       goob.New(hubCtx),
		active:   make(map[string]TaskEvent),
		lastDone: make(map[string]TaskEvent),
	}
	return h, &TaskPublisher{h: h}
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
// routes (GET/DELETE .../index-rebuilds/{id} and
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

// PublishTask records and broadcasts a task event — the lifecycle machine's
// manual index rebuild is the one production task — so the jobs UI sees it
// running while it runs, then one terminal, and the Subscribe snapshot carries
// either. A terminal is recorded BEFORE it is published: a Subscribe in
// between then finds it in lastDone rather than seeing the task running and
// never receiving its end (PR #623 F1).
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

// Shutdown cancels the hub's context, which closes every subscription.
func (p *TaskPublisher) Shutdown() {
	h := p.h
	h.cancel()
}
