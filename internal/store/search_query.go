package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
)

// RecentFactEntry is a lightweight record for the recent-facts endpoint.
type RecentFactEntry struct {
	Path        string  `json:"path"`
	Title       string  `json:"title"`
	Type        string  `json:"type"`
	CommittedAt int64   `json:"committed_at"`
	Operation   string  `json:"operation,omitempty"`
	Score       float64 `json:"score,omitempty"`
}

// RecentFacts returns facts on the given branch under pathPrefix ordered by
// most recent commit, paginated by offset/limit. If query is non-empty, it
// performs a semantic search first and returns only matching facts (still
// ordered by time). domain, entities, and epOps are optional additional filters.
func (si *searchIndex) RecentFacts(ctx context.Context, branch, pathPrefix, query string, limit, offset int, includeTypes, excludeTypes, domain, entities, epOps []string) ([]RecentFactEntry, int, error) {
	if query != "" {
		return si.recentFactsSearch(ctx, branch, pathPrefix, query, limit, offset, includeTypes, excludeTypes, domain, entities, epOps)
	}

	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return nil, 0, fmt.Errorf("RecentFacts: %w", err)
	}

	flt := newFactFilter(SearchQuery{
		Path:         pathPrefix,
		IncludeTypes: includeTypes,
		ExcludeTypes: excludeTypes,
		Domain:       domain,
		Entities:     entities,
	})

	// Build the ep filter clause (operates on cl.operation from the LEFT JOIN).
	epClause := ""
	epArgs := []any{}
	if len(epOps) > 0 {
		ph := strings.Repeat("?,", len(epOps))
		epArgs = make([]any, len(epOps))
		for i, op := range epOps {
			epArgs[i] = op
		}
		epClause = " AND COALESCE(cl.operation, '') IN (" + ph[:len(ph)-1] + ")"
	}

	countArgs := append(append([]any{branchID}, flt.args...), epArgs...)
	var total int
	if err := conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 LEFT JOIN commit_log cl ON bf.commit_hash = cl.commit_hash AND f.path = cl.path
		 WHERE bf.branch_id = ?`+flt.SQL()+epClause,
		countArgs...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("RecentFacts count: %w", err)
	}

	queryArgs := append(append(append([]any{branchID}, flt.args...), epArgs...), limit, offset)
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
		`SELECT f.path, f.title, f.type, COALESCE(cl.committed_at, 0), COALESCE(cl.operation, '')
		 FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 LEFT JOIN commit_log cl ON bf.commit_hash = cl.commit_hash AND f.path = cl.path
		 WHERE bf.branch_id = ?`+flt.SQL()+epClause+`
		 ORDER BY cl.committed_at DESC, f.path ASC
		 LIMIT ? OFFSET ?`,
		queryArgs...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("RecentFacts query: %w", err)
	}
	defer rows.Close()

	var entries []RecentFactEntry
	for rows.Next() {
		var e RecentFactEntry
		if err := rows.Scan(&e.Path, &e.Title, &e.Type, &e.CommittedAt, &e.Operation); err != nil {
			return nil, 0, fmt.Errorf("RecentFacts scan: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

// recentFactsSearch uses semantic search to find matching facts, then returns
// them ordered by committed_at with pagination.
func (si *searchIndex) recentFactsSearch(ctx context.Context, branch, pathPrefix, query string, limit, offset int, includeTypes, excludeTypes, domain, entities, epOps []string) ([]RecentFactEntry, int, error) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return nil, 0, fmt.Errorf("RecentFacts search: %w", err)
	}

	results, err := si.Search(ctx, branch, SearchQuery{
		Text:         query,
		Path:         pathPrefix,
		IncludeTypes: includeTypes,
		ExcludeTypes: excludeTypes,
		Domain:       domain,
		Entities:     entities,
		EpisodeOps:   epOps,
		Limit:        500, // large enough to get all matches for pagination
	})
	if err != nil {
		return nil, 0, fmt.Errorf("RecentFacts search: %w", err)
	}
	if len(results) == 0 {
		return []RecentFactEntry{}, 0, nil
	}

	// Build score map from search results
	scoreByPath := make(map[string]float64, len(results))
	placeholders := make([]string, len(results))
	args := make([]any, len(results)+1)
	args[0] = branchID
	for i, r := range results {
		placeholders[i] = "?"
		args[i+1] = r.Path
		scoreByPath[r.Path] = r.Score
	}

	rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
		`SELECT f.path, f.title, f.type, COALESCE(cl.committed_at, 0), COALESCE(cl.operation, '')
		 FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 LEFT JOIN commit_log cl ON bf.commit_hash = cl.commit_hash AND f.path = cl.path
		 WHERE bf.branch_id = ? AND f.path IN (`+join(placeholders, ",")+`)
		 ORDER BY cl.committed_at DESC, f.path ASC`,
		args...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("RecentFacts search query: %w", err)
	}
	defer rows.Close()

	var all []RecentFactEntry
	for rows.Next() {
		var e RecentFactEntry
		if err := rows.Scan(&e.Path, &e.Title, &e.Type, &e.CommittedAt, &e.Operation); err != nil {
			return nil, 0, fmt.Errorf("RecentFacts search scan: %w", err)
		}
		e.Score = scoreByPath[e.Path]
		all = append(all, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	total := len(all)
	if offset >= total {
		return []RecentFactEntry{}, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return all[offset:end], total, nil
}

// LastCommitForPath returns the commit hash of the most recent commit_log
// entry for the given path, provided that entry's action is not 'deleted'.
// Returns ("", false) if the path is not found or its latest action is deleted.
func (si *searchIndex) LastCommitForPath(ctx context.Context, branch, path string) (string, bool) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return "", false
	}
	var hash, action string
	// Try branch-scoped query first (entries with branch_id set).
	err = conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT commit_hash, action FROM commit_log WHERE branch_id = ? AND path = ? ORDER BY rowid DESC LIMIT 1`,
		branchID, path,
	).Scan(&hash, &action)
	if err != nil {
		// Fallback: legacy rows with NULL branch_id.
		err = conn(ctx, si.rh.db).QueryRowContext(ctx,
			`SELECT commit_hash, action FROM commit_log WHERE path = ? ORDER BY rowid DESC LIMIT 1`,
			path,
		).Scan(&hash, &action)
	}
	if err != nil || hash == "" || action == "deleted" {
		return "", false
	}
	return hash, true
}

