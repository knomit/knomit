package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---- F07 PR 2: the `on: due` store surface (migration 000030).

// Migration 000030: the due-mark table exists after Open (the body re-runs
// cleanly, which the recovery tests exercise for every migration).
func TestMigration000030_TableExists(t *testing.T) {
	svc := newChangesService(t)
	var n int
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'trigger_due_fires'`).Scan(&n))
	require.Equal(t, 1, n)
	var sql string
	require.NoError(t, svc.rh.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'trigger_due_fires'`).Scan(&sql))
	require.Contains(t, sql, "PRIMARY KEY (branch, trigger, path)", "the key starts with branch so the per-run read is an index range")
}

func dueAt(t *testing.T, svc *Service, branch string, nowUnix int64) map[string]int64 {
	t.Helper()
	rows, err := svc.Triggers().DueCandidates(context.Background(), branch, nowUnix)
	require.NoError(t, err)
	out := map[string]int64{}
	for _, r := range rows {
		out[r.Path] = r.ExpiresAt
	}
	return out
}

// DueCandidates is the LIVENESS join, not fact_expires alone: a fact is a
// candidate only while the branch's branch_facts points at a dated version,
// and only at or after its instant (F03's inclusive boundary). The same
// version live on main and deleted on the agent branch keeps its fact_expires
// row (main still holds it) and is NOT a candidate on the agent branch;
// clearing expires replaces the version with an undated one; changing expires
// yields the NEW instant only. Sabotage: query fact_expires without the
// branch_facts join (the deleted and main-only facts appear); `<` for `<=`
// (the boundary case disappears).
func TestDueCandidates_LivenessJoinAndBoundary(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	at := expNow.Unix()
	write := func(branch, path string, expires time.Time) {
		writeExpiresFact(t, svc, branch, path, "hypothesis", expires.Format(time.RFC3339))
	}
	write("agent/a", "kb/alpha/edge.md", expNow)
	write("agent/a", "kb/alpha/shared.md", expNow.Add(-time.Hour))
	write("main", "kb/alpha/shared.md", expNow.Add(-time.Hour)) // the same version, held by main too
	write("main", "kb/alpha/main-only.md", expNow.Add(-time.Hour))
	write("agent/a", "kb/alpha/future.md", expNow.Add(time.Hour))
	write("agent/a", "kb/alpha/cleared.md", expNow.Add(-time.Hour))
	write("agent/a", "kb/alpha/moved.md", expNow.Add(-2*time.Hour))

	require.Equal(t, map[string]int64{
		"kb/alpha/edge.md":    at,
		"kb/alpha/shared.md":  at - 3600,
		"kb/alpha/cleared.md": at - 3600,
		"kb/alpha/moved.md":   at - 7200,
	}, dueAt(t, svc, "agent/a", at), "inclusive boundary; main-only and future facts are not candidates")
	require.NotContains(t, dueAt(t, svc, "agent/a", at-1), "kb/alpha/edge.md", "one second before its instant a fact is not due")
	require.Equal(t, map[string]int64{"kb/alpha/shared.md": at - 3600, "kb/alpha/main-only.md": at - 3600},
		dueAt(t, svc, "main", at), "main's own view")

	// Retract on the agent branch while main holds the same version.
	_, err := svc.Facts().DeleteFact(ctx, "agent/a", "kb/alpha/shared.md", "retract")
	require.NoError(t, err)
	var rows int
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM fact_expires fe JOIN facts f ON f.id = fe.fact_id WHERE f.path = 'kb/alpha/shared.md'`).Scan(&rows))
	require.Equal(t, 1, rows, "fixture: fact_expires keeps the version's row (main holds it) — the join is what makes it not a candidate")
	require.NotContains(t, dueAt(t, svc, "agent/a", at), "kb/alpha/shared.md")

	// An update that clears expires: the branch points at an undated version.
	writeExpiresFact(t, svc, "agent/a", "kb/alpha/cleared.md", "hypothesis", "")
	require.NotContains(t, dueAt(t, svc, "agent/a", at), "kb/alpha/cleared.md")
	// An update that changes expires: the new instant only.
	write("agent/a", "kb/alpha/moved.md", expNow.Add(-30*time.Minute))
	require.Equal(t, at-1800, dueAt(t, svc, "agent/a", at)["kb/alpha/moved.md"])
	var versions int
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM fact_expires fe JOIN facts f ON f.id = fe.fact_id WHERE f.path = 'kb/alpha/moved.md'`).Scan(&versions))
	require.Equal(t, 2, versions, "fixture: the old version's row is an orphan in fact_expires; only the join keeps it out")
}

