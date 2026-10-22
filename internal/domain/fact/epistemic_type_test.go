package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInsight_IsValidEpistemicType(t *testing.T) {
	require.True(t, EpistemicTypes[Insight], "insight must be in the EpistemicTypes set")
	require.True(t, Epistemic.AllowsType(Insight), "epistemic kind must allow insight")
	require.False(t, Pragmatic.AllowsType(Insight), "pragmatic kind must not allow insight")
	require.Contains(t, AllEpistemicTypes(), Insight, "insight must appear in the stable ordering")
}

func TestAllEpistemicTypes_MatchesSet(t *testing.T) {
	ordered := AllEpistemicTypes()
	require.Len(t, ordered, len(EpistemicTypes),
		"AllEpistemicTypes() and EpistemicTypes must stay in sync — add new types to both")
	for _, ty := range ordered {
		require.True(t, EpistemicTypes[ty], "ordered type %q missing from EpistemicTypes set", ty)
	}
}

func TestParseFact_Epistemic_Insight(t *testing.T) {
	const content = `---
type: insight
domain: [fact]
confidence: 0.8
sources: 0
entities: []
refs: []
---
# Connecting the chokepoint to the synthesis pipeline

Body.
`
	f, err := ParseFact("kb/i.md", content)
	require.NoError(t, err)
	require.Equal(t, Epistemic, f.Kind)
	require.Equal(t, Insight, f.Type)
}

func TestSerializeFact_RoundTrip_Insight(t *testing.T) {
	f := NewFact("kb/i.md")
	f.Title = "Connecting the chokepoint to the synthesis pipeline"
	f.Body = "Body content."
	f.Kind = Epistemic
	f.Type = Insight
	f.Domain = []string{"fact"}
	f.Confidence = 0.8
	f.Entities = []string{}
	f.Refs = []string{}

	out, err := SerializeFact(f)
	require.NoError(t, err)
	require.Contains(t, out, "type: insight")
	require.NotContains(t, out, "kind:",
		"insight is epistemic, so kind must be omitted for round-trip fidelity")

	parsed, err := ParseFact("kb/i.md", out)
	require.NoError(t, err)
	require.Equal(t, Epistemic, parsed.Kind)
	require.Equal(t, Insight, parsed.Type)
	require.Equal(t, f.Title, parsed.Title)
	require.Equal(t, f.Body, parsed.Body)
}
