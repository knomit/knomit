package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// P1 merge-time guard, consensus round: two pushed branches in ONE merger
// run — agent/bad-… reverts the agent branch's .knomit/ontology.yaml change,
// agent/good-… adds a kb/ fact. The bad one is refused (one warning naming
// the path, remembered per tip) and does not stop the good one: exactly one
// merge, one refusal, and the agent branch keeps the newer ontology.
//
// SABOTAGE: guardPrivate unset in MergeConsensus → the bad branch merges,
// Merges == 2, the ontology reverts → red.
func TestConsensus_PrivatePathRevertRefusedOthersMerge(t *testing.T) {
	ctx := context.Background()
	old := triggerOntology(autoOffAttrs)
	h := newConsensusHost(t, old, hostOpts{})
	svc := h.svc(t)

	// good forks before the host's ontology edit (carries the old one,
	// unchanged); bad forks after it and puts the old one back.
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent/good-33333333", cHostAgent))
	newer := old + "# host edit\n"
	h.setOntology(t, newer)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent/bad-22222222", cHostAgent))
	_, err := svc.Facts().WriteFact(ctx, "agent/bad-22222222", OntologyPath, old, "revert", "updated")
	require.NoError(t, err)
	writeOn(t, h.ri, "agent/good-33333333", "kb/tasks/good.md")
	require.True(t, h.ri.IsPushedBranch("agent/bad-22222222"))
	require.True(t, h.ri.IsPushedBranch("agent/good-33333333"))

	h.settle(t)
	s := h.stats()
	require.Equal(t, 1, s.Merges, "the good branch merges: %v", s.Warnings)
	require.Equal(t, 1, s.Refused, "the bad branch is refused: %v", s.Warnings)
	require.Equal(t, 1, warningsWith(s, "agent/bad-22222222"), "%v", s.Warnings)
	require.Equal(t, 1, warningsWith(s, "would change .knomit/ontology.yaml"), "%v", s.Warnings)
	require.True(t, h.has(t, cHostAgent, "kb/tasks/good.md"))
	require.Equal(t, newer, h.content(t, cHostAgent, OntologyPath), "the agent branch keeps the newer ontology")
}
