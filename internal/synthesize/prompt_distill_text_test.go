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
	require.Contains(t, content.Prompt, "Zero retractions is the normal and expected outcome")
	require.NotContains(t, content.Prompt, "preserves ALL of its conditions and caveats",
		"the old total-preservation retract rule made the retract limb unreachable")
}
