package store

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
)

// Measurements for ref_edges (PR B). Env-gated by KNOMIT_REFEDGES_MEASURE, so
// CI never runs them; each runs against a store FILE (a copy, never a live
// store):
//
//	KNOMIT_REFEDGES_MEASURE_DB      the store file
//	KNOMIT_REFEDGES_MEASURE_BRANCH  its branch (default agent/test)
//	KNOMIT_REFEDGES_MEASURE_UNITS   shape B roots to seed (default 500)
//	KNOMIT_REFEDGES_MEASURE_ORDER   "flat" or "eav": which read goes first
//	                                (the first read after open is the cold one)
//
// Shape B (the round-4 worst case, regenerated after the scratchpad was lost):
// 500 roots under kb/m4/b/u/, each citing 499 facts of its own under
// kb/m4/b/d/; each of those cites the next two of its unit, so a unit has a
// second level (998 rows). Bodies ~2.5 KiB, small directories (as the
// proto/factset-streaming harness, which this mirrors).

func measureEnvOrSkip(t *testing.T) (string, string) {
	t.Helper()
	path := os.Getenv("KNOMIT_REFEDGES_MEASURE_DB")
	if os.Getenv("KNOMIT_REFEDGES_MEASURE") == "" || path == "" {
		t.Skip("set KNOMIT_REFEDGES_MEASURE=1 and KNOMIT_REFEDGES_MEASURE_DB to a store COPY")
	}
	branch := os.Getenv("KNOMIT_REFEDGES_MEASURE_BRANCH")
	if branch == "" {
		branch = "agent/test"
	}
	return path, branch
}

func measureEnvInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func openMeasureStore(t *testing.T, path, branch string, create bool) *Service {
	t.Helper()
	_, statErr := os.Stat(path)
	t0 := time.Now()
	svc, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	if os.IsNotExist(statErr) && create {
		require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, branch))
	} else {
		require.NoError(t, svc.OpenRepo())
	}
	t.Logf("open (migrations included): %s", time.Since(t0))
	return svc
}

func measureName(parts ...any) string {
	h := fnv.New64a()
	fmt.Fprint(h, parts...)
	return fmt.Sprintf("%012x", h.Sum64()&0xffffffffffff)
}

func measureFact(t *testing.T, path, title string, refs []string) string {
	t.Helper()
	ff := fact.NewFact(path)
	ff.Title, ff.Type = title, fact.Observation
	var sb strings.Builder
	for sb.Len() < 2500 {
		sb.WriteString("This fact records an observation made while measuring the flat reference table. ")
		sb.WriteString("It is written to look like a real fact, so pages and blobs behave like production. ")
		sb.WriteString(path + "\n\n")
	}
	ff.Body = sb.String()
	ff.Domain, ff.Entities, ff.Refs = []string{"measure"}, []string{"ref_edges"}, refs
	ff.Confidence, ff.Sources = 0.8, 1
	out, err := fact.SerializeFact(ff)
	require.NoError(t, err)
	return out
}

// TestRefEdges_MeasureSeed seeds shape B into KNOMIT_REFEDGES_MEASURE_DB
// (creating the store if absent).
func TestRefEdges_MeasureSeed(t *testing.T) {
	path, branch := measureEnvOrSkip(t)
	if os.Getenv("KNOMIT_REFEDGES_MEASURE_SEED") == "" {
		t.Skip("set KNOMIT_REFEDGES_MEASURE_SEED=1 to seed")
	}
	svc := openMeasureStore(t, path, branch, true)
	ctx := context.Background()
	units := measureEnvInt("KNOMIT_REFEDGES_MEASURE_UNITS", 500)
	start := time.Now()
	for i := 0; i < units; i++ {
		own := make([]string, 499)
		for j := range own {
			own[j] = fmt.Sprintf("kb/m4/b/d/g%02d/r%03d/s%d/%s.md", i/25, i, j/50, measureName("b", i, j))
		}
		files := map[string]string{}
		for j := range own {
			files[own[j]] = measureFact(t, own[j], fmt.Sprintf("B %d.%d", i, j), []string{own[(j+1)%499], own[(j+2)%499]})
		}
		_, _, err := svc.Facts().BatchWriteFacts(ctx, branch, files, nil, "measure seed", "learn")
		require.NoError(t, err)
		p := fmt.Sprintf("kb/m4/b/u/g%02d/r%03d.md", i/25, i)
		_, _, err = svc.Facts().BatchWriteFacts(ctx, branch, map[string]string{p: measureFact(t, p, fmt.Sprintf("B result %d", i), own)}, nil, "measure seed", "learn")
		require.NoError(t, err)
		if (i+1)%25 == 0 || i+1 == units {
			t.Logf("seed B: %d units in %s", i+1, time.Since(start))
		}
	}
}

