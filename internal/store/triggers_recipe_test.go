package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---- F07 PR 5: the recipe columns (migration 000031) and the late rows.

// Migration000031: trigger_fires carries recipe_source, recipe_rev and run_id
// (text, default ”), and the run-id lookup is an index range.
func TestMigration000031_RecipeColumns(t *testing.T) {
	svc := newChangesService(t)
	rows, err := svc.rh.db.Query(`SELECT name, "notnull", dflt_value FROM pragma_table_info('trigger_fires')`)
	require.NoError(t, err)
	defer rows.Close()
	cols := map[string]string{}
	for rows.Next() {
		var name string
		var notnull int
		var dflt *string
		require.NoError(t, rows.Scan(&name, &notnull, &dflt))
		if dflt != nil {
			cols[name] = *dflt
		} else {
			cols[name] = "<null>"
		}
	}
	for _, c := range []string{"recipe_source", "recipe_rev", "run_id"} {
		require.Equal(t, "''", cols[c], "column %s", c)
	}
	require.Len(t, cols, 25, "the 21 columns of 000029, the three, and 000033's message")
	var n int
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name IN ('trigger_fires_run', 'trigger_fires_branch')`).Scan(&n))
	require.Equal(t, 2, n)
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'trigger_fires_000031'`).Scan(&n))
	require.Equal(t, 0, n, "the copy table is gone")
}

// Migration000031_ReRunsAndKeepsTheLog: the body re-runs cleanly over an
// applied schema (upWithRecovery's re-run), the down and the up both keep the
// existing fire log with its ids, and AUTOINCREMENT continues after them.
// Sabotage: ALTER TABLE ADD COLUMN (red: duplicate column on the re-run); a
// DROP without the copy (red: the log is gone).
func TestMigration000031_ReRunsAndKeepsTheLog(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	tr := svc.Triggers()
	_, err := tr.RecordTriggerRuns(ctx, []TriggerRun{{Branch: "agent/a", RangeFrom: "f", RangeTo: "t",
		Rows: []TriggerFire{{Trigger: "w", Path: "kb/x.md", Outcome: TriggerOutcomeEmitted}}}})
	require.NoError(t, err)
	before := mustFires(t, tr, "agent/a")
	require.Len(t, before, 2)
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("migrate", "repo", name))
		require.NoError(t, err)
		return string(b)
	}
	up, down := read("000031_trigger_recipe.up.sql"), read("000031_trigger_recipe.down.sql")
	// 000033 rebuilds the table again (it adds `message`); a re-run of 000031
	// at head is followed by the later migrations, exactly as recovery's Up()
	// continues, so the head schema the store reads is restored after each.
	later := read("000033_trigger_fire_message.up.sql")
	_, err = svc.rh.db.Exec(up)
	require.NoError(t, err, "the body re-runs over an applied schema")
	_, err = svc.rh.db.Exec(later)
	require.NoError(t, err)
	require.Equal(t, before, mustFires(t, tr, "agent/a"), "a re-run keeps the log")
	_, err = svc.rh.db.Exec(down)
	require.NoError(t, err)
	_, err = svc.rh.db.Exec(up)
	require.NoError(t, err)
	_, err = svc.rh.db.Exec(later)
	require.NoError(t, err)
	require.Equal(t, before, mustFires(t, tr, "agent/a"), "down then up keeps the log and its ids")
	_, err = tr.RecordTriggerRuns(ctx, []TriggerRun{{Branch: "agent/a", RangeFrom: "t", RangeTo: "u"}})
	require.NoError(t, err)
	after := mustFires(t, tr, "agent/a")
	require.Greater(t, after[0].ID, before[0].ID, "ids continue after the copied ones")
}

