package synthesize

import (
	"strings"
	"testing"

	"knomit/internal/store"

	"github.com/stretchr/testify/require"
)

// knomit#116. The vocabulary health line counts RAW df, and its "authored facts
// only" qualifier filters ORIGIN, not KIND — which is the whole misdirection: a
// reader takes the qualifier as naming the population, and it names a different
// axis.
//
// The activation floor counts df >= 2 in the post-AcceptSeed EPISTEMIC pool.
// So the number in the health block and the number the activation decision
// reads are computed over different populations, and nothing labelled either.
//
// Measured divergence, four corpora: up to 5.0x. The sharpest single exhibit is
// `wrong-deciding-variable` — raw df 5, epistemic df 1, with all five carriers
// under kb/decisions/, four pragmatic heuristics and one observation in one
// folder. And knomit-io-kb printed both numbers in ONE block, four lines apart:
// "22 clusters, recurrence 14% (3 recur)" beside "0 recurring motif(s) … below
// the 3-motif validity floor".
//
// The fix is labelling, not a redefinition: raw df keeps its meaning, and the
// number the floor actually reads is reported beside it.
func TestMotifVocabularyHealthLines_ReportsBothPopulations(t *testing.T) {
	line := strings.Join(motifVocabularyHealthLines(store.MotifVocabularyHealth{
		Clusters:           22,
		Recurring:          3,
		EpistemicRecurring: 0,
		Mints:              22,
		Links:              4,
	}), "\n")

	require.Contains(t, line, "3 recur",
		"raw recurrence keeps its meaning — this fix adds a number, it does not "+
			"redefine one")
	require.Contains(t, line, "0",
		"and the epistemic count, which is what the activation floor reads")

	// BOTH must be LABELLED. An unlabelled second number is the original defect
	// with more digits: the reader still cannot tell which population either
	// figure describes.
	lower := strings.ToLower(line)
	require.Contains(t, lower, "epistemic",
		"the activation-relevant population must be named; 'authored facts only' "+
			"filters ORIGIN, not KIND, and reading it as the population is the bug")
	require.Contains(t, lower, "authored",
		"and the raw population stays named too")
}

// The df CAVEAT (#127 cross-link). df counts CARRIERS, and duplicate records of
// one event inflate it — measured live: `test-becomes-the-incident` read df 4
// resting on ONE event, its three genuinely independent carriers still
// unassigned in the pool.
//
// So labelling the population correctly does not make the count correct. A
// reader who acts on the corrected line is still acting on an upper bound, and
// this fix must say so rather than shipping a more precise wrong number.
//
// THE CAVEAT IS ONE SENTENCE, ON ITS OWN LINE. #127's PR removes it when the
// merge + df recount lands, and that removal should be a deletion rather than a
// rewrite — so nothing else in the block may depend on its presence or its
// position.
func TestMotifVocabularyHealthLines_CarriesTheDfCaveatAsOneRemovableLine(t *testing.T) {
	lines := motifVocabularyHealthLines(store.MotifVocabularyHealth{
		Clusters: 22, Recurring: 3, EpistemicRecurring: 1, Mints: 22, Links: 4,
	})
	require.Len(t, lines, 2,
		"the caveat is its OWN line, so #127 deletes a line rather than editing one")

	caveat := lines[1]
	require.Contains(t, strings.ToLower(caveat), "carrier",
		"the caveat names what df actually counts")
	require.Contains(t, caveat, "#127",
		"and cites the issue whose fix removes it, so a reader can check whether "+
			"it is still true")

	// The metrics line must stand alone without it: removing the caveat leaves
	// a complete, correct line rather than a dangling clause.
	require.NotContains(t, lines[0], "#127",
		"the metrics line carries no reference to the caveat — deleting line 2 "+
			"must leave line 1 untouched and self-contained")
}

// An empty vocabulary says nothing at all, caveat included. A corpus with no
// motifs has no df to be inflated, and a lone caveat with no metrics beside it
// would send a reader looking for a number that was never printed.
func TestMotifVocabularyHealthLines_SilentOnAnEmptyVocabulary(t *testing.T) {
	require.Empty(t, motifVocabularyHealthLines(store.MotifVocabularyHealth{}))
}
