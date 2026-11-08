package events

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ysmood/goob"
)

// A TERMINAL IS RECORDED BEFORE IT IS PUBLISHED (PR #623 F1). A Subscribe
// racing a task's terminal PublishTask must either find the terminal in its
// snapshot, or find the task running AND then receive the terminal on its
// channel. Published first, a Subscribe in the window between the publish
// and the bookkeeping saw the task running and never got its end, and a
// per-job events stream hung. There is no seam to stop the publish in that
// window, so this is a bounded stress loop: the publish-first order failed it
// within a few hundred iterations.
func TestTaskHub_SubscribeRacingATerminalNeverMissesIt(t *testing.T) {
	const iterations = 2000
	for i := 0; i < iterations; i++ {
		hub, pub := NewTaskHub(context.Background())
		id := fmt.Sprintf("rebuild-%d", i+1)
		pub.PublishTask(TaskEvent{Op: "rebuild", ID: id, Status: "running"})

		// Subscribe over and over while the terminal is published, until a
		// snapshot shows it: every subscription whose snapshot still showed
		// the task running must then receive the terminal.
		ctx, cancel := context.WithCancel(context.Background())
		var sawRunning []goob.Events
		go pub.PublishTask(TaskEvent{Op: "rebuild", ID: id, Status: "done"})
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