// RecordTriggerResults writes late result rows as given — their own branch
// and range, the recipe columns, the duration — with NO run row; the lookup
// by run id returns a run's rows oldest first; run rows written by
// RecordTriggerRuns carry the columns too. Sabotage: drop run_id from the
// insert (red: the lookup finds nothing).
func TestTriggerFires_ResultsAndLookupByRun(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	tr := svc.Triggers()
	const id = "run-0123456789abcdef0123456789abcdef"
	_, err := tr.RecordTriggerRuns(ctx, []TriggerRun{{Branch: "agent/a", RangeFrom: "f", RangeTo: "t", Rows: []TriggerFire{{
		Trigger: "w", Path: "kb/x.md", Outcome: TriggerOutcomeStarted, RecipeSource: RecipeSourceLocal, RecipeRev: "abc", RunID: id,
	}}}})
	require.NoError(t, err)
	before := len(mustFires(t, tr, "agent/a"))
	require.NoError(t, tr.RecordTriggerResults(ctx, []TriggerFire{{
		Trigger: "w", Branch: "agent/a", Path: "kb/x.md", Outcome: TriggerOutcomeDone, Error: "ok", RangeFrom: "f", RangeTo: "t",
		DurationMS: 1234, RecipeSource: RecipeSourceLocal, RecipeRev: "abc", RunID: id,
	}}))
	require.Len(t, mustFires(t, tr, "agent/a"), before+1, "one row and no run row")

	got, err := tr.TriggerFiresByRun(ctx, "agent/a", id)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, TriggerOutcomeStarted, got[0].Outcome)
	require.Equal(t, TriggerOutcomeDone, got[1].Outcome)
	require.Equal(t, int64(1234), got[1].DurationMS)
	require.Equal(t, "f", got[1].RangeFrom)
	require.Equal(t, "t", got[1].RangeTo)
	for _, r := range got {
		require.Equal(t, id, r.RunID)
		require.Equal(t, RecipeSourceLocal, r.RecipeSource)
		require.Equal(t, "abc", r.RecipeRev)
	}
	other, err := tr.TriggerFiresByRun(ctx, "agent/b", id)
	require.NoError(t, err)
	require.Empty(t, other, "per branch")
	none, err := tr.TriggerFiresByRun(ctx, "agent/a", "")
	require.NoError(t, err)
	require.Empty(t, none)
}

// Migration000033_MessageColumn (rehearsal F5): trigger_fires gains
// `message` (text, default ”); the body re-runs over an applied schema, and
// down then up keeps the log, its ids and the other columns. (The up body
// copies the 24 older columns, so a re-run resets `message` to ”; no row can
// be written between a body's commit and its re-run, and the table is a local
// cache.) Sabotage: ALTER TABLE ADD COLUMN (red: duplicate column on the
// re-run).
func TestMigration000033_MessageColumn(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	tr := svc.Triggers()
	var dflt string
	require.NoError(t, svc.rh.db.QueryRow(`SELECT dflt_value FROM pragma_table_info('trigger_fires') WHERE name = 'message'`).Scan(&dflt))
	require.Equal(t, "''", dflt)
	require.NoError(t, tr.RecordTriggerResults(ctx, []TriggerFire{{
		Trigger: "w", Branch: "agent/a", Path: "kb/x.md", Outcome: TriggerOutcomeDone, Message: "exit 0 cost=1.03",
		RangeFrom: "f", RangeTo: "t", RunID: "run-0123456789abcdef0123456789abcdef",
	}}))
	before := mustFires(t, tr, "agent/a")
	require.Len(t, before, 1)
	require.Equal(t, "exit 0 cost=1.03", before[0].Message)
	require.Empty(t, before[0].Error, "a done row's text is a message")

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("migrate", "repo", name))
		require.NoError(t, err)
		return string(b)
	}
	up, down := read("000033_trigger_fire_message.up.sql"), read("000033_trigger_fire_message.down.sql")
	_, err := svc.rh.db.Exec(up)
	require.NoError(t, err, "the body re-runs over an applied schema")
	after := mustFires(t, tr, "agent/a")
	require.Len(t, after, 1)
	require.Equal(t, before[0].ID, after[0].ID, "ids survive")
	require.Equal(t, before[0].RunID, after[0].RunID, "the other columns survive")
	_, err = svc.rh.db.Exec(down)
	require.NoError(t, err)
	var n int
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('trigger_fires') WHERE name = 'message'`).Scan(&n))
	require.Equal(t, 0, n, "down removes the column")
	_, err = svc.rh.db.Exec(up)
	require.NoError(t, err)
	require.Len(t, mustFires(t, tr, "agent/a"), 1, "down then up keeps the log")
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'trigger_fires_000033'`).Scan(&n))
	require.Equal(t, 0, n, "the copy table is gone")
}

func mustFires(t *testing.T, tr TriggerIndex, branch string) []TriggerFire {
	t.Helper()
	rows, err := tr.RecentTriggerFires(context.Background(), branch, 100)
	require.NoError(t, err)
	return rows
}
