package synthesize

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// knomit#124. `coverage` counts facts carrying >= 1 motif, but the backlog
// leaves by TWO doors: a fact that gained a motif leaves via fact_motifs, and a
// fact an agent judged to carry none leaves via motif_backfill_judged.
//
// So an honest decline removes a fact from the backlog WITHOUT adding it to
// coverage — and coverage can never reach 100% on any corpus where a single
// fact was honestly declined. Nothing anywhere states that ceiling, and drain
// progress (the number that CAN reach 100%) is surfaced nowhere at all.
//
// The consequence is behavioural, not cosmetic, and it was measured in the
// field: a completion criterion written against coverage reads as a permanent
// stall, and the way to keep the number moving is to STOP DECLINING. The metric
// pays for dishonest judgments. That is what separates this from the rest of
// the truthful-instrumentation family — the others cost a reader a misreading.
//
// The three populations partition the corpus exactly:
//
//	total = covered + declined + backlog
//
// so every number below is derived from that identity rather than from a
// second count that could disagree with it.
func TestRecordBackfillHealth_SurfacesDrainProgressAndTheCeiling(t *testing.T) {
	// 100 authored facts: 40 carry motifs, 10 were honestly declined, 50 are
	// still in the backlog.
	sess := &store.PipelineSession{}
	recordBackfillHealth(sess, backfillHealth{
		WithMotifs: 40,
		TotalFacts: 100,
		Backlog:    50,
		Offered:    8,
		Vocabulary: 5,
	})
	require.Len(t, sess.Health, 1)
	line := sess.Health[0]

	require.Contains(t, line, "coverage 40%",
		"the existing number keeps its meaning — this fix adds, it does not redefine")

	// DRAIN PROGRESS: the number a completion criterion should read. 50 of 100
	// facts have been resolved (40 covered + 10 declined), and unlike coverage
	// it can reach 100%.
	require.Contains(t, line, "50/100",
		"judged/total must be surfaced: it is the number that CAN complete, and "+
			"it was available nowhere")

	// THE CEILING, stated rather than left for a reader to derive. With 10
	// honest declines, coverage can never exceed 90%.
	require.Contains(t, line, "90%",
		"the ceiling must be stated: a completion criterion written against "+
			"coverage otherwise reads as a permanent stall")
	require.Contains(t, strings.ToLower(line), "declin",
		"and it must name WHY the ceiling is where it is — an unexplained "+
			"ceiling invites someone to 'fix' it by declining less")
}

// A corpus with no declines has no ceiling below 100%, and must not carry a
// clause saying so. A line that always mentions a ceiling trains a reader to
// skip it — the same reasoning as #130's drop clause.
func TestRecordBackfillHealth_NoCeilingClauseWhenNothingDeclined(t *testing.T) {
	sess := &store.PipelineSession{}
	recordBackfillHealth(sess, backfillHealth{
		WithMotifs: 30,
		TotalFacts: 100,
		Backlog:    70, // 30 covered + 0 declined + 70 backlog
	})
	require.Len(t, sess.Health, 1)
	line := sess.Health[0]

	require.Contains(t, line, "30/100", "drain progress is always reported")
	require.NotContains(t, strings.ToLower(line), "declin",
		"no declines, no ceiling clause")
}

// The identity total = covered + declined + backlog is what every derived
// number rests on. If a caller ever supplies figures that violate it — a
// backlog larger than the uncovered population, say — the line must not print
// a negative decline count or a ceiling above 100%. It degrades to the plain
// form rather than rendering arithmetic nobody can act on.
func TestRecordBackfillHealth_DegradesRatherThanPrintNonsense(t *testing.T) {
	sess := &store.PipelineSession{}
	recordBackfillHealth(sess, backfillHealth{
		WithMotifs: 40,
		TotalFacts: 100,
		Backlog:    90, // impossible: 40 + 90 > 100
	})
	require.Len(t, sess.Health, 1)
	line := sess.Health[0]

	require.NotContains(t, line, "-",
		"a negative derived count must never reach an operator")
	require.NotContains(t, strings.ToLower(line), "declin",
		"an inconsistent input yields no ceiling claim rather than a wrong one")
}

// COMPOSITION, from the drain and stated on the issue: a corpus can drain
// PERFECTLY and still show single-digit coverage forever, if most of its facts
// are honestly motif-less. Coverage measures the corpus's composition; drain
// progress measures the work. Reporting only the first makes a finished job
// look like a failed one.
func TestRecordBackfillHealth_FullyDrainedCorpusReportsCompletion(t *testing.T) {
	sess := &store.PipelineSession{}
	recordBackfillHealth(sess, backfillHealth{
		WithMotifs: 3,
		TotalFacts: 100,
		Backlog:    0, // fully drained: 3 covered, 97 declined, nothing pending
	})
	require.Len(t, sess.Health, 1)
	line := sess.Health[0]

	require.Contains(t, line, "coverage 3%",
		"composition is what it is")
	require.Contains(t, line, "100/100",
		"but the DRAIN is complete, and that must be visible — otherwise a "+
			"finished corpus reads as a 3% failure forever")
}
