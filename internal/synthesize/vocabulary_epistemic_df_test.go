package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// knomit#116, the store half. Reporting an epistemic recurrence figure is only
// worth doing if it is THE NUMBER THE ACTIVATION FLOOR READS — a similar-looking
// count computed over a slightly different population would be the original
// defect with an extra column.
//
// This reproduces the field's sharpest exhibit in miniature: a motif whose
// carriers are mostly PRAGMATIC. Raw df counts them all; the epistemic df sees
// only the epistemic ones; and the gap between the two numbers is exactly what
// an operator was reading as activation progress.
//
// `wrong-deciding-variable` was the worst measured case — raw df 5, epistemic
// df 1, all five carriers in one folder, four pragmatic heuristics and one
// observation. The path told the topic; it never told the kind.
func TestVocabularyHealth_EpistemicRecurringCountsOnlyEpistemicCarriers(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)

	// One motif on three carriers: two PRAGMATIC, one EPISTEMIC.
	// Raw df = 3 (recurring). Epistemic df = 1 (NOT recurring).
	writeKindFactWithMotifs(t, env, "kb/decisions/a.md", fact.Pragmatic, fact.Policy,
		[]string{"wrong-deciding-variable"})
	writeKindFactWithMotifs(t, env, "kb/decisions/b.md", fact.Pragmatic, fact.Heuristic,
		[]string{"wrong-deciding-variable"})
	writeKindFactWithMotifs(t, env, "kb/decisions/c.md", fact.Epistemic, fact.Observation,
		[]string{"wrong-deciding-variable"})

	// A second motif carried by two EPISTEMIC facts: recurring on BOTH counts,
	// so the test distinguishes "epistemic count works" from "epistemic count
	// is always lower".
	writeKindFactWithMotifs(t, env, "kb/gotchas/d.md", fact.Epistemic, fact.Observation,
		[]string{"genuinely-recurring"})
	writeKindFactWithMotifs(t, env, "kb/gotchas/e.md", fact.Epistemic, fact.Observation,
		[]string{"genuinely-recurring"})

	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	h, err := env.svc.Motifs().VocabularyHealth(ctx, env.branch)
	require.NoError(t, err)

	require.Equal(t, 2, h.Clusters, "precondition: two distinct motifs")
	require.Equal(t, 2, h.Recurring,
		"RAW: both motifs have >= 2 authored carriers, so both count as recurring")
	require.Equal(t, 1, h.EpistemicRecurring,
		"EPISTEMIC: only the second motif has >= 2 epistemic carriers — the "+
			"pragmatic-carried one contributes nothing to activation, and that "+
			"gap is precisely what a reader was mistaking for progress")
}

// writeKindFactWithMotifs writes a fact of a chosen KIND carrying motifs.
//
// The kind is the point: the existing motif helper writes observations only,
// and this test is entirely about the epistemic/pragmatic split that the
// vocabulary line was blind to.
func writeKindFactWithMotifs(t *testing.T, env *restatementEnv, path string, kind fact.Kind, typ fact.Type, motifs []string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = "T " + path
	f.Body = "body for " + path
	f.Kind = kind
	f.Type = typ
	f.Domain = []string{"alpha"}
	f.Entities = []string{"Widget"}
	f.Refs = []string{}
	f.Confidence = 0.7
	f.Sources = 1
	f.Motifs = motifs
	rendered, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = env.svc.Facts().WriteFact(context.Background(), env.branch, f.Path(),
		rendered, "write "+path, "test")
	require.NoError(t, err)
}
