package embeddings

import (
	"runtime"
	"sync"
	"testing"
)

// rss.go is silent by design — every failure path returns without logging. That
// makes it exactly the kind of diagnostic that can be broken for months without
// anyone noticing, and it is not decorative: nonEmbeddingReserve is a 1 GiB
// placeholder that sizes every derived budget, and this log is the documented
// mechanism for replacing it with a measurement. If it never fires, the
// placeholder becomes permanent by default.

func TestProcessRSSBytes_ReportsSomethingPlausible(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs is Linux-only")
	}
	got, err := processRSSBytes()
	if err != nil {
		t.Fatalf("processRSSBytes: %v — the RSS diagnostic is silent on failure, so a "+
			"broken reader would never surface at runtime", err)
	}
	// A running Go test process is comfortably above 1 MiB and far below 1 TiB;
	// the point is to catch a unit error (kB read as bytes) or a parse landing
	// on the wrong field, not to assert a specific footprint.
	const mib = int64(1) << 20
	if got < mib || got > (1<<40) {
		t.Errorf("processRSSBytes = %d bytes, implausible for this process — "+
			"suspect a kB/bytes confusion or the wrong /proc field", got)
	}
}

// TestFirstRunRSS_FiresExactlyOnce pins the sync.Once contract under the
// concurrency the embedder actually sees. The measurement is only meaningful
// before the FIRST inference — logging it again later would report the arena's
// retained high-water mark and quietly contradict the fact it exists to record.
func TestFirstRunRSS_FiresExactlyOnce(t *testing.T) {
	var e Embedder
	var mu sync.Mutex
	calls := 0

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.firstRunRSS.Do(func() {
				mu.Lock()
				calls++
				mu.Unlock()
			})
		}()
	}
	wg.Wait()

	if calls != 1 {
		t.Errorf("first-run hook fired %d times, want exactly 1", calls)
	}
}
