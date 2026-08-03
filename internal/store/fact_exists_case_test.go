package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// newCaseFixture builds a fresh service+branch holding one fact at a known
// lowercase path.
func newCaseFixture(t *testing.T) (*Service, string, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	const path = "kb/decisions/x/abc.md"
	_, err = svc.Facts().WriteFact(
		context.Background(), "main", path,
		testFactBody(path, 0.5, nil), "init "+path, "",
	)
	require.NoError(t, err)
	return svc, "main", path
}

// Fact paths are lowercase-canonical: NewFact lowercases unconditionally and
// NormalizePath lowercases everything after the ontology root. Every sibling
// read already honours that — ReadFact and ListDir lowercase, and
// treeFileInsensitive walks case-insensitively — so the existence checks must
// too, or knomit_update and knomit_retract report "does not exist" for a fact
// that is sitting right there.
func TestFactExists_CaseInsensitive(t *testing.T) {
	ctx := context.Background()
	svc, branch, _ := newCaseFixture(t)

	for _, p := range []string{
		"kb/decisions/x/abc.md",
		"kb/Decisions/X/Abc.md",
		"KB/DECISIONS/X/ABC.MD",
	} {
		ok, err := svc.Facts().FactExists(ctx, branch, p)
		require.NoErrorf(t, err, "FactExists(%q)", p)
		require.Truef(t, ok, "FactExists(%q) = false, want true", p)
	}

	// A genuinely absent path must still report false — the fix must not
	// degrade into "everything exists".
	ok, err := svc.Facts().FactExists(ctx, branch, "kb/decisions/x/nope.md")
	require.NoError(t, err)
	require.False(t, ok, "absent path reported as existing")
}

func TestFactExistsAt_CaseInsensitive(t *testing.T) {
	ctx := context.Background()
	svc, branch, _ := newCaseFixture(t)

	head, err := svc.Facts().(*factIndex).rh.resolveRef(ctx, branch)
	require.NoError(t, err)

	casings := []string{
		"kb/decisions/x/abc.md",
		"kb/Decisions/X/Abc.md",
		"KB/DECISIONS/X/ABC.MD",
	}

	// HEAD anchor (commit == "") reads branch_facts; branch_facts.path is plain
	// TEXT with no COLLATE NOCASE, so `WHERE path = ?` is case-sensitive.
	for _, p := range casings {
		ok, err := svc.FactQuery().FactExistsAt(ctx, branch, p, "")
		require.NoErrorf(t, err, "FactExistsAt(%q, HEAD)", p)
		require.Truef(t, ok, "FactExistsAt(%q, HEAD) = false, want true", p)
	}

	// Pinned-commit anchor takes a different code path.
	for _, p := range casings {
		ok, err := svc.FactQuery().FactExistsAt(ctx, branch, p, head.String())
		require.NoErrorf(t, err, "FactExistsAt(%q, %s)", p, head)
		require.Truef(t, ok, "FactExistsAt(%q, pinned) = false, want true", p)
	}

	ok, err := svc.FactQuery().FactExistsAt(ctx, branch, "kb/decisions/x/nope.md", "")
	require.NoError(t, err)
	require.False(t, ok, "absent path reported as existing")
}
