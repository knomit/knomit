package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A dedup merge must never turn a stored non-hypothesis fact into a hypothesis.
// Before: an incoming hypothesis with the same text and HIGHER confidence won
// newFactWins, so mergeFacts took its Type and the stored observation became a
// prediction, with only "merged into existing fact" in the response.
//
// Now the stored fact keeps its whole identity AND its confidence (a prediction
// is not evidence: it adds no sources either), and the response says the
// hypothesis was absorbed.
//
// SABOTAGE 1: drop the absorbedHypothesis conjunct in incomingIdentityWins →
// the type flips → red. SABOTAGE 2: drop the confidence override in mergeFacts
// → confidence becomes 0.9 → red.
func TestLearnHandler_IncomingHypothesisIsAbsorbedByStoredObservation(t *testing.T) {
	_, ctx, emb := newDedupAttrRepo(t)
	seedFact := settledFact("notes", "a", "Launch happens in October", "observation")
	seedFact["confidence"] = 0.6
	seedFact["sources"] = 2
	seed := settledWritten(t, settledLearn(t, ctx, emb, "seed", nil, seedFact))[0]

	hyp := settledFact("notes", "a", "Launch happens in October", "hypothesis")
	hyp["confidence"] = 0.9
	res := settledLearn(t, ctx, emb, "predict", nil, hyp)

	require.Equal(t, []string{seed}, settledWritten(t, res), "merged at the stored path")
	require.Equal(t,
		[]string{"fact 0: incoming hypothesis absorbed by observation " + seed + "; type kept; confidence kept at 0.6; sources unchanged."},
		settledNotesOf(res))

	svc := testRepoService(t, ctx)
	got := readBack(t, svc, seed)
	require.Equal(t, "observation", string(got.Type), "the stored type is kept")
	require.Equal(t, 0.6, got.Confidence, "a prediction does not raise a stored fact's confidence")
	require.Equal(t, 2, got.Sources)
}