func join(ss []string, sep string) string {
	var b strings.Builder
	for i, s := range ss {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(s)
	}
	return b.String()
}

// ── Search ────────────────────────────────────────────────────────────────────
// Search: vector similarity search over the fact index. Supports text queries
// (via embeddings), entity/domain/path/confidence filters, and cosine
// similarity thresholds.

// SearchQuery describes a hybrid search request.
type SearchQuery struct {
	Text          string
	Entities      []string
	Domain        []string
	Path          string
	MinConfidence float64
	MinSimilarity float64   // cosine similarity threshold (0–1); 0 uses default 0.40
	Limit         int
	GraphHops     int       // number of graph traversal hops to expand results (0 = disabled)
	QueryVec      []float32 // pre-computed embedding vector; if set, skips Embed(Text)
	IncludeTypes  []string  // only return facts with these types (empty = all)
	ExcludeTypes  []string  // exclude facts with these types
	EpisodeOps    []string  // filter by episode operation type (e.g. "learn", "update", "retract"); filtered post-query in Go
}

// SearchResult is a FactWithBody paired with a relevance score in [0, 100].
type SearchResult struct {
	FactWithBody
	Score float64 `json:"score"`
}

// ── factFilter ────────────────────────────────────────────────────────────────

// factFilter builds the shared WHERE-clause fragment used by both search paths.
// Each Add call appends one "AND ..." clause and its bind parameters.
// SQL() returns the concatenated fragment (empty string if no filters set).
type factFilter struct {
	clauses []string
	args    []any
}

func (f *factFilter) add(clause string, args ...any) {
	f.clauses = append(f.clauses, clause)
	f.args = append(f.args, args...)
}

func (f *factFilter) SQL() string { return strings.Join(f.clauses, "") }

func newFactFilter(q SearchQuery) *factFilter {
	f := &factFilter{}
	if q.MinConfidence > 0 {
		f.add(" AND f.confidence >= ?", q.MinConfidence)
	}
	if q.Path != "" {
		f.add(" AND f.path LIKE ?", q.Path+"%")
	}
	if len(q.IncludeTypes) > 0 {
		ph := strings.Repeat("?,", len(q.IncludeTypes))
		args := make([]any, len(q.IncludeTypes))
		for i, t := range q.IncludeTypes {
			args[i] = t
		}
		f.add(" AND f.type IN ("+ph[:len(ph)-1]+")", args...)
	}
	if len(q.ExcludeTypes) > 0 {
		ph := strings.Repeat("?,", len(q.ExcludeTypes))
		args := make([]any, len(q.ExcludeTypes))
		for i, t := range q.ExcludeTypes {
			args[i] = t
		}
		f.add(" AND f.type NOT IN ("+ph[:len(ph)-1]+")", args...)
	}
	if len(q.Entities) > 0 {
		ph := strings.Repeat("?,", len(q.Entities))
		ph = ph[:len(ph)-1]
		args := make([]any, len(q.Entities)+1)
		for i, e := range q.Entities {
			args[i] = e
		}
		args[len(q.Entities)] = len(q.Entities)
		f.add(
			" AND (SELECT COUNT(DISTINCT entity) FROM fact_entities WHERE fact_id = f.id AND entity IN ("+ph+")) >= ?",
			args...,
		)
	}
	for _, d := range q.Domain {
		f.add(
			" AND EXISTS (SELECT 1 FROM fact_domains WHERE fact_id = f.id AND (domain = ? OR domain LIKE ?))",
			d, d+"/%",
		)
	}
	return f
}

