package repos

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResolveOntology_NewOntologyRefusesMisplacedRootAttribute: creating a repo
// from a custom ontology is the one moment a misplaced repository-level
// attribute can still be fixed, so it is refused here, while the open path only
// warns (see fact.ParseNewOntology).
func TestResolveOntology_NewOntologyRefusesMisplacedRootAttribute(t *testing.T) {
	const misplaced = "id: x\nname: X\ntopics:\n  notes:\n    description: d\n    attributes:\n      verify_signatures: log\n"
	_, err := resolveOntology(CreateSpec{Mode: "custom", OntologyYAML: misplaced})
	require.ErrorContains(t, err, "repository-level")

	const good = "id: x\nname: X\nattributes:\n  verify_signatures: log\ntopics:\n  notes:\n    description: d\n"
	o, err := resolveOntology(CreateSpec{Mode: "custom", OntologyYAML: good})
	require.NoError(t, err)
	require.Equal(t, "log", o.Attributes["verify_signatures"])
}
