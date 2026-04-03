package repos

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoadOntology_fallsBackToDefault verifies that a repo with no
// domains/ontology.yaml in git still gets a non-nil default ontology.
func TestLoadOntology_fallsBackToDefault(t *testing.T) {
	b := &repoBuilder{
		name:        "test",
		agentBranch: "machine/test",
	}
	// svc is nil; loadOntology must not panic and must return the default ontology.
	b.loadOntology()
	require.NotNil(t, b.ontology)
}
