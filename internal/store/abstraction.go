package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// abstractionIndex implements AbstractionIndex: the title-embedding axis and
// the restatement shortlist built on it.
//
// Both are derived state consumed EXCLUSIVELY by the review pipeline. Nothing
// on the runtime paths (query / explain / learn) may reach them, which is why
// this sub-service is exposed on its own accessor rather than folded into the
// SearchIndex composite that mcp and web depend on.
//
// Like its siblings it holds only *repoHandler and never reaches sideways to
// another sub-service (invariants/store/sub-index-up-not-sideways).
type abstractionIndex struct{ rh *repoHandler }

var _ AbstractionIndex = (*abstractionIndex)(nil)

// epistemicLiveJoin is the join every query here shares: the version of each
// path that `branch` currently sees, epistemic facts only.
//
// Epistemic-only is a CORRECTNESS filter, not a preference. Prune's decision
// path does not carry Kind through mergedFact, so a pragmatic fact that reached
// synthesis would be silently rewritten as epistemic — the same reason
// reviewStrategy.AcceptSeed exists. A shortlist that offered one would corrupt
// it. The cost is a known limitation: restatements among pragmatic facts stay
// unjudged by this mechanism.
const epistemicLiveJoin = `
	FROM branch_facts bf
	JOIN branches b ON b.id = bf.branch_id
	JOIN facts f ON f.id = bf.fact_id
	WHERE b.name = ? AND f.kind = 'epistemic'`

