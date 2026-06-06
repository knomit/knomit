package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
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

// failingEmbedder returns a MockBatchEmbedder whose Embed and EmbedBatch
// always error. Regresses the upsert silent-failure path: when Embed
// errors, the fact must still be indexed (branch_facts row) but without
// a facts_vec row, and the failure must be surfaced (logged).
func failingEmbedder(ctrl *gomock.Controller) *MockBatchEmbedder {
	m := NewMockBatchEmbedder(ctrl)
	m.EXPECT().EmbedQuery(gomock.Any()).Return(nil, errors.New("embed boom")).AnyTimes()
	m.EXPECT().EmbedDocument(gomock.Any(), gomock.Any()).Return(nil, errors.New("embed boom")).AnyTimes()
	m.EXPECT().EmbedDocuments(gomock.Any(), gomock.Any()).Return(nil, errors.New("embed batch boom")).AnyTimes()
	m.EXPECT().Dim().Return(768).AnyTimes()
	m.EXPECT().ID().Return("failing").AnyTimes()
	return m
}

// emptyVecEmbedder returns a MockBatchEmbedder that returns no error but
// a zero-length slice. Regresses the second silent-failure mode in upsert.
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
	return m
}

// wrongDimEmbedder returns a MockBatchEmbedder that returns a vector of
// the wrong dimension. The upsert path must reject the vector (skip
// facts_vec insert) and log; the fact must still be indexed.
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
	return m
}

// TestUpsert_EmbedderError_StillIndexesWithoutVector regresses the silent
// failure where `if err == nil && len(vec) > 0` swallowed Embed errors
// and left the fact in branch_facts but absent from facts_vec — with no
// log line to explain why. The fix: log the failure, still index the
// fact (so tag/keyword retrieval works), skip the vec insert.
func TestUpsert_EmbedderError_StillIndexesWithoutVector(t *testing.T) {
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
	require.NoError(t, err, "WriteFact must succeed even when embedder errors")

	// Fact is in branch_facts (tag/keyword retrieval works).
	got, err := svc.Search().GetByPath(context.Background(), "agent/a", "kb/synth/x.md")
	require.NoError(t, err)
	require.NotNil(t, got, "fact must be readable via path despite embedder failure")
	require.Equal(t, "X", got.Title)

	// Fact is NOT in facts_vec (vector retrieval will fall back to tag-only).
	require.False(t, hasFactsVecRow(t, svc, "agent/a", "kb/synth/x.md"),
		"failing embedder must not produce a facts_vec row")
}

// TestUpsert_EmbedderEmptyVector_StillIndexesWithoutVector covers the
// second silent-failure mode: embedder returns no error but an empty
// vector. Same observable behavior as the error case.
func TestUpsert_EmbedderEmptyVector_StillIndexesWithoutVector(t *testing.T) {
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
	require.NoError(t, err)

	got, err := svc.Search().GetByPath(context.Background(), "agent/a", "kb/synth/x.md")
	require.NoError(t, err)
	require.NotNil(t, got)

	require.False(t, hasFactsVecRow(t, svc, "agent/a", "kb/synth/x.md"),
		"empty-vec embedder must not produce a facts_vec row")
}

// TestUpsert_EmbedderWrongDim_StillIndexesWithoutVector regresses the
// case where the embedder produces a vector of the wrong dimension.
// Inserting it would violate the schema's FLOAT[768] invariant, so the
// vec insert is skipped; the fact still indexes for tag/keyword search.
func TestUpsert_EmbedderWrongDim_StillIndexesWithoutVector(t *testing.T) {
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
	require.NoError(t, err)

	got, err := svc.Search().GetByPath(context.Background(), "agent/a", "kb/synth/x.md")
	require.NoError(t, err)
	require.NotNil(t, got)

	require.False(t, hasFactsVecRow(t, svc, "agent/a", "kb/synth/x.md"),
		"wrong-dim embedder must not produce a facts_vec row")
}

// TestRelevantMethodologyForFact_FailingEmbedder_StillRetrievableByTag
// closes the loop end-to-end: a source fact written with a failing
// embedder still surfaces relevant methodology via tag-only ranking,
// rather than producing an error or empty result. This is what the
// methodology helper's "no stored embedding for source fact; ranking
// tag-only" warn-and-continue path exists to enable.
func TestRelevantMethodologyForFact_FailingEmbedder_StillRetrievableByTag(t *testing.T) {
	ctrl := gomock.NewController(t)
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(failingEmbedder(ctrl))
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
