package embeddings

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Settles the batch-serialization cost premise: do two concurrent session.Run
// calls actually buy wall-clock, or does ORT's intra-op parallelism mean they
// only contend? Gated on KNOMIT_CONCURRENCY_BENCH.
func TestConcurrentVsSerializedInference(t *testing.T) {
	if os.Getenv("KNOMIT_CONCURRENCY_BENCH") == "" {
		t.Skip("set KNOMIT_CONCURRENCY_BENCH=1")
	}
	// Safety guard: the worst-case shape is ~1820 MiB per concurrent batch and
	// this box may also be serving. Abort rather than risk a second OOM.
	if avail := memAvailableMiB(t); avail < 5000 {
		t.Skipf("MemAvailable %d MiB is too low to run this safely", avail)
	}
	e := testEmbedder(t, "embeddinggemma")

	// 4 rows x 2048 tokens per batch: ~910 MiB each, so 4 concurrent stay well
	// inside this machine while the server is also running.
	// Far more words than MaxTokens, so encode() truncates to exactly 2048 —
	// the worst-case width, without needing a binary search for the boundary.
	var sb strings.Builder
	for i := range 6000 {
		fmt.Fprintf(&sb, "w%d ", i)
	}
	text := sb.String()
	rowsPerBatch := envInt("KNOMIT_BENCH_ROWS", 4)
	makeRows := func() []encodedRow {
		texts := make([]string, rowsPerBatch)
		for i := range texts {
			texts[i] = fmt.Sprintf("d%d %s", i, text)
		}
		return e.encodeAll(texts)
	}
	if _, err := e.runRows(makeRows()); err != nil { // warm the arena
		t.Fatal(err)
	}

	workers := envInt("KNOMIT_BENCH_WORKERS", 4)
	run := func(sem batchSem) time.Duration {
		start := time.Now()
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rows := makeRows()
				if _, err := embedInBatches(context.Background(), rows, 8192, sem, e.runRows); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		return time.Since(start)
	}

	concurrent := run(nil)
	serialized := run(newBatchSem())

	fmt.Printf("RESULT\tworkers=%d\tconcurrent=%.2fs\tserialized=%.2fs\tratio=%.2f\n",
		workers, concurrent.Seconds(), serialized.Seconds(),
		serialized.Seconds()/concurrent.Seconds())
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func memAvailableMiB(t *testing.T) int64 {
	t.Helper()
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 1 << 40 // not Linux; no guard available
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemAvailable:"); ok {
			f := strings.Fields(rest)
			n, _ := strconv.ParseInt(f[0], 10, 64)
			return n / 1024
		}
	}
	return 0
}
