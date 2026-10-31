package index

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/facts"
	storegit "knomit/internal/engine/store/git"
	"knomit/internal/engine/store/internal/vcs"
)

// ref_edges (migration 000035) is the flat form of a fact version's
// references: one row per fact ref, keyed (source_blob, ord), ord being the
// ref's position in the version's frontmatter refs list (the author's order;
// DERIVED_FROM edge ids do not follow it, kb/gotchas/store/graph/derived-from-order).
//
// It mirrors the per-blob DERIVED_FROM edges: a blob's rows are derived from
// its edges and its refs list INSIDE the transaction that writes its claim and
// edges (applyBlobRefs -> writeRefRowsTx), so the two are never out of step on
// the write path. Local rows exist only where an edge exists (and so only for
// real blobs); cross-KB refs get a row with target_repo set and no pinned
// version (they resolve at read time against that repo's frozen tip). Verify
// compares local rows with the edges (RefEdgeDrift); a rebuild repairs drift
// (repairRefEdges); an existing store is filled once (BackfillRefEdges).

// refSpec is one fact ref of a version: its position in the version's refs
// list, its repo ("" for this repo) and its repo-relative path.
type refSpec struct {
	ord  int
	repo string
	path string
}

// refSpecsOf is the fact refs of a refs list, in the author's order, each
// (repo, path) once at its first position. ord counts EVERY entry of refs
// (URLs, src://, system files included), so it equals json_each(facts.refs).key.
// A ref into this repo (bare, or kb://<this repo>/...) is local; a kb:// ref
// into another repo is cross-KB; every other ref gets no spec. localRepoID ""
// makes every kb:// ref cross-KB, as localFactRefs did.
func refSpecsOf(refs []string, localRepoID string) []refSpec {
	var out []refSpec
	seen := map[[2]string]bool{}
	for i, raw := range refs {
		c := fact.ClassifyRef(raw, localRepoID)
		var k [2]string
		switch c.Kind {
		case fact.RefLocalFact:
			k = [2]string{"", c.Path}
		case fact.RefForeignFact:
			k = [2]string{c.RepoID, c.Path}
		default:
			continue
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, refSpec{ord: i, repo: k[0], path: k[1]})
	}
	return out
}

// localPaths is the local refs of specs, in order: what reference edges are
// resolved for.
func localPaths(specs []refSpec) []string {
	var out []string
	for _, s := range specs {
		if s.repo == "" {
			out = append(out, s.path)
		}
	}
	return out
}

// RefEdgeRow is one row of ref_edges.
//   - Repo "" is a ref into this repo, pinned at (TargetBlob, TargetCommit).
//   - Repo set is a cross-KB ref (the other repo's 12-hex id): TargetBlob and
//     TargetCommit are "" because the version is not fixed at write time; a
//     reader resolves Path at that repo's frozen tip.
type RefEdgeRow struct {
	SourceBlob   string
	Ord          int
	Repo         string
	Path         string
	TargetBlob   string
	TargetCommit string
}

