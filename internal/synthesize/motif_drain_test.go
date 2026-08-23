package synthesize

import (
	"context"
	"fmt"
	"testing"

	"knomit/internal/store"

	"github.com/stretchr/testify/require"
)

// Backfill is a ONE-TIME DRAIN, not a recurring job.
//
// The backlog it works through is facts that predate the motif field — the
// corpus did not change, the SYSTEM did. Once every fact has been judged once,
// there is nothing left to do and a quiet corpus must fall silent.
//
// Making that true needs a record of the NEGATIVE answer. Before it existed,
// only a positive assignment left a trace, so a fact an agent correctly judged
// to carry no regularity was re-offered every session forever — and on a corpus
// with enough such facts, they fill every slot and the sweep dies without ever
// reaching the tail. That is not hypothetical: it is what stalled the gate
// annex's merged run against a corpus of market-news facts.
//
// The record is content-addressed for free. `facts` rows are immutable and
// unique on (path, blob_hash), so an EDITED fact is a different row with a
// different id, has never been judged, and is back in the backlog — which is
// correct, because its content is what the judgement was about.

// The core claim: "none apply" is an ANSWER, and an answered question is not
// asked again.
func TestMotifDrain_EmptyJudgmentIsNotReOffered(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/a.md", "Alpha", "body alpha")
	env.writeFact("kb/b.md", "Bravo", "body bravo")
	d := env.deps()

	before, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, maxBackfillFacts)
	require.NoError(t, err)
	require.Len(t, before, 2, "precondition: both facts are in the backlog")

	// The agent judges kb/a.md and finds no regularity. kb/b.md it does not
	// reach — a distinction the next test pins down.
	require.NoError(t, applyMotifBackfill(ctx, d, env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/a.md", Motifs: nil}},
	}))

	after, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, maxBackfillFacts)
	require.NoError(t, err)
	paths := backfillPaths(after)
	require.NotContains(t, paths, "kb/a.md",
		"a fact judged to carry no motif must not be offered again — without this "+
			"record the pass has no way to distinguish 'not yet asked' from 'asked and answered'")
	require.Contains(t, paths, "kb/b.md", "an unjudged fact stays in the backlog")
}

// THE RULING'S TEST, both halves. A quiet corpus with a backlog must PROGRESS
// to drained, and then go SILENT — and neither half alone is the property.
//
// Asserting only that it drains would pass for a pass that keeps planning empty
// items forever. Asserting only that it eventually goes silent would pass for a
// pass that never ran at all.
func TestMotifDrain_QuietCorpusDrainsThenGoesSilent(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	const total = maxBackfillFacts*2 + 3 // three sessions' worth, unevenly
	for i := range total {
		env.writeFact(fmt.Sprintf("kb/f%02d.md", i), fmt.Sprintf("Fact %d", i),
			fmt.Sprintf("body %d", i))
	}
	d := env.deps()

	// HALF ONE: it progresses. Every session takes a real bite, no session
	// re-offers what an earlier one answered, and the backlog reaches zero.
	judged := map[string]bool{}
	sessions := 0
	for range 10 {
		targets, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, maxBackfillFacts)
		require.NoError(t, err)
		if len(targets) == 0 {
			break
		}
		sessions++
		res := motifBackfillResult{}
		for _, tgt := range targets {
			require.Falsef(t, judged[tgt.Path],
				"session %d re-offered %s, which an earlier session already answered",
				sessions, tgt.Path)
			judged[tgt.Path] = true
			// Every one answered "no regularity here" — the case that used to
			// leave no trace at all.
			res.Assignments = append(res.Assignments, motifAssignment{Path: tgt.Path})
		}
		require.NoError(t, applyMotifBackfill(ctx, d, env.branch, res))
	}
	require.Len(t, judged, total, "every fact must have been reached")
	require.Equal(t, 3, sessions, "a %d-fact backlog at %d per session is three sessions",
		total, maxBackfillFacts)

	// HALF TWO: it goes silent. Not "plans an empty item" — plans NO item.
	remaining, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, maxBackfillFacts)
	require.NoError(t, err)
	require.Empty(t, remaining, "the backlog is drained")

	out := env.vocabSession()
	require.Empty(t, out.itemsOfType(motifBackfillStepType),
		"a DRAINED corpus must plan no backfill item at all: the backlog is "+
			"unfinished one-time work, and when there is none the session is genuinely empty")

	// And silence is stable — a second session does not resurrect it.
	out = env.vocabSession()
	require.Empty(t, out.itemsOfType(motifBackfillStepType),
		"nothing re-runs on true no-change, ever")
}

