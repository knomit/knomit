package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
// reaches the merge: the sibling is written at its own path in both rows, with
// or without a shared entity (W0g1 and W0g2 share webInterfaceAllowlist), and
// W0g1's file is byte-identical. With a shared entity the pair sits in the
// same-subject band inside the merge's scope, so the response names W0g1 (see
// TestLearnHandler_InScopeBandNeighbourIsWrittenWithANote).
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
				require.False(t, r.IsError, resultText(t, r))
				require.NotEqual(t, seed, mergedFactPath(t, r), "the sibling is written at its own path, not merged")
				require.Equal(t, before+1, liveFactCount(t, svc))
				require.Equal(t, seedBefore, dfRead(t, svc, seed), "W0g1 is byte-identical")
			})
		}
	}
}

// The user's ruling on #638: inside learn's merge scope, a candidate in the
// same-subject band below Dedup is written at its own path, never refused. The
// response notes the nearest neighbour (path, cosine, knomit_update) and the
// commit message carries ONE short "near:" line. A cross-category pair at the
// same cosine is still refused.
//
// SABOTAGE: route the in-scope band candidate to `kept` (the pre-ruling
// refusal) → the write is refused → red.
func TestLearnHandler_InScopeBandNeighbourIsWrittenWithANote(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name+"/in-scope", func(t *testing.T) {
			cos := inBand(ts.th)
			require.Less(t, cos, ts.th.Dedup)
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, cos)
			seed := mergedFactPath(t, dfLearn(t, ctx, emb, siblingDecision(w0g1Title, w0g1Body(), 0.85, []any{}, nil)))
			before := liveFactCount(t, svc)

			r := dfLearn(t, ctx, emb, siblingDecision(w0g2Title, w0g2Body(), 0.85, []any{}, nil))
			require.False(t, r.IsError, "an in-scope band sibling is written, not refused: %s", resultText(t, r))
			written := mergedFactPath(t, r)
			require.NotEqual(t, seed, written, "written at its own new path")
			require.Equal(t, before+1, liveFactCount(t, svc))

			notes := readNotes(t, resultText(t, r)).Notes
			require.Len(t, notes, 1)
			require.Contains(t, notes[0], seed, "the note names the neighbour's path")
			require.Contains(t, notes[0], fmt.Sprintf("(%.2f)", cos), "and the cosine")
			require.Contains(t, notes[0], "fold it into that fact with knomit_update and retract this one")

			head, err := svc.HeadCommit(context.Background(), "agent/test")
			require.NoError(t, err)
			info, _ := commitDiff(t, svc, head)
			require.Contains(t, info.Message, fmt.Sprintf("\nnear: %s (%.2f)", seed, cos),
				"the commit message carries the one-line near: marker")
			require.Equal(t, 1, strings.Count(info.Message, "near:"))
			require.Len(t, strings.Split(strings.TrimSpace(info.Message), "\n"), 3, "subject, blank line, one near: line")
		})

		t.Run(ts.name+"/cross-category-still-refused", func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, inBand(ts.th))
			seeded := seedRampFact(t, ctx, emb)
			before := liveFactCount(t, svc)
			r, err := LearnHandler(emb)(ctx, sameSubjectLearnReq(
				"collide", "gotchas", "tools/ai/spend",
				"Enterprise AI spend is plateauing",
				"A "+probeMarker+" note on Ramp's numbers for paid AI seats.",
				[]any{"Ramp"}, nil))
			require.NoError(t, err)
			require.True(t, r.IsError, resultText(t, r))
			require.Contains(t, resultText(t, r), seeded)
			require.Equal(t, before, liveFactCount(t, svc))
		})
	}
}

func siblingIn(category, title, body string) map[string]any {
	f := siblingDecision(title, body, 0.85, []any{}, nil)
	f["category"] = category
	return f
}

// The merge's reach is a PREFIX of the incoming category directory, so a band
// neighbour in a sub-category or a prefix-sibling category is in scope (written,
// with a note), while one in the PARENT directory is not (refused). Pins the
// scope from both sides.
//
// SABOTAGE: inMergeScope narrowed to the exact directory → the first two rows
// are refused → red. inMergeScope returning true → the parent row is written → red.
func TestLearnHandler_InMergeScopePrefixReach(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		for _, tc := range []struct {
			name, seedCat, probeCat string
			written                 bool
		}{
			{"descendant", siblingCategory + "/child", siblingCategory, true},
			{"prefix-sibling", siblingCategory + "x", siblingCategory, true},
			{"parent", siblingCategory, siblingCategory + "/child", false},
		} {
			t.Run(ts.name+"/"+tc.name, func(t *testing.T) {
				svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, inBand(ts.th))
				seed := mergedFactPath(t, dfLearn(t, ctx, emb, siblingIn(tc.seedCat, w0g1Title, w0g1Body())))
				before := liveFactCount(t, svc)

				r := dfLearn(t, ctx, emb, siblingIn(tc.probeCat, w0g2Title, w0g2Body()))
				if !tc.written {
					require.True(t, r.IsError, "outside the merge scope: refused: %s", resultText(t, r))
					require.Contains(t, resultText(t, r), seed)
					require.Equal(t, before, liveFactCount(t, svc))
					return
				}
				require.False(t, r.IsError, resultText(t, r))
				require.Equal(t, before+1, liveFactCount(t, svc))
				notes := readNotes(t, resultText(t, r)).Notes
				require.Len(t, notes, 1)
				require.Contains(t, notes[0], seed)
			})
		}
	}
}

