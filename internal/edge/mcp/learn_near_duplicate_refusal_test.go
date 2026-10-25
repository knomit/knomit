package mcp

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// The 2026-10-23 curator incident: knomit_learn was asked to write a NEW
// decision about step W0g2. Its dedup merge matched the sibling decision about
// step W0g1 (same category, cosine above the Dedup floor), the incoming fact
// won the identity tiebreak on confidence, and its title and body were written
// over the W0g1 decision at the W0g1 path. The response listed one fact
// written and, because the merge corroborated (a new ref), carried no note.
//
// The auto-merge now absorbs only an exact restatement (sameStatement). A
// near-duplicate that states something different refuses the whole call
// (nearDuplicateRefusal): nothing is written, so the existing fact keeps its
// text AND its confidence, sources and refs.

const siblingCategory = "pipeline/steps/w0"

func siblingDecision(title, body string, conf float64, refs []any, distinctFrom []any) map[string]any {
	f := map[string]any{
		"topic": "decisions", "category": siblingCategory, "title": title, "body": body,
		"type": "observation", "domain": []any{"pipeline"}, "confidence": conf, "sources": 1,
		"entities": []any{"FactReader", "readFact"}, "refs": refs,
	}
	if distinctFrom != nil {
		f["distinct_from"] = distinctFrom
	}
	return f
}

const (
	w0g1Title = "Step W0g1 deletes FactReader and routes reads through readFact"
	w0g2Title = "Step W0g2 moves the cursor read behind the same readFact seam"
)

func w0g1Body() string { return "The " + seedMarker + " decision for W0g1: FactReader goes." }
func w0g2Body() string { return "A " + probeMarker + " decision for W0g2: the cursor read moves." }

// A distinct sibling decision above the Dedup floor, MORE confident than the
// existing one and bringing a new ref: refused, nothing written, the existing
// file byte-identical (confidence, sources, refs included), and the refusal
// names the match and both escapes. Resubmitted with distinct_from, it lands
// at its own path and the existing file is still untouched.
//
// SABOTAGE A: drop the !sameStatement refusal in applyDedupMerge → the
// sibling merges, W0g1's file changes → red.
// SABOTAGE B: refusal ignores distinct_from (check moved above the
// namedIn decline) → the resubmit is refused → red.
func TestLearnHandler_DistinctSiblingIsRefusedNotMerged(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, ts.th.Dedup+(1-ts.th.Dedup)*0.5)

			seed := mergedFactPath(t, dfLearn(t, ctx, emb, siblingDecision(w0g1Title, w0g1Body(), 0.85,
				[]any{"https://github.com/knomit/knomit/pull/615"}, nil)))
			seedBefore := dfRead(t, svc, seed)
			require.Contains(t, seedBefore, "confidence: 0.85", "fixture")
			require.Contains(t, seedBefore, "sources: 1", "fixture")
			before := liveFactCount(t, svc)

			r := dfLearn(t, ctx, emb, siblingDecision(w0g2Title, w0g2Body(), 0.95,
				[]any{"https://github.com/knomit/knomit/pull/614"}, nil))
			require.True(t, r.IsError, "a different statement above the Dedup floor must be refused: %s", resultText(t, r))
			msg := resultText(t, r)
			require.Contains(t, msg, "near-duplicate")
			require.Contains(t, msg, "nothing was written")
			require.Contains(t, msg, fmt.Sprintf("fact 0: %q", w0g2Title))
			require.Contains(t, msg, "near-duplicate of "+seed)
			require.Contains(t, msg, "knomit_update")
			require.Contains(t, msg, "distinct_from")

			require.Equal(t, before, liveFactCount(t, svc), "nothing written")
			require.Equal(t, seedBefore, dfRead(t, svc, seed),
				"the existing decision is byte-identical: text, confidence, sources and refs")

			// The escape the refusal names works.
			r2 := dfLearn(t, ctx, emb, siblingDecision(w0g2Title, w0g2Body(), 0.95,
				[]any{"https://github.com/knomit/knomit/pull/614"}, []any{seed}))
			require.False(t, r2.IsError, resultText(t, r2))
			sib := mergedFactPath(t, r2)
			require.NotEqual(t, seed, sib)
			require.Equal(t, before+1, liveFactCount(t, svc))
			require.Contains(t, dfRead(t, svc, sib), "# "+w0g2Title)
			require.Equal(t, seedBefore, dfRead(t, svc, seed), "still untouched after the resubmit")
		})
	}
}

