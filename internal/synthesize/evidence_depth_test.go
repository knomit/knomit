package synthesize

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// Tests for depth composition in evidence_weight (spec §5.2).
//
// Setting `sources: 1` on share-type derivations (§5.1) left a gap: at depth 2
// every cited synthesis contributed `confidence × 1`, so a synthesis resting
// on ten well-corroborated facts scored the same as one resting on two flimsy
// ones. The evidence depth was simply invisible one level up.
//
// The repair is NOT to recover the mass arithmetically from the ref's stored
// weight (`w/(1-w)`, which is exact since w = S/(S+1)): that reintroduces the
// double-count the §5.1 split removed, because two syntheses sharing a leaf
// each recover it in full — and RAPTOR clusters round-1 outputs by similarity,
// so overlapping refs are the expected case.
//
// Instead the weight walks the lineage and deduplicates by path. The walk
// terminates correctly at exactly the boundaries that make a transitive walk
// impossible for `sources`:
//
//   - a TRANSFER output (merge) has already pooled its deleted inputs into its
//     own `sources`, so terminating there is exact — the walk never needs to
//     reach leaves that no longer exist;
//   - a SHARE output (distill, discover) left its sources alive, so the walk
//     passes through to them;
//   - an AUTHORED fact is always terminal, because its refs are citations
//     rather than lineage;
//   - a derived fact whose lineage is entirely gone (it retracted its own
//     sources, or they were deleted under it) falls back to its own mass
//     rather than contributing nothing.

