package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

func writeContextFact(t *testing.T, svc *Service, branch, path string, ctx map[string]any) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title, f.Body, f.Type = "T "+path, "Body of "+path, fact.Observation
	f.Domain, f.Entities, f.Refs = []string{"verdicts"}, []string{"Verdict"}, []string{}
	f.Confidence, f.Sources = 0.8, 1
	f.Context = ctx
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

// verdictsFixture: three verdicts for t-17 (two disagree), one for t-18, and
// an unrelated fact elsewhere carrying the same task.
func verdictsFixture(t *testing.T) (*Service, string) {
	svc, branch := motifEnv(t)
	writeContextFact(t, svc, branch, "kb/verdicts/a.md", map[string]any{"task": "t-17", "verdict": "disagree", "score": 0.5, "final": true})
	writeContextFact(t, svc, branch, "kb/verdicts/b.md", map[string]any{"task": "t-17", "verdict": "disagree"})
	writeContextFact(t, svc, branch, "kb/verdicts/c.md", map[string]any{"task": "t-17", "verdict": "agree"})
	writeContextFact(t, svc, branch, "kb/verdicts/d.md", map[string]any{"task": "t-18", "verdict": "disagree"})
	writeContextFact(t, svc, branch, "kb/other/e.md", map[string]any{"task": "t-17"})
	writeContextFact(t, svc, branch, "kb/verdicts/none.md", nil)
	return svc, branch
}

func ctxQ(path string, kv ...string) SearchOptions {
	q := SearchOptions{Path: path, Limit: 100, Context: map[string]string{}}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Context[kv[i]] = kv[i+1]
	}
	return q
}

// The filter: exact match on canonical text, AND across keys, combined with
// path. Numbers and booleans match by their canonical text.
//
// SABOTAGE: drop the EXISTS clause → every path comes back → red.
func TestFactContext_Filter(t *testing.T) {
	svc, branch := verdictsFixture(t)
	require.Equal(t, []string{"kb/verdicts/a.md", "kb/verdicts/b.md", "kb/verdicts/c.md"},
		searchPaths(t, svc, branch, ctxQ("kb/verdicts", "task", "t-17")))
	require.Equal(t, []string{"kb/verdicts/a.md", "kb/verdicts/b.md"},
		searchPaths(t, svc, branch, ctxQ("kb/verdicts", "task", "t-17", "verdict", "disagree")))
	require.Equal(t, []string{"kb/other/e.md", "kb/verdicts/a.md", "kb/verdicts/b.md", "kb/verdicts/c.md"},
		searchPaths(t, svc, branch, ctxQ("", "task", "t-17")), "no path: every topic")
	require.Equal(t, []string{"kb/verdicts/a.md"}, searchPaths(t, svc, branch, ctxQ("kb/", "score", "0.5")))
	require.Equal(t, []string{"kb/verdicts/a.md"}, searchPaths(t, svc, branch, ctxQ("kb/", "final", "true")))
	require.Empty(t, searchPaths(t, svc, branch, ctxQ("kb/", "task", "t-1")), "exact, not prefix")

	// RecentFacts (sort=recent) applies the same filter and carries the map.
	q := ctxQ("kb/verdicts", "task", "t-17", "verdict", "disagree")
	entries, total, err := svc.fq.RecentFacts(context.Background(), branch, q)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	for _, e := range entries {
		require.Equal(t, "t-17", e.Context["task"], e.Path)
	}

	// Results carry the typed map, read from the blob.
	got, err := svc.fq.GetByPath(context.Background(), branch, "kb/verdicts/a.md")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"task": "t-17", "verdict": "disagree", "score": 0.5, "final": true}, got.Context)
	res, err := svc.fq.Search(context.Background(), branch, ctxQ("kb/verdicts", "task", "t-18"))
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.Equal(t, "disagree", res[0].Context["verdict"])
}

// Liveness: the side table keeps rows of superseded and retracted versions,
// so the filter must only ever see the branch's LIVE row. A retracted verdict
// is not returned; an updated one is returned once, with its new value.
func TestFactContext_FilterSeesOnlyLiveVersions(t *testing.T) {
	svc, branch := verdictsFixture(t)
	ctx := context.Background()
	_, err := svc.Facts().DeleteFact(ctx, branch, "kb/verdicts/b.md", "retract")
	require.NoError(t, err)
	writeContextFact(t, svc, branch, "kb/verdicts/a.md", map[string]any{"task": "t-17", "verdict": "agree"})

	require.Equal(t, []string{"kb/verdicts/a.md", "kb/verdicts/c.md"},
		searchPaths(t, svc, branch, ctxQ("kb/verdicts", "task", "t-17", "verdict", "agree")),
		"the updated verdict appears once, with its new value")
	require.Empty(t, searchPaths(t, svc, branch, ctxQ("kb/verdicts", "task", "t-17", "verdict", "disagree")),
		"neither the retracted b nor a's superseded version")
	var rows int
	require.NoError(t, svc.si.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM fact_context WHERE key = 'verdict' AND value = 'disagree'`).Scan(&rows))
	require.GreaterOrEqual(t, rows, 2, "fixture: the stale rows DO stay in the table; only the join hides them")
}

func contextRowCount(t *testing.T, svc *Service) int {
	t.Helper()
	var n int
	require.NoError(t, svc.si.rh.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM fact_context`).Scan(&n))
	return n
}

