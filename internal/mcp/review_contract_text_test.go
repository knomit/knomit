package mcp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The continuation contract is stated where the agent reads it: knomit_review
// requires session_id on every call after the first, and knomit_bind's handle
// selects the repo without carrying review state. A model that conflates the
// two drops session_id and cannot continue.
func TestReviewAndBindDescriptions_StateTheContinuationContract(t *testing.T) {
	review := reviewTool().Description
	require.Contains(t, review, "session_id is required on every call after the first")
	require.Contains(t, review, "takeover")
	require.Contains(t, review, "resumes")
	require.Contains(t, review, "item_id with every response")
	item := reviewTool().InputSchema.Properties["item_id"].(map[string]any)["description"].(string)
	require.Contains(t, item, "Required with every response")

	bind := bindTool().Description
	require.Contains(t, bind, "carries no review state")

	banner := handleBanner("h")
	require.Contains(t, banner, "carries no review state")

	for name, text := range map[string]string{"review": review, "bind": bind, "banner": banner} {
		require.False(t, strings.Contains(text, "#1") || strings.Contains(text, "#2"),
			"%s: agent-facing text states the rule, never an issue reference", name)
	}
}
