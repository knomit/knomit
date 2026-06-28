package mcp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProfileInstructions_HypothesizeStepIsPlain verifies that the per-fact
// hypothesize loop's step 5 no longer carries the misplaced bridge/discovered
// language. A single-fact, self-reasoned hypothesis is authored by default;
// the discovered origin is decided at proposal time in the discover work-item
// prompt, not in this loop. Renders cleanly across all MCP profiles.
func TestProfileInstructions_HypothesizeStepIsPlain(t *testing.T) {
	for _, profile := range []string{"code", "chat", "generic"} {
		t.Run(profile, func(t *testing.T) {
			out := ProfileInstructions(profile, "kb", nil)
			require.NotEmpty(t, out)

			require.Contains(t, out, "call knomit_learn with type: hypothesis",
				"step 5 must keep the plain hypothesis-write instruction")
			// The reverted-out phrasings must be gone — origin is not decided by
			// whether the agent previewed the fact.
			require.NotContains(t, out, "previewed before saving",
				"the review-act trigger must be removed from the instructions")
			require.NotContains(t, out, "stays origin: authored",
				"the misplaced bridge case must be removed from the hypothesize loop")
		})
	}
}

// TestProfileInstructions_LearnDescriptionKeysOffGrouping verifies the base
// knomit_learn description ties origin to how the candidate group was formed,
// not to the review act.
func TestProfileInstructions_LearnDescriptionKeysOffGrouping(t *testing.T) {
	out := ProfileInstructions("code", "kb", nil)
	// Find the knomit_learn bullet and assert it speaks of work-item grouping.
	require.Contains(t, out, "origin reflects how the candidate group was formed, not whether you previewed it")
	require.True(t, strings.Contains(out, "discovered for a cross-cluster bridge"),
		"learn description must map bridge → discovered")
}