// Content-addressed re-eligibility. Editing a fact makes it a new row that has
// never been judged, so it returns to the backlog — which is correct, because
// the judgement was about content that no longer exists.
func TestMotifDrain_EditedFactReturnsToBacklog(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/a.md", "Alpha", "body alpha")
	d := env.deps()

	require.NoError(t, applyMotifBackfill(ctx, d, env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/a.md"}},
	}))
	after, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, maxBackfillFacts)
	require.NoError(t, err)
	require.NotContains(t, backfillPaths(after), "kb/a.md", "precondition: it is judged")

	// The fact is rewritten with different content.
	env.writeFact("kb/a.md", "Alpha", "body alpha, materially revised")

	after, err = env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, maxBackfillFacts)
	require.NoError(t, err)
	require.Contains(t, backfillPaths(after), "kb/a.md",
		"an EDITED fact is a new version that has never been judged, and must return "+
			"to the backlog — the judgement was about the content, not about the path")
}

// The distinction that keeps the record honest: a judgement is the AGENT
// saying "none apply", never the SYSTEM refusing what the agent said.
//
// A five-word motif is refused by the write gate. The agent judged the fact and
// found a regularity; the shape rule rejected the name. Marking that judged
// would bury the fact with no motif and no record of why — the opposite of what
// the annex measured, where refused facts correctly came back next session.
func TestMotifDrain_RefusedAssignmentIsReOffered(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/a.md", "Alpha", "body alpha")
	d := env.deps()

	// Five words: over the 2-4 word ceiling, refused by SerializeFact.
	const tooLong = "instrument-fault-reads-as-signal"
	require.NoError(t, applyMotifBackfill(ctx, d, env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/a.md", Motifs: []string{tooLong}}},
	}))

	rec, err := env.svc.Search().GetByPath(ctx, env.branch, "kb/a.md")
	require.NoError(t, err)
	require.Empty(t, rec.Motifs, "precondition: the write gate refused the name")

	after, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, maxBackfillFacts)
	require.NoError(t, err)
	require.Contains(t, backfillPaths(after), "kb/a.md",
		"a motif REFUSED by the write gate is not a judgement of 'none apply' — the "+
			"fact must come back, or a naming error silently costs it its motif forever")
}

// A fact the agent did not mention was not judged. Offering eight and answering
// six leaves two unanswered, and they must return.
func TestMotifDrain_UnansweredFactIsReOffered(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/a.md", "Alpha", "body alpha")
	env.writeFact("kb/b.md", "Bravo", "body bravo")
	d := env.deps()

	require.NoError(t, applyMotifBackfill(ctx, d, env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/a.md"}},
	}))

	after, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, maxBackfillFacts)
	require.NoError(t, err)
	require.Contains(t, backfillPaths(after), "kb/b.md",
		"silence about a fact is not an answer about it")
}

// A fact that took a motif needs no judged record — it self-excludes by
// carrying one. Asserted so the drain record cannot quietly become the only
// thing keeping answered facts out of the backlog.
func TestMotifDrain_AssignedFactLeavesTheBacklogByItsMotif(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/a.md", "Alpha", "body alpha")
	d := env.deps()

	require.NoError(t, applyMotifBackfill(ctx, d, env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/a.md", Motifs: []string{"silent-fallback"}}},
	}))
	rec, err := env.svc.Search().GetByPath(ctx, env.branch, "kb/a.md")
	require.NoError(t, err)
	require.NotEmpty(t, rec.Motifs, "precondition: the motif landed")

	after, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, maxBackfillFacts)
	require.NoError(t, err)
	require.NotContains(t, backfillPaths(after), "kb/a.md")
}

func backfillPaths(targets []store.BackfillTarget) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Path)
	}
	return out
}
