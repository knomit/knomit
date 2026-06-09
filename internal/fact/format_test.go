package fact

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSerializeFact_URLWithQueryString_Parseable regresses the production
// bug observed on agent/mindev.local-8ef0cd32: a learn-call wrote a fact
// whose refs contained a URL with `?rc=foo` query string. The old
// hand-rolled serializer's quoting predicate (`,]"`) didn't include `?`,
// so the URL went into the inline list unquoted. YAML's flow context
// treats `?` as the explicit complex-key indicator, breaking the parse:
//
//	yaml: line 5: did not find expected ',' or ']'
//
// The new yaml.v3-based SerializeFact must produce content that ParseFact
// reads back losslessly.
func TestSerializeFact_URLWithQueryString_Parseable(t *testing.T) {
	f := NewFact("kb/x.md")
	f.Title = "Google signs classified AI deal with DOD for Gemini models"
	f.Body = "Body text."
	f.Type = Observation
	f.Domain = []string{"AI governance", "national security", "defense"}
	f.Confidence = 0.85
	f.Sources = 0
	f.Entities = []string{"Google", "Gemini", "DOD", "Pentagon"}
	f.Refs = []string{"https://www.theinformation.com/articles/google-signs-classified-ai-deal-pentagon-amid-employee-opposition?rc=pt5ur4"}

	out, err := SerializeFact(f)
	require.NoError(t, err)

	// Round-trip: ParseFact must read back the same data.
	parsed, err := ParseFact("kb/x.md", out)
	require.NoError(t, err, "URL with `?` query string must round-trip cleanly")
	require.Equal(t, f.Title, parsed.Title)
	require.Equal(t, f.Body, parsed.Body)
	require.Equal(t, f.Type, parsed.Type)
	require.Equal(t, f.Domain, parsed.Domain)
	require.Equal(t, f.Confidence, parsed.Confidence)
	require.Equal(t, f.Sources, parsed.Sources)
	require.Equal(t, f.Entities, parsed.Entities)
	require.Equal(t, f.Refs, parsed.Refs)
}

// TestSerializeFact_RoundTrip covers a representative set of values that
// previously could (or did) trip the hand-rolled serializer. Every
// item-level YAML flow-context indicator must survive a full
// SerializeFact → ParseFact cycle.
func TestSerializeFact_RoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		domain   []string
		entities []string
		refs     []string
	}{
		{
			name:     "url with question mark",
			refs:     []string{"https://example.com/x?q=v"},
			entities: []string{"A"},
			domain:   []string{"d"},
		},
		{
			name:     "item with colon",
			entities: []string{"foo:bar"},
			domain:   []string{"d"},
			refs:     []string{},
		},
		{
			name:     "item with comma",
			entities: []string{"Smith, John"},
			domain:   []string{"d"},
			refs:     []string{},
		},
		{
			name:     "item with brackets",
			entities: []string{"Acme [redacted]"},
			domain:   []string{"d"},
			refs:     []string{},
		},
		{
			name:     "item with quote",
			entities: []string{`said "hello"`},
			domain:   []string{"d"},
			refs:     []string{},
		},
		{
			name:     "yaml-keyword-looking item",
			entities: []string{"No", "yes", "null", "true"},
			domain:   []string{"d"},
			refs:     []string{},
		},
		{
			name:     "leading hyphen",
			entities: []string{"-leading-hyphen"},
			domain:   []string{"d"},
			refs:     []string{},
		},
		{
			name:     "all empty lists",
			domain:   []string{},
			entities: []string{},
			refs:     []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFact("kb/test.md")
			f.Title = "T"
			f.Body = "B"
			f.Type = Observation
			f.Confidence = 0.5
			f.Sources = 1
			f.Domain = tc.domain
			f.Entities = tc.entities
			f.Refs = tc.refs

			out, err := SerializeFact(f)
			require.NoError(t, err, "serialize must not error for valid struct")

			parsed, err := ParseFact("kb/test.md", out)
			require.NoError(t, err, "round-trip must parse cleanly:\n%s", out)
			require.Equal(t, f.Domain, parsed.Domain, "domain mismatch")
			require.Equal(t, f.Entities, parsed.Entities, "entities mismatch")
			require.Equal(t, f.Refs, parsed.Refs, "refs mismatch")
			require.Equal(t, f.Title, parsed.Title)
			require.Equal(t, f.Body, parsed.Body)
		})
	}
}

