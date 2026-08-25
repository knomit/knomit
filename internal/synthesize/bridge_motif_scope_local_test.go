package synthesize

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// knomit#119. The activation counter is computed over THIS SESSION'S SEED POOL
// — correct mechanics — but the sentence beside it claimed something about the
// corpus:
//
//	"the corpus has too little repeated vocabulary for a shared-motif pair to
//	 mean anything yet"
//
// On a scoped session that is false, and measurably so: it printed on a corpus
// roughly 4x past the activation floor, in the same health block as a
// corpus-wide vocabulary line reporting 43 clusters and 16 recurring.
//
// It is the sharpest member of its family because the other members require a
// reader to misread a number. This line performed the misreading itself, in
// English, adjacent to the line that contradicted it — and since scoped
// sessions are the standard operating mode, it printed routinely.
//
// The severity is not hypothetical. An operator with ~50 sessions of exposure
// re-derived the confusion from scratch and escalated a structural-
// unreachability concern for a corpus that had already had bridging enumerate
// live. A false sentence printed every session eventually colonises any reader.
func TestMotifBridgeHealthLines_InactiveLineIsScopeLocal(t *testing.T) {
	lines := strings.Join(motifBridgeHealthLines(motifEnumHealth{
		Activation: motifActivation{
			Evaluated:   true,
			Active:      false,
			DF2Clusters: 0,
			Pairs:       0,
			Seeds:       2,
		},
	}, 0, 0), "\n")

	require.Contains(t, lines, "motif bridging inactive",
		"the line still fires — the mechanics were never the defect")

	// THE FIX. The claim must be about the pool the number was computed over.
	require.Contains(t, lines, "2",
		"the line names how many seed facts the decision was made over — "+
			"without that a reader cannot tell a 2-fact scope from the corpus")

	// THE DEFECT, stated as its own assertion so it cannot come back by
	// rewording. Any sentence generalising from this session's pool to the
	// corpus is the bug, whatever words carry it.
	lower := strings.ToLower(lines)
	require.NotContains(t, lower, "the corpus has too little",
		"the retired claim, verbatim")
	require.NotContains(t, lower, "the corpus",
		"this line may not make ANY claim about the corpus: it is computed over "+
			"one session's seed pool, and on a scoped session the corpus can be "+
			"far past the floor while this pool is not")
}

// The second half of #119's severity, and a separate assertion because it is a
// separate misreading. The retired sentence ended:
//
//	"Recomputed every session; it activates itself as recurrence accumulates."
//
// which the operator read as "keep going and it will switch on" — a promise
// about the future of a corpus, made by a line that only ever measured one
// session's pool. On a scoped session the pool does not accumulate anything;
// the next narrow scope reports inactive again.
func TestMotifBridgeHealthLines_InactiveLinePromisesNoFutureActivation(t *testing.T) {
	lines := strings.ToLower(strings.Join(motifBridgeHealthLines(motifEnumHealth{
		Activation: motifActivation{Evaluated: true, Active: false, DF2Clusters: 1, Seeds: 3},
	}, 0, 0), "\n"))

	require.NotContains(t, lines, "activates itself",
		"the retired promise, verbatim — it invited an operator to wait for an "+
			"activation that a narrow scope will never reach")
	require.NotContains(t, lines, "accumulates",
		"nothing accumulates across sessions here: the count is recomputed from "+
			"the pool in hand, and a scoped pool is a fresh pool each time")
}

// The seed count has to be REAL, not a field someone can forget to populate.
// motifActive is where the decision is made and where the pool is in hand, so
// that is where the count must come from — a Seeds field left at zero would
// make the line say "among 0 seed facts" on a pool that had plenty.
func TestMotifActive_RecordsThePoolItDecidedOver(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/a/1.md", Motifs: []string{"lone-shape"}},
		{File: "kb/b/2.md", Motifs: []string{"lone-shape"}},
		{File: "kb/c/3.md", Motifs: []string{"other-shape"}},
	}
	act := motifActive(seeds, identityResolver)

	require.True(t, act.Evaluated)
	require.Equal(t, len(seeds), act.Seeds,
		"the activation decision must carry the size of the pool it was made "+
			"over, or the health line cannot say what it measured")
}

// End-to-end through buildMotifBridges, so the field is populated on the path
// that actually produces the health block — not only when motifActive is
// called directly. This is the wiring half: a Seeds field that only the unit
// test ever sets would leave the shipped line saying "among 0 seed facts".
func TestBuildMotifBridges_BelowFloorLineCarriesTheSeedCount(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/a/1.md", Motifs: []string{"lone-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/b/2.md", Motifs: []string{"lone-shape"}, Entities: []string{"Beta"}},
	}
	clusters, labels := oneCommunityEach(seeds)

	_, _, health, err := buildMotifBridges(context.Background(), &pairwiseIndex{}, "main",
		seeds, clusters, EffortHigh, motifQualityConfig(), identityResolver, labels, constPairCos(0.1))
	require.NoError(t, err)
	require.False(t, health.Activation.Active, "precondition: below the floor")

	require.Equal(t, len(seeds), health.Activation.Seeds,
		"the seed count must survive the real enumeration path, not just a "+
			"direct motifActive call")
	require.Contains(t, strings.Join(motifBridgeHealthLines(health, 0, 0), "\n"), "2",
		"and reach the rendered line")
}
