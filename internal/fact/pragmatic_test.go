package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseFact_Pragmatic_Policy(t *testing.T) {
	const content = `---
kind: pragmatic
type: policy
domain: [security]
confidence: 0.9
sources: 0
entities: []
refs: []
---
# Always rotate secrets quarterly

Body.
`
	f, err := ParseFact("kb/p.md", content)
	require.NoError(t, err)
	require.Equal(t, Pragmatic, f.Kind)
	require.Equal(t, Policy, f.Type)
	require.Equal(t, "Always rotate secrets quarterly", f.Title)
}

func TestParseFact_Pragmatic_Heuristic(t *testing.T) {
	const content = `---
kind: pragmatic
type: heuristic
domain: [code-review]
confidence: 0.7
sources: 0
entities: []
refs: []
---
# Prefer small PRs

Body.
`
	f, err := ParseFact("kb/h.md", content)
	require.NoError(t, err)
	require.Equal(t, Pragmatic, f.Kind)
	require.Equal(t, Heuristic, f.Type)
}

func TestParseFact_MissingKindDefaultsEpistemic(t *testing.T) {
	const content = `---
type: observation
domain: []
confidence: 0.8
sources: 0
entities: []
refs: []
---
# An existing fact

Body.
`
	f, err := ParseFact("kb/e.md", content)
	require.NoError(t, err)
	require.Equal(t, Epistemic, f.Kind,
		"missing kind must default to epistemic for backward compatibility")
	require.Equal(t, Observation, f.Type)
}

func TestParseFact_RejectsCrossKindMismatch(t *testing.T) {
	const content = `---
kind: pragmatic
type: observation
domain: []
confidence: 0.5
sources: 0
entities: []
refs: []
---
# Bad

Body.
`
	_, err := ParseFact("kb/bad.md", content)
	require.Error(t, err)
	require.Contains(t, err.Error(), "type")
	require.Contains(t, err.Error(), "pragmatic")
}

func TestParseFact_RejectsUnknownKind(t *testing.T) {
	const content = `---
kind: speculative
type: observation
domain: []
confidence: 0.5
sources: 0
entities: []
refs: []
---
# Bad

Body.
`
	_, err := ParseFact("kb/bad2.md", content)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid kind")
}

func TestParseFact_RejectsPragmaticWithoutType(t *testing.T) {
	// Pragmatic facts require an explicit type — policy and heuristic
	// are not interchangeable, so there is no safe default.
	const content = `---
kind: pragmatic
domain: []
confidence: 0.5
sources: 0
entities: []
refs: []
---
# Bad

Body.
`
	_, err := ParseFact("kb/bad3.md", content)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pragmatic")
}

func TestSerializeFact_Pragmatic_EmitsKind(t *testing.T) {
	f := NewFact("kb/p.md")
	f.Title = "Always rotate secrets quarterly"
	f.Body = ""
	f.Kind = Pragmatic
	f.Type = Policy
	f.Domain = []string{"security"}
	f.Confidence = 0.9
	f.Entities = []string{}
	f.Refs = []string{}

	out, err := SerializeFact(f)
	require.NoError(t, err)
	require.Contains(t, out, "kind: pragmatic")
	require.Contains(t, out, "type: policy")
}

func TestSerializeFact_Epistemic_OmitsKind(t *testing.T) {
	f := NewFact("kb/e.md")
	f.Title = "Login latency spike"
	f.Body = ""
	f.Kind = Epistemic
	f.Type = Observation
	f.Domain = []string{}
	f.Entities = []string{}
	f.Refs = []string{}

	out, err := SerializeFact(f)
	require.NoError(t, err)
	require.NotContains(t, out, "kind:",
		"epistemic facts must serialize without a kind field for round-trip fidelity with existing files")
}

func TestSerializeFact_DefaultKindOmitsKind(t *testing.T) {
	// Fact constructed without explicitly setting Kind. Treated as epistemic.
	f := NewFact("kb/e.md")
	f.Title = "Login latency spike"
	f.Type = Observation
	f.Domain = []string{}
	f.Entities = []string{}
	f.Refs = []string{}

	out, err := SerializeFact(f)
	require.NoError(t, err)
	require.NotContains(t, out, "kind:")
}

func TestSerializeFact_RejectsCrossKindMismatch(t *testing.T) {
	f := NewFact("kb/bad.md")
	f.Title = "Bad"
	f.Kind = Pragmatic
	f.Type = Observation
	f.Domain = []string{}
	f.Entities = []string{}
	f.Refs = []string{}

	_, err := SerializeFact(f)
	require.Error(t, err)
	require.Contains(t, err.Error(), "type")
}

func TestSerializeFact_RejectsEmptyType(t *testing.T) {
	f := NewFact("kb/bad.md")
	f.Title = "Bad"
	f.Kind = Epistemic
	f.Type = ""
	f.Domain = []string{}
	f.Entities = []string{}
	f.Refs = []string{}

	_, err := SerializeFact(f)
	require.Error(t, err)
}

func TestSerializeFact_RoundTrip_Pragmatic(t *testing.T) {
	f := NewFact("kb/p.md")
	f.Title = "Always rotate secrets quarterly"
	f.Body = "Body content."
	f.Kind = Pragmatic
	f.Type = Policy
	f.Domain = []string{"security"}
	f.Confidence = 0.9
	f.Entities = []string{}
	f.Refs = []string{}

	out, err := SerializeFact(f)
	require.NoError(t, err)

	parsed, err := ParseFact("kb/p.md", out)
	require.NoError(t, err)
	require.Equal(t, Pragmatic, parsed.Kind)
	require.Equal(t, Policy, parsed.Type)
	require.Equal(t, f.Title, parsed.Title)
	require.Equal(t, f.Body, parsed.Body)
	require.Equal(t, f.Domain, parsed.Domain)
	require.Equal(t, f.Confidence, parsed.Confidence)
}