func TestInMergeScope_IsCaseInsensitivePrefix(t *testing.T) {
	require.True(t, inMergeScope("kb/Decisions/Pipeline/a.md", "kb/decisions/pipeline"))
	require.True(t, inMergeScope("kb/decisions/pipeline/child/a.md", "kb/decisions/pipeline"))
	require.True(t, inMergeScope("kb/decisions/pipelinex/a.md", "kb/decisions/pipeline"))
	require.False(t, inMergeScope("kb/decisions/a.md", "kb/decisions/pipeline"))
	require.False(t, inMergeScope("kb/decisions/pipeline/a.md", ""))
}

// An in-scope candidate AT or above Dedup that the merge did NOT take is still
// refused: the ruling covers only "below Dedup". Here two incoming facts match
// one seed in a sub-category; the merge absorbs one (an existing fact absorbs at
// most one per call) and the other reaches the gate untouched, in scope but not
// the exact directory, so the Dedup cap does not exempt it.
//
// SABOTAGE: drop `&& c.Similarity < th.Dedup` from the near case → written → red.
func TestLearnHandler_InScopeAtOrAboveDedupDeclinedByMergeIsRefused(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			above := ts.th.Dedup + (1-ts.th.Dedup)*0.5
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, above)
			seed := mergedFactPath(t, dfLearn(t, ctx, emb, siblingIn(siblingCategory+"/child", w0g1Title, w0g1Body())))
			before := liveFactCount(t, svc)

			r, err := LearnHandler(emb)(ctx, sameSubjectLearnReqMulti("two",
				siblingIn(siblingCategory, w0g2Title, w0g2Body()),
				siblingIn(siblingCategory, w0g2Title+" again", w0g2Body()+" again")))
			require.NoError(t, err)
			require.True(t, r.IsError, resultText(t, r))
			require.Contains(t, resultText(t, r), seed)
			require.Equal(t, before, liveFactCount(t, svc), "refused whole")
		})
	}
}

// Several noted facts in one call share ONE short near: line, and a neighbour
// shared by several facts is listed once.
//
// SABOTAGE: join with "\nnear: " (a line per neighbour) or skip the dedupe → red.
func TestLearnHandler_MultiFactCommitCarriesOneNearLine(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, inBand(ts.th))
			seed := mergedFactPath(t, dfLearn(t, ctx, emb, siblingDecision(w0g1Title, w0g1Body(), 0.85, []any{}, nil)))

			r, err := LearnHandler(emb)(ctx, sameSubjectLearnReqMulti("two",
				siblingDecision(w0g2Title, w0g2Body(), 0.85, []any{}, nil),
				siblingDecision(w0g2Title+" again", w0g2Body()+" again", 0.85, []any{}, nil)))
			require.NoError(t, err)
			require.False(t, r.IsError, resultText(t, r))
			require.Len(t, readNotes(t, resultText(t, r)).Notes, 2, "a note per fact")

			head, err := svc.HeadCommit(context.Background(), "agent/test")
			require.NoError(t, err)
			info, _ := commitDiff(t, svc, head)
			require.Equal(t, 1, strings.Count(info.Message, "near:"), info.Message)
			require.Equal(t, 1, strings.Count(info.Message, seed), "the shared neighbour is listed once")
		})
	}
}

// With two in-band neighbours, the NEAREST one is reported.
//
// SABOTAGE: keep the farthest instead (`<` for `>`) → red.
func TestLearnHandler_ReportsTheNearestNeighbour(t *testing.T) {
	const farMarker = "FARMARKER"
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			near := inBand(ts.th)
			far := (ts.th.ReflectNovelty + near) / 2
			probeAngle := angleFor(near)
			emb := newAngleEmbedder(t, ts.model, ts.th, map[string]float64{
				seedMarker:  0,
				probeMarker: probeAngle,
				farMarker:   probeAngle + angleFor(far),
			})
			svc, ctx, _ := newRepoWithEmbedder(t, emb)
			nearPath := mergedFactPath(t, dfLearn(t, ctx, emb, siblingDecision(w0g1Title, w0g1Body(), 0.85, []any{}, nil)))
			farPath := mergedFactPath(t, dfLearn(t, ctx, emb, siblingDecision(
				"Step W0g3 is a farther sibling", "A "+farMarker+" decision for W0g3.", 0.85, []any{}, nil)))
			require.NotEqual(t, nearPath, farPath)

			r := dfLearn(t, ctx, emb, siblingDecision(w0g2Title, w0g2Body(), 0.85, []any{}, nil))
			require.False(t, r.IsError, resultText(t, r))
			notes := readNotes(t, resultText(t, r)).Notes
			require.Len(t, notes, 1)
			require.Contains(t, notes[0], nearPath)
			require.NotContains(t, notes[0], farPath)
			require.Contains(t, notes[0], fmt.Sprintf("(%.2f)", near))

			head, err := svc.HeadCommit(context.Background(), "agent/test")
			require.NoError(t, err)
			info, _ := commitDiff(t, svc, head)
			require.Contains(t, info.Message, "near: "+nearPath)
			require.NotContains(t, info.Message, farPath)
		})
	}
}

// Distinct neighbours of several facts still make ONE commit line.
//
// SABOTAGE: join with "\nnear: " → red.
func TestNearCommitLine_IsOneLine(t *testing.T) {
	mk := func(i int, p string, sim float64) nearNeighbour {
		return nearNeighbour{Index: i, Candidate: sameSubjectCandidate{Path: p, Similarity: sim}}
	}
	line := nearCommitLine([]nearNeighbour{mk(0, "kb/a.md", 0.86), mk(1, "kb/b.md", 0.84), mk(2, "kb/a.md", 0.86)})
	require.Equal(t, "near: kb/a.md (0.86), kb/b.md (0.84)", line)
	require.Empty(t, nearCommitLine(nil))
}
