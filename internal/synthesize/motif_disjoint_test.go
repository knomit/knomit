package synthesize

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// labelsWith builds a label distribution SHAPED LIKE A REAL ONE, then overlays
// the caller's own counts.
//
// The shape matters, and the first version of this helper got it wrong in a way
// the preconditions below caught: it made every filler label df=1, which is a
// corpus where 95% of labels are hapax, and a p90 over that is 1 — a cut no
// shared label can ever fall under, since a shared label has df >= 2 by
// definition. Measured on knomit-kb at 406 live facts (live index, raw
// domain+entity labels): 2010 distinct labels, 69% hapax, p90 = df 3, umbrella
// = df 81. The tail below reproduces that shape — roughly seven in ten hapax, a
// middle band, and a thin common head — so a fixture built on it exercises the
// same arithmetic production will.
//
// Keys are STEMMED tokens, because that is what SubjectLabelDF stores and what
// fact.SubjectTokens looks up: "agents" is keyed "agent", "gotchas" is keyed
// "gotcha". A fixture keyed on the unstemmed spelling silently reads df 0 for
// the token it meant to describe.
//
// Every fact's path begins with the ontology root, so "kb" is carried by the
// whole corpus and is an umbrella by construction. Fixtures must say so: a
// token ABSENT from this map reads as df 0, which is maximally specific, and a
// fixture that forgot the root would have every pair blocked by it.
func labelsWith(liveFacts int, counts map[string]int) store.SubjectLabelDF {
	d := store.SubjectLabelDF{LiveFacts: liveFacts, DF: map[string]int{"kb": liveFacts}}
	for i := 0; i < 70; i++ {
		d.DF[fmt.Sprintf("hapax%d", i)] = 1
	}
	for i := 0; i < 20; i++ {
		d.DF[fmt.Sprintf("middle%d", i)] = 2 + i%3
	}
	for i := 0; i < 10; i++ {
		d.DF[fmt.Sprintf("common%d", i)] = 5 + i
	}
	for k, v := range counts {
		d.DF[k] = v
	}
	return d
}

// TestSubjectDisjoint_RejectsTheNearDuplicatePair — MN7's own fixture. The
// near-duplicate population (8ebd5d90) is what the axis must never re-find:
// two facts about the same event, sharing a motif, sharing a RARE entity.
func TestSubjectDisjoint_RejectsTheNearDuplicatePair(t *testing.T) {
	labels := labelsWith(200, map[string]int{
		"cognition": 2, "devin": 2, "agent": 30, "technology": 40, "ai": 35})
	p := resolveDisjointnessPoint(labels)

	// Preconditions (lesson 5): this fixture only tests the DISTINCTION if it
	// runs the graded path and its two dfs straddle the operating point.
	require.False(t, p.Strict, "fixture must exercise the graded path, not the fallback")
	require.LessOrEqual(t, labels.DF["cognition"], p.Cut, "the rare entity must sit below the cut")
	require.Greater(t, labels.DF["agent"], p.Cut, "the common tag must sit above it")

	a := factForLLM{File: "kb/technology/ai/agents/1.md",
		Entities: []string{"Cognition", "Devin"}, Domain: []string{"agents"}}
	b := factForLLM{File: "kb/technology/ai/agents/2.md",
		Entities: []string{"Cognition", "Devin"}, Domain: []string{"agents"}}

	require.False(t, subjectDisjoint(a, b, labels, p),
		"two facts about the same rare entity are one subject, not a bridge")
}

// TestSubjectDisjoint_AdmitsAPairSharingOnlyACommonTag — the other direction of
// the MN7 fixture, and the reason the criterion was made finer at all: the gate
// annex's four near-misses (§7.4) are genuinely different situations, sharing
// no entities, rejected by the strict test on ONE coarse domain tag.
func TestSubjectDisjoint_AdmitsAPairSharingOnlyACommonTag(t *testing.T) {
	labels := labelsWith(200, map[string]int{
		"evaluation": 30, "cognition": 2, "rlhf": 3, "ai": 35})
	p := resolveDisjointnessPoint(labels)

	require.False(t, p.Strict)
	require.Greater(t, labels.DF["evaluation"], p.Cut, "precondition: the shared tag is common")
	require.LessOrEqual(t, labels.DF["evaluation"], p.Umbrella,
		"precondition: it is admitted by the GRADED path, not by the umbrella exclusion")
	require.LessOrEqual(t, labels.DF["cognition"], p.Cut, "precondition: the unshared entities are rare")

	a := factForLLM{File: "kb/gotchas/ai/uitesting/1.md",
		Entities: []string{"Cognition"}, Domain: []string{"evaluation"}}
	b := factForLLM{File: "kb/technology/ai/rewardhacking/2.md",
		Entities: []string{"RLHF"}, Domain: []string{"evaluation"}}

	require.True(t, subjectDisjoint(a, b, labels, p),
		"one coarse shared tag is not a shared subject (gate annex §7.4)")
}