// filterByEpisodeOps removes results whose latest commit operation is not in
// the allowed set. It performs a single bulk SQL lookup of operations by
// commit_hash from commit_log (same database). If ops is empty, all results
// are kept unchanged.
func (si *searchIndex) filterByEpisodeOps(ctx context.Context, results []SearchResult, ops []string) ([]SearchResult, error) {
	if len(ops) == 0 || len(results) == 0 {
		return results, nil
	}

	// Build a set of allowed operations for O(1) lookup.
	allowed := make(map[string]bool, len(ops))
	for _, op := range ops {
		allowed[op] = true
	}

	// Collect unique commit hashes from results.
	hashes := make([]string, 0, len(results))
	seen := make(map[string]bool, len(results))
	for _, r := range results {
		if r.CommitHash != "" && !seen[r.CommitHash] {
			hashes = append(hashes, r.CommitHash)
			seen[r.CommitHash] = true
		}
	}
	if len(hashes) == 0 {
		return nil, nil
	}

	ph := strings.Repeat("?,", len(hashes))
	args := make([]any, len(hashes))
	for i, h := range hashes {
		args[i] = h
	}

	rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
		`SELECT commit_hash, operation FROM commit_log WHERE commit_hash IN (`+ph[:len(ph)-1]+`) GROUP BY commit_hash`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("filterByEpisodeOps: %w", err)
	}
	defer rows.Close()

	opByHash := make(map[string]string, len(hashes))
	for rows.Next() {
		var hash, op string
		if err := rows.Scan(&hash, &op); err != nil {
			return nil, fmt.Errorf("filterByEpisodeOps scan: %w", err)
		}
		opByHash[hash] = op
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := results[:0]
	for _, r := range results {
		op := opByHash[r.CommitHash]
		if allowed[op] {
			out = append(out, r)
		}
	}
	return out, nil
}

