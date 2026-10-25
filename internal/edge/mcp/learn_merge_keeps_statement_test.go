package mcp

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/modelparams"
)

// The 2026-10-23 curator incident: knomit_learn was asked to write a NEW
// decision about step W0g2. Its dedup merge matched the sibling decision about
// step W0g1 (same category, cosine above the Dedup floor), the incoming fact
// won the identity tiebreak on confidence, and its title and body were written
// over the W0g1 decision at the W0g1 path. The response listed one fact
// written and, because the merge corroborated (a new ref), carried no note.
//
// A merge now never replaces an existing statement with a different one
// (incomingIdentityWins): the existing fact keeps its title and body, and the
// response says the incoming statement was not written and how to keep it.

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

type learnNotes struct {
	Notes   []string `json:"notes"`
	Summary string   `json:"summary"`
}

func readNotes(t *testing.T, text string) learnNotes {
	t.Helper()
	var n learnNotes
	require.NoError(t, json.Unmarshal([]byte(text), &n))
	return n
}

// A distinct sibling decision above the Dedup floor, MORE confident than the
// existing one and bringing a new ref (so the merge corroborates and, before
// the fix, said nothing): the existing decision's title and body survive at
// its path, and the response names the statement that was not written.
//
// SABOTAGE: incomingIdentityWins without the sameStatement conjunct (the old
// newFactWins tiebreak) → the W0g1 file holds the W0g2 title → red.
func TestLearnHandler_MergeNeverOverwritesSiblingStatement(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, ts.th.Dedup+(1-ts.th.Dedup)*0.5)

			seed := mergedFactPath(t, dfLearn(t, ctx, emb, siblingDecision(w0g1Title, w0g1Body(), 0.85,
				[]any{"https://github.com/knomit/knomit/pull/615"}, nil)))
			before := liveFactCount(t, svc)

			r := dfLearn(t, ctx, emb, siblingDecision(w0g2Title, w0g2Body(), 0.95,
				[]any{"https://github.com/knomit/knomit/pull/614"}, nil))
			require.False(t, r.IsError, resultText(t, r))

			// Precondition: it really was the merge path, not a fresh write.
			require.Equal(t, seed, mergedFactPath(t, r), "fixture: the sibling must dedup-match the seed")
			require.Equal(t, before, liveFactCount(t, svc))

			got := dfRead(t, svc, seed)
			require.Contains(t, got, "# "+w0g1Title, "the existing decision's title survives")
			require.Contains(t, got, w0g1Body(), "the existing decision's body survives")
			require.NotContains(t, got, w0g2Title, "the sibling's statement is not written over it")

			n := readNotes(t, resultText(t, r))
			require.Len(t, n.Notes, 1, "exactly the statement note: %v", n.Notes)
			note := n.Notes[0]
			require.Contains(t, note, "fact 0: merged into existing fact "+seed+"; existing fact kept")
			require.Contains(t, note, fmt.Sprintf("%q was not written", w0g2Title))
			require.Contains(t, note, fmt.Sprintf("distinct_from: [%q]", seed))
			require.Contains(t, note, "knomit_update on "+seed)
			require.Contains(t, n.Summary, note, "the summary carries the note too")

			// The escape the note names works: resubmitted with distinct_from,
			// the sibling lands at its own path and both statements are live.
			r2 := dfLearn(t, ctx, emb, siblingDecision(w0g2Title, w0g2Body(), 0.95,
				[]any{"https://github.com/knomit/knomit/pull/614"}, []any{seed}))
			require.False(t, r2.IsError, resultText(t, r2))
			sib := mergedFactPath(t, r2)
			require.NotEqual(t, seed, sib)
			require.Equal(t, before+1, liveFactCount(t, svc))
			require.Contains(t, dfRead(t, svc, sib), "# "+w0g2Title)
			require.Contains(t, dfRead(t, svc, seed), "# "+w0g1Title)
		})
	}
}

