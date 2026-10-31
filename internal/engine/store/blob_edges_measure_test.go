package store

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestBlobEdges_MeasureUpgrade runs the GraphSchemaVersion 8 upgrade on a
// COPY of a real store (KNOMIT_BLOBEDGES_MEASURE_DB; branches
// KNOMIT_BLOBEDGES_MEASURE_BRANCHES, comma-separated, rebuilt in order as the
// heal does): reference-edge counts before and after, duplicate groups and
// multi-pinned refs (both must reach 0), and the rebuild's duration. Env-gated
// (KNOMIT_BLOBEDGES_MEASURE), so CI never runs it.
func TestBlobEdges_MeasureUpgrade(t *testing.T) {
	path := os.Getenv("KNOMIT_BLOBEDGES_MEASURE_DB")
	if os.Getenv("KNOMIT_BLOBEDGES_MEASURE") == "" || path == "" {
		t.Skip("set KNOMIT_BLOBEDGES_MEASURE=1 and KNOMIT_BLOBEDGES_MEASURE_DB to a store COPY")
	}
	ctx := context.Background()
	svc, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.OpenRepo())
	db := svc.si.RepoForTest().DB()
	report := func(label string) {
		var edges, dupGroups, multi, multiNode int64
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM edges WHERE type = 'DERIVED_FROM'`).Scan(&edges))
		require.NoError(t, db.QueryRow(`
			WITH r AS (
			  SELECT sb.value AS sblob, tp.value AS tpath, e.target_id AS t, tc.value AS tc
			    FROM edges e
			    JOIN node_props_text sb ON sb.node_id = e.source_id AND sb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash')
			    JOIN node_props_text tp ON tp.node_id = e.target_id AND tp.key_id = (SELECT id FROM property_keys WHERE key = 'path')
			    JOIN edge_props_text tc ON tc.edge_id = e.id AND tc.key_id = (SELECT id FROM property_keys WHERE key = 'target_commit')
			   WHERE e.type = 'DERIVED_FROM')
			SELECT (SELECT COUNT(*) FROM (SELECT 1 FROM r GROUP BY sblob, tpath, tc HAVING COUNT(*) > 1)),
			       (SELECT COUNT(*) FROM (SELECT 1 FROM r GROUP BY sblob, tpath HAVING COUNT(DISTINCT tc) > 1)),
			       (SELECT COUNT(*) FROM (SELECT 1 FROM r GROUP BY sblob, tpath HAVING COUNT(DISTINCT t) > 1))`).
			Scan(&dupGroups, &multi, &multiNode))
		t.Logf("%s: DERIVED_FROM edges %d; duplicate groups (blob, path, target_commit) %d; refs pinned at >1 target_commit %d; at >1 target version %d",
			label, edges, dupGroups, multi, multiNode)
	}
	report("before")
	var branches []string
	if b := os.Getenv("KNOMIT_BLOBEDGES_MEASURE_BRANCHES"); b != "" {
		branches = strings.Split(b, ",")
	}
	total := time.Now()
	for _, b := range branches {
		t0 := time.Now()
		require.NoError(t, svc.IndexManager().Rebuild(ctx, b, nil))
		t.Logf("rebuild %s: %s", b, time.Since(t0))
	}
	t.Logf("rebuild total: %s", time.Since(total))
	report("after")
	var stale sql.NullString
	_ = db.QueryRow(`SELECT value FROM meta WHERE key = 'derived_from_per_blob'`).Scan(&stale)
	require.Equal(t, "1", stale.String)
}
