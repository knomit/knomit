package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// motifEnv opens a store on a temp dir with one branch, ready for writes.
func motifEnv(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	return svc, "main"
}

// writeMotifFact writes a fact carrying motifs. Domain and entities are fixed
// and deliberately unrelated to the motifs used below, so the subject strip
// never fires by accident and hides a storage bug.
func writeMotifFact(t *testing.T, svc *Service, branch, path string, motifs []string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = "T " + path
	f.Body = "Body of " + path
	f.Type = fact.Observation
	f.Domain = []string{"alpha"}
	f.Entities = []string{"Widget"}
	f.Refs = []string{}
	f.Confidence = 0.8
	f.Sources = 1
	f.Motifs = motifs
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

// motifRows reads the junction rows for the LIVE revision of path on branch.
func motifRows(t *testing.T, svc *Service, branch, path string) []string {
	t.Helper()
	rows, err := svc.si.rh.db.QueryContext(context.Background(), `
		SELECT m.motif
		  FROM fact_motifs m
		  JOIN branch_facts bf ON bf.fact_id = m.fact_id
		  JOIN branches b ON b.id = bf.branch_id
		 WHERE bf.path = ? AND b.name = ?
		 ORDER BY m.motif`, path, branch)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestFactMotifs_PopulatedOnUpsert(t *testing.T) {
	svc, branch := motifEnv(t)
	writeMotifFact(t, svc, branch, "kb/alpha/one.md",
		[]string{"silent-fallback", "config-drift"})

	require.Equal(t, []string{"config-drift", "silent-fallback"},
		motifRows(t, svc, branch, "kb/alpha/one.md"))
}

func TestFactMotifs_MotiflessFactHasNoRows(t *testing.T) {
	svc, branch := motifEnv(t)
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", nil)
	require.Empty(t, motifRows(t, svc, branch, "kb/alpha/one.md"))
}

// TestFactMotifs_RebuildRepopulatesJunction is the regression this table's
// migration comment warns about: the bulk rebuild repopulates junctions from
// the facts table's JSON columns, NOT by re-parsing, so a junction without a
// matching column is never filled on a machine that only ever rebuilds — a
// fresh clone, or any repo whose index was dropped.
//
// The junction rows are CLEARED before the rebuild, deliberately. Without
// that this test passes vacuously: rebuildFacts upserts facts with
// ON CONFLICT DO UPDATE, which preserves rowids and therefore fires no
// cascade, so rows the incremental upsert already wrote simply survive
// untouched and the rebuild path is never exercised at all. Clearing first is
// what makes the assertion mean "the rebuild populated these", rather than
// "something populated these once".
func TestFactMotifs_RebuildRepopulatesJunction(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md",
		[]string{"silent-fallback", "config-drift"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/three.md", nil)

	fromIncremental := motifRows(t, svc, branch, "kb/alpha/one.md")
	require.Len(t, fromIncremental, 2, "fixture must have rows to lose")

	_, err := svc.si.rh.db.ExecContext(ctx, `DELETE FROM fact_motifs`)
	require.NoError(t, err)
	require.Empty(t, motifRows(t, svc, branch, "kb/alpha/one.md"),
		"clearing must actually empty the junction, or the rebuild below proves nothing")

	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	require.Equal(t, fromIncremental, motifRows(t, svc, branch, "kb/alpha/one.md"),
		"the rebuild path must reproduce exactly what the incremental path built")
	require.Equal(t, []string{"silent-fallback"},
		motifRows(t, svc, branch, "kb/alpha/two.md"))
	require.Empty(t, motifRows(t, svc, branch, "kb/alpha/three.md"))
}

// TestFactMotifs_StrippedMotifNeverReachesJunction — the strip happens at
// serialize time, so the committed bytes never carry a subject motif and no
// index path can resurrect one. This asserts that end-to-end rather than
// trusting the layering.
func TestFactMotifs_StrippedMotifNeverReachesJunction(t *testing.T) {
	svc, branch := motifEnv(t)
	// "widget-alpha" is entity ∪ domain for these fixtures.
	writeMotifFact(t, svc, branch, "kb/alpha/one.md",
		[]string{"widget-alpha", "silent-fallback"})

	require.Equal(t, []string{"silent-fallback"},
		motifRows(t, svc, branch, "kb/alpha/one.md"))
}

func TestTokenDF_MotifKind(t *testing.T) {
	svc, branch := motifEnv(t)
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"silent-fallback", "config-drift"})
	writeMotifFact(t, svc, branch, "kb/alpha/three.md", nil)

	ctx := context.Background()
	for _, tc := range []struct {
		token string
		want  int
	}{
		{"silent-fallback", 2},
		{"config-drift", 1},
		{"never-written", 0},
	} {
		n, err := svc.Search().TokenDF(ctx, branch, tc.token, "motif")
		require.NoError(t, err)
		require.Equalf(t, tc.want, n, "df for %q", tc.token)
	}
}

// TestMigration019_DownUpCycle pins that the rollback actually rolls back.
//
// The first version of this down migration DROPped the table but kept
// facts.motifs, citing 000012 as precedent — which drops its column. Two things
// were wrong with that: the precedent said the opposite, and re-applying the up
// migration afterwards fails on "duplicate column name: motifs", leaving the
// schema dirty. A down migration nothing ever exercises is a rollback nobody
// can perform.
func TestMigration019_DownUpCycle(t *testing.T) {
	svc, branch := motifEnv(t)
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	require.NotEmpty(t, motifRows(t, svc, branch, "kb/alpha/one.md"))

	db := svc.si.rh.db
	hasMotifsColumn := func() bool {
		var n int
		require.NoError(t, db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('facts') WHERE name = 'motifs'`).Scan(&n))
		return n == 1
	}
	require.True(t, hasMotifsColumn())

	_, err := db.Exec(migrationSQL(t, "000019_fact_motifs.down.sql"))
	require.NoError(t, err, "the down migration must apply cleanly")
	require.False(t, hasMotifsColumn(), "down must take the column, not only the table")

	// The cycle: up must re-apply on a rolled-back schema.
	_, err = db.Exec(migrationSQL(t, "000019_fact_motifs.up.sql"))
	require.NoError(t, err, "re-applying up after down must not collide with a leftover column")
	require.True(t, hasMotifsColumn())

	// And the junction is rebuildable from git, which is what makes the
	// rollback safe to perform at all.
	require.NoError(t, svc.IndexManager().Rebuild(context.Background(), branch, nil))
	require.Equal(t, []string{"silent-fallback"}, motifRows(t, svc, branch, "kb/alpha/one.md"))
}

// TestVerify_DetectsMotifJunctionDrift — checkDerivedTables' own doc comment
// names this failure mode: the facts are all present and correct, and a lookup
// over them returns nothing. For motifs the symptom is worse than silent,
// because document-frequency is an INPUT to later phases: a half-populated
// junction makes a motif look rarer than it is, and nothing downstream can tell.
func TestVerify_DetectsMotifJunctionDrift(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback", "config-drift"})

	clean, err := svc.Verify(ctx, VerifyOpts{})
	require.NoError(t, err)
	require.Empty(t, derivedTableIssues(clean),
		"a healthy repo must report no derived-table issues, or the test below proves nothing")

	// Junction row lost while the column still lists it — what a rebuild that
	// forgot to repopulate would leave behind.
	_, err = svc.si.rh.db.ExecContext(ctx,
		`DELETE FROM fact_motifs WHERE motif = 'config-drift'`)
	require.NoError(t, err)

	drifted, err := svc.Verify(ctx, VerifyOpts{})
	require.NoError(t, err)
	issues := derivedTableIssues(drifted)
	require.NotEmpty(t, issues, "a lost junction row must be reported")
	require.Contains(t, issues[0].Detail, "absent from fact_motifs")

	// And the reverse: a junction row for a motif the column no longer lists.
	_, err = svc.si.rh.db.ExecContext(ctx,
		`INSERT INTO fact_motifs(fact_id, motif) SELECT id, 'never-authored' FROM facts LIMIT 1`)
	require.NoError(t, err)

	both, err := svc.Verify(ctx, VerifyOpts{})
	require.NoError(t, err)
	var details []string
	for _, i := range derivedTableIssues(both) {
		details = append(details, i.Detail)
	}
	require.Len(t, details, 2, "both directions must be reported, not just the first")
}

// TestVerify_DetectsMotifOrphans — a junction row pointing at a facts row that
// no longer exists.
func TestVerify_DetectsMotifOrphans(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})

	// The FK plus ON DELETE CASCADE makes an orphan impossible while foreign
	// keys are ON, which is exactly why the check exists: SQLite's
	// foreign_keys pragma is PER-CONNECTION and defaults OFF, so any tool that
	// writes this DB without setting it can leave rows the schema would have
	// refused. Reproducing that is the only honest way to test the check.
	conn, err := svc.si.rh.db.Conn(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx,
		`INSERT INTO fact_motifs(fact_id, motif) VALUES (999999, 'orphaned-row')`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	report, err := svc.Verify(ctx, VerifyOpts{})
	require.NoError(t, err)
	var found bool
	for _, i := range derivedTableIssues(report) {
		if strings.Contains(i.Detail, "fact_motifs has 1 row(s) referencing a missing facts row") {
			found = true
		}
	}
	require.True(t, found, "an orphaned fact_motifs row must be reported")
}

func derivedTableIssues(r IntegrityReport) []IntegrityIssue {
	var out []IntegrityIssue
	for _, i := range r.Issues {
		if i.Category == CategoryDerivedTables {
			out = append(out, i)
		}
	}
	return out
}
