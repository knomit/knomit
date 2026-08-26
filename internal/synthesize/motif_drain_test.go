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
	require.NoError(t, applyMotifBackfill(ctx, d, sessionForBackfillTest(t, ctx, env), env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/a.md", Motifs: nil}},
	}, offeredBackfillForTest(t, ctx, env)))

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
		require.NoError(t, applyMotifBackfill(ctx, d, sessionForBackfillTest(t, ctx, env), env.branch, res, offeredBackfillForTest(t, ctx, env)))
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

	require.NoError(t, applyMotifBackfill(ctx, d, sessionForBackfillTest(t, ctx, env), env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/a.md"}},
	}, offeredBackfillForTest(t, ctx, env)))
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
	require.NoError(t, applyMotifBackfill(ctx, d, sessionForBackfillTest(t, ctx, env), env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/a.md", Motifs: []string{tooLong}}},
	}, offeredBackfillForTest(t, ctx, env)))

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

	require.NoError(t, applyMotifBackfill(ctx, d, sessionForBackfillTest(t, ctx, env), env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/a.md"}},
	}, offeredBackfillForTest(t, ctx, env)))

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

	require.NoError(t, applyMotifBackfill(ctx, d, sessionForBackfillTest(t, ctx, env), env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/a.md", Motifs: []string{"silent-fallback"}}},
	}, offeredBackfillForTest(t, ctx, env)))
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

// ── The judgement binds to the version that was judged ───────────────────────

// M-A. A judgement is about CONTENT. The pass offers a fact, an agent answers
// about what it was shown, and between those two moments an ordinary
// knomit_learn/update can rewrite the fact.
//
// Resolving the path to whatever is live AT APPLY TIME stamps a verdict on
// content nobody read — and because the stamp is what removes a fact from the
// backlog, the new claim is then permanently silent. The offered fact id is
// carried through the payload for the same reason the define pass carries its
// cluster key: it is how an answer is routed back to the thing it was about.
func TestMotifDrain_EmptyJudgementBindsToTheVersionThatWasJudged(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/a.md", "Alpha", "the body the agent was shown")
	d := env.deps()

	offered := offeredBackfillForTest(t, ctx, env)
	require.Len(t, offered.Facts, 1)
	require.NotZero(t, offered.Facts[0].FactID, "the payload must carry the offered version's id")

	// Between render and apply, a learn/update rewrites the fact.
	env.writeFact("kb/a.md", "Alpha", "a materially different claim the agent never saw")

	// The agent's answer about the OLD content arrives.
	require.NoError(t, applyMotifBackfill(ctx, d, sessionForBackfillTest(t, ctx, env), env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/a.md"}},
	}, offered))

	after, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, maxBackfillFacts)
	require.NoError(t, err)
	require.Contains(t, backfillPaths(after), "kb/a.md",
		"the fact was edited after it was offered, so the agent's 'none apply' is about "+
			"content that no longer exists. Stamping the CURRENT version buries a claim "+
			"nobody judged — the exact failure the content-addressing was supposed to prevent")
}

// The same rule on the POSITIVE branch. Motifs chosen for one claim must not be
// written onto a different one; the existing guard only skips a fact that has
// GAINED motifs, which an edit need not do.
func TestMotifDrain_AssignmentBindsToTheVersionThatWasJudged(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/a.md", "Alpha", "the body the agent was shown")
	d := env.deps()

	offered := offeredBackfillForTest(t, ctx, env)
	env.writeFact("kb/a.md", "Alpha", "a materially different claim the agent never saw")

	require.NoError(t, applyMotifBackfill(ctx, d, sessionForBackfillTest(t, ctx, env), env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/a.md", Motifs: []string{"silent-fallback"}}},
	}, offered))

	rec, err := env.svc.Search().GetByPath(ctx, env.branch, "kb/a.md")
	require.NoError(t, err)
	require.Empty(t, rec.Motifs,
		"a motif named for the previous claim must not be written onto the new one")

	after, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, maxBackfillFacts)
	require.NoError(t, err)
	require.Contains(t, backfillPaths(after), "kb/a.md",
		"and the new version returns to the backlog, never having been judged")
}

// ── A fully-stripped answer is a judgement ───────────────────────────────────

// M-B. The subject strip is SILENT by contract: a motif that merely renames its
// fact's subject is dropped without telling anyone. When it absorbs the agent's
// ENTIRE answer, the fact ends the pass with no motif and — before this — no
// record, so it came back next session with identical content, identical hints,
// and every reason to draw the identical answer. A permanent slot occupant.
//
// The agent DID judge it. Only subject-restatements came back, which is "no
// regularity here" in every sense that matters to the backlog.
func TestMotifDrain_FullyStrippedAssignmentIsAJudgement(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	// The path IS the subject, so "silent-fallback" restates it and the strip
	// takes the whole answer. It passes the shape gate — this is not a refusal.
	env.writeFact("kb/silent/fallback.md", "Alpha", "body alpha")
	d := env.deps()

	offered := offeredBackfillForTest(t, ctx, env)
	require.NoError(t, applyMotifBackfill(ctx, d, sessionForBackfillTest(t, ctx, env), env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/silent/fallback.md", Motifs: []string{"silent-fallback"}}},
	}, offered))

	rec, err := env.svc.Search().GetByPath(ctx, env.branch, "kb/silent/fallback.md")
	require.NoError(t, err)
	require.Empty(t, rec.Motifs, "precondition: the subject strip absorbed the whole answer")

	after, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, maxBackfillFacts)
	require.NoError(t, err)
	require.NotContains(t, backfillPaths(after), "kb/silent/fallback.md",
		"an answer the subject strip absorbed entirely is still an ANSWER. Re-offering it "+
			"asks the same question of the same content and gets the same reply forever, "+
			"which is the standing-job pathology the drain exists to end")
}

// The distinction that keeps M-B honest: a PARTIAL strip is not a judgement of
// emptiness, because something survived and the fact leaves by carrying it.
func TestMotifDrain_PartiallyStrippedAssignmentKeepsWhatSurvived(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/silent/fallback.md", "Alpha", "body alpha")
	d := env.deps()

	offered := offeredBackfillForTest(t, ctx, env)
	require.NoError(t, applyMotifBackfill(ctx, d, sessionForBackfillTest(t, ctx, env), env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{
			Path:   "kb/silent/fallback.md",
			Motifs: []string{"silent-fallback", "unbounded-retry"},
		}},
	}, offered))

	rec, err := env.svc.Search().GetByPath(ctx, env.branch, "kb/silent/fallback.md")
	require.NoError(t, err)
	require.Equal(t, []string{"unbounded-retry"}, rec.Motifs,
		"the subject restatement is stripped and the real regularity is kept")

	after, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, maxBackfillFacts)
	require.NoError(t, err)
	require.NotContains(t, backfillPaths(after), "kb/silent/fallback.md",
		"and it leaves the backlog by CARRYING a motif, not by being judged empty")
}

// offeredBackfillForTest builds the payload the pass would have been planned
// with, through the same construction planMotifBackfillWork uses — so a test
// cannot drift from what production actually offers.
func offeredBackfillForTest(t *testing.T, ctx context.Context, env *restatementEnv) backfillPayload {
	t.Helper()
	targets, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, maxBackfillFacts)
	require.NoError(t, err)
	return backfillPayload{Facts: backfillFactsFor(targets)}
}
