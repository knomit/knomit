package cmd

import (
	"sync"
	"testing"
	"time"

	"knomit/internal/observe"
)

func TestObserverDebounce(t *testing.T) {
	var mu sync.Mutex
	var calls []string

	obs := observe.New(50*time.Millisecond, func(hash string) {
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

	obs := observe.New(1*time.Second, func(hash string) {
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
