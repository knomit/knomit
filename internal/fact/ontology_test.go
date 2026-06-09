package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func mustParseOntology(t *testing.T, y string) *Ontology {
	t.Helper()
	o, err := ParseOntology([]byte(y))
	require.NoError(t, err)
	return o
}

func TestIsSubsetOf_Trivial(t *testing.T) {
	const y = `
id: t
name: T
topics:
  invariants:
    description: x
  principles:
    description: y
`
	a := mustParseOntology(t, y)
	b := mustParseOntology(t, y)
	require.True(t, a.IsSubsetOf(b))
	require.True(t, b.IsSubsetOf(a))
}

func TestIsSubsetOf_PresetAddsTopics(t *testing.T) {
	stored := mustParseOntology(t, `
id: t
name: T
topics:
  invariants:
    description: x
`)
	preset := mustParseOntology(t, `
id: t
name: T
topics:
  invariants:
    description: x
  principles:
    description: y
`)
	require.True(t, stored.IsSubsetOf(preset), "stored (invariants only) should be subset of preset (invariants + principles)")
	require.False(t, preset.IsSubsetOf(stored), "preset has principles that stored does not — not a subset")
}

func TestIsSubsetOf_PresetAddsValidations(t *testing.T) {
	stored := mustParseOntology(t, `
id: t
name: T
topics:
  principles:
    description: y
    validations:
      - name: must-have-designer
        message: missing
        rule: "fact.entities.includes('designer')"
`)
	preset := mustParseOntology(t, `
id: t
name: T
topics:
  principles:
    description: y
    validations:
      - name: must-have-designer
        message: missing
        rule: "fact.entities.includes('designer')"
      - name: must-be-pragmatic-policy
        message: needs policy
        rule: "fact.kind === 'pragmatic' && fact.type === 'policy'"
      - name: domain-non-empty
        message: needs domain
        rule: "fact.domain.length > 0"
      - name: domain-mutually-exclusive
        message: domain mix
        rule: "!(fact.domain.includes('global') && fact.domain.length > 1)"
`)
	require.True(t, stored.IsSubsetOf(preset), "stored (1 rule) should be subset of preset (4 rules)")
	require.False(t, preset.IsSubsetOf(stored), "preset has rules that stored lacks — not a subset")
}

func TestIsSubsetOf_UserAddedTopic(t *testing.T) {
	stored := mustParseOntology(t, `
id: t
name: T
topics:
  invariants:
    description: x
  custom-topic:
    description: user-only
`)
	preset := mustParseOntology(t, `
id: t
name: T
topics:
  invariants:
    description: x
`)
	require.False(t, stored.IsSubsetOf(preset), "stored has a custom topic the preset lacks — not a subset")
}

func TestIsSubsetOf_UserAddedValidation(t *testing.T) {
	stored := mustParseOntology(t, `
id: t
name: T
topics:
  principles:
    description: y
    validations:
      - name: must-have-designer
        message: missing
        rule: "fact.entities.includes('designer')"
      - name: custom-user-rule
        message: user-only
        rule: "fact.confidence > 0.5"
`)
	preset := mustParseOntology(t, `
id: t
name: T
topics:
  principles:
    description: y
    validations:
      - name: must-have-designer
        message: missing
        rule: "fact.entities.includes('designer')"
`)
	require.False(t, stored.IsSubsetOf(preset), "stored has a custom validation the preset lacks — not a subset")
}

func TestIsSubsetOf_UserAddedChildTopic(t *testing.T) {
	stored := mustParseOntology(t, `
id: t
name: T
topics:
  invariants:
    description: x
    children:
      architecture:
        description: a
      custom-child:
        description: user-only
`)
	preset := mustParseOntology(t, `
id: t
name: T
topics:
  invariants:
    description: x
    children:
      architecture:
        description: a
`)
	require.False(t, stored.IsSubsetOf(preset), "stored has a custom child topic — not a subset")
}

func TestIsSubsetOf_RootValidationNotInPreset(t *testing.T) {
	stored := mustParseOntology(t, `
id: t
name: T
validations:
  - name: stored-root-rule
    message: x
    rule: "fact.kind === 'foo'"
topics:
  invariants:
    description: x
`)
	preset := mustParseOntology(t, `
id: t
name: T
topics:
  invariants:
    description: x
`)
	require.False(t, stored.IsSubsetOf(preset), "stored has a root validation preset lacks — not a subset")
}

func TestEmbeddedPresetByID(t *testing.T) {
	t.Run("source-code returns CodeOntology", func(t *testing.T) {
		o := EmbeddedPresetByID("source-code")
		require.NotNil(t, o)
		require.Equal(t, "source-code", o.ID)
		require.Same(t, CodeOntology(), o)
	})
	t.Run("general returns DefaultOntology", func(t *testing.T) {
		o := EmbeddedPresetByID("general")
		require.NotNil(t, o)
		require.Equal(t, "general", o.ID)
		require.Same(t, DefaultOntology(), o)
	})
	t.Run("unknown id returns nil", func(t *testing.T) {
		require.Nil(t, EmbeddedPresetByID("made-up-id"))
		require.Nil(t, EmbeddedPresetByID(""))
	})
}

func TestParseOntology_ValidationsParsed(t *testing.T) {
	const y = `
id: t
name: T
topics:
  principles:
    description: x
    validations:
      - name: must-have-designer
        message: missing designer
        rule: "fact.entities.includes('designer')"
`
	o, err := ParseOntology([]byte(y))
	require.NoError(t, err)
	node := o.Topics["principles"]
	require.NotNil(t, node)
	require.Len(t, node.Validations, 1)
	require.Equal(t, "must-have-designer", node.Validations[0].Name)
	require.Equal(t, "missing designer", node.Validations[0].Message)
	require.Equal(t, "fact.entities.includes('designer')", node.Validations[0].Rule)
}

// TestSerialize_RoundTripsValidations guards against Serialize silently
// dropping the validations: block. If anyone round-trips an ontology with
// rules through Serialize, the rules must survive at both root and node
// level — otherwise the validation gate goes dark on the next parse.
func TestSerialize_RoundTripsValidations(t *testing.T) {
	const y = `id: t
name: T
validations:
  - name: root-rule
    message: must hold
    rule: "fact.kind === 'pragmatic'"
topics:
  principles:
    description: x
    validations:
      - name: must-have-designer
        message: missing designer
        rule: "fact.entities.includes('designer')"
`
	o, err := ParseOntology([]byte(y))
	require.NoError(t, err)

	out, err := o.Serialize()
	require.NoError(t, err)

	// Round-trip: parse the serialized output and confirm rules persist.
	o2, err := ParseOntology(out)
	require.NoError(t, err)
	require.Len(t, o2.Validations, 1)
	require.Equal(t, "root-rule", o2.Validations[0].Name)
	require.Len(t, o2.Topics["principles"].Validations, 1)
	require.Equal(t, "must-have-designer", o2.Topics["principles"].Validations[0].Name)
}
