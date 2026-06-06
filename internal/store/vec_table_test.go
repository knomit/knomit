package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// makeVec returns a deterministic dim-length float32 vector.
func makeVec(dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(i%7) / 7.0
	}
	return v
}

// seedEmbedIdentity writes meta.embed_model_id / meta.embed_dim, simulating a
// prior successful Rebuild that persisted the embedding identity.
func seedEmbedIdentity(t *testing.T, si *searchIndex, modelID string, dim int) {
	t.Helper()
	ctx := context.Background()
	_, err := si.rh.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key, value) VALUES ('embed_model_id', ?)`, modelID)
	require.NoError(t, err)
	_, err = si.rh.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key, value) VALUES ('embed_dim', ?)`, dim)
	require.NoError(t, err)
}

func insertVec(t *testing.T, si *searchIndex, rowid int64, vec []float32) error {
	t.Helper()
	_, err := si.rh.db.ExecContext(context.Background(),
		`INSERT INTO facts_vec(rowid, embedding) VALUES (?, ?)`, rowid, float32SliceToBytes(vec))
	return err
}

func vecRowCount(t *testing.T, si *searchIndex) int {
	t.Helper()
	var n int
	require.NoError(t, si.rh.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM facts_vec`).Scan(&n))
	return n
}

// openSI opens a fresh migrated store and returns its searchIndex. Open already
// creates facts_vec at the default dim via ensureFactsVecDefault.
func openSI(t *testing.T) *searchIndex {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	return svc.Search().(*searchIndex)
}

// TestEnsureFactsVecCreatesAtDim verifies that on a migrated DB ensureFactsVec
// creates facts_vec at exactly the requested dimension and that the table
// accepts a vector of that width.
func TestEnsureFactsVecCreatesAtDim(t *testing.T) {
	ctx := context.Background()
	si := openSI(t)

	// Open created facts_vec at the default dim; recreate at 1024.
	require.NoError(t, si.ensureFactsVec(ctx, "m", 1024))

	exists, err := si.factsVecExists(ctx)
	require.NoError(t, err)
	require.True(t, exists, "facts_vec must exist after ensureFactsVec")

	// A 1024-float32 vector must insert cleanly.
	require.NoError(t, insertVec(t, si, 1, makeVec(1024)),
		"facts_vec must accept a 1024-dim vector")

	// A wrong-width vector must be rejected, proving the table is at 1024.
	require.Error(t, insertVec(t, si, 2, makeVec(768)),
		"facts_vec at dim 1024 must reject a 768-dim vector")
}

// TestEnsureFactsVecIdempotent verifies that calling ensureFactsVec twice with
// the same identity (matching persisted meta) is a no-op: it does not drop the
// table, so previously-inserted rows survive.
func TestEnsureFactsVecIdempotent(t *testing.T) {
	ctx := context.Background()
	si := openSI(t)

	seedEmbedIdentity(t, si, "m", 768)
	require.NoError(t, si.ensureFactsVec(ctx, "m", 768))

	require.NoError(t, insertVec(t, si, 42, makeVec(768)))
	require.Equal(t, 1, vecRowCount(t, si))

	// Second call with identical identity must not recreate the table.
	require.NoError(t, si.ensureFactsVec(ctx, "m", 768))
	require.Equal(t, 1, vecRowCount(t, si),
		"idempotent ensureFactsVec must not drop the table or lose rows")
}

// TestEnsureFactsVecRecreatesOnModelChange verifies that a model-id change at
// the same dimension recreates facts_vec EMPTY, discarding stale vectors.
func TestEnsureFactsVecRecreatesOnModelChange(t *testing.T) {
	ctx := context.Background()
	si := openSI(t)

	seedEmbedIdentity(t, si, "nomic", 768)
	require.NoError(t, si.ensureFactsVec(ctx, "nomic", 768))
	require.NoError(t, insertVec(t, si, 7, makeVec(768)))
	require.Equal(t, 1, vecRowCount(t, si))

	// Same dim, different model → table recreated empty.
	require.NoError(t, si.ensureFactsVec(ctx, "embeddinggemma", 768))
	require.Equal(t, 0, vecRowCount(t, si),
		"model-id change must recreate facts_vec empty")
}

// TestEnsureFactsVecRecreatesOnDimChange verifies that a dimension change
// recreates facts_vec EMPTY at the new width.
func TestEnsureFactsVecRecreatesOnDimChange(t *testing.T) {
	ctx := context.Background()
	si := openSI(t)

	seedEmbedIdentity(t, si, "nomic", 768)
	require.NoError(t, si.ensureFactsVec(ctx, "nomic", 768))
	require.NoError(t, insertVec(t, si, 9, makeVec(768)))
	require.Equal(t, 1, vecRowCount(t, si))

	// Dim 768 → 1024 → table recreated empty and now accepts 1024-dim vectors.
	require.NoError(t, si.ensureFactsVec(ctx, "nomic", 1024))
	require.Equal(t, 0, vecRowCount(t, si),
		"dim change must recreate facts_vec empty")
	require.NoError(t, insertVec(t, si, 10, makeVec(1024)),
		"recreated facts_vec must accept a 1024-dim vector")
	require.Error(t, insertVec(t, si, 11, makeVec(768)),
		"recreated facts_vec must reject the old 768-dim vector")
}