// Due marks round-trip per (branch, trigger, path), a re-mark REPLACES the
// instant, and a NAME in del forgets the name's marks together with its
// bookmark — on that branch only. Sabotage: keep the marks when the name
// leaves; key the marks without the branch.
func TestDueMarks_RoundTripAndForgottenWithTheName(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	tr := svc.Triggers()
	marks := func(branch string) map[DueKey]int64 {
		m, err := tr.DueMarks(ctx, branch)
		require.NoError(t, err)
		return m
	}
	require.Empty(t, marks("agent/a"))

	require.NoError(t, tr.AdvanceTriggerWatermarks(ctx, "agent/a", map[string]string{"x": "h1", "y": "h1"}, nil, []DueMark{
		{Trigger: "x", Path: "kb/a.md", ExpiresAt: 100, FiredAt: 500},
		{Trigger: "x", Path: "kb/b.md", ExpiresAt: 200, FiredAt: 500},
		{Trigger: "y", Path: "kb/a.md", ExpiresAt: 100, FiredAt: 500},
	}))
	require.NoError(t, tr.AdvanceTriggerWatermarks(ctx, "agent/b", nil, nil, []DueMark{{Trigger: "x", Path: "kb/a.md", ExpiresAt: 100, FiredAt: 500}}))
	require.Equal(t, map[DueKey]int64{
		{Trigger: "x", Path: "kb/a.md"}: 100,
		{Trigger: "x", Path: "kb/b.md"}: 200,
		{Trigger: "y", Path: "kb/a.md"}: 100,
	}, marks("agent/a"))

	// Re-arm: the same key with a new instant replaces the row.
	require.NoError(t, tr.AdvanceTriggerWatermarks(ctx, "agent/a", nil, nil, []DueMark{{Trigger: "x", Path: "kb/a.md", ExpiresAt: 150, FiredAt: 600}}))
	require.EqualValues(t, 150, marks("agent/a")[DueKey{Trigger: "x", Path: "kb/a.md"}])
	var n int
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM trigger_due_fires`).Scan(&n))
	require.Equal(t, 4, n, "three keys on agent/a and one on agent/b")

	// The name x leaves: its bookmark and its marks go, y's stay, agent/b's stay.
	require.NoError(t, tr.AdvanceTriggerWatermarks(ctx, "agent/a", nil, []string{"x"}, nil))
	require.Equal(t, map[DueKey]int64{{Trigger: "y", Path: "kb/a.md"}: 100}, marks("agent/a"))
	wms, err := tr.TriggerWatermarks(ctx, "agent/a")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"y": "h1"}, wms)
	require.Equal(t, map[DueKey]int64{{Trigger: "x", Path: "kb/a.md"}: 100}, marks("agent/b"), "another branch's marks are untouched")
}

// [T24] The marks are never capped and never hit SQLite's bound-parameter
// limit: 12,000 marks in ONE call all land, while the same run's 12,000 fire
// rows are trimmed to TriggerFireRetention. Sabotage: one unchunked INSERT
// (too many SQL variables); apply the retention cap to the marks (10,000).
func TestAdvanceTriggerWatermarks_DueMarksUnbounded(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	const n = 12000
	due := make([]DueMark, 0, n)
	for i := 0; i < n; i++ {
		due = append(due, DueMark{Trigger: "big", Path: fmt.Sprintf("kb/big/%05d.md", i), ExpiresAt: 100, FiredAt: 500})
	}
	logged, err := svc.Triggers().RecordTriggerRun(ctx, TriggerRun{Branch: "agent/a", RangeFrom: "h", RangeTo: "h", Paths: n, Evaluated: n, Fires: n, Rows: fireRows(n, "big")})
	require.NoError(t, err)
	require.Equal(t, TriggerFireRetention, logged, "fixture: the fire rows ARE capped")
	require.NoError(t, svc.Triggers().AdvanceTriggerWatermarks(ctx, "agent/a", map[string]string{"big": "h"}, nil, due))
	marks, err := svc.Triggers().DueMarks(ctx, "agent/a")
	require.NoError(t, err)
	require.Len(t, marks, n, "every processed fact is marked, whatever the log kept")
	var rows int
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM trigger_due_fires WHERE branch = 'agent/a' AND trigger = 'big'`).Scan(&rows))
	require.Equal(t, n, rows)
}

