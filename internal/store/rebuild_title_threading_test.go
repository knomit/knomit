package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// recordingEmbedder is a BatchEmbedder that records the (title, body) pairs it
// is handed via the document-embedding methods, so a test can assert the fact
// title is threaded into EmbedDocuments separately from the body. It delegates
// the actual vector to stub768Embedder so vec0 accepts the result.
type recordingEmbedder struct {
	stub768Embedder
	mu        sync.Mutex
	docTitles []string
	docBodies []string
}

func (e *recordingEmbedder) EmbedDocument(ctx context.Context, title, body string) ([]float32, error) {
	e.mu.Lock()
	e.docTitles = append(e.docTitles, title)
	e.docBodies = append(e.docBodies, body)
	e.mu.Unlock()
	return e.stub768Embedder.EmbedDocument(ctx, title, body)
}

func (e *recordingEmbedder) EmbedDocuments(ctx context.Context, titles, bodies []string) ([][]float32, error) {
	e.mu.Lock()
	e.docTitles = append(e.docTitles, titles...)
	e.docBodies = append(e.docBodies, bodies...)
	e.mu.Unlock()
	return e.stub768Embedder.EmbedDocuments(ctx, titles, bodies)
}

// TestRebuildEmbeddings_ThreadsTitleIntoDocuments asserts that the rebuild
// embedding phase passes the fact title to EmbedDocuments as a distinct
// argument from the body (rather than concatenating them into one text blob,
// as the pre-refactor Embed(title+" "+body) path did). The body argument must
// carry the fact body but NOT the title.
func TestRebuildEmbeddings_ThreadsTitleIntoDocuments(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// Write the fact WITHOUT an embedder configured, so no inline embedding
	// happens at upsert time and facts_vec starts empty — forcing the rebuild
	// embedding phase to run the batch document path.
	f := fact.NewFact("placeholder.md")
	f.Title = "Unique-Title-Marker"
	f.Body = "Unique-Body-Marker spanning several words."
	f.Confidence = 0.9
	f.Sources = 1
	f.Domain = []string{"governance"}
	f.Entities = []string{"x"}
	f.Type = fact.Observation
	out, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/a.md", out, "init", "")
	require.NoError(t, err)

	rec := &recordingEmbedder{}
	svc.SetEmbedder(rec)

	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Contains(t, rec.docTitles, "Unique-Title-Marker",
		"rebuild must pass the fact title as a distinct EmbedDocuments title arg")

	// The body arg paired with our title must carry the body but not the title.
	var pairedBody string
	for i, title := range rec.docTitles {
		if title == "Unique-Title-Marker" {
			pairedBody = rec.docBodies[i]
			break
		}
	}
	require.Contains(t, pairedBody, "Unique-Body-Marker",
		"the body argument must carry the fact body")
	require.NotContains(t, pairedBody, "Unique-Title-Marker",
		"the title must not be concatenated into the body argument")
}

// EmbedShortStrings satisfies store.BatchEmbedder. Short strings render
// through the model's short-string template in production; a stub has no
// template, so it embeds each string as a title-only document.
func (e *recordingEmbedder) EmbedShortStrings(ctx context.Context, texts []string) ([][]float32, error) {
	return e.EmbedDocuments(ctx, texts, make([]string, len(texts)))
}
