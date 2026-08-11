package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// indexedPaths returns every path the index holds for a branch.
func indexedPaths(t *testing.T, svc *Service, branch string) []string {
	t.Helper()
	rows, err := svc.rh.db.QueryContext(context.Background(),
		`SELECT bf.path FROM branch_facts bf
		 JOIN branches b ON b.id = bf.branch_id
		 WHERE b.name = ? ORDER BY bf.path`, branch)
	require.NoError(t, err)
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		paths = append(paths, p)
	}
	require.NoError(t, rows.Err())
	return paths
}

// A .md file OUTSIDE the ontology root is not a fact, however well-formed its
// contents happen to be. README.md is the live case: it is the repo's root
// manifest and its content is user-editable through PATCH /repos/{repo}, so
// "does this parse as a fact" is an attacker-/user-controlled question and
// cannot be what decides index membership.
//
// Verify already encodes the rule this test pins (checkFactsCoherence builds
// its expected set from kb/ only), so an indexed stray is not merely noise —
// it is reported as a "ghost" branch_facts row on the next integrity run.
func TestIndex_SkipsFilesOutsideOntologyRoot(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))
	ctx := context.Background()

	// Frontmatter + an H1 — the ordinary shape of a markdown document, and
	// enough for fact.ParseFact to accept it. Written via WriteRootFile (not
	// WriteFact) so the path lands as README.md rather than being lowercased to
	// readme.md — the case a git provider actually looks for, and the same case
	// this test needs to prove location (not case) decides index membership.
	const manifest = "---\ntitle: Core manifest\n---\n\n# Core\n\nGuidance for agents.\n"
	_, err = svc.Facts().WriteRootFile(ctx, "agent/a", "README.md", manifest, "docs: manifest", "update")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "agent/a", "kb/obs/real.md", testFactBody("Real", 0.9, nil), "add", "")
	require.NoError(t, err)

	require.Equal(t, []string{"kb/obs/real.md"}, indexedPaths(t, svc, "agent/a"),
		"only files under the ontology root belong in the index")

	// A rebuild must reach the same set — and must EVICT a stray that an
	// earlier version of the indexer admitted, not merely stop adding new ones.
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "agent/a", nil))
	require.Equal(t, []string{"kb/obs/real.md"}, indexedPaths(t, svc, "agent/a"),
		"rebuild must not re-admit files outside the ontology root")

	// The whole point: the tree and the index agree, so Verify is clean.
	report, err := svc.Verify(ctx, VerifyOpts{Deep: true})
	require.NoError(t, err)
	for _, iss := range report.Issues {
		require.NotEqual(t, CategoryFactsCoherence, iss.Category,
			"stray index row would surface here as a ghost: %+v", iss)
	}
}

// Deleting a stray still evicts it through the INCREMENTAL sync path. The scope
// filter guards what goes IN; extending it to deletions would strand rows an
// older build (or a previous ontology root) had already admitted, since a
// filtered delete never reaches si.delete.
func TestIndex_DeleteEvictsPathOutsideOntologyRoot(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))
	ctx := context.Background()

	// A file outside the root, in the tree but (correctly) not indexed...
	_, err = svc.Facts().WriteFact(ctx, "agent/a", "stray.md", testFactBody("Stray", 0.9, nil), "add", "")
	require.NoError(t, err)
	// ...and the legacy state we have to be able to repair: a branch_facts row
	// for it, left behind by a build that indexed on parseability alone. The
	// fact_id is borrowed from a real fact — only the stray PATH matters here,
	// and the FK has to point somewhere.
	_, err = svc.Facts().WriteFact(ctx, "agent/a", "kb/obs/real.md", testFactBody("Real", 0.9, nil), "add", "")
	require.NoError(t, err)
	_, err = svc.rh.db.ExecContext(ctx,
		`INSERT INTO branch_facts(branch_id, path, fact_id, commit_hash)
		 SELECT b.id, 'stray.md', f.id, ''
		 FROM branches b, facts f WHERE b.name = ? AND f.path = 'kb/obs/real.md'`, "agent/a")
	require.NoError(t, err)
	require.Contains(t, indexedPaths(t, svc, "agent/a"), "stray.md", "precondition: legacy row present")

	// Removing the file drives the incremental sync's delete branch.
	_, err = svc.Facts().DeleteFact(ctx, "agent/a", "stray.md", "remove stray")
	require.NoError(t, err)
	require.NotContains(t, indexedPaths(t, svc, "agent/a"), "stray.md",
		"a delete outside the ontology root must still evict the row")
}

// The root is the CONFIGURED one, not the literal "kb": a deployment with
// ontology_root = "knowledge" must index that tree and skip kb/.
func TestIndex_HonoursConfiguredOntologyRoot(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetOntologyRoot("knowledge")
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))
	ctx := context.Background()

	_, err = svc.Facts().WriteFact(ctx, "agent/a", "knowledge/obs/a.md", testFactBody("A", 0.9, nil), "add", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "agent/a", "kb/obs/b.md", testFactBody("B", 0.9, nil), "add", "")
	require.NoError(t, err)

	require.Equal(t, []string{"knowledge/obs/a.md"}, indexedPaths(t, svc, "agent/a"))
}

// A dot-directory UNDER the ontology root is a private stash. The file below
// parses perfectly as a fact — that is the point: parsing is not what decides
// membership, location is, and a dot-prefixed segment puts it out of scope.
//
// Verify must agree, or the stash surfaces as a ghost branch_facts row on the
// next integrity run — the same failure mode the ontology-root rule fixed.
func TestIndex_SkipsPrivatePathsUnderOntologyRoot(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))
	ctx := context.Background()

	_, err = svc.Facts().WriteFact(ctx, "agent/a", "kb/.drafts/draft.md",
		testFactBody("Draft", 0.9, nil), "add", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "agent/a", "kb/obs/real.md",
		testFactBody("Real", 0.9, nil), "add", "")
	require.NoError(t, err)

	require.Equal(t, []string{"kb/obs/real.md"}, indexedPaths(t, svc, "agent/a"),
		"a dot-prefixed segment is private, however well-formed the file")

	// A rebuild must reach the same set — and must EVICT a private path an
	// earlier build admitted, not merely stop adding new ones.
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "agent/a", nil))
	require.Equal(t, []string{"kb/obs/real.md"}, indexedPaths(t, svc, "agent/a"),
		"rebuild must not re-admit a private path")

	report, err := svc.Verify(ctx, VerifyOpts{Deep: true})
	require.NoError(t, err)
	for _, iss := range report.Issues {
		require.NotEqual(t, CategoryFactsCoherence, iss.Category,
			"a private path must be invisible to Verify, not a ghost: %+v", iss)
	}
}
