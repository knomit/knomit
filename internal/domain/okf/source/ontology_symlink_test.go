package source

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/stretchr/testify/require"
)

// A symlinked ontology is never followed and its link text never parsed (user
// ruling 2026-10-08): the bundle degrades to the embedded default, as for any
// unusable ontology, and says why with the path named. The link text here is
// itself a valid ontology, so a reader that parsed it would pass silently.
// Sabotage: drop the symlink clause in okfOntologyDoc → no warning and the
// crafted name → red.
func TestOntology_SymlinkIsNotFollowedAndIsWarned(t *testing.T) {
	r := newFixtureRepo(t)
	wt, err := r.Worktree()
	require.NoError(t, err)
	require.NoError(t, wt.Filesystem.MkdirAll(".knomit", 0o755))
	require.NoError(t, wt.Filesystem.Symlink(testOntologyYAML, ".knomit/ontology.yaml"))
	_, err = wt.Add(".knomit/ontology.yaml")
	require.NoError(t, err)
	h := commitFiles(t, r, "seed", "a+learn@agents.knomit.io", map[string]string{
		"kb/decisions/x/aaaaaaaa.md": factBody("Alpha", 0.9),
	})
	c, err := r.CommitObject(h)
	require.NoError(t, err)
	f, err := c.File(".knomit/ontology.yaml")
	require.NoError(t, err)
	require.Equal(t, filemode.Symlink, f.Mode, "the fixture commits a real symlink entry")

	snap, err := Load(r.Storer, h)
	require.NoError(t, err)
	require.NotEqual(t, "Source Code Knowledge", snap.Ontology.Name, "the link text is not the ontology")
	require.Len(t, snap.Warnings, 1)
	require.Contains(t, snap.Warnings[0], ".knomit/ontology.yaml is a symlink")
	require.Contains(t, snap.Warnings[0], "does not follow symlinks")
}
