package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/merge"
)

// ref_edges is THE read path for references in both directions (PR #694
// scope addition): IncomingAtCommit, BlastRadius / ReverseDependentPaths and
// the highlights' out-degree read the table; the DERIVED_FROM edges are kept
// for Verify's drift check (and for the reads before the backfill). These
// tests pin every table read equal to the edge read it replaced.

// readsSnapshot is every reference read of a store, over every (path, commit)
// commit_log knows and every given branch.
type readsSnapshot struct {
	incoming   map[string][]index.RefSummary
	blast      map[string]int
	dependents map[string]map[string]struct{}
	highlights map[string][]index.Highlight
	stats      map[string]index.StatsResult
}

func kbPathsAndCommits(t *testing.T, db *sql.DB) ([][2]string, []string) {
	t.Helper()
	rows, err := db.Query(`SELECT DISTINCT path, commit_hash FROM commit_log WHERE path LIKE 'kb/%' ORDER BY path, commit_hash`)
	require.NoError(t, err)
	defer rows.Close()
	var pcs [][2]string
	seen := map[string]bool{}
	var paths []string
	for rows.Next() {
		var p, c string
		require.NoError(t, rows.Scan(&p, &c))
		pcs = append(pcs, [2]string{p, c})
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	require.NoError(t, rows.Err())
	return pcs, paths
}

// takeReads reads everything, by the table (byEdges=false) or by the edges.
func takeReads(t *testing.T, svc *Service, branches []string, byEdges bool) readsSnapshot {
	t.Helper()
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	pcs, paths := kbPathsAndCommits(t, db)
	s := readsSnapshot{incoming: map[string][]index.RefSummary{}, blast: map[string]int{},
		dependents: map[string]map[string]struct{}{}, highlights: map[string][]index.Highlight{}, stats: map[string]index.StatsResult{}}
	svc.fq.SetOutdegreeByEdgesForTest(byEdges)
	defer svc.fq.SetOutdegreeByEdgesForTest(false)
	for _, b := range branches {
		for _, pc := range pcs {
			var in []index.RefSummary
			var err error
			if byEdges {
				in, err = svc.gq.IncomingByEdgesForTest(ctx, b, pc[0], pc[1])
			} else {
				in, err = svc.gq.IncomingAtCommit(ctx, b, pc[0], pc[1])
			}
			require.NoError(t, err)
			s.incoming[b+" "+pc[0]+"@"+pc[1]] = in
		}
		for _, p := range paths {
			var n int
			var err error
			if byEdges {
				n, err = svc.gq.BlastRadiusByEdgesForTest(ctx, b, p)
			} else {
				n, err = svc.gq.BlastRadius(ctx, b, p)
			}
			require.NoError(t, err)
			s.blast[b+" "+p] = n
		}
		for _, axis := range []string{index.AxisImpact, index.AxisConfidence, index.AxisRecent} {
			h, _, err := svc.fq.Highlights(ctx, b, "", axis)
			require.NoError(t, err)
			s.highlights[b+" "+axis] = h
			st, err := svc.fq.Stats(ctx, b, "", axis)
			require.NoError(t, err)
			s.stats[b+" "+axis] = st
		}
	}
	for _, p := range paths {
		var d map[string]struct{}
		var err error
		if byEdges {
			d, err = svc.gq.ReverseDependentPathsByEdgesForTest(ctx, p)
		} else {
			d, err = svc.gq.ReverseDependentPaths(ctx, p)
		}
		require.NoError(t, err)
		s.dependents[p] = d
	}
	return s
}

// requireTableReadsEqualEdgeReads asserts the table reads == the edge reads,
// order included, and that the comparison was not vacuous: it returns how
// many incoming rows and how much blast radius it compared.
func requireTableReadsEqualEdgeReads(t *testing.T, svc *Service, branches ...string) (incomingRows, blastTotal int) {
	t.Helper()
	db := svc.si.RepoForTest().DB()
	var v string
	require.NoError(t, db.QueryRow(`SELECT COALESCE((SELECT value FROM meta WHERE key = 'ref_edges_backfilled'), '')`).Scan(&v))
	if v != "1" {
		// A rebuild's v8 reset reopened the backfill; run it, so the table
		// reads are really table reads.
		_, err := svc.si.BackfillRefEdges(context.Background(), branches[0])
		require.NoError(t, err)
	}
	byRows := takeReads(t, svc, branches, false)
	byEdges := takeReads(t, svc, branches, true)
	require.Equal(t, byEdges.incoming, byRows.incoming, "incoming: table == edges")
	require.Equal(t, byEdges.blast, byRows.blast, "blast radius: table == edges")
	require.Equal(t, byEdges.dependents, byRows.dependents, "reverse dependents: table == edges")
	require.Equal(t, byEdges.highlights, byRows.highlights, "highlights: table == edges")
	require.Equal(t, byEdges.stats, byRows.stats, "stats: table == edges")
	for _, in := range byRows.incoming {
		incomingRows += len(in)
	}
	for _, n := range byRows.blast {
		blastTotal += n
	}
	return incomingRows, blastTotal
}

// The #693 incoming scenarios, each re-run with the table reads compared to
// the edge reads at the end: rename, the same bytes on two branches and their
// merges, a retracted citer on a second branch, every version that cited
// (F2), and the chunked holdings.
func TestRefEdgesReaders_EqualTheEdgeReadsOnTheBlobEdgesScenarios(t *testing.T) {
	scenarios := map[string]func(t *testing.T) (*Service, []string){
		"rename": func(t *testing.T) (*Service, []string) {
			svc := newMergeTestStore(t)
			ctx := context.Background()
			write(t, svc, "main", "kb/r.md", "R")
			write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
			write(t, svc, "main", "kb/c.md", "C", "kb/f.md")
			_, _, err := svc.Facts().BatchWriteFacts(ctx, "main", map[string]string{"kb/g.md": testFactBody("F", 0.8, []string{"kb/r.md"})},
				[]string{"kb/f.md"}, "rename f -> g", "")
			require.NoError(t, err)
			require.NoError(t, svc.IndexManager().Sync(ctx, "main"))
			return svc, []string{"main"}
		},
		"two-branches-and-merges": func(t *testing.T) (*Service, []string) {
			svc := newMergeTestStore(t)
			ctx := context.Background()
			write(t, svc, "main", "kb/r.md", "R")
			for _, b := range []string{"brA", "brC"} {
				require.NoError(t, svc.CreateBranch(ctx, b, "main"))
				require.NoError(t, svc.IndexManager().Sync(ctx, b))
			}
			write(t, svc, "brA", "kb/x.md", "X", "kb/r.md")
			write(t, svc, "brC", "kb/x.md", "X", "kb/r.md")
			write(t, svc, "brC", "kb/y.md", "X", "kb/r.md")
			write(t, svc, "brC", "kb/r.md", "R on C")
			require.NoError(t, svc.Merge().MergeBranch(ctx, "brC", "brA", merge.StrategyLocalWins))
			require.NoError(t, svc.IndexManager().Sync(ctx, "brA"))
			return svc, []string{"main", "brA", "brC"}
		},
		"retracted-citer-and-every-version": func(t *testing.T) (*Service, []string) {
			svc := newMergeTestStore(t)
			ctx := context.Background()
			write(t, svc, "main", "kb/r.md", "R")
			write(t, svc, "main", "kb/f.md", "F v1", "kb/r.md")
			write(t, svc, "main", "kb/f.md", "F v2", "kb/r.md")
			write(t, svc, "main", "kb/f.md", "F v3")
			write(t, svc, "main", "kb/d.md", "D", "kb/r.md")
			_, err := svc.Facts().DeleteFact(ctx, "main", "kb/d.md", "retract")
			require.NoError(t, err)
			write(t, svc, "main", "kb/e.md", "E", "kb/d.md")
			return svc, []string{"main"}
		},
		"chunked-holdings": func(t *testing.T) (*Service, []string) {
			svc := newMergeTestStore(t)
			write(t, svc, "main", "kb/r.md", "R")
			for _, p := range []string{"kb/a.md", "kb/b.md", "kb/c.md", "kb/d.md", "kb/e.md"} {
				write(t, svc, "main", p, "X", "kb/r.md")
			}
			return svc, []string{"main"}
		},
	}
	for name, build := range scenarios {
		t.Run(name, func(t *testing.T) {
			svc, branches := build(t)
			if name == "chunked-holdings" {
				old := index.SetBlobHoldingsChunkForTest(2)
				defer index.SetBlobHoldingsChunkForTest(old)
			}
			in, blast := requireTableReadsEqualEdgeReads(t, svc, branches...)
			require.Greater(t, in, 0, "incoming compared real rows")
			if name != "two-branches-and-merges" {
				require.Greater(t, blast, 0, "blast radius compared real reach")
			}
		})
	}
}

// The incoming order is documented (sortIncomingCandidates): chronological in
// the citing version's source commit (commitEarlier's total order), then
// blob, then path, whatever the edge ids. Three citers written in order a, b,
// c read back a, b, c; a rebuild (which renumbers edges) does not reorder.
func TestRefEdgesReaders_IncomingOrderIsChronological(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	cR, _ := write(t, svc, "main", "kb/r.md", "R")
	for _, p := range []string{"kb/z1.md", "kb/a2.md", "kb/m3.md"} {
		write(t, svc, "main", p, "cites R "+p, "kb/r.md")
	}
	order := func() []string {
		in, err := svc.gq.IncomingAtCommit(ctx, "main", "kb/r.md", cR)
		require.NoError(t, err)
		var ps []string
		for _, r := range in {
			ps = append(ps, r.Path)
		}
		return ps
	}
	want := []string{"kb/z1.md", "kb/a2.md", "kb/m3.md"}
	require.Equal(t, want, order(), "write order, not path order")
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	require.Equal(t, want, order(), "a rebuild does not reorder")
}

// The N2 fallback covers incoming, blast radius and highlights too: on a
// store whose ref_edges is not backfilled yet (rows gone, sentinel unset)
// they answer exactly as with the table.
//
// Sabotage: drop the readiness check in incomingCandidates,
// reverseDependentPaths or outdegreeByRows, and the reads come back empty.
func TestRefEdgesReaders_FallBackBeforeTheBackfill(t *testing.T) {
	svc := newMergeTestStore(t)
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	write(t, svc, "main", "kb/s.md", "S", "kb/r.md")
	write(t, svc, "main", "kb/f.md", "F", "kb/r.md", "kb/s.md")
	write(t, svc, "main", "kb/g.md", "G", "kb/f.md")
	want := takeReads(t, svc, []string{"main"}, false)
	require.Equal(t, 3, want.blast["main kb/r.md"], "precondition: r reaches s, f, g")
	_, err := db.Exec(`DELETE FROM ref_edges`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM meta WHERE key = 'ref_edges_backfilled'`)
	require.NoError(t, err)
	got := takeReads(t, svc, []string{"main"}, false)
	require.Equal(t, want.incoming, got.incoming, "incoming before the backfill == with the table")
	require.Equal(t, want.blast, got.blast, "blast radius before the backfill")
	require.Equal(t, want.dependents, got.dependents)
	require.Equal(t, want.highlights, got.highlights, "highlights before the backfill")
	require.Equal(t, want.stats, got.stats)
}

// requireTableReadsEqualEdgeReadsOnEveryBranch is the comparison over every
// branch of the store (for the #693 tests' t.Cleanup).
func requireTableReadsEqualEdgeReadsOnEveryBranch(t *testing.T, svc *Service) {
	t.Helper()
	rows, err := svc.si.RepoForTest().DB().Query(`SELECT name FROM branches ORDER BY name`)
	require.NoError(t, err)
	var branches []string
	for rows.Next() {
		var b string
		require.NoError(t, rows.Scan(&b))
		if _, err := svc.HeadCommit(context.Background(), b); err == nil {
			branches = append(branches, b)
		}
	}
	rows.Close()
	require.NotEmpty(t, branches)
	in, _ := requireTableReadsEqualEdgeReads(t, svc, branches...)
	require.Greater(t, in, 0, "incoming compared real rows")
}

// PR #694 review round 2, P1: the incoming read's plan is the fast one: a
// COVERING probe of idx_ref_edges_target (no table lookup per row), and each
// citing blob's nodes found through (key_id, value), not a skip-scan.
//
// Sabotage: put back the target_repo filter (not covering), or unpin the join
// order (FROM s CROSS JOIN k JOIN node_props_text fb: the skip-scan).
//
// Production stores carry planner statistics (PRAGMA optimize at open), and
// the bad plan only appears with realistic ones, so the test loads the
// statistics measured on the 250k-fact store before asking for the plan.
func TestRefEdgesReaders_IncomingPlanIsCoveringAndPinned(t *testing.T) {
	svc := newMergeTestStore(t)
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	_, err := db.Exec(`ANALYZE`)
	require.NoError(t, err)
	for _, st := range [][3]string{
		{"node_props_text", "idx_node_props_text_key_value", "2255547 1001 1 1"},
		{"node_props_text", "sqlite_autoindex_node_props_text_1", "2255547 9 1"},
		{"ref_edges", "idx_ref_edges_target", "748500 3"},
		{"ref_edges", "ref_edges", "748500 2 1"},
	} {
		_, err := db.Exec(`DELETE FROM sqlite_stat1 WHERE tbl = ? AND idx = ?`, st[0], st[1])
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES (?, ?, ?)`, st[0], st[1], st[2])
		require.NoError(t, err)
	}
	_, err = db.Exec(`ANALYZE sqlite_schema`)
	require.NoError(t, err)
	plan, err := svc.gq.IncomingPlanForTest(context.Background())
	require.NoError(t, err)
	require.Contains(t, plan, "USING COVERING INDEX idx_ref_edges_target (target_blob=?)", "%s", plan)
	require.Contains(t, plan, "(key_id=? AND value=?)", "%s", plan)
	require.NotContains(t, plan, "ANY(key_id)", "%s", plan)
}
