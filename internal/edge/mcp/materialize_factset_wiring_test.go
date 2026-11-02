package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
)

// knomit_materialize's first call embeds the query once for the whole fan-out
// (fanoutQueryVec) on a lens of ANY size (materialize.LensFactSet), a
// one-member lens included, and never on a repository. White-box: with one
// mount, a one-member lens embeds once either way and its pages cannot tell
// the wirings apart.
//
// Sabotage: key the lens embedder on b.IsLens() (the one-member lens gets no
// Embed).
func TestFactSetSearchWiring_OneMemberLensGetsTheLensEmbedder(t *testing.T) {
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "w", UID: "uid-w", AgentBranch: "agent/test"})
	solo := repos.NewLensBindingForTest("solo", "uid-solo", ri, repos.ReadTarget{RI: ri, Branch: "agent/test"})
	require.False(t, solo.IsLens())
	require.True(t, solo.FromLens())

	lens := materialize.FactSetRequest{Binding: solo}
	factsetSearchWiring(&lens, nil)
	require.NotNil(t, lens.Embed, "a one-member lens embeds once per fan-out")
	require.NotNil(t, lens.Clusters)

	repo := materialize.FactSetRequest{Binding: repos.NewBindingOfRepo(ri, "agent/test")}
	factsetSearchWiring(&repo, nil)
	require.Nil(t, repo.Embed, "control: a repository leaves embedding to its store")
}
