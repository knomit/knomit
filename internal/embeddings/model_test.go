package embeddings

import (
	"strings"
	"testing"

	"knomit/internal/embeddings/params"
)

// TestModelThresholdsPopulated asserts every registered model carries a full,
// non-zero set of cosine thresholds. A zero Dedup would merge everything
// (data loss); a zero SimilarTo would make the graph nearly complete — so a
// missing value is a real bug, not a harmless default.
func TestModelThresholdsPopulated(t *testing.T) {
	for _, id := range IDs() {
		m, _ := Lookup(id)
		th := m.Thresholds
		if th.Dedup <= 0 || th.ReflectNovelty <= 0 || th.SimilarTo <= 0 ||
			th.SearchFloor <= 0 || th.RerankHigh <= 0 || th.RerankLow <= 0 {
			t.Errorf("%q: incomplete thresholds: %+v", id, th)
		}
		if th.RerankHigh <= th.RerankLow {
			t.Errorf("%q: RerankHigh %.3f must exceed RerankLow %.3f", id, th.RerankHigh, th.RerankLow)
		}
	}
}

// TestNomicKeepsDefaults: nomic was the original default, so its thresholds are
// the historical literals (params.Defaults). This is the baseline the other
// models' values were ported FROM.
func TestNomicKeepsDefaults(t *testing.T) {
	m, _ := Lookup("nomic-v1.5")
	if m.Thresholds != params.Defaults() {
		t.Errorf("nomic thresholds = %+v, want params.Defaults() %+v", m.Thresholds, params.Defaults())
	}
}

// TestEmbeddingGemmaThresholdsAreCooler regresses the calibration finding:
// EmbeddingGemma's cosine distribution runs markedly cooler than nomic's, so
// EVERY cutoff must be strictly below the nomic default. Shipping nomic's 0.92
// dedup under gemma would (almost) never fire — silent duplicate accumulation.
func TestEmbeddingGemmaThresholdsAreCooler(t *testing.T) {
	g, _ := Lookup("embeddinggemma")
	n := params.Defaults()
	pairs := []struct {
		name       string
		gemma, nom float64
	}{
		{"Dedup", g.Thresholds.Dedup, n.Dedup},
		{"ReflectNovelty", g.Thresholds.ReflectNovelty, n.ReflectNovelty},
		{"SimilarTo", g.Thresholds.SimilarTo, n.SimilarTo},
		{"SearchFloor", g.Thresholds.SearchFloor, n.SearchFloor},
		{"RerankHigh", g.Thresholds.RerankHigh, n.RerankHigh},
		{"RerankLow", g.Thresholds.RerankLow, n.RerankLow},
	}
	for _, p := range pairs {
		if !(p.gemma < p.nom) {
			t.Errorf("embeddinggemma %s = %.3f, must be below nomic %.3f", p.name, p.gemma, p.nom)
		}
	}
	// Dedup must stay inside the validated safety gap (distinct p99 ~0.77,
	// true near-dup p05 ~0.96): high enough not to merge distinct facts, low
	// enough to still catch real duplicates.
	if g.Thresholds.Dedup < 0.77 || g.Thresholds.Dedup > 0.95 {
		t.Errorf("embeddinggemma Dedup %.3f outside calibrated safety gap [0.77, 0.95]", g.Thresholds.Dedup)
	}
}

func TestLookupKnownModels(t *testing.T) {
	for _, id := range []string{"embeddinggemma", "nomic-v1.5"} {
		m, err := Lookup(id)
		if err != nil {
			t.Fatalf("Lookup(%q) error: %v", id, err)
		}
		if m.ID != id {
			t.Errorf("ID = %q, want %q", m.ID, id)
		}
		if m.Dim <= 0 || m.TokenizerURL == "" || m.ModelURL == "" {
			t.Errorf("%q: incomplete descriptor: %+v", id, m)
		}
		if len(m.ONNXInputs) == 0 || len(m.ONNXOutputs) == 0 {
			t.Errorf("%q: missing ONNX I/O names", id)
		}
	}
}

// TestModelsHaveTokenCap guards the truncation safety net: every registered
// model must declare a positive MaxTokens so an oversized fact is capped before
// it can exceed the graph's max position embeddings.
func TestModelsHaveTokenCap(t *testing.T) {
	for _, id := range IDs() {
		m, _ := Lookup(id)
		if m.MaxTokens <= 0 {
			t.Errorf("%q: MaxTokens = %d, want > 0", id, m.MaxTokens)
		}
	}
}

// TestQwenRemoved asserts the untested qwen3-0.6b descriptor stays out of the
// live registry; it was removed because its ONNX I/O and pooling were never
// verified against the real graph.
func TestQwenRemoved(t *testing.T) {
	if _, err := Lookup("qwen3-0.6b"); err == nil {
		t.Error("qwen3-0.6b should not be selectable (unverified model)")
	}
}

func TestLookupUnknownListsValidIDs(t *testing.T) {
	_, err := Lookup("does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
	for _, id := range []string{"embeddinggemma", "nomic-v1.5"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error %q should list valid id %q", err.Error(), id)
		}
	}
}

func TestEmbeddingGemmaDescriptorShape(t *testing.T) {
	m, _ := Lookup("embeddinggemma")
	if m.Pooling != PoolNone {
		t.Errorf("gemma Pooling = %v, want PoolNone (graph pre-pools)", m.Pooling)
	}
	if m.Dim != 768 {
		t.Errorf("gemma Dim = %d, want 768", m.Dim)
	}
	if m.ONNXOutputs[0] != "sentence_embedding" {
		t.Errorf("gemma output = %q, want sentence_embedding", m.ONNXOutputs[0])
	}
}
