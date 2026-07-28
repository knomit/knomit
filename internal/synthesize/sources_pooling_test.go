package synthesize

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// Tests for `sources` pooling on every derived-fact write path.
//
// spec/mbekg.md §2.2 defines sources as "Count of independent corroborations
// — how many independent agents or observations produced this fact". Under
// that definition a fact derived from N others is corroborated by everything
// those N rest on, so the derived fact's sources is the sum of its local
// sources' sources — NOT a hardcoded 1, which reports a 40-observation
// synthesis as no better corroborated than a single stray note.
//
// Hypothesis-typed sources are excluded from the sum for the same reason
// §5.2 excludes them from evidence_weight: a hypothesis is a conjecture, not
// a corroboration. And the sum floors at 1 — a derived fact that cites no
// local facts was still produced by one act of synthesis, and letting it fall
// to 0 would reintroduce exactly the zero that makes evidence_weight collapse.

// seedFactWithSources writes an observation carrying an explicit sources
// count, so a test can assert on the pooled total rather than on a count that
// happens to equal the number of inputs.
func seedFactWithSources(t *testing.T, svc *store.Service, branch, path string, sources int) {
	t.Helper()
	seedTypedFactWithSources(t, svc, branch, path, sources, fact.Observation)
}

func seedTypedFactWithSources(t *testing.T, svc *store.Service, branch, path string, sources int, typ fact.Type) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = path
	f.Body = "body of " + path
	f.Type = typ
	f.Domain = []string{"test"}
	f.Confidence = 0.8
	f.Sources = sources
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

func newSourcesTestRepo(t *testing.T) (*store.Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	return svc, "agent/test"
}

// readSources parses the fact at path and returns its sources count.
func readSources(t *testing.T, svc *store.Service, branch, path string) int {
	t.Helper()
	rf, err := svc.Facts().ReadFact(context.Background(), branch, path, nil)
	require.NoError(t, err)
	parsed, err := fact.ParseFact(path, rf.Content)
	require.NoError(t, err)
	return parsed.Sources
}

// TestApplyDistillDecisions_SourcesPoolFromRefs is the acceptance test for
// distill. Three inputs carrying 3 + 4 + 5 corroborations distill into one
// fact that rests on all twelve.
func TestApplyDistillDecisions_SourcesPoolFromRefs(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedFactWithSources(t, svc, branch, "kb/technology/a.md", 3)
	seedFactWithSources(t, svc, branch, "kb/technology/b.md", 4)
	seedFactWithSources(t, svc, branch, "kb/technology/c.md", 5)

	df := distillFact{
		Path: "kb/technology/synth.md", Title: "S", Body: "distilled", Type: "synthesis",
		Domain: []string{"technology"}, Confidence: 0.9,
		Refs: []string{"kb/technology/a.md", "kb/technology/b.md", "kb/technology/c.md"},
	}
	stats, written, err := ApplyDistillDecisions(ctx, svc.Facts(), []distillFact{df}, nil,
		"test", func(ProgressEvent) {}, branch, "kb")
	require.NoError(t, err)
	require.Equal(t, 1, stats.Synthesized)
	require.Len(t, written, 1)

	require.Equal(t, 12, readSources(t, svc, branch, written[0].Path),
		"a distilled fact must carry the pooled corroborations of the facts it was built from")
}

// TestApplyDistillDecisions_SourcesSkipHypothesisRefs mirrors the
// evidence_weight rule: a conjecture corroborates nothing, so it contributes
// no sources even though it is a legitimate ref.
func TestApplyDistillDecisions_SourcesSkipHypothesisRefs(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedFactWithSources(t, svc, branch, "kb/technology/a.md", 3)
	seedTypedFactWithSources(t, svc, branch, "kb/technology/h.md", 9, fact.Hypothesis)

	df := distillFact{
		Path: "kb/technology/synth.md", Title: "S", Body: "distilled", Type: "synthesis",
		Domain: []string{"technology"}, Confidence: 0.9,
		Refs: []string{"kb/technology/a.md", "kb/technology/h.md"},
	}
	_, written, err := ApplyDistillDecisions(ctx, svc.Facts(), []distillFact{df}, nil,
		"test", func(ProgressEvent) {}, branch, "kb")
	require.NoError(t, err)
	require.Len(t, written, 1)

	require.Equal(t, 3, readSources(t, svc, branch, written[0].Path),
		"a hypothesis ref is a conjecture, not a corroboration, and must not be pooled")
}

// TestApplyDistillDecisions_SourcesFloorAtOne pins the floor. A synthesis
// citing nothing local was still produced by one act of synthesis; 0 would
// make its own evidence_weight contribution vanish downstream.
func TestApplyDistillDecisions_SourcesFloorAtOne(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	df := distillFact{
		Path: "kb/technology/synth.md", Title: "S", Body: "distilled", Type: "synthesis",
		Domain: []string{"technology"}, Confidence: 0.9,
		Refs: []string{"https://example.com/paper"},
	}
	_, written, err := ApplyDistillDecisions(ctx, svc.Facts(), []distillFact{df}, nil,
		"test", func(ProgressEvent) {}, branch, "kb")
	require.NoError(t, err)
	require.Len(t, written, 1)

	require.Equal(t, 1, readSources(t, svc, branch, written[0].Path),
		"a derived fact with no local sources still counts as one corroboration, never zero")
}