// LiveFactsMissingTitleVector returns up to limit live epistemic facts with no
// title vector, lowest fact id first.
//
// "Rows lacking a vector" IS the backfill delta: facts rows are content-
// addressed, so editing a fact produces a new row that no vector points at.
func (ax *abstractionIndex) LiveFactsMissingTitleVector(ctx context.Context, branch string, limit int) ([]TitleTarget, error) {
	rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
		`SELECT f.id, f.path, f.title`+epistemicLiveJoin+`
		   AND NOT EXISTS (SELECT 1 FROM fact_titles_vec tv WHERE tv.rowid = f.id)
		 ORDER BY f.id ASC
		 LIMIT ?`, branch, limit)
	if err != nil {
		return nil, fmt.Errorf("abstraction: list missing title vectors: %w", err)
	}
	defer rows.Close()

	var out []TitleTarget
	for rows.Next() {
		var t TitleTarget
		if err := rows.Scan(&t.FactID, &t.Path, &t.Title); err != nil {
			return nil, fmt.Errorf("abstraction: scan title target: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PutTitleVectors stores title vectors by fact id. Delete-then-insert because
// vec0 has no upsert.
func (ax *abstractionIndex) PutTitleVectors(ctx context.Context, vecs []TitleVector) error {
	if len(vecs) == 0 {
		return nil
	}
	db := conn(ctx, ax.rh.db)
	for _, v := range vecs {
		if _, err := db.ExecContext(ctx,
			`DELETE FROM fact_titles_vec WHERE rowid = ?`, v.FactID); err != nil {
			return fmt.Errorf("abstraction: clear title vector %d: %w", v.FactID, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO fact_titles_vec(rowid, embedding) VALUES (?, ?)`,
			v.FactID, float32SliceToBytes(v.Vec)); err != nil {
			return fmt.Errorf("abstraction: put title vector %d: %w", v.FactID, err)
		}
	}
	return nil
}

// TitleVectorCoverage reports how much of the live epistemic corpus is on the
// axis. Surfaced in review health output, because a time-budgeted backfill can
// legitimately leave the axis partial for several sessions and silence would
// read as "nothing to find".
func (ax *abstractionIndex) TitleVectorCoverage(ctx context.Context, branch string) (int, int, error) {
	var have, total int
	err := conn(ctx, ax.rh.db).QueryRowContext(ctx,
		`SELECT
		     COUNT(CASE WHEN EXISTS (SELECT 1 FROM fact_titles_vec tv WHERE tv.rowid = f.id) THEN 1 END),
		     COUNT(f.id)`+epistemicLiveJoin, branch).Scan(&have, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("abstraction: title vector coverage: %w", err)
	}
	return have, total, nil
}

// LiveEpistemicFactIDs is the live set the pair cache is diffed against. The
// symmetric difference with the cached set is the delta, which is why the
// shortlist needs no watermark of its own.
func (ax *abstractionIndex) LiveEpistemicFactIDs(ctx context.Context, branch string) (map[int64]struct{}, error) {
	rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
		`SELECT f.id`+epistemicLiveJoin, branch)
	if err != nil {
		return nil, fmt.Errorf("abstraction: live fact ids: %w", err)
	}
	defer rows.Close()

	out := map[int64]struct{}{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("abstraction: scan live fact id: %w", err)
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// titleKNNOverfetch is a STRUCTURAL BUDGET: how far past k the vec0 window
// reaches so that post-filtering still yields k neighbours.
//
// It is load-bearing, not defensive. vec0 applies its k window BEFORE any WHERE
// clause (the same trap RelevantMethodologyForFact documents), so a query
// asking for exactly k and then filtering to live epistemic facts on this
// branch would come back short — or empty on a repo whose axis carries many
// superseded revisions. graphBuildSimilarityEdges pays the same tax as knnK+1.
const titleKNNOverfetch = 4

// TopTitleNeighbours returns up to k nearest live epistemic neighbours of
// factID on the abstraction axis, self excluded, most similar first.
//
// A fact with no title vector yet returns nothing rather than an error: during
// a partial backfill that is the common case, not a fault.
func (ax *abstractionIndex) TopTitleNeighbours(ctx context.Context, branch string, factID int64, k int) ([]TitleNeighbour, error) {
	if k <= 0 {
		return nil, nil
	}
	var blob []byte
	err := conn(ctx, ax.rh.db).QueryRowContext(ctx,
		`SELECT embedding FROM fact_titles_vec WHERE rowid = ?`, factID).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("abstraction: read title vector %d: %w", factID, err)
	}

	rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
		`SELECT f.id, f.path, (1.0 - tv.distance) AS similarity
		   FROM fact_titles_vec tv
		   JOIN facts f ON f.id = tv.rowid
		   JOIN branch_facts bf ON bf.fact_id = f.id
		   JOIN branches b ON b.id = bf.branch_id
		  WHERE tv.embedding MATCH ? AND tv.k = ?
		    AND b.name = ? AND f.kind = 'epistemic' AND f.id != ?
		  ORDER BY tv.distance ASC`,
		blob, k*titleKNNOverfetch, branch, factID)
	if err != nil {
		return nil, fmt.Errorf("abstraction: title knn for %d: %w", factID, err)
	}
	defer rows.Close()

	var out []TitleNeighbour
	for rows.Next() {
		if len(out) == k {
			break
		}
		var n TitleNeighbour
		var sim sql.NullFloat64
		if err := rows.Scan(&n.FactID, &n.Path, &sim); err != nil {
			return nil, fmt.Errorf("abstraction: scan title neighbour: %w", err)
		}
		// A zero-norm embedding has no meaningful similarity to anything, and
		// its NULL sorts FIRST under "ORDER BY distance ASC" — skip, never
		// treat as a perfect match.
		v, ok := usableKNNSimilarity(sim)
		if !ok {
			continue
		}
		n.Similarity = v
		out = append(out, n)
	}
	return out, rows.Err()
}

// BodyVectorsByFactID returns the STORED blended (title+body) vectors for the
// given facts. It reads facts_vec rather than re-embedding, for the same reason
// dedupCluster does (conventions/synthesize/scoped-cluster-queryby-path): every
// one of these facts is already indexed, and ONNX inference to recompute a
// vector we already hold is pure waste.
func (ax *abstractionIndex) BodyVectorsByFactID(ctx context.Context, ids []int64) (map[int64][]float32, error) {
	out := make(map[int64][]float32, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
		`SELECT rowid, embedding FROM facts_vec WHERE rowid IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("abstraction: body vectors: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("abstraction: scan body vector: %w", err)
		}
		vec, err := bytesToFloat32Slice(blob)
		if err != nil {
			return nil, fmt.Errorf("abstraction: decode body vector %d: %w", id, err)
		}
		out[id] = vec
	}
	return out, rows.Err()
}
