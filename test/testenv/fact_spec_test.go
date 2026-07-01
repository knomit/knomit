package testenv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// TestFactSpec_BuildRoundTrips asserts that Build produces YAML+Markdown
// that round-trips through fact.ParseFact and preserves all the fields set
// via the fluent builder.
func TestFactSpec_BuildRoundTrips(t *testing.T) {
	t.Log("Scenario: Fact(title).Type(...).Confidence(...).Refs(...) builds content that ParseFact round-trips")
	spec := Fact("alpha").
		Type(fact.Observation).
		Confidence(0.7).
		Sources(3).
		Domain("concepts", "physics").
		Entities("Alice").
		Refs("kb/b.md").
		Body("the body")

	content := spec.Build()
	require.True(t, strings.HasPrefix(content, "---\n"), "frontmatter marker missing")
	require.Contains(t, content, "# alpha")

	parsed, err := fact.ParseFact("kb/a.md", content)
	require.NoError(t, err)
	require.Equal(t, fact.Observation, parsed.Type)
	require.InDelta(t, 0.7, parsed.Confidence, 1e-9)
	require.Equal(t, 3, parsed.Sources)
	require.Equal(t, []string{"concepts", "physics"}, parsed.Domain)
	require.Equal(t, []string{"Alice"}, parsed.Entities)
	require.Equal(t, []string{"kb/b.md"}, parsed.Refs)
	require.Contains(t, parsed.Body, "the body")
}

// TestFactSpec_ImmutableChaining asserts that branching off a base spec
// produces independent specs — setting Domain on one variant does not
// leak into another.
func TestFactSpec_ImmutableChaining(t *testing.T) {
	t.Log("Scenario: two variants branched off a base spec have independent domain lists")
	base := Fact("quantum").Type(fact.Observation).Confidence(0.8)
	a := base.Domain("physics")
	b := base.Domain("philosophy")

	aContent := a.Build()
	bContent := b.Build()
	require.Contains(t, aContent, "physics")
	require.NotContains(t, aContent, "philosophy")
	require.Contains(t, bContent, "philosophy")
	require.NotContains(t, bContent, "physics")

	// Base should be unchanged — serializing it produces neither.
	baseContent := base.Build()
	require.NotContains(t, baseContent, "physics")
	require.NotContains(t, baseContent, "philosophy")
}

// TestFactSpec_DefaultsAreSafe asserts that a minimal Fact(title) spec
// produces a valid parseable fact with reasonable defaults.
func TestFactSpec_DefaultsAreSafe(t *testing.T) {
	t.Log("Scenario: Fact(title) alone produces a valid parseable fact with default type and confidence")
	content := Fact("minimal").Build()
	parsed, err := fact.ParseFact("kb/x.md", content)
	require.NoError(t, err)
	require.Equal(t, fact.Observation, parsed.Type)
	require.InDelta(t, 0.5, parsed.Confidence, 1e-9)
	require.Equal(t, "minimal", parsed.Title)
}
