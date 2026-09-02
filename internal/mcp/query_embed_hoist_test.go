package mcp

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/embeddings/params"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// countingFedEmbedder answers every query with the same fixed vector and counts
// the inferences. The vector's VALUE is irrelevant here; the COUNT is the whole
// point, and a constant vector is what makes the hoisted and per-mount runs
// comparable at all.
type countingFedEmbedder struct{ queries *atomic.Int64 }

func (e countingFedEmbedder) vec() []float32 {
	out := make([]float32, 768)
	out[0] = 1
	return out
}

func (e countingFedEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	e.queries.Add(1)
	return e.vec(), nil
}

func (e countingFedEmbedder) EmbedDocument(context.Context, string, string) ([]float32, error) {
	return e.vec(), nil
}

func (e countingFedEmbedder) EmbedDocuments(_ context.Context, titles, _ []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = e.vec()
	}
	return out, nil
}

func (e countingFedEmbedder) EmbedShortStrings(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = e.vec()
	}
	return out, nil
}

func (countingFedEmbedder) Dim() int                      { return 768 }
func (countingFedEmbedder) ID() string                    { return "counting-fed" }
func (countingFedEmbedder) Thresholds() params.Thresholds { return params.Defaults() }

// countingFedRepo builds a repo wired to the SHARED counting embedder, so a
// per-mount embed inside the store is counted by the same meter as the
// handler's hoisted one — which is what lets one assertion distinguish them.
func countingFedRepo(t *testing.T, emb countingFedEmbedder) (*repos.RepoInstance, context.Context) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		UID:          nextTestRepoUID(),
		AgentBranch:  "agent/test",
		Svc:          svc,
		Ontology:     fact.CodeOntology(),
		OntologyRoot: "kb",
		Embedder:     emb,
	})
	return ri, repos.WithRepoInstance(context.Background(), ri)
}

// A federated text query embeds the query ONCE for the whole fan-out, not once
// per mount.
//
// Both sides of this test run the same query over the same 3-mount binding; the
// only difference is whether QueryHandler was given the embedder to hoist with.
// Without it, each mount embeds for itself inside store.Search — 3 inferences of
// the identical string. That is the shape that made the REST twin 88% embedding
// on a 5-mount lens, and it is a fixed per-mount cost, not a corpus-size one.
func TestQueryFederation_EmbedsQueryOncePerFanout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hoists bool
		want   int64
	}{
		{"hoisted", true, 1},
		{"per-mount (pre-fix shape)", false, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var meter atomic.Int64
			emb := countingFedEmbedder{queries: &meter}
			repoA, ctxA := countingFedRepo(t, emb)
			repoB, _ := countingFedRepo(t, emb)
			repoC, _ := countingFedRepo(t, emb)
			seedFedFact(t, ctxA, "seed-a", "mission/store", "alpha fact", "store", nil)

			b := repos.NewBindingForTest(repoA,
				repos.ReadTarget{RI: repoA, Branch: "agent/test"},
				repos.ReadTarget{RI: repoB, Branch: "agent/test"},
				repos.ReadTarget{RI: repoC, Branch: "agent/test"},
			)

			// Seeding embeds documents, not queries, but reset anyway so the
			// number below can only have come from the query path.
			meter.Store(0)

			handler := QueryHandler()
			if tc.hoists {
				handler = QueryHandler(emb)
			}
			var req mcpgo.CallToolRequest
			req.Params.Arguments = map[string]any{"text": "alpha"}
			result, err := handler(repos.WithBinding(context.Background(), b), req)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Falsef(t, result.IsError, "query failed: %s", resultText(t, result))

			if got := meter.Load(); got != tc.want {
				t.Errorf("EmbedQuery called %d times across 3 mounts, want %d", got, tc.want)
			}
		})
	}
}

// A text-LESS query must not reach an embedder at all — the hoist adds no
// inference to a pure filter browse.
func TestQueryFederation_FilterOnlyQueryNeverEmbeds(t *testing.T) {
	var meter atomic.Int64
	emb := countingFedEmbedder{queries: &meter}
	repoA, ctxA := countingFedRepo(t, emb)
	repoB, _ := countingFedRepo(t, emb)
	seedFedFact(t, ctxA, "seed-a", "mission/store", "alpha fact", "store", nil)

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	meter.Store(0)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"domain": []any{"store"}}
	result, err := QueryHandler(emb)(repos.WithBinding(context.Background(), b), req)
	require.NoError(t, err)
	require.Falsef(t, result.IsError, "query failed: %s", resultText(t, result))

	if got := meter.Load(); got != 0 {
		t.Errorf("EmbedQuery called %d times for a filter-only query, want 0", got)
	}
}
