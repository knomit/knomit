package mcp

import (
	"context"
	"maps"
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

// TestLearnHandler_RejectsDiscoveredObservation is a regression test for the
// 2026-07-15 incident: an agent transcribing newsletter facts reasoned "I
// didn't author this, I read it in the world → discovered" and wrote 15
// observations with origin=discovered. origin records which pipeline minted
// the fact, so discovered is only legal on the types the discovery engine
// emits (synthesis, hypothesis); the error must steer the caller to authored.
func TestLearnHandler_RejectsDiscoveredObservation(t *testing.T) {
	_, ctx, emb := newOriginTestRepo(t)

	r, err := LearnHandler(emb)(ctx, learnReq("newsletter-ingest", map[string]any{
		"topic": "technology", "category": "security/advisories",
		"title": "Vendor patched 57 CVEs this Patch Tuesday",
		"body":  "Transcribed from a newsletter the agent read.",
		"type":  "observation", "confidence": 0.8, "sources": 1,
		"domain": []any{"security"}, "entities": []any{"cve"}, "refs": []any{},
		"origin": "discovered",
	}))
	require.NoError(t, err)
	require.True(t, r.IsError, "discovered+observation must be rejected")
	msg := resultText(t, r)
	require.Contains(t, msg, "discovery-engine output")
	require.Contains(t, msg, "authored", "error must name the correct origin for source-transcribed facts")
}

// TestLearnHandler_RejectsDistilledNonSynthesis mirrors the discovered check
// for the distill pipeline: distilled is only legal on type synthesis.
func TestLearnHandler_RejectsDistilledNonSynthesis(t *testing.T) {
	_, ctx, emb := newOriginTestRepo(t)

	r, err := LearnHandler(emb)(ctx, learnReq("bad-distilled", map[string]any{
		"topic": "technology", "category": "ai/agents",
		"title": "An observation mislabeled as distilled",
		"body":  "body",
		"type":  "observation", "confidence": 0.7, "sources": 1,
		"domain": []any{"ai"}, "entities": []any{"x"}, "refs": []any{},
		"origin": "distilled",
	}))
	require.NoError(t, err)
	require.True(t, r.IsError, "distilled+observation must be rejected")
	require.Contains(t, resultText(t, r), "synthesis-pipeline output")
}

// TestLearnHandler_AllowsDiscoveredHypothesis verifies the backward-bridge
// preview path stays open: discovered pairs with type hypothesis.
func TestLearnHandler_AllowsDiscoveredHypothesis(t *testing.T) {
	svc, ctx, emb := newOriginTestRepo(t)

	r, err := LearnHandler(emb)(ctx, learnReq("save-backward-bridge", map[string]any{
		"topic": "technology", "category": "ai/agents",
		"title": "Abductive keystone that would entail the seed cluster",
		"body":  "Hypothesis statement; evidence chain; reasoning; gaps; falsification condition.",
		"type":  "hypothesis", "confidence": 0.5, "sources": 1,
		"domain": []any{"ai"}, "entities": []any{"memory"}, "refs": []any{},
		"origin": "discovered",
	}))
	require.NoError(t, err)
	require.False(t, r.IsError, "discovered+hypothesis must be accepted: %s", resultText(t, r))

	got := readBack(t, svc, mergedFactPath(t, r))
	require.Equal(t, fact.Discovered, got.Origin)
}

// TestLearnHandler_DedupMergeWeightExcludesSelfCitation is a regression test:
// when a discovered fact dedup-merges against an existing fact, the merge
// appends the fact's own resulting path to refs as lineage. The evidence_weight
// computation must NOT count that self-path as a source — otherwise a merged
// derived fact inflates its own weight by citing its predecessor as evidence.
func TestLearnHandler_DedupMergeWeightExcludesSelfCitation(t *testing.T) {
	svc, ctx, emb := newOriginTestRepo(t)

	// Seed the only legitimate source. weight must derive from THIS alone.
	rs, err := LearnHandler(emb)(ctx, learnReq("seed-source", map[string]any{
		"topic": "technology", "category": "ai/agents",
		"title": "Grounded source for the bridge",
		"body":  "An observation the discovered fact cites.",
		"type":  "observation", "confidence": 0.8, "sources": 2,
		"domain": []any{"ai"}, "entities": []any{"memory"}, "refs": []any{},
	}))
	require.NoError(t, err)
	require.False(t, rs.IsError, "seed write failed: %s", resultText(t, rs))
	sourcePath := mergedFactPath(t, rs)

	// First discovered fact citing only the source. Its weight is the baseline
	// (computed from the source alone).
	discoveredFact := map[string]any{
		"topic": "technology", "category": "ai/agents",
		"title": "Emergent bridge claim about memory portability",
		"body":  "Bridging the cluster yields this emergent claim.",
		"type":  "synthesis", "confidence": 0.75, "sources": 1,
		"domain": []any{"ai"}, "entities": []any{"memory"},
		"refs":   []any{sourcePath},
		"origin": "discovered",
	}
	r1, err := LearnHandler(emb)(ctx, learnReq("save-discovered-1", discoveredFact))
	require.NoError(t, err)
	require.False(t, r1.IsError, "first discovered write failed: %s", resultText(t, r1))
	firstPath := mergedFactPath(t, r1)
	baseline := readBack(t, svc, firstPath).EvidenceWeight
	require.Greater(t, baseline, 0.0, "baseline weight must be computed from the source")

	// Save an identical-content discovered fact: the len-stub embedder yields an
	// identical vector, forcing a dedup-merge into firstPath. The merge appends
	// firstPath (the fact's own resulting path) to refs.
	merging := map[string]any{}
	maps.Copy(merging, discoveredFact)
	merging["confidence"] = 0.85 // new fact wins, stays discovered
	r2, err := LearnHandler(emb)(ctx, learnReq("save-discovered-2", merging))
	require.NoError(t, err)
	require.False(t, r2.IsError, "merging discovered write failed: %s", resultText(t, r2))

	merged := readBack(t, svc, mergedFactPath(t, r2))
	require.Equal(t, firstPath, merged.Path(), "second fact must merge into the first's path")
	require.Contains(t, merged.Refs, firstPath,
		"merge appends the fact's own path to refs (lineage) — the condition under test")
	require.Equal(t, fact.Discovered, merged.Origin, "merged fact must stay discovered")
	require.InDelta(t, baseline, merged.EvidenceWeight, 1e-9,
		"weight must derive from the genuine source only, not inflate by counting the fact's own predecessor path as evidence")
}

// TestLocalEvidenceRefs_ExcludesKBRefs pins the evidence-weight local-ref filter
// against classifyRefs drift: a machine-origin fact citing a kb:// cross-repo ref
// must compute its weight from only the genuinely-local refs — the kb:// ref must
// never enter the local-ref set handed to ComputeEvidenceWeight, even though it
// ends in .md. (Today the numeric weight is accidentally correct only because
// ComputeEvidenceWeight's ReadFact fails silently on the literal kb:// path; this
// pins the filter directly so that accident cannot mask a regression.)
func TestLocalEvidenceRefs_ExcludesKBRefs(t *testing.T) {
	f := fact.NewFact("kb/observations/ai/derived.md")
	localRef := "kb/observations/ai/source.md"
	kbRef := "kb://3f9a2c1e8b7d/kb/observations/ai/foreign.md"
	f.Refs = []string{
		localRef,     // genuinely-local: kept
		kbRef,        // cross-repo kb:// (ends in .md): dropped
		f.Path(),     // the fact's own path (dedup-merge lineage): dropped
		"docs/x.txt", // non-.md: dropped
	}

	got := localEvidenceRefs(f)
	require.Equal(t, []string{localRef}, got,
		"only the genuinely-local .md ref survives — the kb:// ref, self-path, and non-.md ref are excluded")
	require.NotContains(t, got, kbRef,
		"a kb:// cross-repo ref must never enter the local evidence set (mirrors classifyRefs)")
}
