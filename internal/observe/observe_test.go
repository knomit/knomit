package observe

import (
	"sync"
	"testing"
	"time"
)

func TestObserverDebounce(t *testing.T) {
	var mu sync.Mutex
	var calls []string

	obs := New(50*time.Millisecond, func(hash string) {
		mu.Lock()
		calls = append(calls, hash)
		mu.Unlock()
	})
	defer obs.Stop()

	// Fire 5 rapid notifications — only the last should trigger the callback.
	for i := 0; i < 5; i++ {
		obs.Notify("hash-" + string(rune('a'+i)))
	}

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

	obs := New(1*time.Second, func(hash string) {
		mu.Lock()
		calls = append(calls, hash)
		mu.Unlock()
	})

	obs.Notify("pending-hash")
	obs.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != "pending-hash" {
		t.Fatalf("expected Stop to flush pending, got %v", calls)
	}
}

func TestObserverStopAfterFired(t *testing.T) {
	var mu sync.Mutex
	var calls []string

	obs := New(20*time.Millisecond, func(hash string) {
		mu.Lock()
		calls = append(calls, hash)
		mu.Unlock()
	})

	obs.Notify("fire-me")
	time.Sleep(60 * time.Millisecond) // let it fire naturally

	obs.Stop() // stop with no pending — should not double-call

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 call, got %d: %v", len(calls), calls)
	}
}

func TestObserverNoCallAfterStop(t *testing.T) {
	var mu sync.Mutex
	var calls []string

	obs := New(200*time.Millisecond, func(hash string) {
		mu.Lock()
		calls = append(calls, hash)
		mu.Unlock()
	})

	obs.Notify("hash1")
	obs.Stop() // flush synchronously

	// Any further Notify after Stop must be ignored.
	obs.Notify("hash2")
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != "hash1" {
		t.Fatalf("expected only hash1, got %v", calls)
	}
}