// Every rendered F07 timestamp is UTC with an explicit Z, whatever the
// process's local zone (maintainer ruling 2026-09-28): fired_at on a fire-log
// row is stored as Unix seconds and rendered by UTCStamp; the SAME stored
// value renders identically under UTC and under America/New_York. Sabotage:
// render with time.Unix(n, 0).Format (local) — the New York run differs.
func TestTriggerFires_FiredAtIsUTCWithZ(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	_, err := svc.Triggers().RecordTriggerRun(ctx, TriggerRun{Branch: "agent/a", RangeFrom: "h", RangeTo: "h", Paths: 1, Evaluated: 1, Fires: 1, Rows: fireRows(1, "z")})
	require.NoError(t, err)
	var stored int64
	require.NoError(t, svc.rh.db.QueryRow(`SELECT fired_at FROM trigger_fires WHERE outcome = 'run'`).Scan(&stored))
	require.InDelta(t, time.Now().UTC().Unix(), stored, 5, "stored as Unix seconds")

	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	rendered := map[string]string{}
	for name, loc := range map[string]*time.Location{"utc": time.UTC, "new-york": ny} {
		orig := time.Local
		time.Local = loc
		fires, err := svc.Triggers().RecentTriggerFires(ctx, "agent/a", 5)
		time.Local = orig
		require.NoError(t, err)
		require.Len(t, fires, 2)
		for _, f := range fires {
			require.True(t, strings.HasSuffix(f.FiredAt, "Z"), "%s: %q must carry an explicit Z", name, f.FiredAt)
			parsed, err := time.Parse(time.RFC3339, f.FiredAt)
			require.NoError(t, err)
			require.Equal(t, stored, parsed.Unix())
			require.Equal(t, UTCStamp(stored), f.FiredAt)
		}
		rendered[name] = fires[0].FiredAt
	}
	require.Equal(t, rendered["utc"], rendered["new-york"], "the local zone must not leak into the rendering")
	require.Equal(t, "2026-09-28T12:34:56Z", UTCStamp(time.Date(2026, 9, 28, 14, 34, 56, 0, time.FixedZone("x", 2*3600)).Unix()))
}

// BenchmarkDueCandidates [T20, report-only]: the sweep's one query over N live
// dated facts on the branch with `due` of them past the instant, rows inserted
// directly (no git). Numbers go in the PR body; nothing is gated on them. The
// cost is linear in the past-dated rows the covering index yields, independent
// of N.
func BenchmarkDueCandidates(b *testing.B) {
	for _, tc := range []struct{ live, due int }{{1000, 0}, {1000, 500}, {10000, 0}, {10000, 100}, {10000, 5000}, {50000, 500}} {
		b.Run(fmt.Sprintf("live=%d/due=%d", tc.live, tc.due), func(b *testing.B) {
			svc, err := Open(filepath.Join(b.TempDir(), "k.db"))
			require.NoError(b, err)
			b.Cleanup(func() { svc.Close() })
			require.NoError(b, svc.InitRepoWithUpstream(context.Background(), map[string]string{}, "main", "agent/a"))
			ctx := context.Background()
			var branchID int64
			require.NoError(b, svc.rh.db.QueryRow(`SELECT id FROM branches WHERE name = 'agent/a'`).Scan(&branchID))
			tx, err := svc.rh.db.BeginTx(ctx, nil)
			require.NoError(b, err)
			const now = int64(1_800_000_000)
			for i := 0; i < tc.live; i++ {
				path := fmt.Sprintf("kb/bench/f%06d.md", i)
				res, err := tx.ExecContext(ctx, `INSERT INTO facts(path, blob_hash, title, kind, type, domain, entities, motifs, confidence, sources, refs, evidence_weight, origin)
					VALUES (?, ?, ?, 'epistemic', 'observation', '[]', '[]', '[]', 0.5, 1, '[]', 0, 'authored')`, path, fmt.Sprintf("%040x", i), path)
				require.NoError(b, err)
				id, _ := res.LastInsertId()
				at := now + 3600 // future
				if i < tc.due {
					at = now - 3600 // past
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO fact_expires(fact_id, expires, expires_at) VALUES (?, ?, ?)`, id, UTCStamp(at), at)
				require.NoError(b, err)
				_, err = tx.ExecContext(ctx, `INSERT INTO branch_facts(branch_id, path, fact_id, commit_hash) VALUES (?, ?, ?, '')`, branchID, path, id)
				require.NoError(b, err)
			}
			require.NoError(b, tx.Commit())
			_, err = svc.rh.db.ExecContext(ctx, `ANALYZE`)
			require.NoError(b, err)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rows, err := svc.Triggers().DueCandidates(ctx, "agent/a", now)
				if err != nil || len(rows) != tc.due {
					b.Fatalf("rows=%d err=%v", len(rows), err)
				}
			}
		})
	}
}
