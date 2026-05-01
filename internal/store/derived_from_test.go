package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// TestResolveTargetCommit covers the three outcomes from the design spec
// write-path step 2: ref to an existing path, ref to a never-created path,
// and ref to a deleted path.
func TestResolveTargetCommit(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// Seed kb/e.md added at c1.
	c1Res, err := svc.Facts().WriteFact(ctx, branch, "kb/e.md", testFactBody("e", 0.9, nil), "init e", "")
	require.NoError(t, err)
	c1 := c1Res.CommitHash

	// kb/d.md at c2 ref's kb/e.md.
	c2Res, err := svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d", 0.8, []string{"kb/e.md"}), "init d", "")
	require.NoError(t, err)
	c2 := c2Res.CommitHash

	si := svc.Search().(*searchIndex)

	// (1) D's ref to E at c2: most recent ancestor touching E is c1 (added) → target_commit = c1.
	got, ok, err := si.resolveTargetCommit(ctx, branch, "kb/e.md", c2)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, c1, got)

	// (2) Forward-broken: no ancestor touches kb/z.md.
	_, ok, err = si.resolveTargetCommit(ctx, branch, "kb/z.md", c2)
	require.NoError(t, err)
	require.False(t, ok, "no ancestor touches kb/z.md → not ok")

	// (3) Tombstoned: delete kb/e.md, then a later source ref's it.
	c3, err := svc.Facts().DeleteFact(ctx, branch, "kb/e.md", "retract e")
	require.NoError(t, err)

	c4Res, err := svc.Facts().WriteFact(ctx, branch, "kb/f.md", testFactBody("f", 0.5, []string{"kb/e.md"}), "init f", "")
	require.NoError(t, err)
	c4 := c4Res.CommitHash
	require.NotEmpty(t, c3)

	// F's ref to E at c4: first ancestor touching E is c3 (deleted) → not ok.
	_, ok, err = si.resolveTargetCommit(ctx, branch, "kb/e.md", c4)
	require.NoError(t, err)
	require.False(t, ok, "first ancestor touching kb/e.md is a deletion → not ok")
}

// testFactBody builds a minimal markdown fact for store-internal tests.
// Uses the existing fact.SerializeFact helper so the format matches what
// the parser expects, avoiding hand-rolled YAML drift.
func testFactBody(title string, conf float64, refs []string) string {
	f := fact.NewFact("placeholder.md")
	f.Title = title
	f.Confidence = conf
	f.Sources = 1
	f.Domain = []string{"test"}
	f.Refs = refs
	return fact.SerializeFact(f)
}