// TestSubjectDisjoint_UmbrellaLabelNeverBlocks — 43ae4cac: a label carried by
// more than a fifth of the corpus proves nothing, and without excluding it the
// gate silently rejects entire corpora.
func TestSubjectDisjoint_UmbrellaLabelNeverBlocks(t *testing.T) {
	labels := labelsWith(100, map[string]int{"agent": 60, "alpha": 1, "beta": 1})
	p := resolveDisjointnessPoint(labels)
	require.Greater(t, labels.DF["agent"], p.Umbrella, "precondition: the shared tag is an umbrella")

	a := factForLLM{File: "kb/alpha/1.md", Domain: []string{"agents"}, Entities: []string{"Alpha"}}
	b := factForLLM{File: "kb/beta/2.md", Domain: []string{"agents"}, Entities: []string{"Beta"}}
	require.True(t, subjectDisjoint(a, b, labels, p))
}

// TestSubjectDisjoint_FallsBackToStrictBelowTheFloor — below the validity floor
// the gate does not guess a cut, it blocks on ANY shared non-umbrella label.
// Conservative is the ruled direction under default-NO.
func TestSubjectDisjoint_FallsBackToStrictBelowTheFloor(t *testing.T) {
	// 40 facts, four distinct labels: enough corpus that a shared label can be
	// below the umbrella (8), too few LABELS for a percentile to mean anything.
	// On a corpus small enough that every shared label is also an umbrella,
	// the gate correctly admits everything and the question is moot — such a
	// corpus produces no bridge candidates anyway.
	labels := store.SubjectLabelDF{LiveFacts: 40,
		DF: map[string]int{"kb": 40, "evaluation": 4, "alpha": 1, "beta": 1}}
	p := resolveDisjointnessPoint(labels)
	require.True(t, p.Strict, "a p90 over four labels is not an estimate of anything")
	require.LessOrEqual(t, labels.DF["evaluation"], p.Umbrella,
		"precondition: the shared label is NOT an umbrella, so strict is what blocks it")

	a := factForLLM{File: "kb/alpha/1.md", Domain: []string{"evaluation"}, Entities: []string{"Alpha"}}
	b := factForLLM{File: "kb/beta/2.md", Domain: []string{"evaluation"}}
	require.False(t, subjectDisjoint(a, b, labels, p),
		"strict blocks on a shared label the graded path would have admitted")

	// The SAME pair on a corpus with enough labels to estimate with IS
	// admitted, so the fallback is the floor doing its job rather than the gate
	// being broken.
	graded := labelsWith(200, map[string]int{"evaluation": 30})
	gp := resolveDisjointnessPoint(graded)
	require.False(t, gp.Strict)
	require.True(t, subjectDisjoint(a, b, graded, gp))
}

// TestSubjectDisjoint_StrictStillExcludesUmbrellas — the umbrella rule sits
// ABOVE the operating point, so it holds in the fallback too. Without this,
// strict mode would block every pair in the corpus on the ontology root that
// every path carries, and "conservative" would mean "off".
func TestSubjectDisjoint_StrictStillExcludesUmbrellas(t *testing.T) {
	labels := store.SubjectLabelDF{LiveFacts: 6, DF: map[string]int{"kb": 6, "alpha": 1, "beta": 1}}
	p := resolveDisjointnessPoint(labels)
	require.True(t, p.Strict)

	a := factForLLM{File: "kb/alpha/1.md", Entities: []string{"Alpha"}}
	b := factForLLM{File: "kb/beta/2.md", Entities: []string{"Beta"}}
	require.True(t, subjectDisjoint(a, b, labels, p),
		"the shared ontology root is an umbrella even when the gate is strict")
}

// TestSubjectDisjoint_PathTokensAreSubjectClaims — a fact's location is a
// subject claim, and the gate reads the same token set the write-time strip
// does. Two facts filed under the same rare category are one subject even when
// their tags say nothing.
func TestSubjectDisjoint_PathTokensAreSubjectClaims(t *testing.T) {
	labels := labelsWith(200, map[string]int{"resolver": 2, "store": 30})
	p := resolveDisjointnessPoint(labels)
	require.LessOrEqual(t, labels.DF["resolver"], p.Cut)

	a := factForLLM{File: "kb/gotchas/store/resolver/1.md"}
	b := factForLLM{File: "kb/incidents/store/resolver/2.md"}
	require.False(t, subjectDisjoint(a, b, labels, p))
}

// TestSubjectDisjoint_UnknownLabelIsTreatedAsSpecific — a token the
// distribution does not know reads as df 0 and BLOCKS. It is the conservative
// reading, and it is the one the caller gets when a label distribution is
// stale, partial, or unavailable.
func TestSubjectDisjoint_UnknownLabelIsTreatedAsSpecific(t *testing.T) {
	labels := labelsWith(200, nil)
	p := resolveDisjointnessPoint(labels)

	a := factForLLM{File: "kb/alpha/1.md", Entities: []string{"Unmapped Thing"}}
	b := factForLLM{File: "kb/beta/2.md", Entities: []string{"Unmapped Thing"}}
	require.Zero(t, labels.DF["unmapped"], "precondition: the shared token is unknown here")
	require.False(t, subjectDisjoint(a, b, labels, p))
}
