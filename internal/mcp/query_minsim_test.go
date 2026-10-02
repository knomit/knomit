package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	emptyVec *atomic.Bool           // when set, EmbedQuery returns an empty vector and a nil error
	calls    *atomic.Int64          // counts EmbedQuery calls when set
}

func (e cosEmbedder) doc() []float32 {
	out := make([]float32, 768)
	out[0] = 1
	return out
}

func (e cosEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	if e.calls != nil {
		e.calls.Add(1)
	}
	if e.emptyVec != nil && e.emptyVec.Load() {
		return []float32{}, nil
	}
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

// minSimMounts builds n repos wired to emb, each seeded with one fact, and a
// binding that reads all of them (the first is the write repo). The returned
// runner calls knomit_query with the given arguments.
func minSimMounts(t *testing.T, emb cosEmbedder, n int) (run func(args map[string]any) (string, bool), svcs []*store.Service) {
	t.Helper()
	var ris []*repos.RepoInstance
	for i := 0; i < n; i++ {
		svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Close() })
		svc.SetEmbedder(emb)
		require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
		repo := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
			Name: fmt.Sprintf("mount%d", i), UID: nextTestRepoUID(), AgentBranch: "agent/test", Svc: svc,
			Ontology: fact.CodeOntology(), OntologyRoot: "kb", Embedder: emb,
		})
		seedFedFact(t, repos.WithRepoInstance(context.Background(), repo), "seed", "mission/store",
			fmt.Sprintf("alpha fact %d", i), "store", nil)
		ris = append(ris, repo)
		svcs = append(svcs, svc)
	}
	reads := make([]repos.ReadTarget, len(ris))
	for i, ri := range ris {
		reads[i] = repos.ReadTarget{RI: ri, Branch: "agent/test"}
	}
	b := repos.NewBindingForTest(ris[0], reads...)
	h := QueryHandler(emb)
	return func(args map[string]any) (string, bool) {
		var req mcpgo.CallToolRequest
		req.Params.Arguments = args
		r, err := h(repos.WithBinding(context.Background(), b), req)
		require.NoError(t, err)
		return mcpgo.GetTextFromContent(r.Content[0]), r.IsError
	}, svcs
}

// minSimHarness is a one-repo binding seeded with one fact whose cosine to any
// text query is emb.cos.
func minSimHarness(t *testing.T, emb cosEmbedder) func(args map[string]any) (string, bool) {
	t.Helper()
	run, _ := minSimMounts(t, emb, 1)
	return run
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

// An embedder that reports success but hands back no vector is a failure, not an
// empty corpus: the query errors on relevance and sort=recent alike.
func TestQuery_EmptyVectorWithNilErrorIsAnError(t *testing.T) {
	var empty atomic.Bool
	run := minSimHarness(t, cosEmbedder{cos: 0.8, emptyVec: &empty})
	empty.Store(true)
	for _, sort := range []string{"relevance", "recent"} {
		text, isErr := run(map[string]any{"text": "alpha", "sort": sort})
		require.True(t, isErr, "sort=%s: an empty vector must surface, got %s", sort, text)
		require.Contains(t, strings.ToLower(text), "empty")
	}
}

// When the caller's cutoff removes every candidate of a sort=recent text query,
// that response explains it too — the recent path used to answer a bare empty list.
func TestQuery_RecentCutoffEmptiedResultIsExplained(t *testing.T) {
	run := minSimHarness(t, cosEmbedder{cos: 0.8})
	text, isErr := run(map[string]any{"text": "alpha", "sort": "recent", "min_similarity": 0.95})
	require.False(t, isErr, text)
	resp := decodeQuery(t, text)
	require.Empty(t, resp.Facts)
	require.Contains(t, resp.Notice, "0.95")
	require.Contains(t, resp.Notice, "0.80")

	text, isErr = run(map[string]any{"text": "alpha", "sort": "recent", "min_similarity": 0.5})
	require.False(t, isErr, text)
	resp = decodeQuery(t, text)
	require.NotEmpty(t, resp.Facts)
	require.Empty(t, resp.Notice)
}

// A caller cutoff that KEPT its candidates, followed by a filter (type) that
// removed them, is the filter's doing: no notice may blame the cutoff.
func TestQuery_CutoffPlusTypeFilterGivesNoNotice(t *testing.T) {
	run := minSimHarness(t, cosEmbedder{cos: 0.8})
	for _, sort := range []string{"relevance", "recent"} {
		text, isErr := run(map[string]any{"text": "alpha", "sort": sort, "min_similarity": 0.5, "type": []any{"hypothesis"}})
		require.False(t, isErr, text)
		resp := decodeQuery(t, text)
		require.Empty(t, resp.Facts, "sort=%s: the seeded policy fact is filtered out by type", sort)
		require.Empty(t, resp.Notice, "sort=%s: the type filter, not the cutoff, emptied it", sort)
	}
}

// Lens fan-out: a failing mount fails the whole query (a lens never silently
// shrinks its read set), on both orderings.
func TestQuery_LensFanoutMountFailureIsAnError(t *testing.T) {
	run, svcs := minSimMounts(t, cosEmbedder{cos: 0.8}, 3)
	text, isErr := run(map[string]any{"text": "alpha"})
	require.False(t, isErr, text)
	require.Len(t, decodeQuery(t, text).Facts, 3)

	require.NoError(t, svcs[2].Close())
	for _, sort := range []string{"relevance", "recent"} {
		text, isErr = run(map[string]any{"text": "alpha", "sort": sort})
		require.True(t, isErr, "sort=%s: a dead mount must fail the query, got %s", sort, text)
	}
}

// Lens fan-out: the notice appears only when EVERY mount's cutoff emptied it,
// and reports the best cosine across mounts.
func TestQuery_LensFanoutCutoffNotice(t *testing.T) {
	run, _ := minSimMounts(t, cosEmbedder{cos: 0.8}, 3)
	for _, sort := range []string{"relevance", "recent"} {
		text, isErr := run(map[string]any{"text": "alpha", "sort": sort, "min_similarity": 0.95})
		require.False(t, isErr, text)
		resp := decodeQuery(t, text)
		require.Empty(t, resp.Facts)
		require.Contains(t, resp.Notice, "0.95", "sort=%s", sort)
		require.Contains(t, resp.Notice, "0.80", "sort=%s", sort)

		text, _ = run(map[string]any{"text": "alpha", "sort": sort, "min_similarity": 0.5})
		resp = decodeQuery(t, text)
		require.Len(t, resp.Facts, 3, "sort=%s", sort)
		require.Empty(t, resp.Notice, "sort=%s: mounts kept their candidates", sort)
	}
}

// An embedder failure fails a 3-mount fan-out ONCE, up front: one inference,
// not one per mount, on both orderings.
func TestQuery_LensFanoutEmbedFailureFailsOnce(t *testing.T) {
	var qerr atomic.Pointer[error]
	var calls atomic.Int64
	run, _ := minSimMounts(t, cosEmbedder{cos: 0.8, queryErr: &qerr, calls: &calls}, 3)
	e := errors.New("simulated inference failure")
	qerr.Store(&e)
	for _, sort := range []string{"relevance", "recent"} {
		calls.Store(0)
		text, isErr := run(map[string]any{"text": "alpha", "sort": sort})
		require.True(t, isErr, text)
		require.Contains(t, text, "simulated inference failure")
		require.EqualValues(t, 1, calls.Load(), "sort=%s: the failing embed must not be repeated per mount", sort)
	}
}