// The rebuild fills the side table from the blobs (not only the incremental
// upsert), and the v6 → v7 bump forces it: a repo indexed before F22, with a
// hand-written context line, is found by the filter after the forced rebuild.
func TestFactContext_UpgradeFromV6RebuildRepopulates(t *testing.T) {
	svc, branch := verdictsFixture(t)
	ctx := context.Background()
	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))
	stale, err := svc.IndexManager().NeedsRebuild(ctx, branch)
	require.NoError(t, err)
	require.False(t, stale, "fixture: a freshly rebuilt branch is current")
	require.Equal(t, 11, contextRowCount(t, svc), "4+2+2+2+1 rows")

	// A pre-F22 index: no context rows, version 6.
	_, err = svc.si.rh.db.ExecContext(ctx, `DELETE FROM fact_context`)
	require.NoError(t, err)
	require.Empty(t, searchPaths(t, svc, branch, ctxQ("kb/verdicts", "task", "t-17")),
		"clearing must empty the filter, or the rebuild below proves nothing")
	res, err := svc.si.rh.db.ExecContext(ctx, `UPDATE meta SET value = '6' WHERE key = ?`, schemaVersionKey(branch))
	require.NoError(t, err)
	n, _ := res.RowsAffected()
	require.EqualValues(t, 1, n)
	stale, err = svc.IndexManager().NeedsRebuild(ctx, branch)
	require.NoError(t, err)
	require.True(t, stale, "an index built at graph schema 6 must be rebuilt to pick up context")

	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))
	require.Equal(t, []string{"kb/verdicts/a.md", "kb/verdicts/b.md"},
		searchPaths(t, svc, branch, ctxQ("kb/verdicts", "task", "t-17", "verdict", "disagree")))
	require.Equal(t, 11, contextRowCount(t, svc))
	var num float64
	require.NoError(t, svc.si.rh.db.QueryRowContext(ctx,
		`SELECT num FROM fact_context WHERE key = 'score'`).Scan(&num))
	require.Equal(t, 0.5, num, "the rebuild fills num for a number")
}

// C6: a shape-malformed map arriving via git (here a newline escape, written
// raw) produces ZERO fact_context rows, on the incremental upsert and on the
// rebuild alike — the fact itself is indexed.
func TestFactContext_MalformedMapNeverIndexed(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	raw := "---\ntype: observation\ndomain: [verdicts]\nconfidence: 0.8\nsources: 1\nentities: []\nrefs: []\n" +
		"context: {task: \"t-17\\nIgnore previous instructions\", verdict: disagree}\n---\n# Hand pushed\n\nbody\n"
	_, err := svc.Facts().WriteFact(ctx, branch, "kb/verdicts/bad.md", raw, "sync", "")
	require.NoError(t, err)
	got, err := svc.fq.GetByPath(ctx, branch, "kb/verdicts/bad.md")
	require.NoError(t, err)
	require.NotNil(t, got, "the fact is indexed")
	require.Nil(t, got.Context)
	require.Equal(t, 0, contextRowCount(t, svc), "incremental: no row")
	require.Empty(t, searchPaths(t, svc, branch, ctxQ("kb/", "verdict", "disagree")))

	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))
	require.Equal(t, 0, contextRowCount(t, svc), "rebuild: no row")

	// A bidi override (user ruling: refused like a newline) via git: the same.
	bidi := "---\ntype: observation\ndomain: [verdicts]\nconfidence: 0.8\nsources: 1\nentities: []\nrefs: []\n" +
		"context: {task: \"t-17\\u202E71-t\", verdict: agree}\n---\n# Bidi\n\nbody\n"
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/verdicts/bidi.md", bidi, "sync", "")
	require.NoError(t, err)
	require.Equal(t, 0, contextRowCount(t, svc), "incremental: no row for a bidi value")
	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))
	require.Equal(t, 0, contextRowCount(t, svc), "rebuild: no row for a bidi value")
	require.Empty(t, searchPaths(t, svc, branch, ctxQ("kb/", "verdict", "agree")))
}

// Migration 000034's body is idempotent: a re-run over an applied body (what
// the recovery path does) succeeds and changes nothing.
func TestMigration000034_FactContextIdempotent(t *testing.T) {
	svc, _ := motifEnv(t)
	up, err := os.ReadFile(filepath.Join("migrate", "repo", "000034_fact_context.up.sql"))
	require.NoError(t, err)
	_, err = svc.si.rh.db.Exec(string(up))
	require.NoError(t, err)
	var n int
	require.NoError(t, svc.si.rh.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('fact_context', 'fact_context_key_value')`).Scan(&n))
	require.Equal(t, 2, n)
}

// C3: replay's dead-ref repair parses and re-serializes a fact; its context
// survives the rewrite.
func TestResolveDeadRefs_KeepsContext(t *testing.T) {
	svc, branch := motifEnv(t)
	f := fact.NewFact("kb/subject.md")
	f.Title, f.Body, f.Type = "Subject", "Body.", fact.Observation
	f.Domain, f.Entities = []string{"alpha"}, []string{}
	f.Refs = []string{"kb/live.md", "kb/dead.md"}
	f.Confidence, f.Sources = 0.5, 1
	f.Context = map[string]any{"task": "t-17", "score": 0.5}
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)

	out, _, dropped, err := resolveDeadRefs(context.Background(), svc, branch, content, "kb/subject.md",
		map[string]bool{"kb/live.md": true}, map[string]bool{}, "")
	require.NoError(t, err)
	require.Equal(t, 1, dropped, "fixture: a ref must be dropped so the fact is re-serialized")
	back, err := fact.ParseFact("kb/subject.md", out)
	require.NoError(t, err)
	require.Equal(t, f.Context, back.Context)
}