// dedup-not-append still holds for a TRUE duplicate: the same statement
// learned again, more confident and with a new ref, subsumes into the existing
// fact (no second fact), raises its confidence, pools the sources, takes the
// new ref, and is not reported as an unwritten statement. Rows with trailing
// whitespace on the incoming body pin sameStatement's trim: the incoming body
// is stored as sent while ParseFact trims the existing one.
//
// SABOTAGE: incomingIdentityWins returning false always (existing always
// wins) → the merged type stays the existing one's → red on the type check.
// SABOTAGE: sameStatement without TrimSpace → the suffix rows keep the
// existing type and gain a statement note → red.
func TestLearnHandler_TrueDuplicateStillSubsumes(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		for _, suffix := range []string{"", "\n", "  \n\n"} {
			t.Run(fmt.Sprintf("%s/suffix=%q", ts.name, suffix), func(t *testing.T) {
				trueDuplicateCase(t, ts.model, ts.th, suffix)
			})
		}
	}
}

func trueDuplicateCase(t *testing.T, modelID string, th modelparams.Thresholds, suffix string) {
	t.Helper()
	svc, ctx, emb := newSameSubjectRepo(t, modelID, th, th.Dedup+(1-th.Dedup)*0.5)

	seedFact := siblingDecision(w0g1Title, w0g1Body(), 0.6,
		[]any{"https://github.com/knomit/knomit/pull/615"}, nil)
	seed := mergedFactPath(t, dfLearn(t, ctx, emb, seedFact))
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
	require.Empty(t, readNotes(t, resultText(t, r)).Notes, "nothing was dropped, so nothing to report")
}

// incidentCosine is the W0g1/W0g2 pair's cosine, measured for #638 on the
// knomit corpus with each model the way the index embeds a fact (title plus
// body): the pair that merged on 2026-10-23 under the old 0.82 Dedup floor.
var incidentCosine = map[string]float64{
	modelparams.DefaultModelID: 0.8616,
	modelparams.NomicModelID:   0.8389,
}

// The incident pair's measured score, with the SHIPPED floors, no longer
// reaches the merge: with no shared entity the sibling is written at its own
// path; with a shared entity (W0g1 and W0g2 share webInterfaceAllowlist) the
// same-category band of the same-subject gate refuses it instead, naming
// W0g1. In both, W0g1's file is byte-identical.
//
// The shipped floor must sit above the score (asserted, so a later re-sweep
// that drops it under the incident pair turns this red, not green-by-accident).
//
// SABOTAGE: DefaultModelID Dedup back to 0.82 → the gemma rows merge → red.
func TestLearnHandler_IncidentPairScoreNoLongerMerges(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		for _, shared := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/sharedEntity=%v", ts.name, shared), func(t *testing.T) {
				cos := incidentCosine[ts.model]
				require.NotZero(t, cos, "fixture: no measured incident score for %s", ts.model)
				require.Greater(t, ts.th.Dedup, cos, "the shipped Dedup floor must sit above the incident pair")

				svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, cos)
				g1 := siblingDecision(w0g1Title, w0g1Body(), 0.85, []any{}, nil)
				g1["entities"] = []any{"commitsProvider", "webInterfaceAllowlist"}
				seed := mergedFactPath(t, dfLearn(t, ctx, emb, g1))
				seedBefore := dfRead(t, svc, seed)
				before := liveFactCount(t, svc)

				g2 := siblingDecision(w0g2Title, w0g2Body(), 0.95, []any{}, nil)
				g2["entities"] = []any{"factSubProvider"}
				if shared {
					g2["entities"] = []any{"factSubProvider", "webInterfaceAllowlist"}
				}
				r := dfLearn(t, ctx, emb, g2)
				inSubjectBand := cos > ts.th.ReflectNovelty
				switch {
				case shared && inSubjectBand:
					require.True(t, r.IsError, "a same-category sibling sharing an entity is refused: %s", resultText(t, r))
					require.Contains(t, resultText(t, r), seed)
					require.Equal(t, before, liveFactCount(t, svc))
				default:
					require.False(t, r.IsError, resultText(t, r))
					require.NotEqual(t, seed, mergedFactPath(t, r), "the sibling is written at its own path, not merged")
					require.Equal(t, before+1, liveFactCount(t, svc))
				}
				require.Equal(t, seedBefore, dfRead(t, svc, seed), "W0g1 is byte-identical")
			})
		}
	}
}
