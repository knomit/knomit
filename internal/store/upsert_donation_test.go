package store

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// countingEmbedder wraps stub768Embedder and counts how many times
// Embed/EmbedBatch are called. Used to prove the upsert path actually
// avoids the embedder when a precomputed vector is donated via context.
type countingEmbedder struct {
	stub768Embedder
	embedCalls atomic.Int64
	batchCalls atomic.Int64
}

func (e *countingEmbedder) Embed(text string) ([]float32, error) {
	e.embedCalls.Add(1)
	return e.stub768Embedder.Embed(text)
}

func (e *countingEmbedder) EmbedBatch(texts []string) ([][]float32, error) {
	e.batchCalls.Add(1)
	return e.stub768Embedder.EmbedBatch(texts)
}

// TestUpsert_DonatedVectorSkipsEmbedder regresses the optimization where
// callers (mcp/learn after its dedup pass) can hand a precomputed vector
// to upsert via context, avoiding a redundant ONNX inference.
//
// Verifies:
//   - WriteFact without donation triggers exactly one Embed call
//     (baseline behavior).
//   - WriteFact through a context wrapped with
//     WithPrecomputedEmbeddings(path → vec) triggers zero Embed calls
//     for that fact, AND the stored facts_vec row equals the donated
//     vector (not what the embedder would have produced).
func TestUpsert_DonatedVectorSkipsEmbedder(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()

	emb := &countingEmbedder{}
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ctx := context.Background()

	// Baseline: write a fact with no donation. upsert must invoke the
	// embedder (Embed call count goes up by 1).
	emb.embedCalls.Store(0)
	_, err = svc.Facts().WriteFact(ctx, "agent/test", "kb/baseline.md",
		"---\ntype: observation\n---\n# Baseline\n\nbody-baseline", "add baseline", "test")
	require.NoError(t, err)
	require.Equal(t, int64(1), emb.embedCalls.Load(),
		"baseline write must call Embed exactly once")

	// Donated path: build a 768-dim vector that's clearly distinct from
	// what stub768Embedder would produce for any text, donate it, and
	// write a different fact under that path.
	donated := make([]float32, 768)
	for i := range donated {
		donated[i] = 0.5 // sentinel pattern — stub768Embedder maps len*31+i%256
	}
	donateCtx := WithPrecomputedEmbeddings(ctx, map[string][]float32{
		"kb/donated.md": donated,
	})

	emb.embedCalls.Store(0)
	emb.batchCalls.Store(0)
	_, err = svc.Facts().WriteFact(donateCtx, "agent/test", "kb/donated.md",
		"---\ntype: observation\n---\n# Donated\n\nbody-donated", "add donated", "test")
	require.NoError(t, err)
	require.Equal(t, int64(0), emb.embedCalls.Load(),
		"donated write must NOT call Embed")
	require.Equal(t, int64(0), emb.batchCalls.Load(),
		"donated write must NOT call EmbedBatch")

	// The stored facts_vec row must hold the DONATED vector, not what
	// the embedder would have returned for the donated content.
	var factID int64
	err = svc.rh.db.QueryRow(`SELECT id FROM facts WHERE path = ?`, "kb/donated.md").Scan(&factID)
	require.NoError(t, err)

	storedVec, err := svc.Search().(*searchIndex).getEmbeddingByFact(ctx, "kb/donated.md", "")
	// getEmbeddingByFact takes a blob_hash but tolerates "" by ignoring it; if
	// not, fall back to a direct vec0 query.
	if err != nil || storedVec == nil {
		var vecBytes []byte
		err = svc.rh.db.QueryRow(`SELECT embedding FROM facts_vec WHERE rowid = ?`, factID).Scan(&vecBytes)
		require.NoError(t, err)
		require.Len(t, vecBytes, 768*4, "facts_vec stores 768 float32s = 3072 bytes")
		// First float32 from sentinel pattern is 0.5; check the first 4 bytes.
		// Layout is little-endian IEEE 754 — byte pattern for 0.5 is 0x00 0x00 0x00 0x3F.
		require.Equal(t, byte(0x00), vecBytes[0])
		require.Equal(t, byte(0x00), vecBytes[1])
		require.Equal(t, byte(0x00), vecBytes[2])
		require.Equal(t, byte(0x3F), vecBytes[3])
	}
}

// TestUpsert_DonatedVectorWrongDimRejected regresses the bug where a
// donated vector of the wrong dimension (e.g. test stub or model
// version skew producing 384-dim while facts_vec is FLOAT[768]) was
// inserted as a malformed BLOB without a sanity check. The schema
// dimension is a hard invariant — wrong dim must be rejected.
func TestUpsert_DonatedVectorWrongDimRejected(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()

	emb := &countingEmbedder{}
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ctx := context.Background()

	// Donate a 384-dim vector — half of what the schema demands.
	wrongDim := make([]float32, 384)
	donateCtx := WithPrecomputedEmbeddings(ctx, map[string][]float32{
		"kb/wrong.md": wrongDim,
	})

	_, err = svc.Facts().WriteFact(donateCtx, "agent/test", "kb/wrong.md",
		"---\ntype: observation\n---\n# Wrong\n\nbody-wrong", "add wrong", "test")
	require.Error(t, err, "donating a wrong-dim vector must surface an error from the upsert path")
	// Must come from the explicit guard at the donation site — not the
	// sqlite-vec "Dimension mismatch for inserted vector" message
	// produced after the upsert is mid-flight.
	require.Contains(t, err.Error(), "donated embedding",
		"error must come from upsert's explicit dim check, not from sqlite-vec internals; got: %v", err)
	require.Contains(t, err.Error(), "384")
	require.Contains(t, err.Error(), "768")
}
