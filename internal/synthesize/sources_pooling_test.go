package synthesize

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// Tests for `sources` on every derived-fact write path in this package.
//
// spec/mbekg.md §2.2 defines sources as "Count of independent corroborations
// — how many independent agents or observations produced this fact", and §5.1
// splits the derived writes in two by whether the inputs survive:
//
//   - TRANSFER (prune-merge, dedupCluster): the inputs are DELETED, so their
//     corroborations have nowhere else to live and must move to the output.
//     sources = sum of the subsumed facts', read before deletion.
//   - SHARE (distill, discover, reflect-propose): the inputs stay ALIVE
//     holding their own counts. sources = 1, one act of synthesis. Summing
//     here would record one observation in two live facts at once, and RAPTOR
//     distills over its own output, so the inflation compounds per level.
//     The underlying evidence is carried by evidence_weight instead.
//
// Hypothesis-typed sources are excluded from every pool, matching §5.2's
// exclusion from evidence_weight: a conjecture corroborates nothing.

func newSourcesTestRepo(t *testing.T) (*store.Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	return svc, "agent/test"
}

// seedFactWithSources writes an observation carrying an explicit sources
// count, so a test can assert on a pooled total rather than on a count that
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

// readSources parses the fact at path and returns its sources count.
func readSources(t *testing.T, svc *store.Service, branch, path string) int {
	t.Helper()
	rf, err := svc.Facts().ReadFact(context.Background(), branch, path, nil)
	require.NoError(t, err)
	parsed, err := fact.ParseFact(path, rf.Content)
	require.NoError(t, err)
	return parsed.Sources
}

// collectWarns returns an onProgress sink plus a pointer to the warn messages
// it accumulates.
func collectWarns() (func(ProgressEvent), *[]string) {
	var warns []string
	return func(e ProgressEvent) {
		if e.Phase == "warn" {
			warns = append(warns, e.Message)
		}
	}, &warns
}

// ── SHARE: distill, discover, reflect-propose ─────────────────────────────

// TestApplyDistillDecisions_SourcesIsOne_NotPooled is the acceptance test for
// the share rule. Its three inputs carry 3 + 4 + 5 corroborations and all
// survive the distill, so the new fact claims one — its own act of synthesis
// — rather than the twelve that are still recorded on the living sources.
func TestApplyDistillDecisions_SourcesIsOne_NotPooled(t *testing.T) {
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

	require.Equal(t, 1, readSources(t, svc, branch, written[0].Path),
		"a distilled fact shares its sources with facts that stay alive, so it claims one act of synthesis, not their pooled counts")

	// The inputs are untouched — which is exactly why pooling would double-count.
	require.Equal(t, 3, readSources(t, svc, branch, "kb/technology/a.md"))
	require.Equal(t, 4, readSources(t, svc, branch, "kb/technology/b.md"))
	require.Equal(t, 5, readSources(t, svc, branch, "kb/technology/c.md"))
}

// TestApplyDistillDecisions_EvidenceWeightStillPoolsRefs pins the other half
// of the share rule: sources stops carrying the underlying evidence, so
// evidence_weight must still carry it, or the depth information is simply
// gone.
func TestApplyDistillDecisions_EvidenceWeightStillPoolsRefs(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedFactWithSources(t, svc, branch, "kb/technology/a.md", 3)
	seedFactWithSources(t, svc, branch, "kb/technology/b.md", 4)

	df := distillFact{
		Path: "kb/technology/synth.md", Title: "S", Body: "distilled", Type: "synthesis",
		Domain: []string{"technology"}, Confidence: 0.9,
		Refs: []string{"kb/technology/a.md", "kb/technology/b.md"},
	}
	_, written, err := ApplyDistillDecisions(ctx, svc.Facts(), []distillFact{df}, nil,
		"test", func(ProgressEvent) {}, branch, "kb")
	require.NoError(t, err)

	rf, err := svc.Facts().ReadFact(ctx, branch, written[0].Path, nil)
	require.NoError(t, err)
	parsed, err := fact.ParseFact(written[0].Path, rf.Content)
	require.NoError(t, err)

	// Σ(0.8×3 + 0.8×4) = 5.6 → 5.6/6.6
	require.InDelta(t, 5.6/6.6, parsed.EvidenceWeight, 1e-9,
		"evidence_weight must still pool the cited sources — it is where the underlying evidence now lives")
}

