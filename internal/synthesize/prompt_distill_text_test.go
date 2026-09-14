package synthesize

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The distill prompt must give the model something to PASS, not only things
// to avoid failing. The mechanism sentence is the same test the alias prompt
// uses ("state the mechanism or answer different"), mirrored here.
func TestDistillPrompt_CarriesPositiveMechanismTest(t *testing.T) {
	content, err := RenderDistillWorkItem(nil, "kb", "")
	require.NoError(t, err)
	require.Contains(t, content.Prompt, "state in one sentence the mechanism that ALL members instantiate")
	require.Contains(t, content.Prompt, "If you cannot write that sentence, return no synthesis")
	require.Contains(t, content.Prompt, "Put that sentence first in the body")
	require.Contains(t, content.Prompt, "Declining is itself a verdict you are accountable for")
}

func TestDistillPrompt_SplitsLoadBearingFromProvenance(t *testing.T) {
	content, err := RenderDistillWorkItem(nil, "kb", "")
	require.NoError(t, err)
	require.Contains(t, content.Prompt, "Load-bearing conditions")
	require.Contains(t, content.Prompt, "Provenance detail")
	require.Contains(t, content.Prompt, "MUST NOT be copied wholesale")
	require.Contains(t, content.Prompt, "Zero retractions is the normal outcome over a cluster of observations")
	require.NotContains(t, content.Prompt, "preserves ALL of its conditions and caveats",
		"the old total-preservation retract rule made the retract limb unreachable")
}

// The retraction rule and the hypothesis bullet used to contradict each other:
// "retract ONLY a strict restatement" forbade the "retract a confirmed
// hypothesis" the Hypothesis-handling section mandates. A confirmed hypothesis
// that is never retracted stays typed as an unconfirmed prediction, and the
// prompt bars hypotheses from being distill inputs — so the contradiction did
// not merely confuse, it made the fact permanently inert.
func TestDistillPrompt_RetractionYieldsToTheHypothesisRule(t *testing.T) {
	content, err := RenderDistillWorkItem(nil, "kb", "")
	require.NoError(t, err)
	require.Contains(t, content.Prompt, "The hypothesis rule below is separate and takes precedence",
		"the retraction rule must yield, or it forbids the retraction the hypothesis section mandates")
	require.Contains(t, content.Prompt, "type transition, not subsumption",
		"the reason matters: a confirmed hypothesis is retracted despite not being a restatement")
}

// The retract limb must have a reachable live case, not just a narrower reason
// to never fire. Its case is a member whose provenance lives in its refs rather
// than its body — a prior synthesis or digest — whose whole claim the new
// synthesis restates, with those refs carried forward so nothing is orphaned.
func TestDistillPrompt_RetractLimbNamesItsLiveCase(t *testing.T) {
	content, err := RenderDistillWorkItem(nil, "kb", "")
	require.NoError(t, err)
	require.Contains(t, content.Prompt, "provenance is carried by its refs rather than its body",
		"without a named live case the limb is unreachable, which is the defect this replaced")
	require.Contains(t, content.Prompt, "Carry that member's refs into the new synthesis's refs",
		"retracting a member whose refs are its provenance must not orphan them")
	require.Contains(t, content.Prompt, "whose body carries its own measurement, date, source, or affected list stays")
}
