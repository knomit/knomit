package mcp

import (
	"context"
	"path/filepath"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// newOriginTestRepo opens a fresh store with no ontology (so any topic is
// writable) and a deterministic embedder, returning the service and a
// repo-scoped context for driving LearnHandler.
func newOriginTestRepo(t *testing.T) (*store.Service, context.Context, store.BatchEmbedder) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })

	emb := newLenEmbedder(t)
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  "agent/test",
		Svc:          svc,
		OntologyRoot: "kb",
		Embedder:     emb,
	})
	return svc, repos.WithRepoInstance(context.Background(), ri), emb
}

func learnReq(moment string, f map[string]any) mcpgo.CallToolRequest {
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": moment,
		"facts":       []any{f},
	}
	return req
}

func readBack(t *testing.T, svc *store.Service, path string) fact.Fact {
	t.Helper()
	res, err := svc.Facts().ReadFact(context.Background(), "agent/test", path, nil)
	require.NoError(t, err)
	f, err := fact.ParseFact(path, res.Content)
	require.NoError(t, err)
	return f
}

// TestLearnHandler_OriginDiscoveredPersistsAndWeighs verifies that a fact an
// agent saves with origin=discovered (the previewed-discovery workflow) is
// written as discovered AND gets an evidence_weight computed from its local
// refs — matching what the auto-apply discovery path would have produced.
func TestLearnHandler_OriginDiscoveredPersistsAndWeighs(t *testing.T) {
	svc, ctx, emb := newOriginTestRepo(t)

	// Seed a source observation the discovered fact will cite.
	r1, err := LearnHandler(emb)(ctx, learnReq("seed-source", map[string]any{
		"topic": "technology", "category": "ai/agents",
		"title": "Source observation about agent memory",
		"body":  "Agents persist memory across sessions.",
		"type":  "observation", "confidence": 0.8, "sources": 2,
		"domain": []any{"ai"}, "entities": []any{"memory"}, "refs": []any{},
	}))
	require.NoError(t, err)
	require.False(t, r1.IsError, "seed write failed: %s", resultText(t, r1))
	sourcePath := mergedFactPath(t, r1)

	// Save a discovered synthesis fact citing the source.
	r2, err := LearnHandler(emb)(ctx, learnReq("save-discovered", map[string]any{
		"topic": "technology", "category": "ai/agents",
		"title": "Emergent bridge: memory portability becomes the competitive axis",
		"body":  "Bridging the source cluster yields this emergent claim.",
		"type":  "synthesis", "confidence": 0.75, "sources": 1,
		"domain": []any{"ai"}, "entities": []any{"memory"},
		"refs":   []any{sourcePath},
		"origin": "discovered",
	}))
	require.NoError(t, err)
	require.False(t, r2.IsError, "discovered write failed: %s", resultText(t, r2))

	got := readBack(t, svc, mergedFactPath(t, r2))
	require.Equal(t, fact.Discovered, got.Origin, "origin must persist as discovered")
	require.Greater(t, got.EvidenceWeight, 0.0,
		"evidence_weight must be computed from the local ref (source conf 0.8 × 2 sources)")
}

// TestLearnHandler_OriginDefaultsAuthored verifies that omitting origin leaves
// the fact authored (the default) — ordinary learn calls are unchanged, and no
// evidence_weight is computed for authored facts even when they cite a ref.
func TestLearnHandler_OriginDefaultsAuthored(t *testing.T) {
	svc, ctx, emb := newOriginTestRepo(t)

	r1, err := LearnHandler(emb)(ctx, learnReq("seed-source", map[string]any{
		"topic": "technology", "category": "ai/agents",
		"title": "Another source observation",
		"body":  "Some grounded observation.",
		"type":  "observation", "confidence": 0.9, "sources": 3,
		"domain": []any{"ai"}, "entities": []any{"x"}, "refs": []any{},
	}))
	require.NoError(t, err)
	require.False(t, r1.IsError, "seed write failed: %s", resultText(t, r1))
	sourcePath := mergedFactPath(t, r1)

	// Use a non-synthesis type: a synthesis fact with omitted origin would
	// parse back as distilled via the legacy frontmatter heuristic, which is a
	// separate (pre-existing) behavior. An observation cleanly exercises the
	// authored default.
	r2, err := LearnHandler(emb)(ctx, learnReq("save-authored", map[string]any{
		"topic": "technology", "category": "ai/agents",
		"title": "A hand-authored observation that cites a fact",
		"body":  "Authored, not from the discovery engine.",
		"type":  "observation", "confidence": 0.7, "sources": 1,
		"domain": []any{"ai"}, "entities": []any{"x"},
		"refs": []any{sourcePath},
		// origin omitted
	}))
	require.NoError(t, err)
	require.False(t, r2.IsError, "authored write failed: %s", resultText(t, r2))

	got := readBack(t, svc, mergedFactPath(t, r2))
	require.Equal(t, fact.Authored, got.Origin, "omitted origin must default to authored")
	require.Equal(t, 0.0, got.EvidenceWeight,
		"authored facts must not get an auto-computed weight (only distilled/discovered do)")
}

// TestLearnHandler_RejectsInvalidOrigin verifies a bad origin value is a clean
// error, not a silent default.
func TestLearnHandler_RejectsInvalidOrigin(t *testing.T) {
	_, ctx, emb := newOriginTestRepo(t)

	r, err := LearnHandler(emb)(ctx, learnReq("bad-origin", map[string]any{
		"topic": "technology", "category": "ai/agents",
		"title": "Fact with a bogus origin",
		"body":  "body",
		"type":  "observation", "confidence": 0.7, "sources": 1,
		"domain": []any{"ai"}, "entities": []any{"x"}, "refs": []any{},
		"origin": "invented",
	}))
	require.NoError(t, err)
	require.True(t, r.IsError, "invalid origin must produce an error result")
	require.Contains(t, resultText(t, r), "invalid origin")
}
