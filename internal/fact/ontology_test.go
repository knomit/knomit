package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
