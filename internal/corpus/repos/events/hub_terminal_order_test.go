package events

import (
	"context"
	"testing"
	"time"

	"github.com/ysmood/goob"
)

// A TERMINAL IS RECORDED BEFORE IT IS PUBLISHED (PR #623 F1). A Subscribe
// racing a task's terminal emit must either find the terminal in its
// snapshot, or find the task running AND then receive the terminal on its
// channel. Published first, a Subscribe in the window between the publish
// and the bookkeeping saw the task running and never got its end, and a
// per-job events stream hung. There is no seam to stop the emit in that
// window, so this is a bounded stress loop: the old order failed it within a
// few hundred iterations.
func TestTaskHub_SubscribeRacingATerminalNeverMissesIt(t *testing.T) {
	const iterations = 2000
	for i := 0; i < iterations; i++ {
		hub, pub := NewTaskHub(context.Background())
		gate := make(chan struct{})
		id, err := hub.Start("synth", func(_ context.Context, emit func(TaskEvent)) {
			emit(TaskEvent{Status: "running"})
			<-gate
			emit(TaskEvent{Status: "done"})
		})
		if err != nil {
			t.Fatal(err)
		}
		for {
			if ev, ok := hub.Task(id); ok && ev.Status == "running" {
				break
			}
		}

		// Subscribe over and over while the terminal is emitted, until a
		// snapshot shows it: every subscription whose snapshot still showed
		// the task running must then receive the terminal.
		ctx, cancel := context.WithCancel(context.Background())
		var sawRunning []goob.Events
		close(gate)
		for ended := false; !ended; {
			events, snapshot := hub.Subscribe(ctx)
			for _, ev := range snapshot {
				if ev.ID == id && ev.Status == "running" {
					sawRunning = append(sawRunning, events)
				}
				if ev.ID == id && ev.Status == "done" {
					ended = true
				}
			}
		}
		for n, events := range sawRunning {
			deadline := time.After(time.Second)
		wait:
			for {
				select {
				case e := <-events:
					if ev, ok := e.(TaskEvent); ok && ev.ID == id && ev.Status == "done" {
						break wait
					}
				case <-deadline:
					cancel()
					pub.Shutdown()
					t.Fatalf("iteration %d: subscription %d saw %s running and its terminal never arrived", i, n, id)
				}
			}
		}
		cancel()
		pub.Shutdown()
	}
}

// Cancel acts only on its op's running task the hub started: a finished id,
// an unknown one and a task the hub only records (PublishTask) are refused,
// and a refused Cancel cancels nothing.
func TestTaskHub_CancelOnlyTheRunningStartedTask(t *testing.T) {
	hub, pub := NewTaskHub(context.Background())
	defer pub.Shutdown()

	finished, _ := hub.Start("synth", func(_ context.Context, emit func(TaskEvent)) { emit(TaskEvent{Status: "done"}) })
	for {
		if ev, ok := hub.Task(finished); ok && ev.Status == "done" {
			break
		}
	}
	stopped := make(chan string, 1)
	running, _ := hub.Start("synth", func(ctx context.Context, emit func(TaskEvent)) {
		emit(TaskEvent{Status: "running"})
		<-ctx.Done()
		stopped <- "cancelled"
		emit(TaskEvent{Status: "error", Message: ctx.Err().Error()})
	})
	for {
		if ev, ok := hub.Task(running); ok && ev.Status == "running" {
			break
		}
	}
	pub.PublishTask(TaskEvent{Op: "rebuild", ID: "rebuild-77", Status: "running"})

	for _, id := range []string{finished, "synth-99", "rebuild-77"} {
		if _, ok := hub.Cancel(id); ok {
			t.Fatalf("Cancel(%s) = ok, want refused", id)
		}
	}
	select {
	case <-stopped:
		t.Fatal("a refused Cancel cancelled the running task")
	case <-time.After(50 * time.Millisecond):
	}

	ev, ok := hub.Cancel(running)
	if !ok || ev.ID != running || ev.Status != "running" {
		t.Fatalf("Cancel(%s) = %+v %v, want the running task", running, ev, ok)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Cancel did not cancel the task's context")
	}
}
