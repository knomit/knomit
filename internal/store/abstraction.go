package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// sqlIDChunk is a SQL PARAMETER BUDGET: how many ids are bound into one
// statement's IN clause.
//
// SQLite caps parameters per statement (32,766 on this build, 999 on older
// ones). These queries bind one placeholder per fact id — and the session that
// closes a backfill rescans the whole corpus, so it has every live id in hand
// at once. Unchunked, the feature would stop working entirely somewhere past
// ~16k facts, and would say so only in a health line. The value is deliberately
// well under the smaller historical cap.
const sqlIDChunk = 400

// chunkIDs splits ids into batches no larger than sqlIDChunk.
func chunkIDs(ids []int64) [][]int64 {
	var out [][]int64
	for start := 0; start < len(ids); start += sqlIDChunk {
		end := min(start+sqlIDChunk, len(ids))
		out = append(out, ids[start:end])
	}
	return out
}

// idPlaceholders renders "?,?,..." and the matching args for one chunk.
func idPlaceholders(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

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
	// One transaction for the batch: a backfill writes these 32 at a time, and
	// under _txlock=immediate each implicit transaction is a process-wide write
	// lock acquisition.
	ctx, tx, owned, err := beginTxIfNeeded(ctx, ax.rh.db)
	if err != nil {
		return fmt.Errorf("abstraction: begin tx: %w", err)
	}
	if owned {
		defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op
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
	if owned {
		return tx.Commit()
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

// LiveEpistemicFacts is the live set the pair cache is diffed against, keyed by
// fact id with its path. The symmetric difference with the cached set is the
// delta, which is why the shortlist needs no watermark of its own.
//
// It carries paths as well as ids because a candidate pair is keyed by path —
// that is what a prune work item talks in — and resolving them one id at a time
// afterwards would be a query per changed fact.
func (ax *abstractionIndex) LiveEpistemicFacts(ctx context.Context, branch string) (map[int64]string, error) {
	return ax.liveEpistemicFacts(ctx, branch, false)
}

// LiveEpistemicFactsOnAxis is LiveEpistemicFacts restricted to facts that
// actually carry a title vector.
//
// The distinction is load-bearing during a partial backfill. A fact with no
// vector has no neighbours to find, so treating it as "covered" would mark it
// done in the cache state and its KNN would never run — permanently, because
// the cache state is exactly what says "already covered". The refresh therefore
// diffs against THIS set and leaves un-embedded facts for a later session.
func (ax *abstractionIndex) LiveEpistemicFactsOnAxis(ctx context.Context, branch string) (map[int64]string, error) {
	return ax.liveEpistemicFacts(ctx, branch, true)
}

func (ax *abstractionIndex) liveEpistemicFacts(ctx context.Context, branch string, onAxisOnly bool) (map[int64]string, error) {
	query := `SELECT f.id, f.path` + epistemicLiveJoin
	if onAxisOnly {
		query += ` AND EXISTS (SELECT 1 FROM fact_titles_vec tv WHERE tv.rowid = f.id)`
	}
	rows, err := conn(ctx, ax.rh.db).QueryContext(ctx, query, branch)
	if err != nil {
		return nil, fmt.Errorf("abstraction: live facts: %w", err)
	}
	defer rows.Close()

	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, fmt.Errorf("abstraction: scan live fact: %w", err)
		}
		out[id] = path
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
// TitleVectorsByFactID returns STORED title-axis vectors, keyed by fact id.
//
// Symmetric with BodyVectorsByFactID, and for the same reason: the structural
// detection pass finds pairs that no KNN returned, so it has no similarity
// score in hand and must read the two vectors it already has rather than
// re-embed anything.
func (ax *abstractionIndex) TitleVectorsByFactID(ctx context.Context, ids []int64) (map[int64][]float32, error) {
	out := make(map[int64][]float32, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	for _, chunk := range chunkIDs(ids) {
		placeholders, args := idPlaceholders(chunk)
		rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
			`SELECT rowid, embedding FROM fact_titles_vec WHERE rowid IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("abstraction: title vectors: %w", err)
		}
		for rows.Next() {
			var id int64
			var blob []byte
			if err := rows.Scan(&id, &blob); err != nil {
				rows.Close()
				return nil, fmt.Errorf("abstraction: scan title vector: %w", err)
			}
			vec, err := bytesToFloat32Slice(blob)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("abstraction: decode title vector %d: %w", id, err)
			}
			out[id] = vec
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("abstraction: title vectors: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

func (ax *abstractionIndex) BodyVectorsByFactID(ctx context.Context, ids []int64) (map[int64][]float32, error) {
	out := make(map[int64][]float32, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	for _, chunk := range chunkIDs(ids) {
		placeholders, args := idPlaceholders(chunk)
		rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
			`SELECT rowid, embedding FROM facts_vec WHERE rowid IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("abstraction: body vectors: %w", err)
		}
		for rows.Next() {
			var id int64
			var blob []byte
			if err := rows.Scan(&id, &blob); err != nil {
				rows.Close()
				return nil, fmt.Errorf("abstraction: scan body vector: %w", err)
			}
			vec, err := bytesToFloat32Slice(blob)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("abstraction: decode body vector %d: %w", id, err)
			}
			out[id] = vec
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("abstraction: body vectors: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

// ── the standing restatement shortlist ────────────────────────────────────
//
// Candidate pairs live in the DB rather than being recomputed per session, for
// the same reason SIMILAR_TO edges do: the structure is global, the change per
// session is small, and rebuilding it every time would make a corpus-wide
// question cost corpus-wide work forever. What a session pays is proportional
// to what CHANGED.

// CachedPairFactIDs returns the fact ids the pair cache was last built over.
// Diffed against LiveEpistemicFactIDs, this IS the delta — no watermark.
func (ax *abstractionIndex) CachedPairFactIDs(ctx context.Context, branch string) (map[int64]struct{}, error) {
	rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
		`SELECT s.fact_id
		   FROM restatement_cache_state s
		   JOIN branches b ON b.id = s.branch_id
		  WHERE b.name = ?`, branch)
	if err != nil {
		return nil, fmt.Errorf("abstraction: cached pair fact ids: %w", err)
	}
	defer rows.Close()

	out := map[int64]struct{}{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("abstraction: scan cached fact id: %w", err)
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// FactIDsByPath resolves specific live paths to their current fact ids.
//
// Two paths, not the corpus: verdict attribution needs the ids of exactly the
// pair it is recording, and scanning every live fact to find two of them is a
// full table read on every judged item.
func (ax *abstractionIndex) FactIDsByPath(ctx context.Context, branch string, paths []string) (map[string]int64, error) {
	out := make(map[string]int64, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	for start := 0; start < len(paths); start += sqlIDChunk {
		chunk := paths[start:min(start+sqlIDChunk, len(paths))]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, len(chunk)+1)
		args = append(args, branch)
		for _, p := range chunk {
			args = append(args, p)
		}
		rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
			`SELECT f.id, f.path`+epistemicLiveJoin+` AND f.path IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("abstraction: fact ids by path: %w", err)
		}
		for rows.Next() {
			var id int64
			var path string
			if err := rows.Scan(&id, &path); err != nil {
				rows.Close()
				return nil, fmt.Errorf("abstraction: scan fact id: %w", err)
			}
			out[path] = id
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("abstraction: fact ids by path: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

// PartnersOfFacts returns the still-cached partners of the given facts — the
// other endpoint of every standing pair that touches one of them.
//
// The refresh needs this because KNN is ASYMMETRIC: a pair can exist only
// because B's top-K included A, never the other way round. Dropping every pair
// that touches an edited fact and then re-running only that fact's KNN
// therefore destroys pairs that nothing will rediscover. Requeuing the
// surviving partners restores them, at a bounded cost of at most K partners per
// dropped fact.
func (ax *abstractionIndex) PartnersOfFacts(ctx context.Context, branch string, factIDs []int64) (map[int64]struct{}, error) {
	out := map[int64]struct{}{}
	if len(factIDs) == 0 {
		return out, nil
	}
	branchID, err := ax.branchID(ctx, branch)
	if err != nil {
		return nil, err
	}
	dropped := make(map[int64]struct{}, len(factIDs))
	for _, id := range factIDs {
		dropped[id] = struct{}{}
	}

	for _, chunk := range chunkIDs(factIDs) {
		placeholders, ids := idPlaceholders(chunk)
		args := make([]any, 0, len(ids)*2+1)
		args = append(args, branchID)
		args = append(args, ids...)
		args = append(args, ids...)
		rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
			`SELECT a_fact_id, b_fact_id FROM restatement_pairs
			  WHERE branch_id = ?
			    AND (a_fact_id IN (`+placeholders+`) OR b_fact_id IN (`+placeholders+`))`, args...)
		if err != nil {
			return nil, fmt.Errorf("abstraction: partners of facts: %w", err)
		}
		for rows.Next() {
			var a, b int64
			if err := rows.Scan(&a, &b); err != nil {
				rows.Close()
				return nil, fmt.Errorf("abstraction: scan partner: %w", err)
			}
			for _, id := range []int64{a, b} {
				if _, isDropped := dropped[id]; !isDropped {
					out[id] = struct{}{}
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("abstraction: partners of facts: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

// DeleteRestatementPair removes one standing pair.
//
// Called when the judge declines a pair: the verdict log keeps the record, and
// leaving the pair standing would let it occupy the top of the ranking session
// after session, saturating the selection window until nothing else could ever
// be offered. An edit to either fact re-mints the pair through the ordinary KNN
// path, which is the behaviour we want — the judge has not seen that version.
func (ax *abstractionIndex) DeleteRestatementPair(ctx context.Context, branch string, aFactID, bFactID int64) error {
	branchID, err := ax.branchID(ctx, branch)
	if err != nil {
		return err
	}
	if _, err := conn(ctx, ax.rh.db).ExecContext(ctx,
		`DELETE FROM restatement_pairs
		  WHERE branch_id = ?
		    AND ((a_fact_id = ? AND b_fact_id = ?) OR (a_fact_id = ? AND b_fact_id = ?))`,
		branchID, aFactID, bFactID, bFactID, aFactID); err != nil {
		return fmt.Errorf("abstraction: delete pair: %w", err)
	}
	return nil
}

// ReplaceRestatementPairs applies one delta to the cache atomically: every pair
// touching a dropped-or-changed fact goes, the new pairs land, and the cache
// state records what the cache now covers.
//
// One transaction because a half-applied delta is worse than a stale cache: a
// cache that lost its pairs but kept its state rows would never rebuild them,
// since the state rows are what says "already covered".
func (ax *abstractionIndex) ReplaceRestatementPairs(ctx context.Context, branch string, dropFactIDs []int64, add []RestatementPair, coveredNow []int64) error {
	if len(dropFactIDs) == 0 && len(add) == 0 && len(coveredNow) == 0 {
		return nil
	}
	branchID, err := ax.branchID(ctx, branch)
	if err != nil {
		return err
	}

	ctx, tx, owned, err := beginTxIfNeeded(ctx, ax.rh.db)
	if err != nil {
		return fmt.Errorf("abstraction: begin tx: %w", err)
	}
	if owned {
		defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op
	}
	db := conn(ctx, ax.rh.db)

	for _, chunk := range chunkIDs(dropFactIDs) {
		placeholders, ids := idPlaceholders(chunk)
		pairArgs := make([]any, 0, len(ids)*2+1)
		pairArgs = append(pairArgs, branchID)
		pairArgs = append(pairArgs, ids...)
		pairArgs = append(pairArgs, ids...)
		if _, err := db.ExecContext(ctx,
			`DELETE FROM restatement_pairs
			  WHERE branch_id = ?
			    AND (a_fact_id IN (`+placeholders+`) OR b_fact_id IN (`+placeholders+`))`,
			pairArgs...); err != nil {
			return fmt.Errorf("abstraction: drop pairs: %w", err)
		}
		stateArgs := append([]any{branchID}, ids...)
		if _, err := db.ExecContext(ctx,
			`DELETE FROM restatement_cache_state
			  WHERE branch_id = ? AND fact_id IN (`+placeholders+`)`,
			stateArgs...); err != nil {
			return fmt.Errorf("abstraction: drop cache state: %w", err)
		}
	}
	for _, p := range add {
		kind := p.MatchKind
		if kind == "" {
			kind = MatchTitleKNN
		}
		if _, err := db.ExecContext(ctx,
			`INSERT OR REPLACE INTO restatement_pairs
			     (branch_id, a_path, b_path, a_fact_id, b_fact_id, title_cos, match_kind)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			branchID, p.APath, p.BPath, p.AFactID, p.BFactID, p.TitleCos, kind); err != nil {
			return fmt.Errorf("abstraction: insert pair %s|%s: %w", p.APath, p.BPath, err)
		}
	}
	for _, id := range coveredNow {
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO restatement_cache_state(branch_id, fact_id) VALUES (?, ?)`,
			branchID, id); err != nil {
			return fmt.Errorf("abstraction: record cache state for %d: %w", id, err)
		}
	}
	if owned {
		return tx.Commit()
	}
	return nil
}

// RestatementPairsByRank returns the top pairs by title cosine.
//
// Ranking is the whole selection mechanism: the "operating point" is whatever
// absolute cosine the last selected pair happens to sit at IN THIS REPO, which
// is why no cosine threshold appears anywhere in this file.
func (ax *abstractionIndex) RestatementPairsByRank(ctx context.Context, branch string, limit int) ([]RestatementPair, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
		`SELECT p.a_path, p.b_path, p.a_fact_id, p.b_fact_id, p.title_cos, p.match_kind
		   FROM restatement_pairs p
		   JOIN branches b ON b.id = p.branch_id
		  WHERE b.name = ?
		  ORDER BY p.title_cos DESC, p.a_path ASC, p.b_path ASC
		  LIMIT ?`, branch, limit)
	if err != nil {
		return nil, fmt.Errorf("abstraction: rank pairs: %w", err)
	}
	defer rows.Close()
	return scanRestatementPairs(rows)
}

// RestatementPairsByMatchKind ranks WITHIN one detection route.
//
// The cosine ordering is kept inside the population rather than dropped: it
// still orders structural matches sensibly against each other, it just no
// longer decides whether they are reachable at all.
func (ax *abstractionIndex) RestatementPairsByMatchKind(ctx context.Context, branch string, kinds []string, limit int) ([]RestatementPair, error) {
	if limit <= 0 || len(kinds) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	args := make([]any, 0, len(kinds)+2)
	args = append(args, branch)
	for _, k := range kinds {
		args = append(args, k)
	}
	args = append(args, limit)
	rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
		`SELECT p.a_path, p.b_path, p.a_fact_id, p.b_fact_id, p.title_cos, p.match_kind
		   FROM restatement_pairs p
		   JOIN branches b ON b.id = p.branch_id
		  WHERE b.name = ? AND p.match_kind IN (`+placeholders+`)
		  ORDER BY p.title_cos DESC, p.a_path ASC, p.b_path ASC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("abstraction: rank pairs by match kind: %w", err)
	}
	defer rows.Close()
	return scanRestatementPairs(rows)
}

// RestatementPairsByMatchKindOldest sweeps a match-kind population in MINT
// ORDER instead of ranking it.
//
// Same population as RestatementPairsByMatchKind, different question. That one
// asks "which of these is most title-similar"; this asks "which have waited
// longest". The sweep is the right shape for the structural routes because
// their inflow far exceeds what any session can judge, so a cosine ranking
// re-offers the same head forever and the tail is never reached.
//
// `rowid` is the mint order, with the caveat spelled out on the interface: the
// cache is written with INSERT OR REPLACE, so a re-minted pair gets a fresh
// rowid and moves to the BACK. That is the wanted behaviour (a re-minted pair
// is revisited, not skipped) but it means the ordering is only age-EXACT once
// the title axis is complete.
//
// Ties are broken on the paths, so two runs over one corpus agree even if the
// rowids ever collide.
func (ax *abstractionIndex) RestatementPairsByMatchKindOldest(ctx context.Context, branch string, kinds []string, limit int) ([]RestatementPair, error) {
	if limit <= 0 || len(kinds) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	args := make([]any, 0, len(kinds)+2)
	args = append(args, branch)
	for _, k := range kinds {
		args = append(args, k)
	}
	args = append(args, limit)
	rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
		`SELECT p.a_path, p.b_path, p.a_fact_id, p.b_fact_id, p.title_cos, p.match_kind
		   FROM restatement_pairs p
		   JOIN branches b ON b.id = p.branch_id
		  WHERE b.name = ? AND p.match_kind IN (`+placeholders+`)
		  ORDER BY p.rowid ASC, p.a_path ASC, p.b_path ASC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("abstraction: sweep pairs by match kind: %w", err)
	}
	defer rows.Close()
	return scanRestatementPairs(rows)
}

func scanRestatementPairs(rows *sql.Rows) ([]RestatementPair, error) {
	var out []RestatementPair
	for rows.Next() {
		var p RestatementPair
		if err := rows.Scan(&p.APath, &p.BPath, &p.AFactID, &p.BFactID, &p.TitleCos, &p.MatchKind); err != nil {
			return nil, fmt.Errorf("abstraction: scan pair: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LiveEpistemicFactTitles is the structural detection pass's input: what each
// live fact SAYS IT IS, rather than where it sits in the vector space.
func (ax *abstractionIndex) LiveEpistemicFactTitles(ctx context.Context, branch string) (map[int64]LiveFactTitle, error) {
	rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
		`SELECT f.id, f.path, f.title`+epistemicLiveJoin, branch)
	if err != nil {
		return nil, fmt.Errorf("abstraction: live fact titles: %w", err)
	}
	defer rows.Close()

	out := map[int64]LiveFactTitle{}
	for rows.Next() {
		var id int64
		var t LiveFactTitle
		if err := rows.Scan(&id, &t.Path, &t.Title); err != nil {
			return nil, fmt.Errorf("abstraction: scan live fact title: %w", err)
		}
		out[id] = t
	}
	return out, rows.Err()
}

// RestatementPairStats describes the standing pair population. Every field is
// OBSERVABILITY: reported in review health output, read by no branch. They are
// descriptors of a corpus, and a corpus descriptor that decided anything would
// be the corpus-property constant this design exists without.
func (ax *abstractionIndex) RestatementPairStats(ctx context.Context, branch string) (RestatementPairStats, error) {
	var st RestatementPairStats
	if err := conn(ctx, ax.rh.db).QueryRowContext(ctx,
		`SELECT COUNT(*)
		   FROM restatement_pairs p
		   JOIN branches b ON b.id = p.branch_id
		  WHERE b.name = ?`, branch).Scan(&st.Count); err != nil {
		return st, fmt.Errorf("abstraction: pair count: %w", err)
	}
	if st.Count == 0 {
		return st, nil
	}
	if err := conn(ctx, ax.rh.db).QueryRowContext(ctx,
		`SELECT COUNT(*)
		   FROM restatement_pairs p
		   JOIN branches b ON b.id = p.branch_id
		  WHERE b.name = ? AND p.match_kind <> ?`, branch, MatchTitleKNN).Scan(&st.Structural); err != nil {
		return st, fmt.Errorf("abstraction: structural pair count: %w", err)
	}
	var err error
	if st.P99, err = ax.pairQuantile(ctx, branch, st.Count, 0.99); err != nil {
		return st, err
	}
	if st.P999, err = ax.pairQuantile(ctx, branch, st.Count, 0.999); err != nil {
		return st, err
	}
	return st, nil
}

// pairQuantile reads the title cosine at the given upper quantile by offsetting
// into the descending ranking — cheap on the (branch_id, title_cos DESC) index
// and exact, where a sampled estimate would wobble between sessions.
func (ax *abstractionIndex) pairQuantile(ctx context.Context, branch string, count int, q float64) (float64, error) {
	offset := int(float64(count) * (1 - q))
	if offset >= count {
		offset = count - 1
	}
	var v float64
	err := conn(ctx, ax.rh.db).QueryRowContext(ctx,
		`SELECT p.title_cos
		   FROM restatement_pairs p
		   JOIN branches b ON b.id = p.branch_id
		  WHERE b.name = ?
		  ORDER BY p.title_cos DESC
		  LIMIT 1 OFFSET ?`, branch, offset).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("abstraction: pair quantile %.3f: %w", q, err)
	}
	return v, nil
}

// branchID resolves a branch name to its row id through repoHandler's cached
// resolver, so this sub-service shares the cache and returns the same
// ErrBranchNotFound every other caller matches on.
func (ax *abstractionIndex) branchID(ctx context.Context, branch string) (int64, error) {
	id, err := ax.rh.branchID(ctx, branch)
	if err != nil {
		return 0, fmt.Errorf("abstraction: %w", err)
	}
	return id, nil
}

// ── judge verdicts ────────────────────────────────────────────────────────
//
// Two consumers, both of which make a corpus decide its own behaviour from its
// own data: the trailing merge-rate that funds or defunds its shortlist, and
// the kept-pair exclusion that stops one declined pair from occupying the
// funded slots session after session.

// RecordRestatementVerdict records what the judge did with one shortlist pair.
func (ax *abstractionIndex) RecordRestatementVerdict(ctx context.Context, branch string, v RestatementVerdict) error {
	branchID, err := ax.branchID(ctx, branch)
	if err != nil {
		return err
	}
	resolved := 0
	if v.Resolved {
		resolved = 1
	}
	if _, err := conn(ctx, ax.rh.db).ExecContext(ctx,
		`INSERT INTO restatement_verdicts
		     (branch_id, a_path, b_path, a_fact_id, b_fact_id, resolved, judged_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		branchID, v.APath, v.BPath, v.AFactID, v.BFactID, resolved,
		v.JudgedAt.UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("abstraction: record verdict: %w", err)
	}
	return nil
}

// RecentRestatementVerdicts returns the last `window` verdicts, newest first.
func (ax *abstractionIndex) RecentRestatementVerdicts(ctx context.Context, branch string, window int) ([]RestatementVerdict, error) {
	if window <= 0 {
		return nil, nil
	}
	rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
		`SELECT v.a_path, v.b_path, v.a_fact_id, v.b_fact_id, v.resolved, v.judged_at
		   FROM restatement_verdicts v
		   JOIN branches b ON b.id = v.branch_id
		  WHERE b.name = ?
		  ORDER BY v.id DESC
		  LIMIT ?`, branch, window)
	if err != nil {
		return nil, fmt.Errorf("abstraction: recent verdicts: %w", err)
	}
	defer rows.Close()

	var out []RestatementVerdict
	for rows.Next() {
		var v RestatementVerdict
		var resolved int
		var judged string
		if err := rows.Scan(&v.APath, &v.BPath, &v.AFactID, &v.BFactID, &resolved, &judged); err != nil {
			return nil, fmt.Errorf("abstraction: scan verdict: %w", err)
		}
		v.Resolved = resolved == 1
		if t, perr := time.Parse(time.RFC3339, judged); perr == nil {
			v.JudgedAt = t
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// KeptPairFactIDs returns the id-pairs this branch's judge declined to
// resolve, as FactIDPairKey keys.
//
// The refresh consults it when MINTING pairs, not the selector when choosing
// them: a declined pair is deleted outright, and this set is what stops a later
// neighbour rescan from minting it again. Keyed by FACT ID on purpose — ids are
// content-addressed, so "the judge looked at this and kept both" expires
// structurally the moment either fact is edited, with no staleness rule to get
// wrong.
func (ax *abstractionIndex) KeptPairFactIDs(ctx context.Context, branch string) (map[string]struct{}, error) {
	rows, err := conn(ctx, ax.rh.db).QueryContext(ctx,
		`SELECT v.a_fact_id, v.b_fact_id
		   FROM restatement_verdicts v
		   JOIN branches b ON b.id = v.branch_id
		  WHERE b.name = ? AND v.resolved = 0`, branch)
	if err != nil {
		return nil, fmt.Errorf("abstraction: kept pairs: %w", err)
	}
	defer rows.Close()

	out := map[string]struct{}{}
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			return nil, fmt.Errorf("abstraction: scan kept pair: %w", err)
		}
		out[FactIDPairKey(a, b)] = struct{}{}
	}
	return out, rows.Err()
}

// FactIDPairKey is the canonical key for an unordered pair of fact ids, shared
// by the writer and the reader of the kept-pair set so the two can never
// disagree about ordering.
func FactIDPairKey(a, b int64) string {
	if b < a {
		a, b = b, a
	}
	return fmt.Sprintf("%d:%d", a, b)
}

// ProbeSessionsWaited returns how many sessions this branch has waited since it
// last spent a probe slot. Zero for a branch that has never been defunded.
func (ax *abstractionIndex) ProbeSessionsWaited(ctx context.Context, branch string) (int, error) {
	var n int
	err := conn(ctx, ax.rh.db).QueryRowContext(ctx,
		`SELECT s.sessions_since_probe
		   FROM restatement_throttle_state s
		   JOIN branches b ON b.id = s.branch_id
		  WHERE b.name = ?`, branch).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("abstraction: probe wait: %w", err)
	}
	return n, nil
}

// SetProbeSessionsWaited records the probe counter for this branch.
func (ax *abstractionIndex) SetProbeSessionsWaited(ctx context.Context, branch string, n int) error {
	branchID, err := ax.branchID(ctx, branch)
	if err != nil {
		return err
	}
	if _, err := conn(ctx, ax.rh.db).ExecContext(ctx,
		`INSERT INTO restatement_throttle_state(branch_id, sessions_since_probe)
		 VALUES (?, ?)
		 ON CONFLICT(branch_id) DO UPDATE SET sessions_since_probe = excluded.sessions_since_probe`,
		branchID, n); err != nil {
		return fmt.Errorf("abstraction: set probe wait: %w", err)
	}
	return nil
}
