package oauth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// N1 (3a review, carried into 3b): /oauth/authorize is public and runs the
// CIMD fetch before any cap applies, so anyone could make the instance issue
// unbounded concurrent outbound fetches. Two bounds close it: at most
// maxCIMDFetches fetches in flight across all client ids, and concurrent
// resolutions of the SAME id share one fetch.

// slowFixture serves every path as a valid document for that path, after
// holding the request until release is closed, and records how many are in
// flight at once.
func slowFixture(t *testing.T) (f *cimdFixture, release chan struct{}, inFlight, peak *atomic.Int32) {
	f = newCIMDFixture(t)
	release = make(chan struct{})
	inFlight, peak = new(atomic.Int32), new(atomic.Int32)
	base := strings.TrimSuffix(f.url, "/cimd.json")
	f.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.document(base + r.URL.Path)))
	}))
	return f, release, inFlight, peak
}

func TestResolver_BoundsConcurrentFetches(t *testing.T) {
	f, release, inFlight, peak := slowFixture(t)
	r := newResolverFor(f.fetcher(), &clock{t: time.Unix(1_790_000_000, 0)})
	base := strings.TrimSuffix(f.url, "/cimd.json")
	var wg sync.WaitGroup
	const callers = 3 * maxCIMDFetches
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = r.Resolve(context.Background(), fmt.Sprintf("%s/client-%d.json", base, i))
		}(i)
	}
	// Let every caller reach the fetcher, then look at the peak.
	deadline := time.Now().Add(2 * time.Second)
	for inFlight.Load() < maxCIMDFetches && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := peak.Load(); got != maxCIMDFetches {
		t.Errorf("peak concurrent fetches = %d, want exactly %d", got, maxCIMDFetches)
	}
	close(release)
	wg.Wait()
	if got := f.hits.Load(); got != callers {
		t.Errorf("fetches = %d, want %d (every distinct id is still fetched, just not all at once)", got, callers)
	}
}

func TestResolver_SameIDSharesOneFetch(t *testing.T) {
	f, release, inFlight, _ := slowFixture(t)
	r := newResolverFor(f.fetcher(), &clock{t: time.Unix(1_790_000_000, 0)})
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Resolve(context.Background(), f.url)
			errs <- err
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for inFlight.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a shared resolution failed: %v", err)
		}
	}
	if got := f.hits.Load(); got != 1 {
		t.Fatalf("10 concurrent resolutions of one id fetched %d times, want 1", got)
	}
}

// A caller that gives up does not cancel the shared fetch for the others.
func TestResolver_SharedFetchSurvivesACancelledCaller(t *testing.T) {
	f, release, inFlight, _ := slowFixture(t)
	r := newResolverFor(f.fetcher(), &clock{t: time.Unix(1_790_000_000, 0)})
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := r.Resolve(ctx, f.url); first <- err }()
	deadline := time.Now().Add(2 * time.Second)
	for inFlight.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	second := make(chan error, 1)
	go func() { _, err := r.Resolve(context.Background(), f.url); second <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-first
	close(release)
	if err := <-second; err != nil {
		t.Fatalf("the second caller failed because the first gave up: %v", err)
	}
}
