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

// TestParseFactOriginDefaults verifies the type-aware default rule for origin
// applied to legacy fact files (no `origin` field) and rejection of unknown
// values. New facts written by knomit always set origin explicitly; the
// defaults exist so existing files don't need rewriting.
func TestParseFactOriginDefaults(t *testing.T) {
	// Legacy authored fact: no origin field, non-synthesis type → authored.
	authored := "---\ntype: observation\ndomain: [x]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# T\n\nbody"
	f, err := ParseFact("kb/x/a.md", authored)
	if err != nil {
		t.Fatal(err)
	}
	if f.Origin != Authored {
		t.Errorf("legacy observation origin = %q, want authored", f.Origin)
	}

	// Legacy synthesis fact: no origin field → distilled (type-aware default).
	synth := "---\ntype: synthesis\ndomain: [x]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# T\n\nbody"
	fs, err := ParseFact("kb/x/s.md", synth)
	if err != nil {
		t.Fatal(err)
	}
	if fs.Origin != Distilled {
		t.Errorf("legacy synthesis origin = %q, want distilled", fs.Origin)
	}

	// Explicit origin is honored.
	disc := "---\ntype: synthesis\norigin: discovered\ndomain: [x]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# T\n\nbody"
	fd, err := ParseFact("kb/x/d.md", disc)
	if err != nil {
		t.Fatal(err)
	}
	if fd.Origin != Discovered {
		t.Errorf("explicit origin = %q, want discovered", fd.Origin)
	}

	// Invalid origin rejected.
	bad := "---\ntype: observation\norigin: nonsense\ndomain: [x]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# T\n\nbody"
	if _, err := ParseFact("kb/x/bad.md", bad); err == nil {
		t.Error("ParseFact with invalid origin = nil error, want error")
	}
}

// TestSerializeFactOriginRoundTrip confirms that authored facts emit no
// `origin:` line (byte-identical to pre-origin format) while non-default
// origins both appear in the file and round-trip through ParseFact.
func TestSerializeFactOriginRoundTrip(t *testing.T) {
	// Authored: no origin line emitted (byte-identical legacy round-trip).
	a := NewFact("kb/x/a.md")
	a.Title, a.Type, a.Origin = "T", Observation, Authored
	a.Domain, a.Entities, a.Refs = []string{"x"}, []string{}, []string{}
	a.Confidence, a.Sources, a.Body = 0.9, 1, "body"
	out, err := SerializeFact(a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "origin:") {
		t.Errorf("authored fact emitted origin line:\n%s", out)
	}

	// Discovered: origin line emitted and round-trips. Discovered is
	// reserved for synthesis/hypothesis facts, so the type moves with it.
	d := a
	d.Type, d.Origin = Synthesis, Discovered
	out2, err := SerializeFact(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "origin: discovered") {
		t.Errorf("discovered fact missing origin line:\n%s", out2)
	}
	back, err := ParseFact("kb/x/a.md", out2)
	if err != nil {
		t.Fatal(err)
	}
	if back.Origin != Discovered {
		t.Errorf("round-trip origin = %q, want discovered", back.Origin)
	}
}

// TestOriginTypeValidationIsSymmetric pins the round-trip guarantee for the
// origin axis: a fact SerializeFact accepts must parse back, and one it
// rejects must not be readable either. Before this was enforced, an
// origin/type mismatch serialized cleanly and then failed on read-back —
// producing a file the writer could create but nothing could load.
func TestOriginTypeValidationIsSymmetric(t *testing.T) {
	// Every pairing ValidateForType constrains, plus the legal controls.
	cases := []struct {
		name   string
		typ    Type
		origin Origin
		ok     bool
	}{
		{"distilled on synthesis", Synthesis, Distilled, true},
		{"distilled on observation", Observation, Distilled, false},
		{"distilled on hypothesis", Hypothesis, Distilled, false},
		{"discovered on synthesis", Synthesis, Discovered, true},
		{"discovered on hypothesis", Hypothesis, Discovered, true},
		{"discovered on observation", Observation, Discovered, false},
		{"authored on observation", Observation, Authored, true},
		{"authored on synthesis", Synthesis, Authored, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFact("kb/x/a.md")
			f.Title, f.Type, f.Origin = "T", tc.typ, tc.origin
			f.Domain, f.Entities, f.Refs = []string{"x"}, []string{}, []string{}
			f.Confidence, f.Sources, f.Body = 0.9, 1, "body"

			out, err := SerializeFact(f)
			if tc.ok && err != nil {
				t.Fatalf("SerializeFact = %v, want nil", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("SerializeFact = nil error, want rejection of %s/%s", tc.typ, tc.origin)
				}
				return
			}

			// The other half of symmetry: what serialized must parse back.
			parsed, err := ParseFact("kb/x/a.md", out)
			if err != nil {
				t.Fatalf("ParseFact after SerializeFact = %v, want nil", err)
			}
			// Parsing without error is not enough: the origin must survive
			// the trip *unchanged*. Asserting only NoError let authored +
			// synthesis pass while silently round-tripping to distilled,
			// because serialize elided the line as "just the default" and
			// parse resolved the missing line to distilled for synthesis.
			// A human-authored synthesis fact was permanently reattributed
			// to the distill pipeline. Compare the value, not just the error.
			if parsed.Origin != tc.origin {
				t.Fatalf("round-tripped Origin = %q, want %q (serialized form:\n%s)",
					parsed.Origin, tc.origin, out)
			}
		})
	}
}

// TestParseFact_RejectsOriginTypeMismatch covers the read side directly: a
// hand-edited file carrying an illegal pairing must be rejected on parse,
// not silently loaded. Without this, SerializeFact and ParseFact disagree
// about which files are valid.
func TestParseFact_RejectsOriginTypeMismatch(t *testing.T) {
	bad := "---\ntype: observation\norigin: distilled\ndomain: [x]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# T\n\nbody"
	if _, err := ParseFact("kb/x/bad.md", bad); err == nil {
		t.Error("ParseFact with distilled/observation = nil error, want error")
	}
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
