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

// refEdgesIncomingSQL is refEdgesIncomingRows's statement (refs_commit key,
// cited blob).
const refEdgesIncomingSQL = `
		WITH k AS (SELECT (SELECT id FROM property_keys WHERE key = 'path')      AS kp,
		                  (SELECT id FROM property_keys WHERE key = 'blob_hash') AS kb,
		                  (SELECT id FROM property_keys WHERE key = 'title')     AS kt,
		                  (SELECT id FROM property_keys WHERE key = 'type')      AS kty,
		                  (SELECT id FROM property_keys WHERE key = 'deleted')   AS kd,
		                  (SELECT id FROM property_keys WHERE key = ?)           AS krc),
		     s AS (SELECT DISTINCT r.source_blob AS sb
		             FROM ref_edges r
		            WHERE r.target_blob = ?)
		SELECT fb.node_id, s.sb, op.value,
		       COALESCE((SELECT value FROM node_props_text WHERE node_id = fb.node_id AND key_id = k.kt), ''),
		       COALESCE((SELECT value FROM node_props_text WHERE node_id = fb.node_id AND key_id = k.kty), ''),
		       rc.value,
		       COALESCE((SELECT value FROM node_props_text WHERE node_id = fb.node_id AND key_id = k.kd), '')
		  FROM k CROSS JOIN s
		  CROSS JOIN node_props_text fb ON fb.key_id = k.kb AND fb.value = s.sb
		  JOIN node_props_text op ON op.node_id = fb.node_id AND op.key_id = k.kp
		  LEFT JOIN node_props_text rc ON rc.node_id = fb.node_id AND rc.key_id = k.krc`

