package resolutions

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	knomitfact "knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

func testRepo() *repos.RepoInstance {
	return repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "t", AgentBranch: "agent/test", Ontology: knomitfact.CodeOntology(), OntologyRoot: "kb",
	})
}

// F25: a resolution is a fact-tool write whatever its kind, and a dot path is
// closed to the fact tools. Every kind keyed on one is refused — {body},
// which authors content, and "ours"/"theirs", which still decide what lands
// there. The refusal comes before anything is read, so the same answer holds
// on the MCP door and the REST door (both call Normalize).
func TestNormalize_RefusesEveryResolutionOnADotPath(t *testing.T) {
	body := []byte("---\ntype: observation\n---\n# Guidance\n\nINJECTED\n")
	for _, p := range []string{".knomit/guidance/x.md", ".knomit/skills/s/extra.md", "kb/.drafts/x.md", ".github/x.md"} {
		for name, res := range map[string]store.Resolution{
			"body":   {Body: body},
			"ours":   {Side: store.ResolveSrc},
			"theirs": {Side: store.ResolveDst},
		} {
			_, err := Normalize(context.Background(), testRepo(), "agent/test", "exp/x",
				map[string]store.Resolution{p: res})
			require.Errorf(t, err, "%s resolution on %s", name, p)
			require.Containsf(t, err.Error(), "closed to the fact tools", "%s resolution on %s", name, p)
		}
	}
}

// An artifact body is not judged against the ontology (it has no placement),
// and a context on it is refused, as on every other write path.
func TestNormalize_ArtifactBodyRefusesContext(t *testing.T) {
	body := []byte("---\ntype: observation\ncontext: {task: t-1}\n---\n# State\n\nrun 2\n")
	_, err := Normalize(context.Background(), testRepo(), "agent/test", "exp/x",
		map[string]store.Resolution{"artifacts/runs/x.md": {Body: body}})
	require.ErrorIs(t, err, knomitfact.ErrContextWithoutOntology)
}
