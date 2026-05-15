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
