package mcp

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
)

// A dedup merge never changes the stored fact's type. Before: an incoming
// non-hypothesis of a DIFFERENT type with the same text and a higher
// confidence won incomingIdentityWins, so mergeFacts wrote its type (and kind)
// at the stored path with only "merged into existing fact" in the response.
// #642 stopped that for an incoming hypothesis; this is every other pair.
//
// The stored type and kind are kept (a type belongs to exactly one kind, so a
// policy over an observation must not leave a pragmatic kind on an epistemic
// type), the confidence and sources rules are unchanged, and ONE short note
// per fact says what was kept.
//
// SABOTAGE 1: take winner.Type/winner.Kind in mergeFacts again → the type flips
// → red. SABOTAGE 2: drop the kind line only → an invalid (kind, type) pair, the
// file no longer parses → red. SABOTAGE 3: skip the typeKept note → red.
// SABOTAGE 4: make typeKept ignore the type comparison → the same-type control
// row gets a type note → red.
func TestLearnHandler_MergeKeepsStoredType(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stored   string
		incoming string
		keptNote bool
	}{
		{"insight over observation", "observation", "insight", true},
		{"policy over observation, across kinds", "observation", "policy", true},
		{"observation over policy, across kinds", "policy", "observation", true},
		{"heuristic over policy", "policy", "heuristic", true},
		{"control: same type", "observation", "observation", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx, emb := newDedupAttrRepo(t)
			kindOf := func(typ string) string {
				if fact.PragmaticTypes[fact.Type(typ)] {
					return "pragmatic"
				}
				return "epistemic"
			}
			seedFact := settledFact("notes", "a", "Launch happens in October", tc.stored)
			seedFact["kind"] = kindOf(tc.stored)
			seedFact["confidence"] = 0.6
			seed := settledWritten(t, settledLearn(t, ctx, emb, "seed", nil, seedFact))[0]

			in := settledFact("notes", "a", "Launch happens in October", tc.incoming)
			in["kind"] = kindOf(tc.incoming)
			in["confidence"] = 0.9
			res := settledLearn(t, ctx, emb, "again", nil, in)

			require.Equal(t, []string{seed}, settledWritten(t, res), "merged at the stored path")
			got := readBack(t, testRepoService(t, ctx), seed)
			require.Equal(t, tc.stored, string(got.Type), "the stored type is kept")
			require.Equal(t, kindOf(tc.stored), string(got.Kind), "the stored kind goes with it")
			require.Equal(t, 0.9, got.Confidence, "the confidence rule is unchanged: max")
			require.Equal(t, 1, got.Sources, "the sources rule is unchanged")

			notes := settledNotesOf(res)
			if tc.keptNote {
				require.Equal(t, []string{fmt.Sprintf(
					"fact 0: merged into %s %s; stored type kept (incoming was %s); use knomit_update to change it. Sources unchanged: no new refs.",
					tc.stored, seed, tc.incoming)}, notes, "exactly one note")
			} else {
				require.Len(t, notes, 1)
				require.NotContains(t, notes[0], "stored type kept")
			}
		})
	}
}