// seedOriginFact writes a fact with an explicit origin, type, confidence,
// sources and refs — everything the lineage walk dispatches on.
func seedOriginFact(t *testing.T, svc *store.Service, branch, path string,
	typ fact.Type, origin fact.Origin, confidence float64, sources int, refs []string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = path
	f.Body = "body of " + path
	f.Type = typ
	f.Origin = origin
	f.Domain = []string{"test"}
	f.Confidence = confidence
	f.Sources = sources
	f.Refs = refs
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

// TestComputeWeight_ComposesThroughDerivedRefs is the acceptance test. A fact
// citing one distilled synthesis must see the synthesis's underlying leaves,
// not the flat `confidence × 1` the synthesis carries.
func TestComputeWeight_ComposesThroughDerivedRefs(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedOriginFact(t, svc, branch, "kb/l1.md", fact.Observation, fact.Authored, 0.8, 3, nil)
	seedOriginFact(t, svc, branch, "kb/l2.md", fact.Observation, fact.Authored, 0.8, 4, nil)
	seedOriginFact(t, svc, branch, "kb/d1.md", fact.Synthesis, fact.Distilled, 0.9, 1,
		[]string{"kb/l1.md", "kb/l2.md"})

	got := computeWeight(ctx, svc.Facts(), branch, []string{"kb/d1.md"})

	// Σ = 0.8×3 + 0.8×4 = 5.6, not the 0.9×1 the synthesis itself carries.
	require.InDelta(t, 5.6/6.6, got, 1e-9,
		"a cited synthesis must contribute the evidence it rests on, not confidence × 1")
}

// TestComputeWeight_DeduplicatesSharedAncestry is the whole reason for the
// walk. Two syntheses sharing a leaf must count it once — this is precisely
// what `w/(1-w)` mass recovery would get wrong.
func TestComputeWeight_DeduplicatesSharedAncestry(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedOriginFact(t, svc, branch, "kb/shared.md", fact.Observation, fact.Authored, 0.8, 5, nil)
	seedOriginFact(t, svc, branch, "kb/d1.md", fact.Synthesis, fact.Distilled, 0.9, 1,
		[]string{"kb/shared.md"})
	seedOriginFact(t, svc, branch, "kb/d2.md", fact.Synthesis, fact.Distilled, 0.9, 1,
		[]string{"kb/shared.md"})

	got := computeWeight(ctx, svc.Facts(), branch, []string{"kb/d1.md", "kb/d2.md"})

	// Σ = 0.8×5 = 4.0 — counted ONCE, not 8.0.
	require.InDelta(t, 4.0/5.0, got, 1e-9,
		"a leaf reachable through two syntheses is one corroboration, not two")
}

// TestComputeWeight_TransferOutputIsTerminal pins the boundary that makes the
// walk sound. A merge pooled its deleted inputs into its own sources; the walk
// must stop there and use that pooled count rather than chasing refs to files
// that no longer exist.
func TestComputeWeight_TransferOutputIsTerminal(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	// A merge survivor: authored-origin, sources pooled to 8. Its lineage ref
	// points at a fact that is very much alive and carries a large count — so
	// if the walk did NOT terminate here it would import 80 instead of 7.2,
	// and the assertion below discriminates.
	seedOriginFact(t, svc, branch, "kb/live-source.md", fact.Observation, fact.Authored, 0.8, 100, nil)
	seedOriginFact(t, svc, branch, "kb/merged.md", fact.Observation, fact.Authored, 0.9, 8,
		[]string{"kb/live-source.md"})

	got := computeWeight(ctx, svc.Facts(), branch, []string{"kb/merged.md"})

	require.InDelta(t, 7.2/8.2, got, 1e-9,
		"a merge survivor's pooled sources ARE its evidence; the walk must terminate there "+
			"rather than descending into lineage whose counts it already absorbed")
}

// TestComputeWeight_AuthoredRefsAreCitationsNotLineage guards the dispatch on
// origin. An authored fact's refs are "see also", not "derived from", so
// passing through them would import evidence the fact never rested on.
func TestComputeWeight_AuthoredRefsAreCitationsNotLineage(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedOriginFact(t, svc, branch, "kb/big.md", fact.Observation, fact.Authored, 0.8, 10, nil)
	seedOriginFact(t, svc, branch, "kb/a.md", fact.Observation, fact.Authored, 0.9, 2,
		[]string{"kb/big.md"})

	got := computeWeight(ctx, svc.Facts(), branch, []string{"kb/a.md"})

	require.InDelta(t, 1.8/2.8, got, 1e-9,
		"an authored fact is terminal — its refs are citations, not the evidence it rests on")
}

// TestComputeWeight_DerivedWithDeadLineageFallsBackToOwnMass covers the
// distill-that-retracted-its-sources case. Contributing 0 would erase a fact
// whose evidence was real but has since been consolidated away.
func TestComputeWeight_DerivedWithDeadLineageFallsBackToOwnMass(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedOriginFact(t, svc, branch, "kb/d.md", fact.Synthesis, fact.Distilled, 0.9, 1,
		[]string{"kb/retracted.md"})

	got := computeWeight(ctx, svc.Facts(), branch, []string{"kb/d.md"})

	require.InDelta(t, 0.9/1.9, got, 1e-9,
		"a synthesis whose lineage is gone still rests on its own act, not on nothing")
}

// TestComputeWeight_SkipsHypothesisAnywhereInLineage extends §5.2's exclusion
// through the walk: a conjecture launders no evidence at depth 2 either.
func TestComputeWeight_SkipsHypothesisAnywhereInLineage(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedOriginFact(t, svc, branch, "kb/l1.md", fact.Observation, fact.Authored, 0.8, 3, nil)
	seedOriginFact(t, svc, branch, "kb/h.md", fact.Hypothesis, fact.Authored, 0.9, 9, nil)
	seedOriginFact(t, svc, branch, "kb/d1.md", fact.Synthesis, fact.Distilled, 0.9, 1,
		[]string{"kb/l1.md", "kb/h.md"})

	got := computeWeight(ctx, svc.Facts(), branch, []string{"kb/d1.md"})

	require.InDelta(t, 2.4/3.4, got, 1e-9,
		"a hypothesis in the lineage contributes nothing, at any depth")
}

// TestComputeWeight_SurvivesRefCycle is the safety guard. Nothing forbids two
// facts citing each other, and an unguarded walk would not terminate.
func TestComputeWeight_SurvivesRefCycle(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedOriginFact(t, svc, branch, "kb/x.md", fact.Synthesis, fact.Distilled, 0.9, 1,
		[]string{"kb/y.md"})
	seedOriginFact(t, svc, branch, "kb/y.md", fact.Synthesis, fact.Distilled, 0.7, 1,
		[]string{"kb/x.md"})

	// A real deadline: context.Background().Done() is a nil channel, so
	// selecting on it would block forever and turn a non-terminating walk into
	// a hung test rather than a failing one.
	done := make(chan float64, 1)
	go func() { done <- computeWeight(ctx, svc.Facts(), branch, []string{"kb/x.md"}) }()
	select {
	case got := <-done:
		// x cites y, y cites x. y's only ref is a back-edge, so nothing
		// grounds it and it falls back to its own mass (0.7 × 1); x then sees
		// y resolve and contributes nothing of its own. Σ = 0.7.
		//
		// The value is pinned, not just bounded: a cycle that collected
		// NOTHING would return 0, and a 0 evidence_weight is indistinguishable
		// from "rests on no evidence" — the silent zero this whole area exists
		// to avoid.
		require.InDelta(t, 0.7/1.7, got, 1e-9,
			"a circular lineage must ground on its own mass, not collapse to zero")
	case <-time.After(10 * time.Second):
		t.Fatal("computeWeight did not terminate on a ref cycle")
	}
}

// TestComputeTransfer_PooledCountIgnoresLineageDepth keeps the two numbers
// from being confused. The weight composes through lineage; the pooled
// corroboration count is over the DIRECT subsumed facts only, because those
// are the ones the merge is about to delete.
func TestComputeTransfer_PooledCountIgnoresLineageDepth(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()

	seedOriginFact(t, svc, branch, "kb/l1.md", fact.Observation, fact.Authored, 0.8, 6, nil)
	seedOriginFact(t, svc, branch, "kb/d1.md", fact.Synthesis, fact.Distilled, 0.9, 1,
		[]string{"kb/l1.md"})

	_, pooled, readable := computeTransfer(ctx, svc.Facts(), branch, []string{"kb/d1.md"})

	require.Equal(t, 1, pooled,
		"pooled counts the DIRECT subsumed fact's own sources — the lineage below it is not being deleted")
	require.Equal(t, 1, readable)
}