// expectedRefRows derives a blob's rows from its refs (specs) and the
// DERIVED_FROM edges on its claim node (node): one local row per spec whose
// target path has an edge (the first edge by id when there were several),
// one cross-KB row per cross-KB spec. ex may be a transaction.
func expectedRefRows(ctx context.Context, ex storegit.CtxExecer, node int64, blob string, specs []refSpec) ([]RefEdgeRow, error) {
	type pin struct{ blob, commit string }
	pins := map[string]pin{}
	if node != 0 {
		rows, err := ex.QueryContext(ctx, `
			SELECT tp.value, tb.value, COALESCE(tc.value, '')
			  FROM edges e
			  JOIN node_props_text tp ON tp.node_id = e.target_id
			       AND tp.key_id = (SELECT id FROM property_keys WHERE key = 'path')
			  JOIN node_props_text tb ON tb.node_id = e.target_id
			       AND tb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash')
			  LEFT JOIN edge_props_text tc ON tc.edge_id = e.id
			       AND tc.key_id = (SELECT id FROM property_keys WHERE key = 'target_commit')
			 WHERE e.source_id = ? AND e.type = ?
			 ORDER BY e.id`, node, EdgeDerivedFrom)
		if err != nil {
			return nil, fmt.Errorf("ref_edges: edges of %s: %w", blob, err)
		}
		for rows.Next() {
			var p string
			var v pin
			if err := rows.Scan(&p, &v.blob, &v.commit); err != nil {
				rows.Close()
				return nil, fmt.Errorf("ref_edges: edges of %s: %w", blob, err)
			}
			if _, dup := pins[p]; !dup {
				pins[p] = v
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("ref_edges: edges of %s: %w", blob, err)
		}
	}
	out := make([]RefEdgeRow, 0, len(specs))
	for _, s := range specs {
		r := RefEdgeRow{SourceBlob: blob, Ord: s.ord, Repo: s.repo, Path: s.path}
		if s.repo == "" {
			p, ok := pins[s.path]
			if !ok {
				continue // no edge (unresolved, or a missing blob): no row
			}
			r.TargetBlob, r.TargetCommit = p.blob, p.commit
		}
		out = append(out, r)
	}
	return out, nil
}

// storedRefRows is the rows ref_edges holds for blob, in ord order.
func storedRefRows(ctx context.Context, ex storegit.CtxExecer, blob string) ([]RefEdgeRow, error) {
	rows, err := ex.QueryContext(ctx, `
		SELECT ord, target_repo, target_path, COALESCE(target_blob, ''), COALESCE(target_commit, '')
		  FROM ref_edges WHERE source_blob = ? ORDER BY ord`, blob)
	if err != nil {
		return nil, fmt.Errorf("ref_edges: rows of %s: %w", blob, err)
	}
	defer rows.Close()
	var out []RefEdgeRow
	for rows.Next() {
		r := RefEdgeRow{SourceBlob: blob}
		if err := rows.Scan(&r.Ord, &r.Repo, &r.Path, &r.TargetBlob, &r.TargetCommit); err != nil {
			return nil, fmt.Errorf("ref_edges: rows of %s: %w", blob, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func sameRefRows(a, b []RefEdgeRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// refRowsCurrent reports whether the blob's stored rows are exactly the rows
// its edges and refs give (read-only; the no-op check of applyBlobRefs).
func refRowsCurrent(ctx context.Context, ex storegit.CtxExecer, node int64, blob string, specs []refSpec) (bool, error) {
	want, err := expectedRefRows(ctx, ex, node, blob, specs)
	if err != nil {
		return false, err
	}
	have, err := storedRefRows(ctx, ex, blob)
	if err != nil {
		return false, err
	}
	return sameRefRows(want, have), nil
}

// writeRefRowsTx makes the blob's rows exactly what its edges (on node) and
// its refs give, replacing whatever was there. Called inside the transaction
// that wrote the claim and the edges, so the rows never lag them; rows that
// already match are left untouched (no write).
func writeRefRowsTx(ctx context.Context, tx storegit.CtxExecer, node int64, blob string, specs []refSpec) error {
	want, err := expectedRefRows(ctx, tx, node, blob, specs)
	if err != nil {
		return err
	}
	have, err := storedRefRows(ctx, tx, blob)
	if err != nil {
		return err
	}
	if sameRefRows(want, have) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ref_edges WHERE source_blob = ?`, blob); err != nil {
		return fmt.Errorf("ref_edges: clear %s: %w", blob, err)
	}
	for _, r := range want {
		var tb, tc any
		if r.Repo == "" {
			tb, tc = r.TargetBlob, r.TargetCommit
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ref_edges(source_blob, ord, target_repo, target_path, target_blob, target_commit) VALUES (?, ?, ?, ?, ?, ?)`,
			blob, r.Ord, r.Repo, r.Path, tb, tc); err != nil {
			return fmt.Errorf("ref_edges: write %s ord %d: %w", blob, r.Ord, err)
		}
	}
	return nil
}

// refEdgesOutOfBlob is the outgoing reference read of a version: its local
// rows in the author's order, each described from the target's side (its
// graph Fact node at (target_path, target_blob): title, type, deleted) with
// the pinned target_commit. A row whose target node is missing is skipped, as
// the edge read skipped an edge with no target node.
func refEdgesOutOfBlob(ctx context.Context, ex storegit.CtxExecer, blob string) ([]RefSummary, error) {
	rows, err := ex.QueryContext(ctx, `
		WITH k AS (SELECT (SELECT id FROM property_keys WHERE key = 'path')      AS kp,
		                  (SELECT id FROM property_keys WHERE key = 'blob_hash') AS kb,
		                  (SELECT id FROM property_keys WHERE key = 'title')     AS kt,
		                  (SELECT id FROM property_keys WHERE key = 'type')      AS kty,
		                  (SELECT id FROM property_keys WHERE key = 'deleted')   AS kd),
		     r AS (SELECT r.ord, r.target_path, COALESCE(r.target_commit, '') AS tc,
		                  (SELECT nb.node_id
		                     FROM node_props_text nb
		                     JOIN node_props_text np ON np.node_id = nb.node_id
		                          AND np.key_id = k.kp AND np.value = r.target_path
		                    WHERE nb.key_id = k.kb AND nb.value = r.target_blob
		                    ORDER BY nb.node_id LIMIT 1) AS tn
		             FROM ref_edges r CROSS JOIN k
		            WHERE r.source_blob = ? AND r.target_repo = '')
		SELECT r.target_path,
		       COALESCE((SELECT value FROM node_props_text WHERE node_id = r.tn AND key_id = k.kt), ''),
		       COALESCE((SELECT value FROM node_props_text WHERE node_id = r.tn AND key_id = k.kty), ''),
		       r.tc,
		       COALESCE((SELECT value FROM node_props_text WHERE node_id = r.tn AND key_id = k.kd), '')
		  FROM r CROSS JOIN k
		 WHERE r.tn IS NOT NULL
		 ORDER BY r.ord`, blob)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefSummary
	for rows.Next() {
		var rs RefSummary
		var deleted string
		if err := rows.Scan(&rs.Path, &rs.Title, &rs.Type, &rs.Commit, &deleted); err != nil {
			return nil, err
		}
		rs.Deleted = deleted == "true"
		out = append(out, rs)
	}
	return out, rows.Err()
}

// refEdgesChunk is how many source blobs one RefEdgesOfBlobs statement binds
// (one variable each; SQLite's limit is 32766). A variable so a test can
// lower it.
var refEdgesChunk = 4000

// RefEdgesOfBlobs is the batched level read of a reference walk: every
// ref_edges row of a SET of citing blobs, in (source_blob, ord) order, with
// ONE set-based statement per chunk of blobs (a covering range scan of the
// primary key per blob). Duplicate and empty blobs are ignored. It returns
// every row (cross-KB included) and leaves the policy to the caller; it reads
// no git and no graph nodes.
func (gs *GraphStore) RefEdgesOfBlobs(ctx context.Context, blobs []string) ([]RefEdgeRow, error) {
	if err := gs.probe("RefEdgesOfBlobs"); err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(blobs))
	keys := make([]string, 0, len(blobs))
	for _, b := range blobs {
		if b == "" {
			continue
		}
		if _, dup := set[b]; dup {
			continue
		}
		set[b] = struct{}{}
		keys = append(keys, b)
	}
	// Sorted, so the chunks' outputs concatenate in (source_blob, ord) order.
	sort.Strings(keys)
	chunk := max(refEdgesChunk, 1)
	var out []RefEdgeRow
	db := conn(ctx, gs.rh.DB())
	for start := 0; start < len(keys); start += chunk {
		part := keys[start:min(start+chunk, len(keys))]
		args := make([]any, len(part))
		for i, b := range part {
			args[i] = b
		}
		rows, err := db.QueryContext(ctx, `
			SELECT source_blob, ord, target_repo, target_path, COALESCE(target_blob, ''), COALESCE(target_commit, '')
			  FROM ref_edges
			 WHERE source_blob IN (`+strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")+`)
			 ORDER BY source_blob, ord`, args...)
		if err != nil {
			return nil, fmt.Errorf("RefEdgesOfBlobs: %w", err)
		}
		for rows.Next() {
			var r RefEdgeRow
			if err := rows.Scan(&r.SourceBlob, &r.Ord, &r.Repo, &r.Path, &r.TargetBlob, &r.TargetCommit); err != nil {
				rows.Close()
				return nil, fmt.Errorf("RefEdgesOfBlobs: %w", err)
			}
			out = append(out, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("RefEdgesOfBlobs: %w", err)
		}
	}
	return out, nil
}

// ---- drift, repair, backfill ---------------------------------------------

// refEdgeDriftSQL lists the (kind, source_blob, target_path) of every local
// ref_edges row that disagrees with the DERIVED_FROM edges, compared as sets
// of (source blob, target path, target blob, target_commit):
//   - 'missing': an edge with no identical row;
//   - 'orphan':  a local row with no identical edge.
const refEdgeDriftSQL = `
	WITH k AS (SELECT (SELECT id FROM property_keys WHERE key = 'path')          AS kp,
	                  (SELECT id FROM property_keys WHERE key = 'blob_hash')     AS kb,
	                  (SELECT id FROM property_keys WHERE key = 'target_commit') AS ktc),
	     e AS (SELECT sb.value AS b, tp.value AS p, tb.value AS t, COALESCE(tc.value, '') AS c
	             FROM edges e CROSS JOIN k
	             JOIN node_props_text sb ON sb.node_id = e.source_id AND sb.key_id = k.kb
	             JOIN node_props_text tp ON tp.node_id = e.target_id AND tp.key_id = k.kp
	             JOIN node_props_text tb ON tb.node_id = e.target_id AND tb.key_id = k.kb
	             LEFT JOIN edge_props_text tc ON tc.edge_id = e.id AND tc.key_id = k.ktc
	            WHERE e.type = 'DERIVED_FROM'),
	     r AS (SELECT source_blob AS b, target_path AS p, COALESCE(target_blob, '') AS t, COALESCE(target_commit, '') AS c
	             FROM ref_edges WHERE target_repo = '')
	SELECT 'missing', b, p FROM (SELECT b, p, t, c FROM e EXCEPT SELECT b, p, t, c FROM r)
	UNION ALL
	SELECT 'orphan', b, p FROM (SELECT b, p, t, c FROM r EXCEPT SELECT b, p, t, c FROM e)`

// RefEdgeDrift is how ref_edges disagrees with the DERIVED_FROM edges it
// mirrors. MissingRows: edges with no identical local row. OrphanRows: local
// rows with no identical edge. Blobs: the distinct source blobs involved.
// Examples: a few of each, for the report.
type RefEdgeDrift struct {
	MissingRows int
	OrphanRows  int
	Blobs       []string
	Examples    []string
}

// RefEdgeDrift compares ref_edges with the DERIVED_FROM edges (one set-based
// statement). It is Verify's check (fsck category ref-edges) and the input of
// a rebuild's repair.
func (si *Index) RefEdgeDrift(ctx context.Context) (RefEdgeDrift, error) {
	var d RefEdgeDrift
	rows, err := conn(ctx, si.rh.DB()).QueryContext(ctx, refEdgeDriftSQL)
	if err != nil {
		return d, fmt.Errorf("RefEdgeDrift: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var kind, blob, path string
		if err := rows.Scan(&kind, &blob, &path); err != nil {
			return d, fmt.Errorf("RefEdgeDrift: %w", err)
		}
		if kind == "missing" {
			d.MissingRows++
		} else {
			d.OrphanRows++
		}
		if !seen[blob] {
			seen[blob] = true
			d.Blobs = append(d.Blobs, blob)
		}
		if len(d.Examples) < 6 {
			d.Examples = append(d.Examples, fmt.Sprintf("%s: blob %.12s -> %s", kind, blob, path))
		}
	}
	if err := rows.Err(); err != nil {
		return d, fmt.Errorf("RefEdgeDrift: %w", err)
	}
	sort.Strings(d.Blobs)
	return d, nil
}

// refRepairBatch is how many blobs one repair transaction rewrites.
const refRepairBatch = 500

// repairRefBlobs rewrites the rows of each blob from its refs (the blob parsed
// from `objects`; none when the blob is missing, so its rows go) and the edges
// on its claim node. Reads (blob bytes, parse) happen outside the write
// transactions; each batch of blobs is one transaction, and each blob's rows
// are replaced atomically, so a crash leaves every blob either old or new.
// The claim node is read inside the transaction, so a concurrent claim move
// is never undone. Returns how many blobs' rows changed.
//
// dropStray (the rebuild's repair, not the heal's backfill): first, in the
// same transaction, delete every DERIVED_FROM edge out of any node of the
// blob whose target path is not a local ref of the PARSED blob. Such an edge
// cannot come from the write path (corruption only); an edge that should not
// be there is removed rather than reported forever (user ruling, PR #694).
// A blob that is missing from `objects` or does not parse drops nothing: its
// refs are unknown (missing-blob edges are the dangling-edge drop's job).
func (si *Index) repairRefBlobs(ctx context.Context, branch string, blobs []string, dropStray bool) (int, error) {
	if len(blobs) == 0 {
		return 0, nil
	}
	repoID := si.localRepoID(ctx, branch)
	db := si.rh.DB()
	changed := 0
	for start := 0; start < len(blobs); start += refRepairBatch {
		part := blobs[start:min(start+refRepairBatch, len(blobs))]
		specs := make(map[string][]refSpec, len(part))
		parsed := make(map[string]bool, len(part))
		for _, b := range part {
			s, ok, err := si.blobRefSpecs(ctx, b, repoID)
			if err != nil {
				return changed, err
			}
			specs[b], parsed[b] = s, ok
		}
		tctx, tx, own, err := beginTxIfNeeded(ctx, db)
		if err != nil {
			return changed, fmt.Errorf("ref_edges repair: begin: %w", err)
		}
		n, err := func() (int, error) {
			ex := conn(tctx, db)
			n := 0
			for _, b := range part {
				if dropStray && parsed[b] {
					if err := dropStrayRefEdgesTx(tctx, ex, b, specs[b]); err != nil {
						return n, err
					}
				}
				node, err := refsNodeOfBlob(tctx, ex, b)
				if err != nil {
					return n, err
				}
				before, err := storedRefRows(tctx, ex, b)
				if err != nil {
					return n, err
				}
				if err := writeRefRowsTx(tctx, ex, node, b, specs[b]); err != nil {
					return n, err
				}
				after, err := storedRefRows(tctx, ex, b)
				if err != nil {
					return n, err
				}
				if !sameRefRows(before, after) {
					n++
				}
			}
			return n, nil
		}()
		if err != nil {
			if own {
				tx.Rollback() //nolint:errcheck
			}
			return changed, err
		}
		if own {
			if err := tx.Commit(); err != nil {
				return changed, fmt.Errorf("ref_edges repair: commit: %w", err)
			}
		}
		changed += n
	}
	return changed, nil
}

// blobRefSpecs is the fact refs of a blob read from `objects` and parsed (no
// tree walk). A blob missing from `objects`, or not a parsable fact, has none,
// and ok=false says its refs are unknown.
func (si *Index) blobRefSpecs(ctx context.Context, blob, repoID string) (specs []refSpec, ok bool, err error) {
	var data []byte
	err = conn(ctx, si.rh.DB()).QueryRowContext(ctx,
		`SELECT data FROM objects WHERE hash = ? AND type = ?`, blob, vcs.BlobObjectType).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("ref_edges: read blob %s: %w", blob, err)
	}
	rec, err := facts.ParseFactRecord("kb/x.md", string(data))
	if err != nil {
		log.Debug().Err(err).Str("blob", blob).Msg("ref_edges: blob is not a parsable fact; no rows")
		return nil, false, nil
	}
	return refSpecsOf(rec.Refs, repoID), true, nil
}

// dropStrayRefEdgesTx deletes the DERIVED_FROM edges out of any node of blob
// whose target path is not one of the blob's local refs (specs). Called inside
// the repair transaction, before the blob's rows are rewritten from its edges.
func dropStrayRefEdgesTx(ctx context.Context, ex storegit.CtxExecer, blob string, specs []refSpec) error {
	local := map[string]bool{}
	for _, s := range specs {
		if s.repo == "" {
			local[s.path] = true
		}
	}
	rows, err := ex.QueryContext(ctx, `
		SELECT e.id, tp.value
		  FROM node_props_text nb
		  JOIN edges e ON e.source_id = nb.node_id AND e.type = ?
		  JOIN node_props_text tp ON tp.node_id = e.target_id
		       AND tp.key_id = (SELECT id FROM property_keys WHERE key = 'path')
		 WHERE nb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash') AND nb.value = ?`,
		EdgeDerivedFrom, blob)
	if err != nil {
		return fmt.Errorf("ref_edges: edges of %s: %w", blob, err)
	}
	var stray []int64
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return fmt.Errorf("ref_edges: edges of %s: %w", blob, err)
		}
		if !local[p] {
			stray = append(stray, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("ref_edges: edges of %s: %w", blob, err)
	}
	for _, id := range stray {
		if _, err := ex.ExecContext(ctx, `DELETE FROM edges WHERE id = ?`, id); err != nil {
			return fmt.Errorf("ref_edges: drop stray edge %d of %s: %w", id, blob, err)
		}
		log.Info().Str("blob", blob).Int64("edge", id).Msg("ref_edges repair: dropped a reference edge whose target is not among the blob's refs")
	}
	return nil
}

// refsNodeOfBlob is the node that carries the blob's reference edges: the
// node holding its refs_commit claim, else (a store whose edges predate the
// claims) the lowest node of the blob with a DERIVED_FROM edge out; 0 when
// none.
func refsNodeOfBlob(ctx context.Context, ex storegit.CtxExecer, blob string) (int64, error) {
	var node int64
	err := ex.QueryRowContext(ctx, `
		SELECT nb.node_id
		  FROM node_props_text nb
		 WHERE nb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash') AND nb.value = ?
		 ORDER BY EXISTS (SELECT 1 FROM node_props_text rc WHERE rc.node_id = nb.node_id
		                     AND rc.key_id = (SELECT id FROM property_keys WHERE key = ?)) DESC,
		          EXISTS (SELECT 1 FROM edges e WHERE e.source_id = nb.node_id AND e.type = ?) DESC,
		          nb.node_id
		 LIMIT 1`, blob, refsCommitKey, EdgeDerivedFrom).Scan(&node)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("ref_edges: node of blob %s: %w", blob, err)
	}
	return node, nil
}

// repairRefEdges rewrites the rows of every blob RefEdgeDrift names. A rebuild
// runs it after re-resolving its edges, so drift between ref_edges and the
// edges does not outlive a rebuild. Returns how many blobs changed.
func (si *Index) repairRefEdges(ctx context.Context, branch string) (int, error) {
	d, err := si.RefEdgeDrift(ctx)
	if err != nil {
		return 0, err
	}
	if len(d.Blobs) == 0 {
		return 0, nil
	}
	n, err := si.repairRefBlobs(ctx, branch, d.Blobs, true)
	if err != nil {
		return n, err
	}
	log.Info().Int("blobs", n).Int("missing", d.MissingRows).Int("orphan", d.OrphanRows).
		Msg("ref_edges: repaired drift from DERIVED_FROM edges")
	return n, nil
}

// refEdgesBackfillKey is the meta sentinel of the one-time fill of ref_edges
// for a store indexed before the table existed.
const refEdgesBackfillKey = "ref_edges_backfilled"

// BackfillRefEdges fills ref_edges ONCE for a store indexed before it existed
// (meta sentinel ref_edges_backfilled). It rewrites the rows of (a) every
// blob whose local rows disagree with its DERIVED_FROM edges (on a store
// with no rows yet: every blob with edges) and (b) every Fact-node blob whose
// bytes mention kb:// and that has no row yet (cross-KB refs have no edge to
// find them by). Order comes from the blob parsed out of `objects` (no git
// tree walk; many versions have no facts row). Crash-safe: blob batches commit
// separately and are idempotent, and the sentinel is set only at the end, so
// an interrupted backfill is simply run again. Concurrent writers are safe:
// each blob's rows are derived from its edges inside the transaction that
// replaces them. branch only names the branch whose root commit gives this
// repo's id. Returns how many blobs' rows changed.
func (si *Index) BackfillRefEdges(ctx context.Context, branch string) (int, error) {
	db := si.rh.DB()
	var done string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, refEdgesBackfillKey).Scan(&done)
	if err == nil && done == "1" {
		return 0, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("BackfillRefEdges: %w", err)
	}
	if si.localRepoID(ctx, branch) == "" {
		// Without the repo id every kb:// ref would read as cross-KB: wrong
		// rows. Leave the sentinel unset; the next open retries.
		return 0, fmt.Errorf("BackfillRefEdges: repo id unresolved")
	}
	d, err := si.RefEdgeDrift(ctx)
	if err != nil {
		return 0, fmt.Errorf("BackfillRefEdges: %w", err)
	}
	set := map[string]bool{}
	for _, b := range d.Blobs {
		set[b] = true
	}
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT nb.value
		  FROM node_props_text nb
		  JOIN objects o ON o.hash = nb.value AND o.type = ?
		 WHERE nb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash')
		   AND instr(o.data, 'kb://') > 0
		   AND NOT EXISTS (SELECT 1 FROM ref_edges r WHERE r.source_blob = nb.value)`, vcs.BlobObjectType)
	if err != nil {
		return 0, fmt.Errorf("BackfillRefEdges: cross-KB candidates: %w", err)
	}
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			rows.Close()
			return 0, fmt.Errorf("BackfillRefEdges: %w", err)
		}
		set[b] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("BackfillRefEdges: %w", err)
	}
	blobs := make([]string, 0, len(set))
	for b := range set {
		blobs = append(blobs, b)
	}
	sort.Strings(blobs)
	n, err := si.repairRefBlobs(ctx, branch, blobs, false)
	if err != nil {
		return n, fmt.Errorf("BackfillRefEdges: %w", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES (?, '1')`, refEdgesBackfillKey); err != nil {
		return n, fmt.Errorf("BackfillRefEdges: sentinel: %w", err)
	}
	log.Info().Int("blobs", n).Int("candidates", len(blobs)).Msg("ref_edges: backfill complete")
	return n, nil
}
