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

// TestAllPragmaticTypes_MatchesSet mirrors TestAllEpistemicTypes_MatchesSet
// for the pragmatic axis, which was previously unpinned in either direction.
//
// The stakes rose when the tool schemas started building their `type` enum
// from the ordered slices. PragmaticTypes is what Kind.AllowsType consults,
// so adding a third pragmatic type there without extending AllPragmaticTypes
// would ship a protocol enum that REJECTS a type the server itself accepts —
// strictly worse than the prose-only `type` field the enum replaced, because
// the caller is now blocked at the boundary with no way to reach the
// validation that would have allowed it.
//
// ElementsMatch rather than a length check plus membership: the latter is
// satisfied by a duplicate (a slice of {policy, policy} has the right length
// and every element is in the set), which would silently drop a real type
// from the enum.
func TestAllPragmaticTypes_MatchesSet(t *testing.T) {
	setKeys := make([]Type, 0, len(PragmaticTypes))
	for ty := range PragmaticTypes {
		setKeys = append(setKeys, ty)
	}
	require.ElementsMatch(t, setKeys, AllPragmaticTypes(),
		"AllPragmaticTypes() and PragmaticTypes must stay in sync — a type in the set but "+
			"not the slice is accepted by Kind.AllowsType yet rejected by the tool schema enum")
}
