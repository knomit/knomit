package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/embeddings/params"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// cosEmbedder gives every document the unit vector e0 and every query a unit
// vector at a fixed cosine to it, so the similarity a text query sees is exactly
// `cos` — which is what lets a test place a cutoff above or below it.
type cosEmbedder struct {
	cos      float64
	queryErr *atomic.Pointer[error] // when set, EmbedQuery fails
	badDim   *atomic.Bool           // when set, EmbedQuery returns a wrong-dimension vector
}

func (e cosEmbedder) doc() []float32 {
	out := make([]float32, 768)
	out[0] = 1
	return out
}

func (e cosEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	if e.queryErr != nil {
		if p := e.queryErr.Load(); p != nil {
			return nil, *p
		}
	}
	if e.badDim != nil && e.badDim.Load() {
		return []float32{1, 0, 0}, nil
	}
	out := make([]float32, 768)
	out[0] = float32(e.cos)
	out[1] = float32(math.Sqrt(1 - e.cos*e.cos))
	return out, nil
}

func (e cosEmbedder) EmbedDocument(context.Context, string, string) ([]float32, error) {
	return e.doc(), nil
}

func (e cosEmbedder) EmbedDocuments(_ context.Context, titles, _ []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = e.doc()
	}
	return out, nil
}

func (e cosEmbedder) EmbedShortStrings(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = e.doc()
	}
	return out, nil
}

func (cosEmbedder) Dim() int                      { return 768 }
func (cosEmbedder) ID() string                    { return "cos-fixed" }
func (cosEmbedder) Thresholds() params.Thresholds { return params.Defaults() }

// minSimHarness is a one-repo binding seeded with one fact whose cosine to any
// text query is emb.cos.
func minSimHarness(t *testing.T, emb cosEmbedder) func(args map[string]any) (string, bool) {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	repo := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "test", UID: nextTestRepoUID(), AgentBranch: "agent/test", Svc: svc,
		Ontology: fact.CodeOntology(), OntologyRoot: "kb", Embedder: emb,
	})
	seedFedFact(t, repos.WithRepoInstance(context.Background(), repo), "seed", "mission/store", "alpha fact", "store", nil)
	b := repos.NewBindingForTest(repo, repos.ReadTarget{RI: repo, Branch: "agent/test"})
	h := QueryHandler(emb)
	return func(args map[string]any) (string, bool) {
		var req mcpgo.CallToolRequest
		req.Params.Arguments = args
		r, err := h(repos.WithBinding(context.Background(), b), req)
		require.NoError(t, err)
		return mcpgo.GetTextFromContent(r.Content[0]), r.IsError
	}
}

func decodeQuery(t *testing.T, text string) queryResponse {
	t.Helper()
	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp), text)
	return resp
}

// min_similarity is a raw cosine in [0, 1]. A value outside it — typically the
// displayed score (cosine×100) passed back — is an error naming the parameter and
// the range, not an empty list and not a rescale.
func TestQuery_MinSimilarityOutOfRangeRejected(t *testing.T) {
	run := minSimHarness(t, cosEmbedder{cos: 0.8})
	for _, v := range []float64{45, 1.01, -0.1, math.NaN()} {
		text, isErr := run(map[string]any{"text": "alpha", "min_similarity": v})
		require.True(t, isErr, "min_similarity=%v must be an error, got %s", v, text)
		require.Contains(t, text, "min_similarity")
		require.Contains(t, text, "between 0 and 1")
	}
	// sort=recent shares the parser, so it rejects too.
	text, isErr := run(map[string]any{"text": "alpha", "sort": "recent", "min_similarity": 45.0})
	require.True(t, isErr, text)
	// Boundaries and ordinary values are accepted.
	for _, v := range []float64{0, 0.5, 1} {
		text, isErr := run(map[string]any{"text": "alpha", "min_similarity": v})
		require.False(t, isErr, "min_similarity=%v: %s", v, text)
	}
}

// A cutoff the caller supplied that removes every candidate is explained in the
// response; the same query with a reachable cutoff returns the fact and no notice.
func TestQuery_CutoffEmptiedResultIsExplained(t *testing.T) {
	run := minSimHarness(t, cosEmbedder{cos: 0.8})

	text, isErr := run(map[string]any{"text": "alpha", "min_similarity": 0.95})
	require.False(t, isErr, text)
	resp := decodeQuery(t, text)
	require.Empty(t, resp.Facts)
	require.Contains(t, resp.Notice, "0.95", "notice must carry the cutoff applied")
	require.Contains(t, resp.Notice, "0.80", "notice must carry the best cosine seen")

	text, isErr = run(map[string]any{"text": "alpha", "min_similarity": 0.5})
	require.False(t, isErr, text)
	resp = decodeQuery(t, text)
	require.Len(t, resp.Facts, 1)
	require.Empty(t, resp.Notice)
	require.NotContains(t, text, "notice", "the field is omitted when there is nothing to explain")

	// No caller cutoff: the model's own floor is not the caller's doing, so an
	// honest empty list stays a plain empty list.
	text, _ = run(map[string]any{"text": "alpha", "entities": []any{"no-such-entity"}})
	require.NotContains(t, text, "notice")
}

// Text search is vector-only, so a query that cannot be embedded is an error —
// it must not come back as `{"facts":[]}`.
func TestQuery_EmbedderFailureIsAnError(t *testing.T) {
	var qerr atomic.Pointer[error]
	emb := cosEmbedder{cos: 0.8, queryErr: &qerr}
	run := minSimHarness(t, emb)

	text, isErr := run(map[string]any{"text": "alpha"})
	require.False(t, isErr, text)
	require.Len(t, decodeQuery(t, text).Facts, 1)

	e := errors.New("simulated inference failure")
	qerr.Store(&e)
	text, isErr = run(map[string]any{"text": "alpha"})
	require.True(t, isErr, "embedder failure must surface, got %s", text)
	require.Contains(t, text, "simulated inference failure")
	text, isErr = run(map[string]any{"text": "alpha", "sort": "recent"})
	require.True(t, isErr, "sort=recent with text embeds too; got %s", text)
}

// A KNN failure (here: a query vector of the wrong dimension, which the vec0
// table refuses) is an error, not an empty result.
func TestQuery_KNNFailureIsAnError(t *testing.T) {
	var bad atomic.Bool
	run := minSimHarness(t, cosEmbedder{cos: 0.8, badDim: &bad})
	bad.Store(true)
	text, isErr := run(map[string]any{"text": "alpha"})
	require.True(t, isErr, "KNN failure must surface, got %s", text)
	require.Contains(t, strings.ToLower(text), "vector query")
}

// Path-prefix and other text-less queries never touch the vector step, so a dead
// embedder and a nonsense-for-text min_similarity do not affect them.
func TestQuery_PathPrefixUnaffectedByEmbedder(t *testing.T) {
	var qerr atomic.Pointer[error]
	run := minSimHarness(t, cosEmbedder{cos: 0.8, queryErr: &qerr})
	e := errors.New("simulated inference failure")
	qerr.Store(&e)

	text, isErr := run(map[string]any{"path": "kb/"})
	require.False(t, isErr, text)
	resp := decodeQuery(t, text)
	require.Len(t, resp.Facts, 1)
	require.Empty(t, resp.Notice)
}
