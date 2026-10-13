//go:build windows

package cmd

import (
	"knomit/internal/serveraddr"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The Windows reload trigger: the server's named event, signalled the way
// `knomit reload` does. Runs in CI's Windows leg; there is no unix equivalent
// to compare against, so the assertions are on what is observed: the callback
// runs once per set, never without one, and a second server for the same home
// does not share the event.

func TestReloadEvent_RunsTheCallbackOncePerSet(t *testing.T) {
	home := t.TempDir()
	var n atomic.Int32
	got := make(chan struct{}, 8)
	stop, err := installReloadSignal(home, func() { n.Add(1); got <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Positive control for "never without a set": nothing has been set yet.
	time.Sleep(200 * time.Millisecond)
	if n.Load() != 0 {
		t.Fatalf("the callback ran %d times without a signal", n.Load())
	}
	for i := 1; i <= 3; i++ {
		if err := serveraddr.SignalReload(home, 0); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
		select {
		case <-got:
		case <-time.After(5 * time.Second):
			t.Fatalf("set %d never reached the callback", i)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n.Load() != 3 {
		t.Fatalf("callback ran %d times for 3 sets", n.Load())
	}
}

func TestReloadEvent_IsKeyedByHomeAndNotShared(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	var na, nb atomic.Int32
	stopA, err := installReloadSignal(a, func() { na.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	defer stopA()
	stopB, err := installReloadSignal(b, func() { nb.Add(1) })
	if err != nil {
		t.Fatalf("a second home must get its own event: %v", err)
	}
	defer stopB()

	// A second server for home a does not take over its event.
	if _, err := installReloadSignal(a, func() {}); err == nil || !strings.Contains(err.Error(), "held by another process") {
		t.Fatalf("a second installer for the same home: %v", err)
	}
	if err := serveraddr.SignalReload(a, 0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for na.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if na.Load() != 1 || nb.Load() != 0 {
		t.Fatalf("home a ran %d, home b ran %d; want 1 and 0", na.Load(), nb.Load())
	}
}

func TestReloadEvent_NoServerIsAnError(t *testing.T) {
	if err := serveraddr.SignalReload(t.TempDir(), 0); err == nil || !strings.Contains(err.Error(), "no running knomit serve") {
		t.Fatalf("signalling a home with no server: %v", err)
	}
}

func TestReloadEvent_StopReleasesTheName(t *testing.T) {
	home := t.TempDir()
	stop, err := installReloadSignal(home, func() {})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	stop2, err := installReloadSignal(home, func() {})
	if err != nil {
		t.Fatalf("the event name was not released by stop: %v", err)
	}
	stop2()
}
