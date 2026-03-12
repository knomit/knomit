package web

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/ysmood/goob"
)

// TaskEvent is the event payload broadcast to SSE clients.
type TaskEvent struct {
	Op      string `json:"op"`
	ID      string `json:"id"`
	Status  string `json:"status"`
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
}

// TaskHub manages async tasks with per-op concurrency control and SSE broadcasting.
type TaskHub struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	ob       *goob.Observable
	active   map[string]TaskEvent // op → latest running event
	lastDone map[string]TaskEvent // op → last terminal event
	maxConc  map[string]int       // op → max concurrent
	counter  int
	// cancels tracks per-task cancel functions so Shutdown can stop all tasks
	cancels map[string]context.CancelFunc // taskID → cancel
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
		maxConc:  map[string]int{"sync": 1, "synth": 1},
		cancels:  make(map[string]context.CancelFunc),
	}
}

// Start launches a task for the given op. Returns the task ID or an error if at capacity.
func (h *TaskHub) Start(op string, fn func(ctx context.Context, emit func(TaskEvent))) (string, error) {
	h.mu.Lock()

	// Check concurrency
	max := h.maxConc[op]
	if max == 0 {
		max = 1
	}
	if _, running := h.active[op]; running {
		existing := h.active[op]
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

	go fn(taskCtx, emit)

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

// Subscribe atomically creates a subscription and returns the current snapshot.
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