// TestApplyPruneDecisions_MergeSourcesPoolFromSubsumed is the merge case. The
// merged fact REPLACES its sources and they are then deleted, so taking the
// count the model happened to emit throws away the corroborations the merge
// just absorbed. §5.3 already mandates summing for dedup-on-learn; this makes
// the synthesis merge agree.
func TestApplyPruneDecisions_MergeSourcesPoolFromSubsumed(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedFactWithSources(t, svc, branch, "kb/technology/a.md", 2)
	seedFactWithSources(t, svc, branch, "kb/technology/b.md", 6)

	merges := []MergeEntry{{
		Paths: []string{"kb/technology/a.md", "kb/technology/b.md"},
		Merged: mergedFact{
			Path: "kb/technology/merged.md", Title: "M", Body: "merged", Type: "observation",
			Domain: []string{"technology"}, Confidence: 0.9,
			// The model lowballs the count; the server must not trust it.
			Sources: 1,
		},
	}}
	stats, err := ApplyPruneDecisions(ctx, svc.Facts(), nil, merges,
		"test", func(ProgressEvent) {}, branch, "kb")
	require.NoError(t, err)
	require.Equal(t, 1, stats.Merged)

	require.Equal(t, 8, readSources(t, svc, branch, "kb/technology/merged.md"),
		"a merged fact must carry the pooled corroborations of the facts it subsumed, not the model's number")
}

// TestApplyDiscoveredProposals_SourcesPoolFromRefs covers the discovery path,
// which wrote a flat 1 for the same reason distill did.
func TestApplyDiscoveredProposals_SourcesPoolFromRefs(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedFactWithSources(t, svc, branch, "kb/a.md", 4)
	seedFactWithSources(t, svc, branch, "kb/b.md", 7)

	payload := DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token: "x", Kind: BridgeEntity,
			Members: []factForLLM{{File: "kb/a.md", Title: "A"}, {File: "kb/b.md", Title: "B"}},
		},
	}
	props := []DiscoveredFact{{
		Path: "kb/d.md", Title: "d", Body: "d", Type: "synthesis",
		Domain: []string{"x"}, Confidence: 0.9, Refs: []string{"kb/a.md", "kb/b.md"},
	}}

	written, err := applyDiscoveredProposals(ctx, svc.Facts(), svc.Search(), nil,
		payload, props, DiscoveryGates{}, branch, "kb", nil)
	require.NoError(t, err)
	require.Len(t, written, 1)

	require.Equal(t, 11, readSources(t, svc, branch, written[0]),
		"a discovered fact must carry the pooled corroborations of its bridge sources")
}

// TestApplyReflectDecisions_ProposeSourcesPoolFromRefs covers the last
// derived-fact writer. A proposed methodology rests on the facts it cites, so
// it pools them like any other synthesis rather than reporting a flat 1.
func TestApplyReflectDecisions_ProposeSourcesPoolFromRefs(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()
	branch := "agent/test"

	seedFactWithSources(t, svc, branch, "kb/technology/a.md", 2)
	seedFactWithSources(t, svc, branch, "kb/technology/b.md", 3)

	sess, err := svc.Pipeline().CreatePipelineSession(ctx, "review", branch)
	require.NoError(t, err)

	result := ReflectResult{Propose: []ProposeEntry{{
		TopicPath: "meta/reasoning", Title: "M", Body: "a methodology",
		NoveltyArgument: "nothing like it exists", Confidence: 0.8,
		TransitionPaths: []string{"kb/technology/a.md"},
		Refs:            []string{"kb/technology/a.md", "kb/technology/b.md"},
	}}}
	require.NoError(t, ApplyReflectDecisions(ctx, svc.Facts(), svc.Search(), result, sess,
		ri.OntologyRoot(), 0.95, nil))

	found := methodologyFactPaths(t, svc, branch)
	require.Len(t, found, 1, "precondition: the propose arm must have written one methodology")
	require.Equal(t, 5, readSources(t, svc, branch, found[0]),
		"a proposed methodology must pool the corroborations of the facts it cites")
}

// methodologyFactPaths lists every methodology-typed fact on the branch.
func methodologyFactPaths(t *testing.T, svc *store.Service, branch string) []string {
	t.Helper()
	hits, err := svc.Search().Search(context.Background(), branch, store.SearchOptions{
		IncludeTypes: []string{string(fact.Methodology)}, Limit: 50,
	})
	require.NoError(t, err)
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Path)
	}
	return out
}
