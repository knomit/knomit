package repos

import (
	"sync"
	"testing"
	"time"
)

func TestObserverDebounce(t *testing.T) {
	var mu sync.Mutex
	var calls []string

	obs := newCommitObserver(50*time.Millisecond, func(hash string) {
		mu.Lock()
		calls = append(calls, hash)
		mu.Unlock()
	})
	defer obs.Stop()

	// Fire 5 rapid notifications — only the last should trigger the callback.
	for i := 0; i < 5; i++ {
		obs.Notify("hash-" + string(rune('a'+i)))
	}

	// Wait for debounce to fire.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected 1 debounced call, got %d: %v", len(calls), calls)
	}
	if calls[0] != "hash-e" {
		t.Fatalf("expected last hash 'hash-e', got %q", calls[0])
	}
}

func TestObserverStopFlushes(t *testing.T) {
	var mu sync.Mutex
	var calls []string

	obs := newCommitObserver(1*time.Second, func(hash string) {
		mu.Lock()
		calls = append(calls, hash)
		mu.Unlock()
	})

	obs.Notify("pending-hash")
	// Stop should flush the pending notification immediately.
	obs.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != "pending-hash" {
		t.Fatalf("expected Stop to flush pending, got %v", calls)
	}
}

// A notification that lands while the callback is still running must still be
// delivered. The callback is heavyweight (Acquire → SyncLocked →
// broadcastStatus), so after a big merge it runs for many seconds; every
// `learn` commit that lands in that window used to have its head silently
// dropped, leaving every browser tab pinned to the merge commit until it was
// reloaded (issue #178).
func TestObserverDeliversNotificationArrivingDuringCallback(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	blockFirst := true

	started := make(chan struct{})
	release := make(chan struct{})

	obs := newCommitObserver(20*time.Millisecond, func(hash string) {
		mu.Lock()
		calls = append(calls, hash)
		blocking := blockFirst
		blockFirst = false
		mu.Unlock()

		// Only the first run blocks — it is the one the second notification
		// has to arrive underneath.
		if blocking {
			close(started)
			<-release
		}
	})
	defer obs.Stop()

	obs.Notify("first")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("callback never ran for the first notification")
	}

	// Arrives while fn is still running, and the debounce timer fires during
	// that run — the exact window in which the head used to be discarded.
	obs.Notify("second")
	time.Sleep(60 * time.Millisecond)
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0] != "first" || calls[1] != "second" {
		t.Fatalf("expected the head that arrived mid-run to be delivered, got %v", calls)
	}
}
