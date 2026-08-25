package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/synthesize"
)

// L-4. Two candidates SHARING a Token in different lanes — reachable because a
// token-2 family is keyed by one of the ids it folded, and the disjointness
// trim can drop one of an edged pair from the family, leaving it with no edge
// while the verbatim group keeps one.
//
// The old classification asked "is this Token in the far-served list?", so
// both rows got the same arm and a near-lane group was judged under the far
// arm's verdict. No lab corpus can exercise it (no candidate set has a
// duplicate token), which is exactly why it needs a fixture rather than a
// measurement.
func TestMotifArmOf_SameTokenDifferentLanesClassifySeparately(t *testing.T) {
	// TIER AND LANE ARE DELIBERATELY CROSSED. The first version of this fixture
	// paired verbatim-with-near and family-with-far, so classifying by the
	// FAMILY FLAG gave the same answers as classifying by lane and the sabotage
	// passed — lesson 5's coinciding values, in the test written to close a
	// mislabelling. Crossed, only the lane can produce both answers.
	//
	// The crossing is realistic: a verbatim pair with no edge is FAR, and the
	// family that folds it can gain a member that creates an edge, making the
	// family NEAR.
	verbatimFar := synthesize.ScoredMotifBridge{
		Token: "shared-key", Lane: string(synthesize.LaneFar), Served: true, Family: false,
	}
	familyNear := synthesize.ScoredMotifBridge{
		Token: "shared-key", Lane: string(synthesize.LaneNear), Served: true, Family: true,
	}

	require.Equal(t, verbatimFar.Token, familyNear.Token,
		"precondition: the two rows must share a Token, or the ambiguity is not exercised")
	require.NotEqual(t, verbatimFar.Family, familyNear.Family,
		"precondition: tier must differ from lane, or classifying by tier passes too")

	require.Equal(t, "MOTIF-FAR", motifArmOf(verbatimFar),
		"the arm comes from the row's own lane — not its tier, and not a token lookup")
	require.Equal(t, "MOTIF-NEAR", motifArmOf(familyNear))
}
