package repos

import (
	"context"
	"sync"
	"testing"
	"time"

	"knomit/internal/config"
)

// TestDispatchRefresh_WaitsForInFlightGoroutine regresses the bug where
// goroutines launched by dispatchRefresh used context.Background() with
// a 5-minute timeout and were not joined on Manager.Close. After the
// fix the dispatcher's WaitGroup tracks each launched goroutine and
// disp.wg.Wait() blocks until they all return.
func TestDispatchRefresh_WaitsForInFlightGoroutine(t *testing.T) {
	// Replace the package-level refresh function with a controllable
	// stub so we can hold a goroutine open and observe wg behavior.
	block := make(chan struct{})
	started := make(chan struct{})
	origRefresh := runRefresh
	runRefresh = func(ctx context.Context, ri *RepoInstance, branch string, resolution float64, minCommunitySize int) error {
		close(started)
		<-block
		return nil
	}
	defer func() { runRefresh = origRefresh }()

	ri := NewTestInstance("test")
	disp := &clusterDispatcher{
		sem: make(chan struct{}, 1),
		wg:  &sync.WaitGroup{},
	}

	dispatchRefresh(ri, "agent/test", 1.0, 2, disp)

	// Wait for the goroutine to actually start before asserting wg
	// state — without an actual start there's no wg increment to
	// observe regardless of which implementation is in place.
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh goroutine never started")
	}

	// disp.wg.Wait() must block while the refresh holds open.
	waited := make(chan struct{})
	go func() {
		disp.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("disp.wg.Wait() returned while refresh goroutine still in flight — Close would race past it")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}

	// Release the goroutine; wg.Wait must now unblock.
	close(block)
	select {
	case <-waited:
		// expected
	case <-time.After(time.Second):
		t.Fatal("disp.wg.Wait() did not return after refresh goroutine completed")
	}
}

// TestDispatchRefresh_WaitGroupReleasedWhenSemPoolFull guards against a
// regression where the wg.Add lives outside the goroutine but wg.Done
// is only run on the happy path. If the pool-full early return ever
// drifts before the deferred wg.Done, stop() would hang forever after
// a single tick that overflows MaxConcurrent.
func TestDispatchRefresh_WaitGroupReleasedWhenSemPoolFull(t *testing.T) {
	// Stub refresh: signal start, then return immediately (will only
	// run for the first dispatch since we pre-fill the sem).
	origRefresh := runRefresh
	runRefresh = func(ctx context.Context, ri *RepoInstance, branch string, resolution float64, minCommunitySize int) error {
		return nil
	}
	defer func() { runRefresh = origRefresh }()

	ri := NewTestInstance("test")
	disp := &clusterDispatcher{
		sem: make(chan struct{}, 1),
		wg:  &sync.WaitGroup{},
	}
	// Pre-fill the sem so the dispatched goroutine takes the
	// "pool full → drop" branch.
	disp.sem <- struct{}{}

	dispatchRefresh(ri, "agent/test", 1.0, 2, disp)

	waited := make(chan struct{})
	go func() {
		disp.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		// expected: dropped goroutine still calls wg.Done via defer
	case <-time.After(time.Second):
		t.Fatal("disp.wg.Wait() blocked after pool-full drop — defer wg.Done is wired wrong")
	}
}

// TestParseClusterCheckerConfig_Resolution pins that the configurable Louvain
// resolution / min community size default (2.0 / 2) when unset and are honoured
// when overridden, so the background checker warms the SAME cache key the read
// path requests.
func TestParseClusterCheckerConfig_Resolution(t *testing.T) {
	def, err := parseClusterCheckerConfig(config.ClusterCacheConfig{})
	if err != nil {
		t.Fatalf("parse default: %v", err)
	}
	if def.Resolution != 2.0 {
		t.Fatalf("default Resolution: want 2.0, got %v", def.Resolution)
	}
	if def.MinCommunitySize != 2 {
		t.Fatalf("default MinCommunitySize: want 2, got %v", def.MinCommunitySize)
	}
	over, err := parseClusterCheckerConfig(config.ClusterCacheConfig{Resolution: 1.5, MinCommunitySize: 3})
	if err != nil {
		t.Fatalf("parse override: %v", err)
	}
	if over.Resolution != 1.5 {
		t.Fatalf("override Resolution: want 1.5, got %v", over.Resolution)
	}
	if over.MinCommunitySize != 3 {
		t.Fatalf("override MinCommunitySize: want 3, got %v", over.MinCommunitySize)
	}
}
