package embeddings_test

import (
	"os"
	"path/filepath"
	"testing"

	"knomit/internal/embeddings"

	"github.com/stretchr/testify/require"
)

// setupTestEmbedder creates an Embedder using the cached model files.
// It skips the test if the model or ORT library is not available.
func setupTestEmbedder(t *testing.T) *embeddings.Embedder {
	t.Helper()

	cacheDir := filepath.Join(os.Getenv("HOME"), ".cache", "knomit", "models")
	modelPath := filepath.Join(cacheDir, "model.onnx")
	tokPath := filepath.Join(cacheDir, "tokenizer.json")

	if _, err := os.Stat(modelPath); err != nil {
		t.Skipf("ONNX model not found at %s: %v", modelPath, err)
	}
	if _, err := os.Stat(tokPath); err != nil {
		t.Skipf("tokenizer not found at %s: %v", tokPath, err)
	}

	e, err := embeddings.NewEmbedder(modelPath, tokPath)
	if err != nil {
		t.Skipf("cannot create embedder (ORT library may be missing): %v", err)
	}
	t.Cleanup(e.Close)
	return e
}

func TestEmbedBatch_ConsistentWithSingle(t *testing.T) {
	e := setupTestEmbedder(t)

	texts := []string{"TCP is reliable", "PostgreSQL uses MVCC"}

	singles := make([][]float32, len(texts))
	for i, text := range texts {
		v, err := e.Embed(text)
		require.NoError(t, err)
		singles[i] = v
	}

	batched, err := e.EmbedBatch(texts)
	require.NoError(t, err)
	require.Len(t, batched, 2)

	// Should be approximately equal (padding may cause tiny float differences).
	for i := range texts {
		require.InDeltaSlice(t, singles[i], batched[i], 0.01)
	}
}

func TestEmbedBatch_Empty(t *testing.T) {
	e := setupTestEmbedder(t)

	result, err := e.EmbedBatch(nil)
	require.NoError(t, err)
	require.Nil(t, result)

	result, err = e.EmbedBatch([]string{})
	require.NoError(t, err)
	require.Nil(t, result)
}

func TestEmbedBatch_Single(t *testing.T) {
	e := setupTestEmbedder(t)

	texts := []string{"hello world"}

	single, err := e.Embed(texts[0])
	require.NoError(t, err)

	batched, err := e.EmbedBatch(texts)
	require.NoError(t, err)
	require.Len(t, batched, 1)

	// Single-element batch delegates to Embed, so should be identical.
	require.InDeltaSlice(t, single, batched[0], 1e-9)
}
