package web

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/ysmood/goob"
)

// TaskEvent is the event payload broadcast to SSE clients via TaskHub.
// Status progresses through: "running" → "done" | "error".
// Phase and Message carry provider-specific detail (e.g. "raptor-depth 2/3").
type TaskEvent struct {
	Op      string `json:"op"`      // operation name, e.g. "synth", "sync"
	ID      string `json:"id"`      // unique task ID, e.g. "synth-3"
	Status  string `json:"status"`  // "running", "done", or "error"
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
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
	active   map[string]TaskEvent           // op → latest running event
	lastDone map[string]TaskEvent           // op → last terminal event
	cancels  map[string]context.CancelFunc  // taskID → cancel
	counter  int
}

// NewTaskHub creates a TaskHub. The provided context is the parent for all task goroutines.
func NewTaskHub(ctx context.Context) *TaskHub {
	taskCtx, taskCancel := context.WithCancel(ctx)
	return &TaskHub{
		ctx:      taskCtx,
		cancel:   taskCancel,
		ob:       goob.New(taskCtx),
		active:   make(map[string]TaskEvent),
		lastDone: make(map[string]TaskEvent),
		cancels:  make(map[string]context.CancelFunc),
	}
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

		h.emit(ev)

		if isTerminal {
			h.mu.Lock()
			delete(h.active, op)
			delete(h.cancels, id)
			h.lastDone[op] = ev
			h.mu.Unlock()
			taskCancel()
		}
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				emit(TaskEvent{Status: "error", Message: fmt.Sprintf("panic: %v", r)})
			}
		}()
		fn(taskCtx, emit)
	}()

	return id, nil
}

// emit broadcasts an event to all subscribers and updates internal state.
func (h *TaskHub) emit(ev TaskEvent) {
	log.Debug().
		Str("op", ev.Op).
		Str("id", ev.ID).
		Str("status", ev.Status).
		Str("phase", ev.Phase).
		Str("message", ev.Message).
		Msg("task event")

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

// Shutdown cancels all active task contexts.
func (h *TaskHub) Shutdown() {
	h.cancel()
}
