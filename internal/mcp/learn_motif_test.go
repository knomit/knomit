package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// motifMergeFixture builds a new/existing pair whose winner is the one the
// caller asked for, asserts that intent against newFactWins so the fixture
// cannot silently invert, and returns the merge result.
func motifMergeFixture(t *testing.T, newMotifs, existingMotifs []string, newWins bool) fact.Fact {
	t.Helper()
	nf := fact.NewFact("kb/alpha/new.md")
	nf.Title = "New"
	nf.Body = "New body"
	nf.Type = fact.Observation
	nf.Confidence = 0.9
	nf.Sources = 1
	nf.Motifs = newMotifs

	ex := fact.NewFact("kb/alpha/existing.md")
	ex.Title = "Existing"
	ex.Body = "Existing body"
	ex.Type = fact.Observation
	ex.Confidence = 0.5
	ex.Sources = 1
	ex.Motifs = existingMotifs

	if !newWins {
		nf.Confidence, ex.Confidence = 0.5, 0.9
	}
	require.Equal(t, newWins, newFactWins(nf, ex), "fixture must produce the intended winner")
	return mergeFacts(nf, ex, "kb/alpha/existing.md")
}

// TestMergeFacts_MotifsWinnerFirstTrimAtCap is the roadmap's conformance case,
// verbatim: winner has 2, loser has 3, and exactly the winner's 2 plus the
// loser's FIRST 1 survive, in that order.
func TestMergeFacts_MotifsWinnerFirstTrimAtCap(t *testing.T) {
	merged := motifMergeFixture(t,
		[]string{"win-one", "win-two"},
		[]string{"lose-one", "lose-two", "lose-three"},
		true)
	require.Equal(t, []string{"win-one", "win-two", "lose-one"}, merged.Motifs)
}

// TestMergeFacts_MotifsFollowTheWinnerNotTheCaller — same inputs, opposite
// winner, so the order flips. This is what distinguishes winner-first from the
// incoming-first union that domain and entities happen to use; without it a
// test could pass on either implementation.
func TestMergeFacts_MotifsFollowTheWinnerNotTheCaller(t *testing.T) {
	merged := motifMergeFixture(t,
		[]string{"new-one", "new-two"},
		[]string{"old-one", "old-two", "old-three"},
		false)
	require.Equal(t, []string{"old-one", "old-two", "old-three"}, merged.Motifs)
}

// TestMergeFacts_DomainAndEntitiesStayIncomingFirst pins the deliberate local
// inconsistency, so a later tidying pass cannot "make them consistent" without
// a failing test to argue with. Domain and entities are uncapped, so their
// order is cosmetic; motifs are capped, so order decides what survives.
func TestMergeFacts_DomainAndEntitiesStayIncomingFirst(t *testing.T) {
	nf := fact.NewFact("kb/alpha/new.md")
	nf.Title, nf.Body, nf.Type = "New", "New body", fact.Observation
	nf.Confidence, nf.Sources = 0.5, 1 // loses
	nf.Domain = []string{"new-domain"}
	nf.Entities = []string{"NewEntity"}

	ex := fact.NewFact("kb/alpha/existing.md")
	ex.Title, ex.Body, ex.Type = "Existing", "Existing body", fact.Observation
	ex.Confidence, ex.Sources = 0.9, 1 // wins
	ex.Domain = []string{"old-domain"}
	ex.Entities = []string{"OldEntity"}

	require.False(t, newFactWins(nf, ex))
	merged := mergeFacts(nf, ex, "kb/alpha/existing.md")

	require.Equal(t, []string{"new-domain", "old-domain"}, merged.Domain,
		"domain stays incoming-first even when the incoming fact loses")
	require.Equal(t, []string{"NewEntity", "OldEntity"}, merged.Entities,
		"entities stay incoming-first even when the incoming fact loses")
}

func TestMergeFacts_MotifsDeduplicateAcrossParents(t *testing.T) {
	merged := motifMergeFixture(t,
		[]string{"shared-shape"},
		[]string{"shared-shape", "other-shape"},
		true)
	require.Equal(t, []string{"shared-shape", "other-shape"}, merged.Motifs)
}

func TestMergeFacts_MotifsEmptyStaysEmpty(t *testing.T) {
	merged := motifMergeFixture(t, nil, nil, true)
	require.Nil(t, merged.Motifs)
}

// TestMergeFacts_MergedFactSerializes — an over-cap union that was not trimmed
// would fail SerializeFact and abort the whole knomit_learn call, turning a
// routine merge into a user-visible error. Trimming in the merge is what keeps
// that from happening.
func TestMergeFacts_MergedFactSerializes(t *testing.T) {
	merged := motifMergeFixture(t,
		[]string{"win-one", "win-two", "win-three"},
		[]string{"lose-one", "lose-two", "lose-three"},
		true)
	merged.Domain = []string{}
	merged.Entities = []string{}
	merged.Refs = []string{}
	_, err := fact.SerializeFact(merged)
	require.NoError(t, err)
	require.Len(t, merged.Motifs, fact.MaxMotifs)
}
