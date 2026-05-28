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
