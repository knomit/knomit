package embeddings

// Memory measurement harness. Skipped unless KNOMIT_MEASURE_BATCH and
// KNOMIT_MEASURE_SEQ are set, so it never runs in CI.
//
// It lives in the repo because the numbers it produces are load-bearing: the
// budget ladder in budget.go, minRowCharge, and the resident-model constant are
// all measured, and every one of them will need re-measuring when the model or
// the runtime changes. A harness kept outside version control is one that will
// not exist when that day comes.
//
// RUN ONE PROCESS PER CONFIGURATION. The ONNX arena retains its high-water
// mark, so a single process measuring several shapes in sequence reports only a
// monotonic maximum and every configuration after the first is contaminated:
//
//	ORT_LIB_PATH=dist/<platform>/lib/libonnxruntime.so \
//	KNOMIT_EMBED_TEST_CACHE=~/.knomit/models \
//	KNOMIT_MEASURE_BATCH=8 KNOMIT_MEASURE_SEQ=2048 \
//	go test ./internal/embeddings/ -run TestMeasurePeakRSS -v

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// procKB reads one VmXXX field (in kB) from /proc/self/status.
func procKB(t *testing.T, field string) int {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, field+":") {
			f := strings.Fields(ln)
			n, err := strconv.Atoi(f[1])
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("%s not found in /proc/self/status", field)
	return 0
}

// resetPeakRSS resets VmHWM to the current VmRSS (clear_refs type 5).
func resetPeakRSS(t *testing.T) {
	t.Helper()
	if err := os.WriteFile("/proc/self/clear_refs", []byte("5\n"), 0); err != nil {
		t.Fatalf("reset VmHWM: %v", err)
	}
}

// textOfTokens builds a text that encodes to exactly want tokens (or as close
// as the tokenizer allows), by binary-searching the word count.
func textOfTokens(t *testing.T, e *Embedder, want int) string {
	t.Helper()
	words := make([]string, 0, want*2)
	for i := 0; len(words) < want*3; i++ {
		words = append(words, fmt.Sprintf("w%d", i))
	}
	build := func(n int) string { return strings.Join(words[:n], " ") }
	lo, hi := 1, len(words)
	for lo < hi {
		mid := (lo + hi) / 2
		ids, _ := e.encode(build(mid))
		if len(ids) < want {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	got := build(lo)
	ids, _ := e.encode(got)
	t.Logf("target %d tokens -> %d words -> %d tokens", want, lo, len(ids))
	return got
}

// TestMeasurePeakRSS runs ONE (batch, seq) config and reports peak RSS.
// Env: KNOMIT_MEASURE_BATCH, KNOMIT_MEASURE_SEQ.
func TestMeasurePeakRSS(t *testing.T) {
	batchS, seqS := os.Getenv("KNOMIT_MEASURE_BATCH"), os.Getenv("KNOMIT_MEASURE_SEQ")
	if batchS == "" || seqS == "" {
		t.Skip("set KNOMIT_MEASURE_BATCH and KNOMIT_MEASURE_SEQ")
	}
	batch, err := strconv.Atoi(batchS)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := strconv.Atoi(seqS)
	if err != nil {
		t.Fatal(err)
	}

	e := testEmbedder(t, "embeddinggemma")

	// Warm up so the ONNX arena, weights and tokenizer are all resident. The
	// number we want is the ADDITIONAL memory a batch needs, not the model load.
	if _, err := e.embedBatch([]string{"warmup"}); err != nil {
		t.Fatal(err)
	}

	text := textOfTokens(t, e, seq)
	texts := make([]string, batch)
	for i := range texts {
		// Distinct prefixes so no dedup/caching can collapse the rows.
		texts[i] = fmt.Sprintf("d%d %s", i, text)
	}

	baseKB := procKB(t, "VmRSS")
	resetPeakRSS(t)

	if _, err := e.embedBatch(texts); err != nil {
		t.Fatalf("embedBatch(batch=%d seq=%d): %v", batch, seq, err)
	}

	peakKB := procKB(t, "VmHWM")
	mib := func(kb int) float64 { return float64(kb) / 1024 }
	fmt.Printf("RESULT\tbatch=%d\tseq=%d\tbase_MiB=%.0f\tpeak_MiB=%.0f\tdelta_MiB=%.0f\n",
		batch, seq, mib(baseKB), mib(peakKB), mib(peakKB-baseKB))
}
