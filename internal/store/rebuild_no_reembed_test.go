package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// TestRebuild_PreservesRowidsAndDoesNotReEmbed regresses the bug where Rebuild
// used INSERT OR REPLACE INTO facts (DELETE+INSERT → new rowid → the
// facts_after_delete trigger wiped facts_vec), so rebuildEmbeddings re-embedded
// the ENTIRE corpus on every rebuild. The upsert is now ON CONFLICT DO UPDATE,
// which preserves the rowid for unchanged blobs, keeping facts_vec alive.
//
// Asserts: after the first rebuild embeds every fact, a SECOND rebuild over the
// same content (a) preserves each fact's rowid and (b) issues zero further embed
// calls — while still regenerating the derived junction/token tables.
func TestRebuild_PreservesRowidsAndDoesNotReEmbed(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	emb := &countingEmbedder{}
	svc.SetEmbedder(emb)

	ctx := context.Background()
	branch := "main"

	mk := func(path, title string, domains []string) {
		f := fact.NewFact("placeholder.md")
		f.Title = title
		f.Confidence = 0.9
		f.Sources = 1
		f.Domain = domains
		f.Entities = []string{"x"}
		f.Type = fact.Observation
		out, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, branch, path, out, "init", "")
		require.NoError(t, err)
	}
	mk("kb/a.md", "Alpha", []string{"ai-governance"})
	mk("kb/b.md", "Beta", []string{"store/resolver"})

	si := svc.Search().(*searchIndex)
	rowids := func() map[string]int64 {
		m := map[string]int64{}
		rows, err := si.rh.db.QueryContext(ctx, `SELECT path, id FROM facts`)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var p string
			var id int64
			require.NoError(t, rows.Scan(&p, &id))
			m[p] = id
		}
		return m
	}
	vecCount := func() int {
		var n int
		require.NoError(t, si.rh.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM facts_vec`).Scan(&n))
		return n
	}

	// WriteFact already embedded each fact inline (via Embed), so facts_vec is
	// populated before the first rebuild.
	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))
	before := rowids()
	require.Len(t, before, 2)
	require.Equal(t, 2, vecCount(), "both facts must have embeddings after setup")

	// Second rebuild over identical content must not re-embed.
	emb.batchCalls.Store(0)
	emb.embedCalls.Store(0)
	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))
	after := rowids()

	require.Equal(t, before, after, "rebuild must preserve facts rowids for unchanged blobs")
	require.Equal(t, 2, vecCount(), "embeddings must survive the rebuild (facts_vec not wiped)")
	require.Zero(t, emb.batchCalls.Load(), "rebuild must NOT re-embed unchanged facts (facts_vec preserved)")
	require.Zero(t, emb.embedCalls.Load(), "rebuild must NOT re-embed unchanged facts (facts_vec preserved)")

	// Derived state is still correct: canonical domains + tokens regenerated.
	res, err := svc.Search().Search(ctx, branch, SearchOptions{Domain: []string{"governance"}})
	require.NoError(t, err)
	found := false
	for _, r := range res {
		if r.Path == "kb/a.md" {
			found = true
		}
	}
	require.True(t, found, "domain token search must still work after the no-re-embed rebuild")
}

// TestNeedsRebuild_TrueWhenVersionStale regresses the silent-upgrade bug: an
// existing DB whose persisted graph_schema_version predates the canonical-domain
// change must report NeedsRebuild=true (so startup heals it), and a freshly
// rebuilt DB must report false.
func TestNeedsRebuild_TrueWhenVersionStale(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	si := svc.Search().(*searchIndex)

	// Simulate an older deployment: stamp a prior schema version.
	_, err = si.rh.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key, value) VALUES ('graph_schema_version', '2')`)
	require.NoError(t, err)

	stale, err := svc.IndexManager().NeedsRebuild(ctx)
	require.NoError(t, err)
	require.True(t, stale, "a pre-canonical-domain version must be reported stale")

	// A successful rebuild bumps the version to current → no longer stale.
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	stale, err = svc.IndexManager().NeedsRebuild(ctx)
	require.NoError(t, err)
	require.False(t, stale, "after Rebuild the persisted version is current")
}

// TestMarkRebuildNeeded_RearmsStaleAfterBump regresses PR #70 review finding #1:
// Rebuild bumps the GLOBAL schema version on every branch it completes, so a
// later branch's rebuild failure would be masked (version reads current → the
// next startup skips the heal). MarkRebuildNeeded must clear the version so a
// partially-failed heal is retried on the next startup.
func TestMarkRebuildNeeded_RearmsStaleAfterBump(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()

	// A successful rebuild bumps the version to current.
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	stale, err := svc.IndexManager().NeedsRebuild(ctx)
	require.NoError(t, err)
	require.False(t, stale, "precondition: rebuilt DB is current")

	// Re-marking (as the heal loop does after a partial failure) makes the next
	// NeedsRebuild report stale again so every branch is retried.
	require.NoError(t, svc.IndexManager().MarkRebuildNeeded(ctx))
	stale, err = svc.IndexManager().NeedsRebuild(ctx)
	require.NoError(t, err)
	require.True(t, stale, "after MarkRebuildNeeded the heal must be retried")
}