// A batch with one innocent fact and one near-duplicate is refused WHOLE, the
// same as the same-subject refusal: writing the innocent fact alone would
// report success for a batch that only half landed.
//
// Two offenders are both listed, so one round trip reports all of them.
//
// SABOTAGE C: return the refusal only for index 0 (or drop collected
// refusals after the first fact) → the batch partly writes → red.
func TestLearnHandler_NearDuplicateRefusesTheWholeBatch(t *testing.T) {
	ts := handlerThresholdSets(t)[0]
	svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, ts.th.Dedup+(1-ts.th.Dedup)*0.5)
	seed := mergedFactPath(t, dfLearn(t, ctx, emb, siblingDecision(w0g1Title, w0g1Body(), 0.85, []any{}, nil)))
	before := liveFactCount(t, svc)

	innocent := siblingDecision("An unrelated decision", "Nothing like the others.", 0.8, []any{}, nil)
	innocent["category"] = "unrelated/area"
	w0g3 := "Step W0g3 deletes the reader fallback"
	r, err := LearnHandler(emb)(ctx, sameSubjectLearnReqMulti("batch",
		innocent,
		siblingDecision(w0g2Title, w0g2Body(), 0.95, []any{}, nil),
		siblingDecision(w0g3, "A "+probeMarker+" decision for W0g3: the fallback goes.", 0.9, []any{}, nil)))
	require.NoError(t, err)
	require.True(t, r.IsError, resultText(t, r))
	msg := resultText(t, r)
	require.Contains(t, msg, "refused: 2 facts in this call are near-duplicates")
	require.Contains(t, msg, fmt.Sprintf("fact 1: %q\n  near-duplicate of %s", w0g2Title, seed))
	require.Contains(t, msg, fmt.Sprintf("fact 2: %q\n  near-duplicate of %s", w0g3, seed))
	require.NotContains(t, msg, "fact 0: ")
	require.Equal(t, before, liveFactCount(t, svc), "the innocent fact is not written either")
}

// The reviewer's batch shape (#638 round 2, LOW-1): an EXACT restatement of
// W0g1 and the DISTINCT W0g2 in one call. Fact 0 merges into W0g1 and marks it
// consumed; fact 1 used to hit that consumed decline, skip the refusal, and be
// written next to W0g1 with no refusal and no distinct_from. It is now judged
// against the in-progress merge and refused like a single learn, so the whole
// call writes nothing and W0g1 is byte-identical.
//
// SABOTAGE F: skip a merged path again (the old `consumed` decline) → the
// call succeeds and writes W0g2 → red.
func TestLearnHandler_NearDuplicateAfterAnInBatchMergeIsRefused(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, ts.th.Dedup+(1-ts.th.Dedup)*0.5)
			seed := mergedFactPath(t, dfLearn(t, ctx, emb, siblingDecision(w0g1Title, w0g1Body(), 0.85,
				[]any{"https://github.com/knomit/knomit/pull/615"}, nil)))
			seedBefore := dfRead(t, svc, seed)
			before := liveFactCount(t, svc)

			r, err := LearnHandler(emb)(ctx, sameSubjectLearnReqMulti("batch",
				siblingDecision(w0g1Title, w0g1Body(), 0.9, []any{"https://github.com/knomit/knomit/pull/616"}, nil),
				siblingDecision(w0g2Title, w0g2Body(), 0.95, []any{"https://github.com/knomit/knomit/pull/614"}, nil)))
			require.NoError(t, err)
			require.True(t, r.IsError, "the distinct sibling must be refused even after an in-batch merge: %s", resultText(t, r))
			msg := resultText(t, r)
			require.Contains(t, msg, fmt.Sprintf("fact 1: %q\n  near-duplicate of %s", w0g2Title, seed))
			require.NotContains(t, msg, "fact 0: ", "the exact restatement is not an offender")
			require.Equal(t, before, liveFactCount(t, svc))
			require.Equal(t, seedBefore, dfRead(t, svc, seed), "nothing written: W0g1 is byte-identical")
		})
	}
}

