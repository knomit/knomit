package kb

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
)

// The REST first call wires the lens list's embedder and motif cluster reader
// on a lens of ANY size (materialize.LensFactSet), a one-member lens included,
// and never on a repository. White-box: with one mount, a one-member lens
// embeds once either way and its pages cannot tell the wirings apart.
//
// Sabotage: key the lens wiring on b.IsLens() (the one-member lens gets
// neither Embed nor Clusters, since emb is nil).
func TestFactSetRequest_OneMemberLensGetsTheLensWiring(t *testing.T) {
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "w", UID: "uid-w", AgentBranch: "agent/test"})
	solo := repos.NewLensBindingForTest("solo", "uid-solo", ri, repos.ReadTarget{RI: ri, Branch: "agent/test"})
	require.False(t, solo.IsLens())
	require.True(t, solo.FromLens())
	repo := repos.NewBindingOfRepo(ri, "agent/test")

	build := func(sf factsetSurface, b *repos.Binding) (bool, bool) {
		t.Helper()
		rec := httptest.NewRecorder()
		req, ok := factsetRequest(rec, httptest.NewRequest("GET", "/x?q=a", nil), url.Values{"q": {"a"}}, sf, b, nil, nil)
		require.True(t, ok, rec.Body.String())
		return req.Embed != nil, req.Clusters != nil
	}
	embed, clusters := build(lensFactSets, solo)
	require.True(t, embed, "a one-member lens embeds once per fan-out (lensEmbed)")
	require.True(t, clusters, "a one-member lens widens motifs over its read set (lensMotifClusters)")
	embed, clusters = build(repoFactSets, repo)
	require.False(t, embed, "control: a repository with no embedder leaves embedding to its store")
	require.False(t, clusters, "control: a repository reads no cluster reader")
}
