package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"knomit/internal/retrieval"
)

// hasFactsVecRow reports whether the fact at (branch, path) has a row in
// facts_vec. The JOIN shape mirrors what RelevantMethodologyForFact does.
func hasFactsVecRow(t *testing.T, svc *Service, branch, path string) bool {
	t.Helper()
	si := svc.Search().(*searchIndex)
	branchID, err := si.rh.branchID(context.Background(), branch)
	require.NoError(t, err)
	var data []byte
	err = conn(context.Background(), si.rh.db).QueryRowContext(context.Background(), `
		SELECT fv.embedding
		FROM facts_vec fv
		JOIN branch_facts bf ON bf.fact_id = fv.rowid
		WHERE bf.branch_id = ? AND bf.path = ?`,
		branchID, path,
	).Scan(&data)
	if err != nil {
		return false
	}
	return len(data) > 0
}

// failingEmbedder returns a MockBatchEmbedder whose EmbedDocument/EmbedDocuments
// always error. Used to regress the mandatory-embeddings invariant: when an
// embedder is configured and embedding fails, the write must FAIL rather than
// silently index a vectorless fact.
func failingEmbedder(ctrl *gomock.Controller) *MockBatchEmbedder {
	m := NewMockBatchEmbedder(ctrl)
	m.EXPECT().EmbedQuery(gomock.Any()).Return(nil, errors.New("embed boom")).AnyTimes()
	m.EXPECT().EmbedDocument(gomock.Any(), gomock.Any()).Return(nil, errors.New("embed boom")).AnyTimes()
	m.EXPECT().EmbedDocuments(gomock.Any(), gomock.Any()).Return(nil, errors.New("embed batch boom")).AnyTimes()
	m.EXPECT().Dim().Return(768).AnyTimes()
	m.EXPECT().ID().Return("failing").AnyTimes()
	m.EXPECT().Thresholds().Return(retrieval.Defaults()).AnyTimes()
	return m
}

// emptyVecEmbedder returns a MockBatchEmbedder that returns no error but a
// zero-length slice. Same observable contract as the error case: the write
// must fail.
func emptyVecEmbedder(ctrl *gomock.Controller) *MockBatchEmbedder {
	m := NewMockBatchEmbedder(ctrl)
	m.EXPECT().EmbedQuery(gomock.Any()).Return([]float32{}, nil).AnyTimes()
	m.EXPECT().EmbedDocument(gomock.Any(), gomock.Any()).Return([]float32{}, nil).AnyTimes()
	m.EXPECT().EmbedDocuments(gomock.Any(), gomock.Any()).DoAndReturn(func(titles, bodies []string) ([][]float32, error) {
		out := make([][]float32, len(titles))
		for i := range titles {
			out[i] = []float32{}
		}
		return out, nil
	}).AnyTimes()
	m.EXPECT().Dim().Return(768).AnyTimes()
	m.EXPECT().ID().Return("empty-vec").AnyTimes()
	m.EXPECT().Thresholds().Return(retrieval.Defaults()).AnyTimes()
	return m
}

// wrongDimEmbedder returns a MockBatchEmbedder that returns a vector of the
// wrong dimension. Inserting it would violate the facts_vec FLOAT[Dim]
// invariant, so the write must fail.
func wrongDimEmbedder(ctrl *gomock.Controller) *MockBatchEmbedder {
	m := NewMockBatchEmbedder(ctrl)
	m.EXPECT().EmbedQuery(gomock.Any()).Return(make([]float32, 10), nil).AnyTimes()
	m.EXPECT().EmbedDocument(gomock.Any(), gomock.Any()).Return(make([]float32, 10), nil).AnyTimes()
	m.EXPECT().EmbedDocuments(gomock.Any(), gomock.Any()).DoAndReturn(func(titles, bodies []string) ([][]float32, error) {
		out := make([][]float32, len(titles))
		for i := range titles {
			out[i] = make([]float32, 10)
		}
		return out, nil
	}).AnyTimes()
	m.EXPECT().Dim().Return(768).AnyTimes()
	m.EXPECT().ID().Return("wrong-dim").AnyTimes()
	m.EXPECT().Thresholds().Return(retrieval.Defaults()).AnyTimes()
	return m
}