// TestApplyDiscoveredProposals_SourcesIsOne covers discovery, where pooling
// would be worse than in distill: a bridge is selected for facts that already
// co-occur, so shared ancestry is the expected case.
func TestApplyDiscoveredProposals_SourcesIsOne(t *testing.T) {
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

	require.Equal(t, 1, readSources(t, svc, branch, written[0]),
		"a discovered fact shares its bridge members, which stay alive; it claims one act of discovery")
}

// TestApplyReflectDecisions_ProposeSourcesIsOne covers the last share path. A
// methodology's refs are the transitions that motivated it — explanatory
// citations, not corroborations of the methodology itself.
func TestApplyReflectDecisions_ProposeSourcesIsOne(t *testing.T) {
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
	require.Equal(t, 1, readSources(t, svc, branch, found[0]),
		"a proposed methodology cites its motivating transitions; they do not corroborate it")
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

// ── TRANSFER: prune-merge ─────────────────────────────────────────────────

// TestApplyPruneDecisions_MergePoolsSubsumedSources is the acceptance test for
// the transfer rule. The merge deletes both inputs, so their corroborations
// move to the survivor — and the count the model proposed is ignored, because
// the facts that carried the real counts are about to stop existing.
func TestApplyPruneDecisions_MergePoolsSubsumedSources(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedFactWithSources(t, svc, branch, "kb/technology/a.md", 2)
	seedFactWithSources(t, svc, branch, "kb/technology/b.md", 6)

	merges := []MergeEntry{{
		Paths: []string{"kb/technology/a.md", "kb/technology/b.md"},
		Merged: mergedFact{
			Path: "kb/technology/merged.md", Title: "M", Body: "merged", Type: "observation",
			Domain: []string{"technology"}, Confidence: 0.9,
			Sources: 1, // the model lowballs; the server must not trust it
		},
	}}
	stats, err := ApplyPruneDecisions(ctx, svc.Facts(), nil, merges,
		"test", func(ProgressEvent) {}, branch, "kb")
	require.NoError(t, err)
	require.Equal(t, 1, stats.Merged)

	require.Equal(t, 8, readSources(t, svc, branch, "kb/technology/merged.md"),
		"a merge deletes its inputs, so their corroborations must transfer to the survivor")

	// And they really are gone, which is what makes the transfer mandatory.
	_, err = svc.Facts().ReadFact(ctx, branch, "kb/technology/a.md", nil)
	require.Error(t, err, "precondition: the merge must have deleted its sources")
}

// TestApplyPruneDecisions_MergeExcludesHypothesisSources keeps a conjecture
// from laundering into the survivor's evidence count.
func TestApplyPruneDecisions_MergeExcludesHypothesisSources(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedFactWithSources(t, svc, branch, "kb/technology/a.md", 2)
	seedTypedFactWithSources(t, svc, branch, "kb/technology/h.md", 9, fact.Hypothesis)

	merges := []MergeEntry{{
		Paths: []string{"kb/technology/a.md", "kb/technology/h.md"},
		Merged: mergedFact{
			Path: "kb/technology/merged.md", Title: "M", Body: "merged", Type: "observation",
			Domain: []string{"technology"}, Confidence: 0.9,
		},
	}}
	_, err := ApplyPruneDecisions(ctx, svc.Facts(), nil, merges,
		"test", func(ProgressEvent) {}, branch, "kb")
	require.NoError(t, err)

	require.Equal(t, 2, readSources(t, svc, branch, "kb/technology/merged.md"),
		"a hypothesis is a conjecture; its count must not pool into the merged fact")
}

// TestApplyPruneDecisions_MergeWarnsWhenSourcesUnreadable pins the loud
// failure on the destructive path. Flooring silently at 1 would dress up
// "I deleted N facts whose evidence I never counted" as a plausible number.
func TestApplyPruneDecisions_MergeWarnsWhenSourcesUnreadable(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	onProgress, warns := collectWarns()
	merges := []MergeEntry{{
		Paths: []string{"kb/technology/gone.md"}, // never written
		Merged: mergedFact{
			Path: "kb/technology/merged.md", Title: "M", Body: "merged", Type: "observation",
			Domain: []string{"technology"}, Confidence: 0.9,
		},
	}}
	_, err := ApplyPruneDecisions(ctx, svc.Facts(), nil, merges, "test", onProgress, branch, "kb")
	require.NoError(t, err)

	require.Equal(t, 1, readSources(t, svc, branch, "kb/technology/merged.md"),
		"the floor still applies — 0 would make the fact invisible to every downstream weight")
	require.NotEmpty(t, *warns, "a merge that could read none of its sources must say so")
	require.Contains(t, (*warns)[0], "could be read",
		"the warn must name the failure, not just the merge")
}

// ── TRANSFER: review dedupCluster ─────────────────────────────────────────

// TestMergeFacts_SumsSourcesOfBothWinnerAndLoser covers the dedup transfer.
// The loser is deleted by dedupCluster, so its corroborations move.
func TestMergeFacts_SumsSourcesOfBothWinnerAndLoser(t *testing.T) {
	a := factForLLM{File: "a.md", Type: "observation", Confidence: 0.9, Sources: 3}
	b := factForLLM{File: "b.md", Type: "observation", Confidence: 0.5, Sources: 4}

	winner, loser := mergeFacts(a, b)
	require.Equal(t, "a.md", winner.File, "higher confidence wins")
	require.Equal(t, "b.md", loser.File)
	require.Equal(t, 7, winner.Sources,
		"the loser is deleted, so its corroborations transfer to the winner")
}

// TestMergeFacts_ExcludesHypothesisLoserSources is the regression anchor for a
// pre-existing laundering hole: dedup summed a hypothesis loser's count into
// the winner, while computeTransfer and learn's subsumeHypothesis both
// correctly refuse to. Write five hypotheses, let dedup absorb them, and the
// survivor read as well-corroborated.
func TestMergeFacts_ExcludesHypothesisLoserSources(t *testing.T) {
	obs := factForLLM{File: "obs.md", Type: "observation", Confidence: 0.4, Sources: 2}
	hyp := factForLLM{File: "hyp.md", Type: "hypothesis", Confidence: 0.99, Sources: 9}

	winner, loser := mergeFacts(obs, hyp)
	require.Equal(t, "obs.md", winner.File, "non-hypothesis wins regardless of confidence")
	require.Equal(t, "hyp.md", loser.File)
	require.Equal(t, 2, winner.Sources,
		"a hypothesis corroborates nothing, so its count must not pool into the winner")
}

// TestMergeFacts_HypothesisWinnerKeepsOwnSources covers the symmetric case:
// two hypotheses merging pool normally, since neither is being treated as
// evidence for the other — the exclusion is about what a hypothesis
// CONTRIBUTES, not about hypotheses being uncountable.
func TestMergeFacts_HypothesisWinnerKeepsOwnSources(t *testing.T) {
	h1 := factForLLM{File: "h1.md", Type: "hypothesis", Confidence: 0.9, Sources: 2}
	h2 := factForLLM{File: "h2.md", Type: "hypothesis", Confidence: 0.5, Sources: 3}

	winner, _ := mergeFacts(h1, h2)
	require.Equal(t, 2, winner.Sources,
		"a hypothesis loser contributes nothing even when the winner is also a hypothesis")
}

// ── the floor ─────────────────────────────────────────────────────────────

// TestComputeTransfer_FloorsAtOne pins the floor directly. 0 is not a neutral
// value: evidence_weight multiplies by sources, so a 0-source fact is
// permanently invisible to every weight computed over it.
func TestComputeTransfer_FloorsAtOne(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	weight, pooled, readable := computeTransfer(ctx, svc.Facts(), branch, []string{"kb/missing.md"})
	require.Equal(t, 1, pooled, "an unreadable source set still floors at one, never zero")
	require.Equal(t, 0, readable, "readable must report the truth so a destructive caller can warn")
	require.Zero(t, weight, "no readable source means no evidence weight")
}

// TestComputeTransfer_ZeroSourceFactsStillFloor covers the other route to
// zero: sources that read fine but genuinely carry 0.
func TestComputeTransfer_ZeroSourceFactsStillFloor(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedFactWithSources(t, svc, branch, "kb/z.md", 0)

	_, pooled, readable := computeTransfer(ctx, svc.Facts(), branch, []string{"kb/z.md"})
	require.Equal(t, 1, pooled)
	require.Equal(t, 1, readable, "the source WAS readable — it just carried no corroborations")
}
