package embeddings

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocText_TitleConcatWhenNoSlot regresses the title-handling fix: when the
// model's DocTemplate has a {title} slot the title fills it; when it does not,
// the title is prepended to the content (the historical behavior) so its signal
// is never silently dropped. Pure unit test — no ONNX needed.
func TestDocText_TitleConcatWhenNoSlot(t *testing.T) {
	withSlot := &Embedder{model: Model{DocTemplate: "title: {title} | text: {content}"}}
	if got := withSlot.docText("T", "body"); got != "title: T | text: body" {
		t.Errorf("with {title} slot: got %q", got)
	}
	noSlot := &Embedder{model: Model{DocTemplate: "search_document: {content}"}}
	if got := noSlot.docText("T", "body"); got != "search_document: T body" {
		t.Errorf("no {title} slot, titled doc: got %q, want title prepended", got)
	}
	if got := noSlot.docText("", "body"); got != "search_document: body" {
		t.Errorf("no {title} slot, untitled doc: got %q", got)
	}
}

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
	e, err := NewEmbedder(context.Background(), m, cache)
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

// TestEmbedderTruncatesLongInput regresses the truncation cap: a document far
// longer than the model's MaxTokens must be truncated and still embed to a
// valid unit vector, not crash inference. (CI-gated on KNOMIT_EMBED_TEST_CACHE.)
func TestEmbedderTruncatesLongInput(t *testing.T) {
	e := testEmbedder(t, "embeddinggemma")
	long := strings.Repeat("knomit stores facts as markdown with frontmatter. ", 4000) // ≫ MaxTokens
	v, err := e.EmbedDocument("big fact", long)
	if err != nil {
		t.Fatalf("oversized doc must truncate, not error: %v", err)
	}
	if len(v) != e.Dim() {
		t.Fatalf("dim = %d, want %d", len(v), e.Dim())
	}
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(n)-1.0) > 1e-3 {
		t.Errorf("truncated embedding not unit-normalized: |v| = %f", math.Sqrt(n))
	}
}

// TestEmbedDocumentsBatchMatchesSingle regresses the batched ONNX path: the
// vectors from a batched EmbedDocuments call must match those from per-document
// EmbedDocument calls (padding must not change a row's result).
// (CI-gated on KNOMIT_EMBED_TEST_CACHE.)
func TestEmbedDocumentsBatchMatchesSingle(t *testing.T) {
	e := testEmbedder(t, "embeddinggemma")
	titles := []string{"alpha", "beta is a longer title than the others here", "g"}
	bodies := []string{"short", "a considerably longer body so rows differ in length and padding kicks in", "x"}
	batch, err := e.EmbedDocuments(titles, bodies)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != len(titles) {
		t.Fatalf("batch returned %d vectors, want %d", len(batch), len(titles))
	}
	for i := range titles {
		single, err := e.EmbedDocument(titles[i], bodies[i])
		if err != nil {
			t.Fatal(err)
		}
		if cos := cosTest(batch[i], single); cos < 0.9999 {
			t.Errorf("row %d: batched vs single cosine %.5f, want ≈1.0 (padding changed the result)", i, cos)
		}
	}
}