// The same TITLE with a different BODY is a different statement: refused. A
// sameStatement that compared titles alone would merge it.
//
// SABOTAGE G: sameStatement compares titles only → merged → red (and
// TestLearnHandler_AutoMergeStillWinsAboveDedup goes red with it).
func TestLearnHandler_SameTitleDifferentBodyIsRefused(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, ts.th.Dedup+(1-ts.th.Dedup)*0.5)
			seed := mergedFactPath(t, dfLearn(t, ctx, emb, siblingDecision(w0g1Title, w0g1Body(), 0.85, []any{}, nil)))
			seedBefore := dfRead(t, svc, seed)
			before := liveFactCount(t, svc)

			r := dfLearn(t, ctx, emb, siblingDecision(w0g1Title,
				"A "+probeMarker+" decision for W0g1, now saying the reader stays.", 0.95, []any{}, nil))
			require.True(t, r.IsError, resultText(t, r))
			require.Contains(t, resultText(t, r), "near-duplicate of "+seed)
			require.Equal(t, before, liveFactCount(t, svc))
			require.Equal(t, seedBefore, dfRead(t, svc, seed))
		})
	}
}

// dedup-not-append still holds for a TRUE duplicate: the same statement
// learned again — including with trailing whitespace on the body, which
// ParseFact trims on disk but the incoming body keeps — subsumes into the
// existing fact (no second fact), raises its confidence, pools the sources
// and takes the new ref.
//
// SABOTAGE D: refuse whenever there is a match (drop the sameStatement
// gate's TRUE branch) → red. SABOTAGE E: sameStatement without TrimSpace →
// the trailing-newline row is refused → red.
func TestLearnHandler_TrueDuplicateStillSubsumes(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		for _, suffix := range []string{"", "\n", "  \n\n"} {
			t.Run(fmt.Sprintf("%s/suffix=%q", ts.name, suffix), func(t *testing.T) {
				svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, ts.th.Dedup+(1-ts.th.Dedup)*0.5)

				seed := mergedFactPath(t, dfLearn(t, ctx, emb, siblingDecision(w0g1Title, w0g1Body(), 0.6,
					[]any{"https://github.com/knomit/knomit/pull/615"}, nil)))
				before := liveFactCount(t, svc)

				again := siblingDecision(w0g1Title, w0g1Body()+suffix, 0.9,
					[]any{"https://github.com/knomit/knomit/pull/616"}, nil)
				// A visible identity field, so the winner can be read off the file.
				again["type"] = "pattern"
				r := dfLearn(t, ctx, emb, again)
				require.False(t, r.IsError, resultText(t, r))

				require.Equal(t, seed, mergedFactPath(t, r), "a true duplicate merges into the existing fact")
				require.Equal(t, before, liveFactCount(t, svc), "and adds no fact")

				got := dfRead(t, svc, seed)
				require.Contains(t, got, "# "+w0g1Title)
				require.Contains(t, got, "type: pattern", "the same statement, more confident: the incoming identity wins")
				require.Contains(t, got, "confidence: 0.9")
				require.Contains(t, got, "sources: 2", "a new ref corroborates: sources pool")
				require.Contains(t, got, "pull/615")
				require.Contains(t, got, "pull/616")
				var res struct {
					Notes []string `json:"notes"`
				}
				require.NoError(t, json.Unmarshal([]byte(resultText(t, r)), &res))
				require.Empty(t, res.Notes, "a corroborating exact restatement needs no note")
			})
		}
	}
}
