package embeddings_test

import (
	"testing"

	"knomit/internal/embeddings"
)

func TestEmbedder(t *testing.T) {
	e, err := embeddings.NewEmbedder("testdata/all-MiniLM-L6-v2.onnx", "testdata/tokenizer.json")
	if err != nil {
		t.Skip("model not available:", err)
	}
	defer e.Close()

	vec, err := e.Embed("hello world")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 384 {
		t.Fatalf("expected 384 dims, got %d", len(vec))
	}
	// L2 norm should be ~1.0
	var norm float32
	for _, v := range vec {
		norm += v * v
	}
	if norm < 0.99 || norm > 1.01 {
		t.Fatalf("expected unit vector, got norm %.4f", norm)
	}
}
