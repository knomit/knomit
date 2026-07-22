package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"knomit/internal/retrieval"

	"github.com/stretchr/testify/require"
)

// cancellingEmbedder is a plain Embedder (deliberately NOT a BatchEmbedder, so
// rebuildEmbeddings takes the per-document branch). The first EmbedDocument
// call cancels the rebuild's context and every call returns ctx.Err(), which
// models a cancellation arriving partway through a chunk.
type cancellingEmbedder struct {
	cancel context.CancelFunc
	calls  int
}

func (e *cancellingEmbedder) EmbedQuery(ctx context.Context, _ string) ([]float32, error) {
	return nil, ctx.Err()
}

func (e *cancellingEmbedder) EmbedDocument(ctx context.Context, _, _ string) ([]float32, error) {
	e.calls++
	e.cancel()
	return nil, context.Canceled
}

func (e *cancellingEmbedder) Dim() int                         { return 8 }
func (e *cancellingEmbedder) ID() string                       { return "cancelling-test-embedder" }
func (e *cancellingEmbedder) Thresholds() retrieval.Thresholds { return retrieval.Thresholds{} }

// TestRebuildEmbeddings_CancelledMidChunk_StopsImmediately: once the rebuild's
// context is cancelled, the per-document embed loop must stop rather than
// logging "embed failed, skipping" for every remaining fact in the chunk. The
// work is doomed either way — BeginTx(ctx) at the bottom of the chunk fails on
// the same cancelled context — so continuing only converts one cancellation
// into a WARN per fact.
func TestRebuildEmbeddings_CancelledMidChunk_StopsImmediately(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	// Seed more than one chunk's worth of facts with no embedder configured, so
	// they all land in facts without rows in facts_vec.
	const nFacts = 20
	bg := context.Background()
	for i := 0; i < nFacts; i++ {
		_, err := svc.Facts().WriteFact(bg, "main", fmt.Sprintf("kb/e%02d.md", i),
			testFactBody(fmt.Sprintf("e%02d", i), 0.9, nil), "seed", "update")
		require.NoError(t, err)
	}

	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	emb := &cancellingEmbedder{cancel: cancel}
	svc.rh.setEmbedder(emb)

	_, rerr := svc.si.rebuildEmbeddings(ctx, nil)
	require.Error(t, rerr, "a cancelled rebuild must report the cancellation, not succeed quietly")

	require.Equal(t, 1, emb.calls,
		"rebuild kept embedding after cancellation: %d EmbedDocument calls (one WARN each) instead of stopping at the first", emb.calls)
}