// refEdgesIncomingRows is the incoming reference read of a version from
// ref_edges: every blob with a local row citing blobHash (through
// idx_ref_edges_target), described by EVERY Fact node of that citing blob
// (its claim node and any other path it has been at: a rename), one row per
// node. Own marks the citing blob's claim node (where #693 keeps its edges,
// the edge read's "own" node); Commit is the claim's refs_commit, the edge
// read's source_commit. The claim is chosen in Go, per citing blob, as the
// lowest node id carrying refs_commit (readBlobClaim's rule), from a LEFT JOIN
// on each node's refs_commit; the join order is pinned (k, then the citing
// blobs, then their nodes) so the node probe uses (key_id, value) instead of
// a skip-scan (PR #694 review round 2, P1). There is no target_repo
// filter: only local rows carry a target_blob (writeRefRowsTx writes NULL for
// a cross-KB row, and the partial index holds no NULL), so the probe is a
// COVERING read of idx_ref_edges_target (target_blob, then the primary key)
// with no table lookup per row. The order is left to the caller
// (sortIncomingCandidates).
func refEdgesIncomingRows(ctx context.Context, ex storegit.CtxExecer, blobHash string) ([]derivedFromNeighbour, error) {
	rows, err := ex.QueryContext(ctx, refEdgesIncomingSQL, refsCommitKey, blobHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []derivedFromNeighbour
	type claim struct {
		node   int64
		commit string
	}
	claims := map[string]claim{}
	for rows.Next() {
		var r derivedFromNeighbour
		var deleted string
		var rc sql.NullString
		if err := rows.Scan(&r.NodeID, &r.Blob, &r.Path, &r.Title, &r.Type, &rc, &deleted); err != nil {
			return nil, err
		}
		if rc.Valid {
			if c, ok := claims[r.Blob]; !ok || r.NodeID < c.node {
				claims[r.Blob] = claim{r.NodeID, rc.String}
			}
		}
		if r.Path == "" {
			continue
		}
		r.Deleted = deleted == "true"
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		c := claims[out[i].Blob]
		out[i].Own = c.node != 0 && out[i].NodeID == c.node
		out[i].Commit = c.commit
	}
	return out, nil
}

// sortIncomingCandidates puts incoming candidate rows in the documented order
// (the same for the ref_edges read and the edge read): by the citing
// version's source commit in commitEarlier's TOTAL order (committed_at, then
// the number of same-second ancestors, then the hash; a commit with no
// readable time last), then the citing blob, then the citer's path. It is
// chronological in the citing version (so it matches write order, as the
// edge read's edge-id order mostly did) and depends on no row id, so a
// rebuild does not reorder it. The same-second ancestor count is computed
// only for commits that tie on committed_at with another one in the list.
func sortIncomingCandidates(ctx context.Context, rh *vcs.Repo, rows []derivedFromNeighbour) error {
	type key struct {
		ok   bool
		t    int64
		anc  int
		hash string
	}
	keys := map[string]*key{}
	perSecond := map[int64]int{}
	for _, r := range rows {
		if _, done := keys[r.Commit]; done {
			continue
		}
		k := &key{hash: r.Commit}
		if r.Commit != "" {
			t, ok, err := commitTimeOf(ctx, rh, r.Commit)
			if err != nil {
				return fmt.Errorf("incoming order: %w", err)
			}
			k.t, k.ok = t, ok
			if ok {
				perSecond[t]++
			}
		}
		keys[r.Commit] = k
	}
	for _, k := range keys {
		if k.ok && perSecond[k.t] > 1 {
			n, err := sameSecondAncestorsOf(ctx, rh, k.hash, k.t)
			if err != nil {
				return fmt.Errorf("incoming order: %w", err)
			}
			k.anc = n
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := keys[rows[i].Commit], keys[rows[j].Commit]
		switch {
		case a.ok != b.ok:
			return a.ok
		case a.t != b.t:
			return a.t < b.t
		case a.anc != b.anc:
			return a.anc < b.anc
		case a.hash != b.hash:
			return a.hash < b.hash
		case rows[i].Blob != rows[j].Blob:
			return rows[i].Blob < rows[j].Blob
		}
		return rows[i].Path < rows[j].Path
	})
	return nil
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
	db := conn(ctx, gs.rh.DB())
	if !refEdgesReady(ctx, db) {
		return gs.derivedRefRows(ctx, db, keys)
	}
	chunk := max(refEdgesChunk, 1)
	var out []RefEdgeRow
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

// ---- before the backfill: the read falls back to the edges ---------------

// refEdgesReady reports whether ref_edges is complete for this store: the
// backfill sentinel is set (by BackfillRefEdges, or by migration 000035 on a
// store with no Fact node yet). It is cleared by the #693 v8 reset. Until it
// is set, the readers derive the rows from the edges and the blobs instead
// (PR #694 review N2: a store upgraded from before ref_edges serves while
// the heal backfills). One primary-key read; an error counts as not ready
// (the fallback is correct, only slower).
func refEdgesReady(ctx context.Context, ex storegit.CtxExecer) bool {
	var v string
	err := ex.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, refEdgesBackfillKey).Scan(&v)
	return err == nil && v == "1"
}

// repoIDOf is this repo's 12-hex id, from branch's root commit, or from the
// first indexed branch when branch is "" ("" when neither resolves).
func (gs *GraphStore) repoIDOf(ctx context.Context, ex storegit.CtxExecer, branch string) string {
	if branch == "" {
		if err := ex.QueryRowContext(ctx, `SELECT name FROM branches ORDER BY id LIMIT 1`).Scan(&branch); err != nil {
			return ""
		}
	}
	lookup := gs.rh.RootCommit
	if f := gs.rootCommit.Load(); f != nil {
		lookup = *f
	}
	root, err := lookup(ctx, branch)
	if err != nil {
		return ""
	}
	return fact.ID12(root)
}

// derivedRefRows is what ref_edges holds (or will hold once backfilled) for
// each blob, derived exactly as the backfill derives it: the blob's refs
// parsed from `objects`, and the edges on its refs node (expectedRefRows).
// keys are sorted; the rows come back in (source_blob, ord) order.
//
// With the repo id unresolved it does not fail (as outgoing does not, PR #694
// review round 2): it learns this repo's id from a kb://<repo>/<path> ref
// whose path the blob has an edge to (selfCandidates, withSelfRepo).
func (gs *GraphStore) derivedRefRows(ctx context.Context, ex storegit.CtxExecer, keys []string) ([]RefEdgeRow, error) {
	repoID := gs.repoIDOf(ctx, ex, "")
	type blobRefs struct {
		node  int64
		specs []refSpec
		cands map[string]bool
	}
	per := make([]blobRefs, len(keys))
	batch := map[string]bool{}
	for i, b := range keys {
		specs, _, err := blobRefSpecsOf(ctx, ex, b, repoID)
		if err != nil {
			return nil, fmt.Errorf("RefEdgesOfBlobs: %w", err)
		}
		node, err := refsNodeOfBlob(ctx, ex, b)
		if err != nil {
			return nil, fmt.Errorf("RefEdgesOfBlobs: %w", err)
		}
		per[i] = blobRefs{node: node, specs: specs, cands: map[string]bool{}}
		if repoID == "" {
			edged, err := edgePathsOfNode(ctx, ex, node)
			if err != nil {
				return nil, fmt.Errorf("RefEdgesOfBlobs: %w", err)
			}
			selfCandidates(specs, edged, per[i].cands)
			selfCandidates(specs, edged, batch)
		}
	}
	// The self id is learned over the whole batch (one blob's kb://self ref
	// with an edge tells it for the others), else per blob.
	batchSelf := onlyCandidate(batch)
	var out []RefEdgeRow
	for i, b := range keys {
		node, specs := per[i].node, per[i].specs
		if repoID == "" {
			self := batchSelf
			if self == "" {
				self = onlyCandidate(per[i].cands)
			}
			specs = withSelfRepo(specs, self)
		}
		rows, err := expectedRefRows(ctx, ex, node, b, specs)
		if err != nil {
			return nil, fmt.Errorf("RefEdgesOfBlobs: %w", err)
		}
		out = append(out, rows...)
	}
	return out, nil
}

// outgoingByEdgesInRefsOrder is the outgoing read before the backfill: the
// DERIVED_FROM edge read, put in the author's refs order (the blob parsed
// from `objects`), so it equals the ref_edges read the backfill makes
// possible. With the repo id unresolved, kb://<self>/ refs are recognised by
// their edges (selfCandidates, withSelfRepo); when the blob is unavailable it keeps
// edge order (the set is still right).
func (gs *GraphStore) outgoingByEdgesInRefsOrder(ctx context.Context, ex storegit.CtxExecer, branch, path, blob string) ([]RefSummary, error) {
	found, err := graphDerivedFromNeighbours(ctx, ex, path, blob, false)
	if err != nil {
		return nil, err
	}
	repoID := gs.repoIDOf(ctx, ex, branch)
	specs, _, err := blobRefSpecsOf(ctx, ex, blob, repoID)
	if err != nil {
		return nil, err
	}
	if repoID == "" {
		node, err := refsNodeOfBlob(ctx, ex, blob)
		if err != nil {
			return nil, err
		}
		edged, err := edgePathsOfNode(ctx, ex, node)
		if err != nil {
			return nil, err
		}
		cands := map[string]bool{}
		selfCandidates(specs, edged, cands)
		specs = withSelfRepo(specs, onlyCandidate(cands))
	}
	ord := map[string]int{}
	for _, s := range specs {
		if s.repo == "" {
			ord[s.path] = s.ord
		}
	}
	at := func(p string) int {
		if o, ok := ord[p]; ok {
			return o
		}
		return int(^uint(0) >> 1)
	}
	sort.SliceStable(found, func(i, j int) bool { return at(found[i].Path) < at(found[j].Path) })
	return found, nil
}

// The fallback reads' answer to an unresolved repo id (specs built with ""
// classify kb://<self>/ refs as cross-KB). The self id is LEARNED, not
// guessed per ref (PR #694 review R3-N1):
//   - a candidate is the repo R of a cross-KB spec kb://R/p whose blob has a
//     DERIVED_FROM edge to p (only local refs form edges) and whose blob has
//     NO explicit local spec for p (else the edge is the local spec's, and
//     kb://R/p may well be another KB's fact at the same path);
//   - with exactly one candidate R, R is this repo: every spec of R becomes
//     local, at its own position (a later duplicate of a path already local
//     is dropped). A kb://R/q ref with no edge (dropped as dangling) then
//     comes out with NO row, as the table has none for a local ref without an
//     edge, instead of a cross-KB row naming this repo;
//   - with no candidate, or several (ambiguous), nothing is converted: a
//     kb://<self>/ ref then reads as cross-KB, the best the blob tells.
// Only the READ fallbacks use it; the write path never builds rows without
// the id.

// edgePathsOfNode is the target paths of a node's DERIVED_FROM edges.
func edgePathsOfNode(ctx context.Context, ex storegit.CtxExecer, node int64) (map[string]bool, error) {
	edged := map[string]bool{}
	if node == 0 {
		return edged, nil
	}
	rows, err := ex.QueryContext(ctx, `
		SELECT DISTINCT tp.value FROM edges e
		  JOIN node_props_text tp ON tp.node_id = e.target_id
		       AND tp.key_id = (SELECT id FROM property_keys WHERE key = 'path')
		 WHERE e.source_id = ? AND e.type = ?`, node, EdgeDerivedFrom)
	if err != nil {
		return nil, fmt.Errorf("edge paths of node %d: %w", node, err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		edged[p] = true
	}
	return edged, rows.Err()
}

// selfCandidates adds to cands the repos that could be this one, per the
// rule above, for one blob's specs and edge paths.
func selfCandidates(specs []refSpec, edged map[string]bool, cands map[string]bool) {
	local := map[string]bool{}
	for _, s := range specs {
		if s.repo == "" {
			local[s.path] = true
		}
	}
	for _, s := range specs {
		if s.repo != "" && edged[s.path] && !local[s.path] {
			cands[s.repo] = true
		}
	}
}

// onlyCandidate is the single candidate, or "" when there is none or several.
func onlyCandidate(cands map[string]bool) string {
	if len(cands) != 1 {
		return ""
	}
	for r := range cands {
		return r
	}
	return ""
}

// withSelfRepo makes every spec of selfID local (selfID "" changes nothing),
// keeping positions and dropping a later duplicate of a local path.
func withSelfRepo(specs []refSpec, selfID string) []refSpec {
	out := make([]refSpec, 0, len(specs))
	local := map[string]bool{}
	for _, s := range specs {
		if selfID != "" && s.repo == selfID {
			s.repo = ""
		}
		if s.repo == "" {
			if local[s.path] {
				continue
			}
			local[s.path] = true
		}
		out = append(out, s)
	}
	return out
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
	if repoID == "" {
		// Without the repo id every kb://<self>/ ref reads as cross-KB: the
		// rows would be wrong and the stray-edge drop would delete VALID
		// edges (PR #694 review F1). Skip the whole repair, as the backfill
		// does; localRepoID retries on the next call.
		log.Warn().Str("branch", branch).Int("blobs", len(blobs)).
			Msg("ref_edges repair skipped: repo id unresolved")
		return 0, nil
	}
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
				node, err := refsNodeOfBlob(tctx, ex, b)
				if err != nil {
					return n, err
				}
				if dropStray && parsed[b] {
					if err := dropStrayRefEdgesTx(tctx, ex, b, node, specs[b]); err != nil {
						return n, err
					}
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
	return blobRefSpecsOf(ctx, conn(ctx, si.rh.DB()), blob, repoID)
}

// blobRefSpecsOf is blobRefSpecs over an execer.
func blobRefSpecsOf(ctx context.Context, ex storegit.CtxExecer, blob, repoID string) (specs []refSpec, ok bool, err error) {
	var data []byte
	err = ex.QueryRowContext(ctx,
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

// dropStrayRefEdgesTx deletes, out of any node of blob, every DERIVED_FROM
// edge that should not be there:
//   - its target path is not one of the blob's local refs (specs);
//   - it is not on the blob's refs node (node: the claim node, where #693
//     keeps a blob's edges);
//   - it is a second edge to a path the refs node already has an edge to
//     (the first by edge id is kept: the one expectedRefRows builds the row
//     from), so the repair converges to one edge per (blob, path).
//
// Called inside the repair transaction, before the blob's rows are rewritten
// from its edges.
func dropStrayRefEdgesTx(ctx context.Context, ex storegit.CtxExecer, blob string, node int64, specs []refSpec) error {
	local := map[string]bool{}
	for _, s := range specs {
		if s.repo == "" {
			local[s.path] = true
		}
	}
	rows, err := ex.QueryContext(ctx, `
		SELECT e.id, e.source_id, tp.value
		  FROM node_props_text nb
		  JOIN edges e ON e.source_id = nb.node_id AND e.type = ?
		  JOIN node_props_text tp ON tp.node_id = e.target_id
		       AND tp.key_id = (SELECT id FROM property_keys WHERE key = 'path')
		 WHERE nb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash') AND nb.value = ?
		 ORDER BY e.id`,
		EdgeDerivedFrom, blob)
	if err != nil {
		return fmt.Errorf("ref_edges: edges of %s: %w", blob, err)
	}
	var stray []int64
	kept := map[string]bool{}
	for rows.Next() {
		var id, src int64
		var p string
		if err := rows.Scan(&id, &src, &p); err != nil {
			rows.Close()
			return fmt.Errorf("ref_edges: edges of %s: %w", blob, err)
		}
		if !local[p] || src != node || kept[p] {
			stray = append(stray, id)
			continue
		}
		kept[p] = true
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
		log.Info().Str("blob", blob).Int64("edge", id).Msg("ref_edges repair: dropped a reference edge that should not be there (target not among the blob's refs, off the refs node, or a second edge to one path)")
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