// requireNoFact asserts the fact at (branch, path) is absent from the index —
// no facts_vec row and not resolvable by path. A failed embed must roll back
// the whole upsert, leaving no partially-indexed (vectorless) fact behind.
func requireNoFact(t *testing.T, svc *Service, branch, path string) {
	t.Helper()
	require.False(t, hasFactsVecRow(t, svc, branch, path),
		"failed embed must not leave a facts_vec row")
	got, err := svc.Search().GetByPath(context.Background(), branch, path)
	require.NoError(t, err)
	require.Nil(t, got, "failed embed must not leave an indexed (vectorless) fact")
}

// TestUpsert_EmbedderError_FailsWrite regresses the mandatory-embeddings
// invariant: with an embedder configured, an embed error must FAIL the write,
// never silently index a vectorless fact (which would be invisible to vector
// search and corrupt retrieval guarantees).
func TestUpsert_EmbedderError_FailsWrite(t *testing.T) {
	ctrl := gomock.NewController(t)
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(failingEmbedder(ctrl))
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	_, err = svc.Facts().WriteFact(context.Background(), "agent/a",
		"kb/synth/x.md",
		srcFactBody("X", "body", []string{"security"}, []string{"Anthropic"}),
		"add", "")
	require.Error(t, err, "embed failure must fail the write")
	requireNoFact(t, svc, "agent/a", "kb/synth/x.md")
}

// TestUpsert_EmbedderEmptyVector_FailsWrite covers the second failure mode:
// embedder returns no error but an empty vector.
func TestUpsert_EmbedderEmptyVector_FailsWrite(t *testing.T) {
	ctrl := gomock.NewController(t)
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(emptyVecEmbedder(ctrl))
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	_, err = svc.Facts().WriteFact(context.Background(), "agent/a",
		"kb/synth/x.md",
		srcFactBody("X", "body", []string{"security"}, nil),
		"add", "")
	require.Error(t, err, "empty vector must fail the write")
	requireNoFact(t, svc, "agent/a", "kb/synth/x.md")
}

// TestUpsert_EmbedderWrongDim_FailsWrite covers a wrong-dimension vector.
func TestUpsert_EmbedderWrongDim_FailsWrite(t *testing.T) {
	ctrl := gomock.NewController(t)
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(wrongDimEmbedder(ctrl))
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	_, err = svc.Facts().WriteFact(context.Background(), "agent/a",
		"kb/synth/x.md",
		srcFactBody("X", "body", []string{"security"}, nil),
		"add", "")
	require.Error(t, err, "wrong-dim vector must fail the write")
	requireNoFact(t, svc, "agent/a", "kb/synth/x.md")
}

// TestRelevantMethodologyForFact_NoEmbedder_StillRetrievableByTag closes the
// loop for the no-embedder context (read-only tooling / tests): facts written
// without an embedder carry no vector, and methodology retrieval must still
// surface relevant lessons via tag-only ranking rather than erroring. (The
// running service always has an embedder — app.New makes it mandatory — so this
// path is defensive, not the production norm.)
func TestRelevantMethodologyForFact_NoEmbedder_StillRetrievableByTag(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	// Deliberately no SetEmbedder: facts index without vectors.
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	_, err = svc.Facts().WriteFact(context.Background(), "agent/a",
		"kb/synth/src.md",
		srcFactBody("Source", "source body",
			[]string{"security"}, []string{"Anthropic"}),
		"src", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), "agent/a",
		"kb/meta/reasoning/m.md",
		methFactBody("M", "lesson body",
			[]string{"meta", "reasoning", "methodology", "security"},
			[]string{"Anthropic"}),
		"meth", "")
	require.NoError(t, err)

	got, err := svc.Search().RelevantMethodologyForFact(context.Background(), "agent/a",
		"kb/synth/src.md",
		[]string{"security"}, []string{"Anthropic"},
		10, 0.0)
	require.NoError(t, err, "retrieval must succeed even when source has no embedding")
	require.Len(t, got, 1)
	require.InDelta(t, 1.0, got[0].TagOverlap, 0.001)
	require.Equal(t, 0.0, got[0].VectorScore, "no source vector → vector score must be zero")
}
