package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"
)

// RecentFactEntry is a lightweight record for the recent-facts endpoint.
type RecentFactEntry struct {
	Path        string   `json:"path"`
	Title       string   `json:"title"`
	Kind        string   `json:"kind"`
	Type        string   `json:"type"`
	Domain      []string `json:"domain,omitempty"`
	Entities    []string `json:"entities,omitempty"`
	CommittedAt int64    `json:"committed_at"`
	Operation   string   `json:"operation,omitempty"`
	Score       float64  `json:"score,omitempty"`
}

// RecentFacts returns facts on the given branch ordered by most recent commit,
// paginated by opts.Offset/opts.Limit. If opts.Text is non-empty, it performs
// a semantic search first and ranks matches by relevance. All filter fields
// (Path, IncludeKinds/ExcludeKinds, IncludeTypes/ExcludeTypes, Domain,
// Entities, EpisodeOps) are applied; vector-search-only fields (QueryVec,
// QueryByPath, MinSimilarity, GraphHops) are inert here.
func (si *searchIndex) RecentFacts(ctx context.Context, branch string, opts SearchOptions) ([]RecentFactEntry, int, error) {
	if opts.Text != "" {
		return si.recentFactsSearch(ctx, branch, opts)
	}

	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return nil, 0, fmt.Errorf("RecentFacts: %w", err)
	}

	flt := newFactFilter(opts)

	// Build the ep filter clause (operates on cl.operation from the LEFT JOIN).
	epClause := ""
	epArgs := []any{}
	if len(opts.EpisodeOps) > 0 {
		ph := strings.Repeat("?,", len(opts.EpisodeOps))
		epArgs = make([]any, len(opts.EpisodeOps))
		for i, op := range opts.EpisodeOps {
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

	queryArgs := append(append(append([]any{branchID}, flt.args...), epArgs...), opts.Limit, opts.Offset)
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
		`SELECT f.path, f.title, f.kind, f.type, f.domain, f.entities,
		        COALESCE(cl.committed_at, 0), COALESCE(cl.operation, '')
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
		var domainJSON, entitiesJSON string
		if err := rows.Scan(&e.Path, &e.Title, &e.Kind, &e.Type, &domainJSON, &entitiesJSON, &e.CommittedAt, &e.Operation); err != nil {
			return nil, 0, fmt.Errorf("RecentFacts scan: %w", err)
		}
		var refs []string
		logFactJSONUnmarshal("RecentFacts", e.Path, domainJSON, entitiesJSON, "null", &e.Domain, &e.Entities, &refs)
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

// recentFactsSearch uses semantic search to find matching facts, then returns
// them ordered by committed_at with pagination.
func (si *searchIndex) recentFactsSearch(ctx context.Context, branch string, opts SearchOptions) ([]RecentFactEntry, int, error) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return nil, 0, fmt.Errorf("RecentFacts search: %w", err)
	}

	// Override Limit for the search phase: we need a large candidate set so
	// pagination by score/time below has enough rows to slice. The original
	// opts.Limit/opts.Offset are applied to the final result list.
	searchOpts := opts
	searchOpts.Limit = 500
	searchOpts.Offset = 0
	results, err := si.Search(ctx, branch, searchOpts)
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
		`SELECT f.path, f.title, f.kind, f.type, f.domain, f.entities,
		        COALESCE(cl.committed_at, 0), COALESCE(cl.operation, '')
		 FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 LEFT JOIN commit_log cl ON bf.commit_hash = cl.commit_hash AND f.path = cl.path
		 WHERE bf.branch_id = ? AND f.path IN (`+strings.Join(placeholders, ",")+`)
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
		var domainJSON, entitiesJSON string
		if err := rows.Scan(&e.Path, &e.Title, &e.Kind, &e.Type, &domainJSON, &entitiesJSON, &e.CommittedAt, &e.Operation); err != nil {
			return nil, 0, fmt.Errorf("RecentFacts search scan: %w", err)
		}
		var refs []string
		logFactJSONUnmarshal("RecentFacts.search", e.Path, domainJSON, entitiesJSON, "null", &e.Domain, &e.Entities, &refs)
		e.Score = scoreByPath[e.Path]
		all = append(all, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// When a query is present the SQL ORDER BY committed_at is only used for
	// stable iteration; rank order is established here by relevance score
	// descending, with committed_at and path as deterministic tiebreakers.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		if all[i].CommittedAt != all[j].CommittedAt {
			return all[i].CommittedAt > all[j].CommittedAt
		}
		return all[i].Path < all[j].Path
	})

	total := len(all)
	if opts.Offset >= total {
		return []RecentFactEntry{}, total, nil
	}
	end := opts.Offset + opts.Limit
	if end > total {
		end = total
	}
	return all[opts.Offset:end], total, nil
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
	err = conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT cl.commit_hash, cl.action
		 FROM commit_log cl
		 JOIN branch_commits bc ON bc.commit_hash = cl.commit_hash
		 WHERE bc.branch_id = ? AND cl.path = ?
		 ORDER BY cl.rowid DESC LIMIT 1`,
		branchID, path,
	).Scan(&hash, &action)
	if err != nil || hash == "" || action == "deleted" {
		return "", false
	}
	return hash, true
}

// ── Search ────────────────────────────────────────────────────────────────────
// Search: vector similarity search over the fact index. Supports text queries
// (via embeddings), entity/domain/path/confidence filters, and cosine
// similarity thresholds.

// SearchOptions is the unified options struct for fact queries — used by both
// Search (vector/text ranking) and RecentFacts (time-ordered pagination).
// Filter fields apply to both; semantic-search-only fields (QueryVec,
// QueryByPath, MinSimilarity, GraphHops) are inert when passed to RecentFacts.
// Pagination (Offset) is only consulted by RecentFacts.
type SearchOptions struct {
	Text     string
	Entities []string
	// Domain matches, by default, slash-hierarchy descendant-or-equal ("store"
	// finds "store", "store/resolver", ...) OR token containment of the
	// de-hyphenized tag ("ai" finds "ai governance", "enterprise ai"; "governance
	// ai" matches "ai governance" order-independently; plurals are stemmed).
	// Query terms are canonicalised the same way the stored tags are.
	Domain []string
	// DomainExact restricts Domain matching to canonical exact-string equality
	// (no descendant, no token containment). Opt-in "I mean exactly this tag".
	DomainExact bool
	// DomainAncestor matches ancestor-or-equal: query "store/resolver" finds
	// facts with domain "store/resolver", "store", ... (any path ancestor).
	// Used by principles-style "what scopes apply to this subarea?" lookups.
	DomainAncestor []string
	Path           string
	MinConfidence float64
	MinSimilarity float64   // cosine similarity threshold (0–1); 0 uses default 0.40
	Limit         int
	Offset        int       // RecentFacts pagination offset; ignored by Search
	GraphHops     int       // number of graph traversal hops to expand results (0 = disabled)
	QueryVec      []float32 // pre-computed embedding vector; if set, skips Embed(Text)
	QueryByPath   string    // resolve query vector from this branch+path's stored embedding via SQL join; skips Embed(Text). Lower priority than QueryVec.
	IncludeTypes  []string  // only return facts with these types (empty = all)
	ExcludeTypes  []string  // exclude facts with these types
	IncludeKinds  []string  // only return facts with these kinds (empty = all)
	ExcludeKinds  []string  // exclude facts with these kinds
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

func newFactFilter(q SearchOptions) *factFilter {
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
	if len(q.IncludeKinds) > 0 {
		ph := strings.Repeat("?,", len(q.IncludeKinds))
		args := make([]any, len(q.IncludeKinds))
		for i, t := range q.IncludeKinds {
			args[i] = t
		}
		f.add(" AND f.kind IN ("+ph[:len(ph)-1]+")", args...)
	}
	if len(q.ExcludeKinds) > 0 {
		ph := strings.Repeat("?,", len(q.ExcludeKinds))
		args := make([]any, len(q.ExcludeKinds))
		for i, t := range q.ExcludeKinds {
			args[i] = t
		}
		f.add(" AND f.kind NOT IN ("+ph[:len(ph)-1]+")", args...)
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
		canon := canonicalizeDomain(d)
		toks := domainTokens(canon)
		// Exact mode (or a degenerate canonical with no tokens) → canonical
		// string equality only.
		if q.DomainExact || len(toks) == 0 {
			f.add(" AND EXISTS (SELECT 1 FROM fact_domains WHERE fact_id = f.id AND domain = ?)", canon)
			continue
		}
		// Default: slash-hierarchy descendant-or-equal OR token containment
		// (a fact has a single domain whose token set ⊇ all query tokens).
		ph := strings.Repeat("?,", len(toks))
		ph = ph[:len(ph)-1]
		args := make([]any, 0, len(toks)+3)
		args = append(args, canon, canon+"/%")
		for _, t := range toks {
			args = append(args, t)
		}
		args = append(args, len(toks))
		f.add(
			" AND (EXISTS (SELECT 1 FROM fact_domains WHERE fact_id = f.id AND (domain = ? OR domain LIKE ?))"+
				" OR EXISTS (SELECT 1 FROM fact_domain_tokens t WHERE t.fact_id = f.id"+
				" AND t.token IN ("+ph+") GROUP BY t.domain HAVING COUNT(DISTINCT t.token) = ?))",
			args...,
		)
	}
	for _, d := range q.DomainAncestor {
		// Ancestor-or-equal match on the canonical slash path: the fact's domain
		// is either exactly the query, or a prefix of it. Kept slash-based (the
		// principles/applies_to scoping feature), not tokenised.
		canon := canonicalizeDomain(d)
		f.add(
			" AND EXISTS (SELECT 1 FROM fact_domains WHERE fact_id = f.id AND (domain = ? OR ? LIKE domain || '/%'))",
			canon, canon,
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
func (si *searchIndex) Search(ctx context.Context, branch string, q SearchOptions) ([]SearchResult, error) {
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
	if q.Text == "" && q.QueryByPath == "" && len(q.QueryVec) == 0 {
		args := append(append([]any{blobObjectType, branchID}, flt.args...), limit)
		rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
			`SELECT f.path, f.title, f.blob_hash, f.kind, f.type, f.domain, f.entities,
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
	kLimit := limit * 5
	if q.MinSimilarity > 0.7 {
		kLimit = limit * 2
	} else if q.MinSimilarity > 0.5 {
		kLimit = limit * 3
	}

	// QueryByPath path: do the source-embedding lookup and the KNN match in
	// one SQL statement, eliminating the round-trip and the embedding
	// inference. The subquery in MATCH resolves to the stored vector for the
	// (branch, path) pair; if the pair has no row in facts_vec, MATCH gets
	// NULL and the outer query returns no rows (caller falls back to filter
	// search, same as if there were no embedder).
	if q.QueryByPath != "" && len(q.QueryVec) == 0 {
		rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
			`SELECT f.path, (1.0 - fv.distance) as similarity
			 FROM facts_vec fv
			 JOIN facts f ON f.id = fv.rowid
			 JOIN branch_facts bf ON bf.fact_id = f.id AND bf.branch_id = ?
			 WHERE fv.embedding MATCH (
			     SELECT fv2.embedding
			     FROM branch_facts bf2
			     JOIN facts_vec fv2 ON fv2.rowid = bf2.fact_id
			     WHERE bf2.branch_id = ? AND bf2.path = ?
			 ) AND fv.k = ?
			 ORDER BY fv.distance ASC`,
			branchID, branchID, q.QueryByPath, kLimit,
		)
		if err != nil {
			log.Warn().Err(err).Str("source_path", q.QueryByPath).Msg("search: query-by-path failed")
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
			log.Debug().Int("vec_hits", len(vecSimByPath)).Str("source_path", q.QueryByPath).Msg("vec search complete (via path)")
		}
	} else {
		emb := si.rh.getEmbedder()
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
		`SELECT f.path, f.title, f.blob_hash, f.kind, f.type, f.domain, f.entities,
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

