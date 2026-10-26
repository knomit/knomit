package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
)

// A dedup merge's origin is LAST-WINS (user ruling): when the stored and
// incoming types agree, the merged origin is the incoming fact's, because the
// incoming fact is the newest, whichever side wins on confidence or identity.
// An unset incoming origin is resolved first (authored; distilled for
// synthesis), so it merges exactly as if sent explicitly. When the types
// differ the stored type is kept and the stored origin stays with it, because
// origin must be valid for the type (Origin.ValidateForType).
//
// SABOTAGE 1: merged.Origin = winner.Origin → the stored-wins rows keep the
// stored origin → red. SABOTAGE 2: always keep existing.Origin → every
// same-type row → red. SABOTAGE 3: always take the incoming origin → the
// different-type rows are reattributed → red. SABOTAGE 4 (unresolved incoming
// origin) is caught by TestMergeFacts_UnsetIncomingOriginIsResolved.
func TestLearnHandler_MergeOriginIsLastWins(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		stored, incoming             string
		storedOrigin, inOrigin, want string // "" = omit
		inConf                       float64
	}{
		// Same type, STORED fact wins on confidence: origin is still the incoming one.
		{"stored wins: authored over distilled", "synthesis", "synthesis", "distilled", "authored", "authored", 0.3},
		{"stored wins: distilled over authored", "synthesis", "synthesis", "authored", "distilled", "distilled", 0.3},
		{"stored wins: discovered over distilled", "synthesis", "synthesis", "distilled", "discovered", "discovered", 0.3},
		// Same type, INCOMING fact wins.
		{"incoming wins: authored over distilled", "synthesis", "synthesis", "distilled", "authored", "authored", 0.9},
		{"incoming wins: discovered over distilled", "synthesis", "synthesis", "distilled", "discovered", "discovered", 0.9},
		{"incoming wins: distilled over discovered", "synthesis", "synthesis", "discovered", "distilled", "distilled", 0.9},
		{"hypothesis: authored over discovered", "hypothesis", "hypothesis", "discovered", "authored", "authored", 0.9},
		{"hypothesis: discovered over authored", "hypothesis", "hypothesis", "", "discovered", "discovered", 0.3},
		// Unset incoming origin resolves to the type default, then wins.
		{"unset on hypothesis, incoming wins", "hypothesis", "hypothesis", "discovered", "", "authored", 0.9},
		{"unset on hypothesis, stored wins", "hypothesis", "hypothesis", "discovered", "", "authored", 0.3},
		{"unset on synthesis, incoming wins", "synthesis", "synthesis", "authored", "", "distilled", 0.9},
		{"unset on synthesis, stored wins", "synthesis", "synthesis", "authored", "", "distilled", 0.3},
		// Different type: the stored type and its origin are kept.
		{"type differs: stored distilled kept", "synthesis", "observation", "distilled", "authored", "distilled", 0.9},
		{"type differs: stored discovered kept", "synthesis", "observation", "discovered", "", "discovered", 0.9},
		{"type differs, stored wins: stored kept", "synthesis", "observation", "distilled", "authored", "distilled", 0.3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx, emb := newDedupAttrRepo(t)
			seedFact := settledFact("notes", "a", "Launch happens in October", tc.stored)
			seedFact["confidence"] = 0.6
			if tc.storedOrigin != "" {
				seedFact["origin"] = tc.storedOrigin
			}
			seed := settledWritten(t, settledLearn(t, ctx, emb, "seed", nil, seedFact))[0]

			in := settledFact("notes", "a", "Launch happens in October", tc.incoming)
			in["confidence"] = tc.inConf
			if tc.inOrigin != "" {
				in["origin"] = tc.inOrigin
			}
			res := settledLearn(t, ctx, emb, "again", nil, in)

			require.Equal(t, []string{seed}, settledWritten(t, res), "merged at the stored path")
			got := readBack(t, testRepoService(t, ctx), seed)
			require.Equal(t, tc.stored, string(got.Type), "the stored type is kept")
			require.Equal(t, tc.want, string(got.Origin))
		})
	}
}

// The in-memory merge result carries the RESOLVED origin, not the raw empty
// one: downstream gates read the raw field (computeEvidenceWeights), so an
// unset incoming origin must merge exactly like the explicit default.
func TestMergeFacts_UnsetIncomingOriginIsResolved(t *testing.T) {
	for _, tc := range []struct {
		typ  fact.Type
		want fact.Origin
	}{
		{fact.Synthesis, fact.Distilled},
		{fact.Hypothesis, fact.Authored},
	} {
		in := fact.NewFact("kb/notes/a/new.md")
		in.Type, in.Confidence = tc.typ, 0.3
		old := fact.NewFact("kb/notes/a/old.md")
		old.Type, old.Confidence, old.Origin = tc.typ, 0.6, fact.Discovered
		require.Equal(t, tc.want, mergeFacts(in, old, "repo").Origin, string(tc.typ))
	}
}
