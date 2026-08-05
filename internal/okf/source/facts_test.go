package source

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The exporter must skip private paths — AND must not report them as
// unparseable. looksLikeFact would otherwise flag every hand-placed draft as
// lost knowledge on every single export: a permanent false alarm that trains
// the reader to ignore a real one.
func TestLoad_SkipsPrivatePathsWithoutWarning(t *testing.T) {
	r := newFixtureRepo(t)
	h := commitFiles(t, r, "seed", "a+learn@agents.knomit.io", map[string]string{
		"kb/decisions/x/aaaaaaaa.md":      factBody("Alpha", 0.9),
		"kb/.drafts/bbbbbbbb.md":          factBody("Draft", 0.9),
		"kb/decisions/x/.wip/cccccccc.md": factBody("Wip", 0.9),
	})

	snap, err := Load(r.Storer, h)
	require.NoError(t, err)
	require.Len(t, snap.Facts, 1)
	require.Equal(t, "kb/decisions/x/aaaaaaaa.md", snap.Facts[0].Fact.Path())
	require.Empty(t, snap.Warnings,
		"a private path is not lost knowledge and must not warn")
}
