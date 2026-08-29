package embeddings

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEmbedInBatches_SerializesBatchInference is the point of the semaphore.
// app.New builds ONE shared Embedder and lockBranch is per-branch, so a rebuild
// on branch A and a learn on branch B can otherwise be inside session.Run at
// the same time and their peak memory ADDS — measured at 4025 MiB across three
// repos where a single repo predicted ~2.5-3.5 GiB. Serializing batch inference
// is what turns the per-run budget into a per-process bound, which is what
// makes a memory-derived cap a guarantee rather than an expectation.
func TestEmbedInBatches_SerializesBatchInference(t *testing.T) {
	sem := newBatchSem()

	var inFlight, maxInFlight atomic.Int32
	run := func(batch []encodedRow) ([][]float32, error) {
		n := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if n <= old || maxInFlight.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond) // widen the window a racy impl would lose
		inFlight.Add(-1)
		return make([][]float32, len(batch)), nil
	}

	rows := make([]encodedRow, 8)
	for i := range rows {
		rows[i] = encodedRow{ids: make([]int64, 2048), mask: make([]int64, 2048)}
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := embedInBatches(context.Background(), rows, 4096, sem, run); err != nil {
				t.Errorf("embedInBatches: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := maxInFlight.Load(); got != 1 {
		t.Errorf("max concurrent session.Run = %d, want 1 — batch inference must be serialized", got)
	}
}

// TestEmbedInBatches_WaitingOnSemaphoreIsCancellable: a caller queued behind a
// long rebuild batch must observe cancellation while WAITING, not only before.
// Without this the semaphore would convert a cancellable wait into an
// unbounded one, which is a worse bug than the concurrency it fixes.
func TestEmbedInBatches_WaitingOnSemaphoreIsCancellable(t *testing.T) {
	sem := newBatchSem()
	sem <- struct{}{} // occupy it; nothing will release

	ctx, cancel := context.WithCancel(context.Background())
	rows := []encodedRow{{ids: make([]int64, 100), mask: make([]int64, 100)}}

	done := make(chan error, 1)
	go func() {
		_, err := embedInBatches(ctx, rows, 16384, sem, func([]encodedRow) ([][]float32, error) {
			t.Error("run must not execute while the semaphore is held")
			return nil, nil
		})
		done <- err
	}()

	time.Sleep(5 * time.Millisecond) // let it reach the wait
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling did not release a caller waiting on the semaphore")
	}
}

// TestEmbedInBatches_NilSemaphoreRuns keeps the packing logic usable without a
// semaphore, which is what the pure batching tests rely on.
func TestEmbedInBatches_NilSemaphoreRuns(t *testing.T) {
	rows := []encodedRow{{ids: make([]int64, 10), mask: make([]int64, 10)}}
	ran := false
	_, err := embedInBatches(context.Background(), rows, 16384, nil, func(b []encodedRow) ([][]float32, error) {
		ran = true
		return make([][]float32, len(b)), nil
	})
	if err != nil || !ran {
		t.Fatalf("err = %v, ran = %v", err, ran)
	}
}

// TestSingleShotPathsBypassTheSemaphore pins the SCOPING decision, which is the
// difference between this being a fix and a regression. Only BATCH inference is
// serialized: EmbedQuery and EmbedDocument go through embedBatch, which never
// touches the semaphore, so an interactive search never queues behind a rebuild
// batch. It contends for cores, as it always did — it does not block.
//
// Asserted structurally: with the semaphore fully held, the single-shot path
// must still reach inference. A zero-value Embedder has no ONNX session, so
// reaching inference panics — that panic IS the evidence it did not block, and
// a deadlock or timeout here would mean the bypass was lost.
func TestSingleShotPathsBypassTheSemaphore(t *testing.T) {
	e := &Embedder{model: Model{QueryTemplate: "search_query: {content}"}, batchSem: newBatchSem()}
	e.batchSem <- struct{}{} // held, never released

	reached := make(chan struct{})
	go func() {
		defer func() {
			_ = recover() // nil session — reaching it is the signal
			close(reached)
		}()
		_, _ = e.EmbedQuery(context.Background(), "anything")
	}()

	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("EmbedQuery blocked on the batch semaphore — single-shot paths must bypass it")
	}
}

// TestEmbedInBatches_PanicDoesNotLeakTheSemaphore: a panic inside inference
// must still release the gate. Leaking it would deadlock every subsequent
// batch for the life of the process — a far worse outcome than the crash that
// caused it, and one that would present as "embedding silently stopped".
func TestEmbedInBatches_PanicDoesNotLeakTheSemaphore(t *testing.T) {
	sem := newBatchSem()
	rows := []encodedRow{{ids: make([]int64, 100), mask: make([]int64, 100)}}

	func() {
		defer func() { _ = recover() }()
		_, _ = embedInBatches(context.Background(), rows, 16384, sem, func([]encodedRow) ([][]float32, error) {
			panic("inference exploded")
		})
	}()

	// The gate must be free again. Acquiring it must not block.
	select {
	case sem <- struct{}{}:
	case <-time.After(time.Second):
		t.Fatal("semaphore was not released after a panic — all later batches would deadlock")
	}
}