// TestSerializeFact_BoundsValidation enforces the numeric field invariants at
// the serialize chokepoint: confidence must be in [0,1] and sources >= 0, so
// no write path can persist an out-of-range fact.
func TestSerializeFact_BoundsValidation(t *testing.T) {
	base := func() Fact {
		f := NewFact("kb/test.md")
		f.Title, f.Body, f.Type = "T", "B", Observation
		f.Confidence, f.Sources = 0.5, 1
		return f
	}
	t.Run("confidence above 1 rejected", func(t *testing.T) {
		f := base()
		f.Confidence = 1.5
		_, err := SerializeFact(f)
		require.Error(t, err)
		require.Contains(t, err.Error(), "confidence")
	})
	t.Run("confidence below 0 rejected", func(t *testing.T) {
		f := base()
		f.Confidence = -0.1
		_, err := SerializeFact(f)
		require.Error(t, err)
		require.Contains(t, err.Error(), "confidence")
	})
	t.Run("negative sources rejected", func(t *testing.T) {
		f := base()
		f.Sources = -1
		_, err := SerializeFact(f)
		require.Error(t, err)
		require.Contains(t, err.Error(), "sources")
	})
	t.Run("boundary values 0 and 1 accepted", func(t *testing.T) {
		for _, c := range []float64{0, 1} {
			f := base()
			f.Confidence = c
			_, err := SerializeFact(f)
			require.NoError(t, err, "confidence %v must be valid", c)
		}
	})
}

// TestParseFact_RejectsOutOfBoundsConfidence confirms the bounds check fires
// on read too (symmetric with serialize), so a hand-corrupted fact file with
// confidence outside [0,1] fails to parse rather than loading silently.
func TestParseFact_RejectsOutOfBoundsConfidence(t *testing.T) {
	const content = `---
type: observation
domain: [d]
confidence: 1.5
sources: 1
entities: []
refs: []
---
# Title

Body.
`
	_, err := ParseFact("kb/test.md", content)
	require.Error(t, err)
	require.Contains(t, err.Error(), "confidence")
}

// TestSerializeFact_FlowStyleSequences confirms that lists render as
// inline `[a, b]` flow style — not block style with one-item-per-line
// — so existing fact files keep their compact one-line-per-key layout.
func TestSerializeFact_FlowStyleSequences(t *testing.T) {
	f := NewFact("kb/x.md")
	f.Title = "T"
	f.Body = "B"
	f.Type = Observation
	f.Confidence = 0.9
	f.Sources = 1
	f.Domain = []string{"a", "b"}
	f.Entities = []string{"X"}
	f.Refs = []string{}

	out, err := SerializeFact(f)
	require.NoError(t, err)

	require.Contains(t, out, "[a, b]", "domain must render as flow-style inline list")
	require.NotContains(t, out, "- a\n", "must not render as block-style sequence")
	require.Contains(t, out, "[X]", "entities must render as flow-style inline list")
	require.Contains(t, out, "[]", "empty refs must render as []")
	// Closing frontmatter, then markdown title.
	require.Contains(t, out, "---\n# T\n")
}

// TestSerializeFact_EvidenceWeightOmittedWhenZero pins the existing
// behavior: evidence_weight only appears in output when > 0.
func TestSerializeFact_EvidenceWeightOmittedWhenZero(t *testing.T) {
	f := NewFact("kb/x.md")
	f.Title = "T"
	f.Type = Observation
	f.Domain = []string{}
	f.Entities = []string{}
	f.Refs = []string{}
	// EvidenceWeight defaults to 0.

	out, err := SerializeFact(f)
	require.NoError(t, err)
	require.NotContains(t, out, "evidence_weight", "EvidenceWeight=0 must not be written")

	// And when set, it appears.
	f.EvidenceWeight = 0.42
	out, err = SerializeFact(f)
	require.NoError(t, err)
	require.Contains(t, out, "evidence_weight: 0.42")
}

// TestSerializeFact_HeaderAndFooter confirms the `---` frontmatter
// delimiters and `# Title` heading land at the expected offsets so that
// ParseFact's offset-based splitter (strings.Index "\n---\n") still
// works on the new serializer's output.
func TestSerializeFact_HeaderAndFooter(t *testing.T) {
	f := NewFact("kb/x.md")
	f.Title = "Hello"
	f.Type = Observation
	f.Domain = []string{}
	f.Entities = []string{}
	f.Refs = []string{}

	out, err := SerializeFact(f)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(out, "---\n"), "must start with opening frontmatter delimiter")
	closeIdx := strings.Index(out[4:], "\n---\n")
	require.NotEqual(t, -1, closeIdx, "must contain closing frontmatter delimiter")
	require.Contains(t, out, "\n# Hello\n")
}