// Search performs a vector similarity search over the index.
//
// Algorithm:
//  1. If Text is present → embed query, compute cosine similarity via vec0 KNN.
//  2. Apply Entities / Domain / Path / MinConfidence filters post-retrieval.
//  3. Normalise top-N scores to [0,100].
//  4. Return sorted by score descending, capped at Limit.
//
// If Text is empty, all facts matching the non-text filters are returned with
// score 100.
func (si *searchIndex) Search(ctx context.Context, branch string, q SearchQuery) ([]SearchResult, error) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	flt := newFactFilter(q)

	// ── Text-less path: return all facts matching filters with score 100 ──
	if q.Text == "" {
		args := append(append([]any{blobObjectType, branchID}, flt.args...), limit)
		rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
			`SELECT f.path, f.title, f.blob_hash, f.type, f.domain, f.entities,
			        f.confidence, f.sources, f.refs, f.evidence_weight,
			        bf.commit_hash, o.data
			 FROM branch_facts bf
			 JOIN facts f ON f.id = bf.fact_id
			 JOIN objects o ON o.hash = f.blob_hash AND o.type = ?
			 WHERE bf.branch_id = ?`+flt.SQL()+` LIMIT ?`,
			args...,
		)
		if err != nil {
			return nil, fmt.Errorf("search: list all: %w", err)
		}
		defer rows.Close()

		var out []SearchResult
		for rows.Next() {
			fb, err := scanFactWithBodyFromRows(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, SearchResult{FactWithBody: *fb, Score: 100})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return si.filterByEpisodeOps(ctx, out, q.EpisodeOps)
	}

	// ── Vector (embedding) search ─────────────────────────────────────────
	type candidate struct {
		rec   FactWithBody
		score float64
	}

	vecSimByPath := make(map[string]float64)
	emb := si.getEmbedder()
	if emb == nil && len(q.QueryVec) == 0 {
		log.Debug().Msg("search: no embedder configured, skipping vec search")
	} else {
		queryVec := q.QueryVec
		if len(queryVec) == 0 {
			var embedErr error
			queryVec, embedErr = emb.Embed(q.Text)
			if embedErr != nil {
				log.Warn().Err(embedErr).Msg("search: embed query failed")
			}
		}
		if queryVec == nil {
			log.Warn().Msg("search: no query vector available")
		} else {
			vecBlob := float32SliceToBytes(queryVec)
			kLimit := limit * 5
			if q.MinSimilarity > 0.7 {
				kLimit = limit * 2
			} else if q.MinSimilarity > 0.5 {
				kLimit = limit * 3
			}
			rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
				`SELECT f.path, (1.0 - fv.distance) as similarity
				 FROM facts_vec fv
				 JOIN facts f ON f.id = fv.rowid
				 JOIN branch_facts bf ON bf.fact_id = f.id AND bf.branch_id = ?
				 WHERE fv.embedding MATCH ? AND fv.k = ?
				 ORDER BY fv.distance ASC`,
				branchID, vecBlob, kLimit,
			)
			if err != nil {
				log.Warn().Err(err).Msg("search: vec query failed")
			} else {
				for rows.Next() {
					var path string
					var sim float64
					if err := rows.Scan(&path, &sim); err != nil {
						break
					}
					vecSimByPath[path] = sim
				}
				rows.Close()
				log.Debug().Int("vec_hits", len(vecSimByPath)).Msg("vec search complete")
			}
		}
	}

	if graphHops := q.GraphHops; graphHops > 0 && len(vecSimByPath) > 0 {
		for path, score := range si.graphExpandSearch(ctx, branchID, vecSimByPath, graphHops) {
			if _, exists := vecSimByPath[path]; !exists {
				vecSimByPath[path] = score
			}
		}
	}

	if len(vecSimByPath) == 0 {
		return nil, nil
	}

	minSim := q.MinSimilarity
	if minSim <= 0 {
		minSim = 0.40
	}

	candidatePaths := make([]string, 0, len(vecSimByPath))
	for path, cosine := range vecSimByPath {
		if cosine > minSim {
			candidatePaths = append(candidatePaths, path)
		}
	}
	if len(candidatePaths) == 0 {
		return nil, nil
	}

	// ── Phase 1: fetch metadata only (no body), apply filters, sort, trim ─
	pathPH := strings.Repeat("?,", len(candidatePaths))
	pathArgs := make([]any, 0, len(candidatePaths)+1)
	pathArgs = append(pathArgs, branchID)
	for _, p := range candidatePaths {
		pathArgs = append(pathArgs, p)
	}

	metaRows, err := conn(ctx, si.rh.db).QueryContext(ctx,
		`SELECT f.path, f.title, f.blob_hash, f.type, f.domain, f.entities,
		        f.confidence, f.sources, f.refs, f.evidence_weight
		 FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 WHERE bf.branch_id = ? AND f.path IN (`+pathPH[:len(pathPH)-1]+`)`+flt.SQL(),
		append(pathArgs, flt.args...)...,
	)
	if err != nil {
		return nil, fmt.Errorf("search: meta fetch: %w", err)
	}
	defer metaRows.Close()

	var candidates []candidate
	for metaRows.Next() {
		rec, err := scanFactRecordFromRows(metaRows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate{rec: FactWithBody{FactRecord: *rec}, score: vecSimByPath[rec.Path]})
	}
	if err := metaRows.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].score > candidates[j-1].score; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	// ── Phase 2: fetch bodies for the top-limit facts only ────────────────
	bodyPH := strings.Repeat("?,", len(candidates))
	bodyArgs := make([]any, 0, len(candidates)+1)
	bodyArgs = append(bodyArgs, blobObjectType)
	for _, c := range candidates {
		bodyArgs = append(bodyArgs, c.rec.BlobHash)
	}
	bodyRows, err := conn(ctx, si.rh.db).QueryContext(ctx,
		`SELECT hash, data FROM objects WHERE type = ? AND hash IN (`+bodyPH[:len(bodyPH)-1]+`)`,
		bodyArgs...,
	)
	if err != nil {
		return nil, fmt.Errorf("search: body fetch: %w", err)
	}
	defer bodyRows.Close()

	bodies := make(map[string]string, len(candidates))
	for bodyRows.Next() {
		var hash string
		var data []byte
		if err := bodyRows.Scan(&hash, &data); err != nil {
			return nil, err
		}
		bodies[hash] = string(data)
	}
	if err := bodyRows.Err(); err != nil {
		return nil, err
	}

	// ── Assemble final results ────────────────────────────────────────────
	out := make([]SearchResult, 0, len(candidates))
	for _, c := range candidates {
		c.rec.Body = bodies[c.rec.BlobHash]
		out = append(out, SearchResult{FactWithBody: c.rec, Score: c.score * 100.0})
	}
	return si.filterByEpisodeOps(ctx, out, q.EpisodeOps)
}

