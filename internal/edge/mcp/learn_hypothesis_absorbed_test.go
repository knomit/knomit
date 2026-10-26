package mcp

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// A dedup merge must never turn a stored non-hypothesis fact into a hypothesis.
// Before: an incoming hypothesis with the same text and HIGHER confidence won
// newFactWins, so mergeFacts took its Type and the stored fact became a
// prediction, with only "merged into existing fact" in the response.
//
// Now the stored fact keeps its whole identity AND its confidence (a prediction
// is not evidence: it adds no sources either), and ONE note says the
// hypothesis was absorbed, was not written, and how to keep it. The note stands
// in for statementNotAppliedNotes, which skips it, so a different prediction
// is never lost silently.
//
// SABOTAGE 1: drop the absorbedHypothesis conjunct in incomingIdentityWins →
// the type flips → red. SABOTAGE 2: drop the confidence override in mergeFacts
// → confidence becomes 0.9 → red. SABOTAGE 3: limit absorbedHypothesis to
// observations → the policy row is red. SABOTAGE 4: skip the hypMerged note →
// the differing-text row loses its note → red.
func TestLearnHandler_IncomingHypothesisIsAbsorbedByStoredFact(t *testing.T) {
	for _, tc := range []struct {
		name       string
		storedType string
		storedText string
		incoming   string
	}{
		{"observation, identical text", "observation", "Launch happens in October", "Launch happens in October"},
		{"observation, different text", "observation", "Launch happens in Octobr", "Launch happens in Novmbr"},
		{"policy, identical text", "policy", "Launch happens in October", "Launch happens in October"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx, emb := newDedupAttrRepo(t)
			seedFact := settledFact("notes", "a", tc.storedText, tc.storedType)
			seedFact["confidence"] = 0.6
			seedFact["sources"] = 2
			if tc.storedType == "policy" {
				seedFact["kind"] = "pragmatic"
			}
			seed := settledWritten(t, settledLearn(t, ctx, emb, "seed", nil, seedFact))[0]

			hyp := settledFact("notes", "a", tc.incoming, "hypothesis")
			hyp["confidence"] = 0.9
			res := settledLearn(t, ctx, emb, "predict", nil, hyp)

			require.Equal(t, []string{seed}, settledWritten(t, res), "merged at the stored path")
			require.Equal(t,
				[]string{fmt.Sprintf("fact 0: incoming hypothesis %q absorbed by %s %s; stored type and confidence (0.6) kept, the prediction was NOT written; to keep it, resubmit with distinct_from: [%q].",
					tc.incoming, tc.storedType, seed, seed)},
				settledNotesOf(res), "one note, no second statement-not-applied note")

			svc := testRepoService(t, ctx)
			got := readBack(t, svc, seed)
			require.Equal(t, tc.storedType, string(got.Type), "the stored type is kept")
			require.Equal(t, tc.storedText, got.Title, "the stored statement is kept")
			require.Equal(t, 0.6, got.Confidence, "a prediction does not raise a stored fact's confidence")
			require.Equal(t, 2, got.Sources)
		})
	}
}
