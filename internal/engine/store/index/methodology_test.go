package index

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFormatMethodologySection_NonEmpty verifies the prompt-section
// formatter renders only ranked bullet lines (title + path + score),
// with no leading heading and with bodies omitted. Each call site
// supplies its own heading wording.
func TestFormatMethodologySection_NonEmpty(t *testing.T) {
	matches := []MethodologyMatch{
		{Path: "kb/meta/reasoning/m1.md", Title: "M1 title", Body: "M1 body about evidence weighting.", Score: 0.87},
		{Path: "kb/meta/reasoning/m2.md", Title: "M2 title", Body: "M2 body about pitfall detection.", Score: 0.62},
	}
	got := FormatMethodologySection(matches)
	require.NotContains(t, got, "Applicable methodology",
		"formatter must not emit a heading; callers own framing")
	require.NotContains(t, got, "Existing methodology")
	require.Contains(t, got, "M1 title")
	require.Contains(t, got, "kb/meta/reasoning/m1.md")
	require.Contains(t, got, "score=0.87")
	require.Contains(t, got, "score=0.62")
	require.NotContains(t, got, "M1 body about evidence weighting.")
	require.Equal(t, 2, strings.Count(got, "•"))
}

// TestFormatMethodologySection_Empty returns empty string for empty input
// so callers can omit the entire section.
func TestFormatMethodologySection_Empty(t *testing.T) {
	require.Equal(t, "", FormatMethodologySection(nil))
	require.Equal(t, "", FormatMethodologySection([]MethodologyMatch{}))
}
