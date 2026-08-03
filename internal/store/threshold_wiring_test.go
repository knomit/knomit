package store

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/embeddings/params"
)

// floorEmbedder emits unit-norm 768-dim vectors with KNOWN cosines to the
// query, and exposes a configurable recall floor. Query and "match" docs map to
// the same axis (cosine 1.0); "near" sits at cosine 0.5; "far" is orthogonal
// (cosine 0.0). The configurable floor lets a test prove search filtering is
// driven by the model's Thresholds rather than a hard-coded constant.
type floorEmbedder struct {
	floor float64
}

func (e *floorEmbedder) vec(text string) []float32 {
	out := make([]float32, 768)
	switch {
	case strings.Contains(text, "match-target"):
		out[0] = 1.0 // cosine 1.0 with the query
	case strings.Contains(text, "near-target"):
		out[0] = 0.5
		out[1] = float32(math.Sqrt(0.75)) // unit norm; cosine 0.5 with the query
	case strings.Contains(text, "far-target"):
		out[1] = 1.0 // orthogonal to the query; cosine 0.0
	default:
		out[2] = 1.0
	}
	return out
}

func (e *floorEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return e.vec(text), nil
}
func (e *floorEmbedder) EmbedDocument(_ context.Context, title, body string) ([]float32, error) {
	return e.vec(title + " " + body), nil
}
func (e *floorEmbedder) EmbedDocuments(_ context.Context, titles, bodies []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = e.vec(titles[i] + " " + bodies[i])
	}
	return out, nil
}
func (e *floorEmbedder) Dim() int   { return 768 }
func (e *floorEmbedder) ID() string { return "floor" }
func (e *floorEmbedder) Thresholds() params.Thresholds {
	th := params.Defaults()
	th.SearchFloor = e.floor
	return th
}

// TestEmbedderThresholds_NilSafe regresses the contract that callers can read
// thresholds without a nil check: a nil embedder yields the historical defaults
// rather than panicking, and a real embedder's values pass through unchanged.
func TestEmbedderThresholds_NilSafe(t *testing.T) {
	require.Equal(t, params.Defaults(), EmbedderThresholds(nil),
		"nil embedder must fall back to params.Defaults()")
	require.Equal(t, 0.83, EmbedderThresholds(&floorEmbedder{floor: 0.83}).SearchFloor,
		"a configured embedder's thresholds must pass through")
}

// TestSearch_RecallFloorComesFromModel regresses the silent mis-calibration the
// pluggable embedder could introduce: the default recall floor used to be a
// hard-coded 0.40. It must now come from the active model's Thresholds, so the
// SAME corpus and query return different results purely because the model's
// SearchFloor differs. "near" (cosine 0.5) passes a permissive 0.40 floor but
// is filtered by a strict 0.60 floor; "match" (1.0) always survives, "far"
// (0.0) never does.
func TestSearch_RecallFloorComesFromModel(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	const branch = "main"

	// Stored vectors are identical regardless of floor (floor only affects
	// query-time filtering), so embed once with a permissive embedder.
	svc.SetEmbedder(&floorEmbedder{floor: 0.40})
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/a.md", testFactBody("match-target alpha", 0.9, nil), "init a", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/b.md", testFactBody("near-target beta", 0.8, nil), "init b", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/c.md", testFactBody("far-target gamma", 0.7, nil), "init c", "")
	require.NoError(t, err)

	// Permissive floor (0.40): match (1.0) and near (0.5) pass; far (0.0) does not.
	permissive, err := svc.Search().Search(ctx, branch, SearchOptions{Text: "match-target alpha", Limit: 10})
	require.NoError(t, err)
	got := pathSet(permissive)
	require.Contains(t, got, "kb/a.md", "match must pass the permissive floor")
	require.Contains(t, got, "kb/b.md", "near (cos 0.5) must pass the 0.40 floor")
	require.NotContains(t, got, "kb/c.md", "far (cos 0.0) must never pass")

	// Strict floor (0.60), same stored vectors: near (0.5) is now filtered out,
	// proving the floor is sourced from the embedder, not a constant.
	svc.SetEmbedder(&floorEmbedder{floor: 0.60})
	strict, err := svc.Search().Search(ctx, branch, SearchOptions{Text: "match-target alpha", Limit: 10})
	require.NoError(t, err)
	got = pathSet(strict)
	require.Contains(t, got, "kb/a.md", "match (cos 1.0) must still pass the strict floor")
	require.NotContains(t, got, "kb/b.md", "near (cos 0.5) must be filtered by the 0.60 floor")
}

func pathSet(results []SearchResult) map[string]bool {
	out := make(map[string]bool, len(results))
	for _, r := range results {
		out[r.Path] = true
	}
	return out
}
