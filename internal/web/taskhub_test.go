package web

import (
	"context"
	"testing"
	"time"
)

func TestTaskHub_StartAndEmit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewTaskHub(ctx)

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	events, snapshot := hub.Subscribe(subCtx)
	if len(snapshot) != 0 {
		t.Fatalf("expected empty snapshot, got %d events", len(snapshot))
	}

	id, err := hub.Start("synth", func(ctx context.Context, emit func(TaskEvent)) {
		emit(TaskEvent{Status: "running", Phase: "llm", Message: "chunk 1/2"})
		emit(TaskEvent{Status: "done", Message: "complete"})
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if id != "synth-1" {
		t.Fatalf("expected synth-1, got %s", id)
	}

	// Collect events
	var got []TaskEvent
	timeout := time.After(2 * time.Second)
	for {
		select {
		case e := <-events:
			ev := e.(TaskEvent)
			got = append(got, ev)
			if ev.Status == "done" || ev.Status == "error" {
				goto done
			}
		case <-timeout:
			t.Fatal("timed out waiting for events")
		}
	}
done:
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	if got[0].Op != "synth" || got[0].ID != "synth-1" || got[0].Status != "running" {
		t.Errorf("event 0: %+v", got[0])
	}
	if got[1].Status != "done" {
		t.Errorf("event 1: %+v", got[1])
	}
}

func TestTaskHub_409OnDuplicateOp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewTaskHub(ctx)

	started := make(chan struct{})
	done := make(chan struct{})

	_, err := hub.Start("sync", func(ctx context.Context, emit func(TaskEvent)) {
		emit(TaskEvent{Status: "running", Message: "syncing"})
		close(started)
		<-done // block until test releases
		emit(TaskEvent{Status: "done", Message: "ok"})
	})
	if err != nil {
		t.Fatalf("first Start failed: %v", err)
	}

	<-started // wait for task to be running

	_, err = hub.Start("sync", func(ctx context.Context, emit func(TaskEvent)) {})
	if err == nil {
		t.Fatal("expected error for duplicate op, got nil")
	}

	close(done) // let first task finish
}

func TestTaskHub_ConcurrentDifferentOps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewTaskHub(ctx)

	done1 := make(chan struct{})
	done2 := make(chan struct{})

	_, err1 := hub.Start("sync", func(ctx context.Context, emit func(TaskEvent)) {
		emit(TaskEvent{Status: "running"})
		<-done1
		emit(TaskEvent{Status: "done"})
	})
	_, err2 := hub.Start("synth", func(ctx context.Context, emit func(TaskEvent)) {
		emit(TaskEvent{Status: "running"})
		<-done2
		emit(TaskEvent{Status: "done"})
	})

	if err1 != nil || err2 != nil {
		t.Fatalf("expected both to start, got err1=%v err2=%v", err1, err2)
	}

	close(done1)
	close(done2)
}

func TestTaskHub_SnapshotAfterDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewTaskHub(ctx)

	done := make(chan struct{})
	hub.Start("synth", func(ctx context.Context, emit func(TaskEvent)) {
		emit(TaskEvent{Status: "running", Message: "go"})
		emit(TaskEvent{Status: "done", Message: "finished"})
		close(done)
	})
	<-done
	// Small sleep to let emit() finish processing
	time.Sleep(50 * time.Millisecond)

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	_, snapshot := hub.Subscribe(subCtx)

	if len(snapshot) != 1 {
		t.Fatalf("expected 1 snapshot event, got %d", len(snapshot))
	}
	if snapshot[0].Op != "synth" || snapshot[0].Status != "done" {
		t.Errorf("snapshot: %+v", snapshot[0])
	}
}

func TestTaskHub_EmitIgnoredAfterTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewTaskHub(ctx)

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	events, _ := hub.Subscribe(subCtx)

	done := make(chan struct{})
	hub.Start("synth", func(ctx context.Context, emit func(TaskEvent)) {
		emit(TaskEvent{Status: "done", Message: "finished"})
		emit(TaskEvent{Status: "running", Message: "should be ignored"})
		close(done)
	})
	<-done
	time.Sleep(50 * time.Millisecond)

	// Should only get 1 event (the done), not the running after it
	select {
	case e := <-events:
		ev := e.(TaskEvent)
		if ev.Status != "done" {
			t.Errorf("expected done, got %+v", ev)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for event")
	}

	// No more events
	select {
	case e := <-events:
		t.Fatalf("unexpected extra event: %+v", e)
	case <-time.After(100 * time.Millisecond):
		// good — no extra events
	}
}

func TestTaskHub_Shutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewTaskHub(ctx)

	taskCtxDone := make(chan struct{})
	hub.Start("sync", func(ctx context.Context, emit func(TaskEvent)) {
		emit(TaskEvent{Status: "running"})
		<-ctx.Done()
		close(taskCtxDone)
	})

	// Give task goroutine time to start
	time.Sleep(50 * time.Millisecond)

	hub.Shutdown()

	select {
	case <-taskCtxDone:
		// good — task context was cancelled
	case <-time.After(2 * time.Second):
		t.Fatal("task context was not cancelled after Shutdown")
	}
}
