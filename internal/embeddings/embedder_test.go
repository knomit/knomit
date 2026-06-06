package embeddings

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// testEmbedder builds an embedder if the model is cached under
// KNOMIT_EMBED_TEST_CACHE; otherwise it skips.
func testEmbedder(t *testing.T, id string) *Embedder {
	t.Helper()
	cache := os.Getenv("KNOMIT_EMBED_TEST_CACHE")
	if cache == "" {
		t.Skip("set KNOMIT_EMBED_TEST_CACHE to a dir with the model cached")
	}
	m, err := Lookup(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(cache, id, filepath.Base(m.ModelURL))); statErr != nil {
		t.Skipf("model %q not cached under %s", id, cache)
	}
	e, err := NewEmbedder(m, cache)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func TestEmbedderProducesUnitVector(t *testing.T) {
	e := testEmbedder(t, "embeddinggemma")
	v, err := e.EmbedQuery("how does knomit store fact embeddings?")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != e.Dim() {
		t.Fatalf("dim = %d, want %d", len(v), e.Dim())
	}
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(n)-1.0) > 1e-3 {
		t.Errorf("not unit-normalized: |v| = %f", math.Sqrt(n))
	}
}

func TestEmbedderRetrievalSeparation(t *testing.T) {
	e := testEmbedder(t, "embeddinggemma")
	q, _ := e.EmbedQuery("how does knomit store fact embeddings?")
	rel, _ := e.EmbedDocument("storage", "Knomit stores 768-dim fact vectors in a sqlite-vec vec0 table by cosine.")
	irr, _ := e.EmbedDocument("tui", "The TUI uses '/' for search and ':' for commands.")
	if cosTest(q, rel) <= cosTest(q, irr) {
		t.Errorf("relevant cos %.3f should exceed irrelevant cos %.3f", cosTest(q, rel), cosTest(q, irr))
	}
}

func cosTest(a, b []float32) float64 {
	var d float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
	}
	return d
}