func dbBytes(t *testing.T, svc *Service) int64 {
	var pages, size int64
	db := svc.si.RepoForTest().DB()
	require.NoError(t, db.QueryRow(`PRAGMA page_count`).Scan(&pages))
	require.NoError(t, db.QueryRow(`PRAGMA page_size`).Scan(&size))
	return pages * size
}

// TestRefEdges_MeasureUpgrade measures the two ways an existing store gets
// its rows: the forced rebuild (the #693 v8 rebuild of the branch, which
// writes ref_edges through the write path) when the store is below v8, and
// the backfill from the edges (what a v8 store gets at its heal). It then
// checks backfill == write path, row for row, and no drift.
func TestRefEdges_MeasureUpgrade(t *testing.T) {
	path, branch := measureEnvOrSkip(t)
	svc := openMeasureStore(t, path, branch, false)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	count := func(q string) int64 {
		var n int64
		require.NoError(t, db.QueryRow(q).Scan(&n))
		return n
	}
	var v string
	_ = db.QueryRow(`SELECT value FROM meta WHERE key = 'derived_from_per_blob'`).Scan(&v)
	if v != "1" {
		t0 := time.Now()
		require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))
		t.Logf("v8 rebuild of %s (reset + every branch re-resolved; writes ref_edges): %s", branch, time.Since(t0))
	}
	t.Logf("facts rows %d, Fact nodes %d, DERIVED_FROM edges %d, objects %d",
		count(`SELECT COUNT(*) FROM facts`), count(`SELECT COUNT(*) FROM node_labels WHERE label = 'Fact'`),
		count(`SELECT COUNT(*) FROM edges WHERE type = 'DERIVED_FROM'`), count(`SELECT COUNT(*) FROM objects`))
	inc := allRefRows(t, db)
	t.Logf("ref_edges after the write path: %d rows, %d cross-KB, %d source blobs", len(inc),
		count(`SELECT COUNT(*) FROM ref_edges WHERE target_repo != ''`), count(`SELECT COUNT(DISTINCT source_blob) FROM ref_edges`))
	td := time.Now()
	d, err := svc.si.RefEdgeDrift(ctx)
	require.NoError(t, err)
	t.Logf("drift check (Verify): missing %d, orphan %d, in %s", d.MissingRows, d.OrphanRows, time.Since(td))

	// Backfill from the edges, from an empty table.
	_, err = db.Exec(`DELETE FROM ref_edges`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM meta WHERE key = 'ref_edges_backfilled'`)
	require.NoError(t, err)
	_, err = db.Exec(`VACUUM`)
	require.NoError(t, err)
	before := dbBytes(t, svc)
	tb := time.Now()
	n, err := svc.si.BackfillRefEdges(ctx, branch)
	require.NoError(t, err)
	t.Logf("backfill: %d blobs written in %s", n, time.Since(tb))
	_, err = db.Exec(`VACUUM`)
	require.NoError(t, err)
	t.Logf("ref_edges size (VACUUMed page delta): %.1f MiB for %d rows", float64(dbBytes(t, svc)-before)/(1<<20), len(inc))
	require.Equal(t, inc, allRefRows(t, db), "backfill == the write path, row for row")
	d, err = svc.si.RefEdgeDrift(ctx)
	require.NoError(t, err)
	require.Zero(t, d.MissingRows+d.OrphanRows, "%v", d.Examples)
	t.Logf("backfill == write path over %d rows; drift 0", len(inc))

	// The outgoing read against the edge read, on every live version.
	rows, err := db.Query(`SELECT f.path, bf.commit_hash FROM branch_facts bf JOIN facts f ON f.id = bf.fact_id
		WHERE bf.branch_id = (SELECT id FROM branches WHERE name = ?)`, branch)
	require.NoError(t, err)
	type pc struct{ p, c string }
	var live []pc
	for rows.Next() {
		var x pc
		require.NoError(t, rows.Scan(&x.p, &x.c))
		live = append(live, x)
	}
	rows.Close()
	same, reordered, refs := 0, 0, 0
	var tEdges, tRows time.Duration
	for _, x := range live {
		t1 := time.Now()
		a, err := svc.gq.OutgoingByEdgesForTest(ctx, branch, x.p, x.c)
		require.NoError(t, err)
		t2 := time.Now()
		b, err := svc.gq.OutgoingAtCommit(ctx, branch, x.p, x.c)
		require.NoError(t, err)
		tEdges += t2.Sub(t1)
		tRows += time.Since(t2)
		require.ElementsMatch(t, a, b, "%s", x.p)
		refs += len(b)
		equalOrder := true
		for i := range a {
			if a[i].Path != b[i].Path {
				equalOrder = false
			}
		}
		if equalOrder {
			same++
		} else {
			reordered++
		}
	}
	t.Logf("OutgoingAtCommit vs the edge read on %d live versions (%d refs): identical sets; %d same order, %d reordered to the author's order; total %s (edges) vs %s (ref_edges)",
		len(live), refs, same, reordered, tEdges, tRows)
}

// eavLevelOfBlobs is the edge-EAV form of one level: the DERIVED_FROM edges
// out of the claim nodes of the given blobs, with target path, blob and
// target_commit, one set-based statement per 4000 blobs (join order pinned).
func eavLevelOfBlobs(t *testing.T, svc *Service, blobs []string) int {
	n := 0
	for lo := 0; lo < len(blobs); lo += 4000 {
		part := blobs[lo:min(len(blobs), lo+4000)]
		args := make([]any, len(part))
		for i, b := range part {
			args[i] = b
		}
		rows, err := svc.si.RepoForTest().DB().Query(`
			WITH s(b) AS (VALUES `+strings.TrimSuffix(strings.Repeat("(?),", len(part)), ",")+`),
			     k AS (SELECT (SELECT id FROM property_keys WHERE key = 'path') AS kp,
			                  (SELECT id FROM property_keys WHERE key = 'blob_hash') AS kb,
			                  (SELECT id FROM property_keys WHERE key = 'target_commit') AS ktc)
			SELECT s.b, tp.value, tb.value, tc.value
			  FROM s CROSS JOIN k
			  CROSS JOIN node_props_text nb ON nb.key_id = k.kb AND nb.value = s.b
			  CROSS JOIN edges e ON e.source_id = nb.node_id AND e.type = 'DERIVED_FROM'
			  CROSS JOIN node_props_text tp ON tp.node_id = e.target_id AND tp.key_id = k.kp
			  CROSS JOIN node_props_text tb ON tb.node_id = e.target_id AND tb.key_id = k.kb
			  CROSS JOIN edge_props_text tc ON tc.edge_id = e.id AND tc.key_id = k.ktc`, args...)
		require.NoError(t, err)
		for rows.Next() {
			var a, b, c, d string
			require.NoError(t, rows.Scan(&a, &b, &c, &d))
			n++
		}
		require.NoError(t, rows.Err())
		rows.Close()
	}
	return n
}

// TestRefEdges_MeasureLevels: the first-level join (500 roots x 499 refs) and
// the per-level joins (each unit's second level), edge-EAV vs ref_edges.
func TestRefEdges_MeasureLevels(t *testing.T) {
	path, branch := measureEnvOrSkip(t)
	svc := openMeasureStore(t, path, branch, false)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	var roots []string
	rows, err := db.Query(`SELECT f.blob_hash FROM branch_facts bf JOIN facts f ON f.id = bf.fact_id
		WHERE bf.branch_id = (SELECT id FROM branches WHERE name = ?) AND f.path LIKE 'kb/m4/b/u/%' ORDER BY f.path`, branch)
	require.NoError(t, err)
	for rows.Next() {
		var b string
		require.NoError(t, rows.Scan(&b))
		roots = append(roots, b)
	}
	rows.Close()
	require.NotEmpty(t, roots, "shape B is not in this store")
	flat := func(blobs []string) int {
		out, err := svc.gq.RefEdgesOfBlobs(ctx, blobs)
		require.NoError(t, err)
		return len(out)
	}
	order := []string{"flat", "eav"}
	if os.Getenv("KNOMIT_REFEDGES_MEASURE_ORDER") == "eav" {
		order = []string{"eav", "flat"}
	}
	for pass := 0; pass < 3; pass++ {
		for _, which := range order {
			t0 := time.Now()
			var n int
			if which == "flat" {
				n = flat(roots)
			} else {
				n = eavLevelOfBlobs(t, svc, roots)
			}
			label := "warm"
			if pass == 0 {
				label = "first after open"
			}
			t.Logf("first level %s, %s: %d roots, %d rows, %s", which, label, len(roots), n, time.Since(t0))
		}
	}
	// Per level: each unit's second level (its 499 children's refs), one set
	// read per unit, as the streaming render does per unit.
	out, err := svc.gq.RefEdgesOfBlobs(ctx, roots)
	require.NoError(t, err)
	children := map[string][]string{}
	for _, r := range out {
		if r.Repo == "" {
			children[r.SourceBlob] = append(children[r.SourceBlob], r.TargetBlob)
		}
	}
	for pass := 0; pass < 2; pass++ {
		var tf, te time.Duration
		nf, ne := 0, 0
		for _, r := range roots {
			t0 := time.Now()
			nf += flat(children[r])
			t1 := time.Now()
			ne += eavLevelOfBlobs(t, svc, children[r])
			te += time.Since(t1)
			tf += t1.Sub(t0)
		}
		require.Equal(t, ne, nf, "the same rows")
		t.Logf("per level pass %d (%d units x one level of 499 sources, %d rows): flat %s (%s/unit), eav %s (%s/unit)",
			pass, len(roots), nf, tf, tf/time.Duration(len(roots)), te, te/time.Duration(len(roots)))
	}
}

// TestRefEdges_MeasureReaders: incoming for a heavily cited target and blast
// radius, edge read vs table read, warm. Seeds (once) a hot target
// kb/m4/hot/t.md cited by KNOMIT_REFEDGES_MEASURE_HOT facts (default 2000)
// into the store copy. Shape B's first root's first leaf is the blast-radius
// probe (its unit's ring of 499 leaves plus the root reach it).
func TestRefEdges_MeasureReaders(t *testing.T) {
	path, branch := measureEnvOrSkip(t)
	svc := openMeasureStore(t, path, branch, false)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	hot := "kb/m4/hot/t.md"
	n := measureEnvInt("KNOMIT_REFEDGES_MEASURE_HOT", 2000)
	var have int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM branch_facts bf JOIN facts f ON f.id = bf.fact_id WHERE f.path = ?`, hot).Scan(&have))
	if have == 0 {
		t0 := time.Now()
		_, _, err := svc.Facts().BatchWriteFacts(ctx, branch, map[string]string{hot: measureFact(t, hot, "hot target", nil)}, nil, "hot", "learn")
		require.NoError(t, err)
		for lo := 0; lo < n; lo += 500 {
			files := map[string]string{}
			for i := lo; i < min(n, lo+500); i++ {
				p := fmt.Sprintf("kb/m4/hot/c/g%02d/%s.md", i/50, measureName("hot", i))
				files[p] = measureFact(t, p, fmt.Sprintf("hot citer %d", i), []string{hot})
			}
			_, _, err := svc.Facts().BatchWriteFacts(ctx, branch, files, nil, "hot citers", "learn")
			require.NoError(t, err)
		}
		t.Logf("seeded the hot target and %d citers in %s", n, time.Since(t0))
	}
	var hotCommit, leaf string
	require.NoError(t, db.QueryRow(`SELECT bf.commit_hash FROM branch_facts bf JOIN facts f ON f.id = bf.fact_id
		WHERE f.path = ? AND bf.branch_id = (SELECT id FROM branches WHERE name = ?)`, hot, branch).Scan(&hotCommit))
	_ = db.QueryRow(`SELECT f.path FROM facts f WHERE f.path LIKE 'kb/m4/b/d/g00/r000/s0/%' ORDER BY f.path LIMIT 1`).Scan(&leaf) // shape B only
	var plan string
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT source_blob FROM ref_edges WHERE target_blob = 'x' AND target_repo = ''`)
	require.NoError(t, err)
	for rows.Next() {
		var a, b, c int
		var d string
		require.NoError(t, rows.Scan(&a, &b, &c, &d))
		plan += d + "; "
	}
	rows.Close()
	t.Logf("incoming probe plan: %s", plan)
	for pass := 0; pass < 3; pass++ {
		t0 := time.Now()
		a, err := svc.gq.IncomingByEdgesForTest(ctx, branch, hot, hotCommit)
		require.NoError(t, err)
		t1 := time.Now()
		b, err := svc.gq.IncomingAtCommit(ctx, branch, hot, hotCommit)
		require.NoError(t, err)
		t2 := time.Now()
		require.Equal(t, a, b)
		t.Logf("incoming of the hot target pass %d: %d rows; edges %s, table %s", pass, len(b), t1.Sub(t0), t2.Sub(t1))
	}
	probes := []string{hot}
	if leaf != "" {
		probes = append([]string{leaf}, probes...)
	}
	// The edge walk's recursive step can scan every DERIVED_FROM edge per
	// frontier row (plan: SEARCH e USING INDEX idx_edges_type): on shape B it
	// did not finish in 37 minutes. It is timed only on request.
	edges := os.Getenv("KNOMIT_REFEDGES_MEASURE_EDGE_BLAST") != ""
	for _, p := range probes {
		for pass := 0; pass < 3; pass++ {
			var a int
			t0 := time.Now()
			if edges {
				var err error
				a, err = svc.gq.BlastRadiusByEdgesForTest(ctx, branch, p)
				require.NoError(t, err)
			}
			t1 := time.Now()
			b, err := svc.gq.BlastRadius(ctx, branch, p)
			require.NoError(t, err)
			t2 := time.Now()
			if edges {
				require.Equal(t, a, b)
			}
			t.Logf("blast radius of %s pass %d: %d; edges %s (timed: %v), table %s", p, pass, b, t1.Sub(t0), edges, t2.Sub(t1))
		}
	}
}
