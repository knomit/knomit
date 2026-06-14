package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// blockingBatchEmbedder signals when embedding starts and blocks until released,
// so a test can probe the DB write lock while a rebuild is mid-embed.
type blockingBatchEmbedder struct {
	stub768Embedder
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *blockingBatchEmbedder) EmbedDocuments(titles, bodies []string) ([][]float32, error) {
	e.once.Do(func() { close(e.started) })
	<-e.release
	out := make([][]float32, len(titles))
	for i := range out {
		out[i], _ = e.stub768Embedder.EmbedDocument(titles[i], bodies[i])
	}
	return out, nil
}

// TestRebuildEmbeddings_DoesNotHoldWriteLockDuringEmbed regresses the
// concurrency bug: the embeddings rebuild used to hold a BEGIN IMMEDIATE write
// transaction across the (slow) ONNX inference, so concurrent writers starved
// and hit "database is locked" for the whole rebuild. Embeddings are now
// computed outside the transaction, so the write lock is free while embedding.
func TestRebuildEmbeddings_DoesNotHoldWriteLockDuringEmbed(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	emb := &blockingBatchEmbedder{started: make(chan struct{}), release: make(chan struct{})}
	svc.SetEmbedder(emb)

	ctx := context.Background()
	const branch = "main"
	for i := 0; i < 3; i++ {
		f := fact.NewFact("placeholder.md")
		f.Title = "Fact"
		f.Confidence = 0.9
		f.Sources = 1
		f.Domain = []string{"ai-governance"}
		f.Entities = []string{"x"}
		f.Type = fact.Observation
		out, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, branch, "kb/f"+string(rune('a'+i))+".md", out, "init", "")
		require.NoError(t, err)
	}

	si := svc.Search().(*searchIndex)
	_, err = si.rh.db.ExecContext(ctx, `DELETE FROM facts_vec`) // force a full re-embed
	require.NoError(t, err)

	rebuildDone := make(chan error, 1)
	go func() { rebuildDone <- svc.IndexManager().Rebuild(ctx, branch, nil) }()

	// Wait until the rebuild is inside EmbedDocuments (the slow phase).
	select {
	case <-emb.started:
	case <-time.After(5 * time.Second):
		t.Fatal("rebuild never reached the embedding phase")
	}

	// The write lock must be FREE now: a BEGIN IMMEDIATE (the DSN uses
	// _txlock=immediate) must acquire it well within the busy_timeout, rather
	// than blocking for the entire embed. Before the fix this timed out.
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	wtx, err := si.rh.db.BeginTx(wctx, nil)
	require.NoError(t, err, "write lock must be free during embedding (embed runs outside the tx)")
	require.NoError(t, wtx.Rollback())

	close(emb.release)
	require.NoError(t, <-rebuildDone)
}
