package merge

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
)

// The policy round-trips through its strategy value, and nothing else parses
// as one.
func TestConflictsPolicy_StrategyRoundTrip(t *testing.T) {
	for _, facts := range []string{fact.ConflictsOff, fact.ConflictsMerge, fact.ConflictsMergeConsensus, fact.ConflictsConsensus} {
		for _, state := range []string{fact.ConflictsOff, fact.ConflictsConsensus} {
			p := ConflictsPolicy{facts: facts, state: state}
			got, ok := ConflictsPolicyOf(p.strategy())
			require.True(t, ok, p.strategy())
			require.Equal(t, p, got)
		}
	}
	for _, s := range []ConflictStrategy{StrategyLocalWins, StrategyRemoteWins, StrategyRefuse, "", "conflicts:", "conflicts:merge",
		"conflicts:merge/merge:consensus", "conflicts:bogus/off", "merge_facts"} {
		_, ok := ConflictsPolicyOf(s)
		require.False(t, ok, s)
	}
}
