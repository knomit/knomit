package store

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRefEdges_MeasureWrite times the index write of a fact citing K existing
// facts (end to end: WriteFact, its commit, its index sync and reference
// edges). It uses only APIs that exist before ref_edges, so the same file
// runs on the base commit for the comparison. Env-gated like the others:
// KNOMIT_REFEDGES_MEASURE=1, KNOMIT_REFEDGES_MEASURE_DB (a COPY),
// KNOMIT_REFEDGES_MEASURE_BRANCH, KNOMIT_REFEDGES_MEASURE_WRITES (default 40),
// KNOMIT_REFEDGES_MEASURE_REFS (default 20).
func TestRefEdges_MeasureWrite(t *testing.T) {
	path := os.Getenv("KNOMIT_REFEDGES_MEASURE_DB")
	if os.Getenv("KNOMIT_REFEDGES_MEASURE") == "" || path == "" {
		t.Skip("set KNOMIT_REFEDGES_MEASURE=1 and KNOMIT_REFEDGES_MEASURE_DB to a store COPY")
	}
	branch := os.Getenv("KNOMIT_REFEDGES_MEASURE_BRANCH")
	if branch == "" {
		branch = "agent/test"
	}
	writes, refs := 40, 20
	if v := os.Getenv("KNOMIT_REFEDGES_MEASURE_WRITES"); v != "" {
		fmt.Sscan(v, &writes)
	}
	if v := os.Getenv("KNOMIT_REFEDGES_MEASURE_REFS"); v != "" {
		fmt.Sscan(v, &refs)
	}
	svc, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.OpenRepo())
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	rows, err := db.Query(`SELECT f.path FROM branch_facts bf JOIN facts f ON f.id = bf.fact_id
		WHERE bf.branch_id = (SELECT id FROM branches WHERE name = ?) AND f.path LIKE 'kb/%' AND f.path NOT LIKE 'kb/zz-measure/%'
		ORDER BY f.path LIMIT ?`, branch, refs)
	require.NoError(t, err)
	var targets []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		targets = append(targets, p)
	}
	rows.Close()
	require.Len(t, targets, refs)
	var all []time.Duration
	for i := 0; i < writes; i++ {
		p := fmt.Sprintf("kb/zz-measure/w%03d.md", i)
		t0 := time.Now()
		_, err := svc.Facts().WriteFact(ctx, branch, p, testFactBody(p, 0.8, targets), "measure "+p, "")
		require.NoError(t, err)
		all = append(all, time.Since(t0))
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	var sum time.Duration
	for _, d := range all {
		sum += d
	}
	t.Logf("%d writes of a fact citing %d facts: mean %s, median %s, p90 %s",
		writes, refs, sum/time.Duration(writes), all[len(all)/2], all[len(all)*9/10])
}
