//go:build desktop

package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// The reason serverBoot exists: the caller must be able to build and show the
// UI while boot is still running, so startServerBoot must return immediately.
func TestStartServerBootDoesNotBlockTheCaller(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	returned := make(chan struct{})
	go func() {
		startServerBoot(context.Background(), func(context.Context) (string, func(), error) {
			<-release
			return "", nil, nil
		})
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("startServerBoot blocked until boot finished")
	}
}

func TestServerBootWaitReturnsTheAPIBase(t *testing.T) {
	b := startServerBoot(context.Background(), func(context.Context) (string, func(), error) {
		return "http://127.0.0.1:19278", func() {}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	base, err := b.wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if base != "http://127.0.0.1:19278" {
		t.Errorf("base = %q, want the booted address", base)
	}
}

func TestServerBootWaitSurfacesTheBootError(t *testing.T) {
	want := errors.New("embedder init failed")
	b := startServerBoot(context.Background(), func(context.Context) (string, func(), error) {
		return "", nil, want
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := b.wait(ctx); !errors.Is(err, want) {
		t.Errorf("wait error = %v, want %v", err, want)
	}
}

// wait is reached from a /config.js request. A boot that never settles must not
// pin that request forever — the caller's context has to win.
func TestServerBootWaitHonoursTheCallersContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	b := startServerBoot(context.Background(), func(context.Context) (string, func(), error) {
		<-release
		return "", nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := b.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wait error = %v, want DeadlineExceeded", err)
	}
}

// Wails' OnShutdown fires AND run() calls stop after Run returns. Tearing the
// server down twice would double-close the listener and re-run app cleanup.
func TestServerBootStopTearsDownExactlyOnce(t *testing.T) {
	var stops atomic.Int32
	b := startServerBoot(context.Background(), func(context.Context) (string, func(), error) {
		return "http://127.0.0.1:19278", func() { stops.Add(1) }, nil
	})

	b.stop()
	b.stop()
	b.stop()

	if got := stops.Load(); got != 1 {
		t.Errorf("teardown ran %d times, want 1", got)
	}
}

// Quit during boot: the teardown function does not exist until boot returns, so
// stop has to wait for it. Not waiting would leak the server and its lockfile.
func TestServerBootStopWaitsForAnInFlightBoot(t *testing.T) {
	release := make(chan struct{})
	var stopped atomic.Bool
	b := startServerBoot(context.Background(), func(context.Context) (string, func(), error) {
		<-release
		return "http://127.0.0.1:19278", func() { stopped.Store(true) }, nil
	})

	done := make(chan struct{})
	go func() { b.stop(); close(done) }()

	select {
	case <-done:
		t.Fatal("stop returned before the in-flight boot settled")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop never returned after boot settled")
	}
	if !stopped.Load() {
		t.Error("the booted server was not torn down")
	}
}

// A boot that failed started nothing, so there is nothing to tear down — and
// stop must not panic on the nil teardown it was handed.
func TestServerBootStopIsSafeAfterAFailedBoot(t *testing.T) {
	b := startServerBoot(context.Background(), func(context.Context) (string, func(), error) {
		return "", nil, errors.New("boom")
	})
	b.stop() // must not panic
}

// shortStopGrace shrinks the give-up window for the duration of one test, so
// the branch below is reachable in milliseconds rather than ten seconds.
func shortStopGrace(t *testing.T, d time.Duration) {
	t.Helper()
	prev := stopGrace
	stopGrace = d
	t.Cleanup(func() { stopGrace = prev })
}

// The give-up branch, which nothing exercised before: a boot that never settles
// must not pin Quit forever, so stop returns once stopGrace elapses.
//
// It returns WITHOUT running any teardown — it cannot, the teardown function
// does not exist until boot returns — and that is precisely why
// NativeService.releaseInstance calls lockfile.Remove explicitly rather than
// trusting stop() to have removed it. If this branch ever starts running a
// teardown, re-read the releaseInstance field comment in native.go before
// deleting that call.
func TestServerBootStopGivesUpOnAWedgedBoot(t *testing.T) {
	shortStopGrace(t, 20*time.Millisecond)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // let the boot goroutine exit with the test
	var tornDown atomic.Bool
	b := startServerBoot(context.Background(), func(context.Context) (string, func(), error) {
		<-release
		return "http://127.0.0.1:19278", func() { tornDown.Store(true) }, nil
	})

	done := make(chan struct{})
	go func() { b.stop(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop never returned; a wedged boot must not pin Quit forever")
	}
	if tornDown.Load() {
		t.Error("stop ran a teardown it could not have had; the boot had not returned one")
	}
}

// The condition native.go:releaseInstance is documented as depending on: once
// stop has given up, the LATER OnShutdown-driven call must still be a no-op.
// Were the Once ever released on the timeout path, that second call could run
// a teardown — and its lockfile.Remove — after a REPLACEMENT instance had
// already written its own lockfile, deleting the new process's lock instead of
// this one's.
func TestServerBootStopStaysSpentAfterGivingUp(t *testing.T) {
	shortStopGrace(t, 20*time.Millisecond)

	settle := make(chan struct{})
	var teardowns atomic.Int32
	b := startServerBoot(context.Background(), func(context.Context) (string, func(), error) {
		<-settle
		return "http://127.0.0.1:19278", func() { teardowns.Add(1) }, nil
	})

	b.stop() // gives up: the boot has not settled

	// The boot now settles, so a teardown genuinely exists. The second stop
	// must still decline to run it.
	close(settle)
	<-b.done
	b.stop()

	if n := teardowns.Load(); n != 0 {
		t.Errorf("teardown ran %d time(s) after stop had already given up; sync.Once must stay spent", n)
	}
}
