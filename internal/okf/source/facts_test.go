package source

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The exporter must skip private paths — AND must not report them as
// unparseable. looksLikeFact would otherwise flag every hand-placed draft as
// lost knowledge on every single export: a permanent false alarm that trains
// the reader to ignore a real one.
//
// "kb/.drafts/dddddddd.md" is the case that actually pins the ORDER of the
// guard relative to looksLikeFact: it opens with "---\n" (so looksLikeFact
// would say yes) but is missing its closing frontmatter delimiter, so
// fact.ParseFact fails on it. Every other fixture here parses cleanly and
// therefore never reaches looksLikeFact at all regardless of where the
// private-path skip sits — this one is what makes a wrong placement (skip
// applied only after the looksLikeFact check, or not applied to the error
// branch) visible as a non-empty snap.Warnings.
func TestLoad_SkipsPrivatePathsWithoutWarning(t *testing.T) {
	r := newFixtureRepo(t)
	h := commitFiles(t, r, "seed", "a+learn@agents.knomit.io", map[string]string{
		"kb/decisions/x/aaaaaaaa.md":      factBody("Alpha", 0.9),
		"kb/.drafts/bbbbbbbb.md":          factBody("Draft", 0.9),
		"kb/decisions/x/.wip/cccccccc.md": factBody("Wip", 0.9),
		"kb/.drafts/dddddddd.md":          "---\nbroken: yes\n",
	})

	snap, err := Load(r.Storer, h)
	require.NoError(t, err)
	require.Len(t, snap.Facts, 1)
	require.Equal(t, "kb/decisions/x/aaaaaaaa.md", snap.Facts[0].Fact.Path())
	require.Empty(t, snap.Warnings,
		"a private path is not lost knowledge and must not warn, even one shaped enough to trip looksLikeFact")
}
