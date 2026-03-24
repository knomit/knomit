// Search: vector similarity search over the fact index. Supports text queries
// (via embeddings), entity/domain/path/confidence filters, and cosine
// similarity thresholds.
package store

import (
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
)

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
	for _, e := range q.Entities {
		f.add(" AND EXISTS (SELECT 1 FROM json_each(f.entities) je WHERE je.value = ? COLLATE NOCASE)", e)
	}
	for _, d := range q.Domain {
		f.add(" AND EXISTS (SELECT 1 FROM json_each(f.domain) jd WHERE jd.value = ? COLLATE NOCASE OR jd.value LIKE ? COLLATE NOCASE)", d, d+"/%")
	}
	return f
}

// ── Search ────────────────────────────────────────────────────────────────────

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
func (idx *Index) Search(q SearchQuery) ([]SearchResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	flt := newFactFilter(q)

	// ── Text-less path: return all facts matching filters with score 100 ──
	if q.Text == "" {
		args := append(append([]any{BlobObjectType}, flt.args...), limit)
		rows, err := idx.db.Query(
			`SELECT f.path, f.title, f.blob_hash, f.type, f.domain, f.entities, f.confidence, f.sources, f.refs, f.commit_hash, f.evidence_weight, o.data
			 FROM facts f
			 JOIN objects o ON o.hash = f.blob_hash AND o.type = ?`+flt.SQL()+` LIMIT ?`,
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
		return out, rows.Err()
	}

	// ── Vector (embedding) search ─────────────────────────────────────────
	type candidate struct {
		rec   FactWithBody
		score float64
	}

	vecSimByPath := make(map[string]float64)
	if idx.embedder == nil && len(q.QueryVec) == 0 {
		log.Debug().Msg("search: no embedder configured, skipping vec search")
	} else {
		queryVec := q.QueryVec
		if len(queryVec) == 0 {
			var embedErr error
			queryVec, embedErr = idx.embedder.Embed(q.Text)
			if embedErr != nil {
				log.Warn().Err(embedErr).Msg("search: embed query failed")
			}
		}
		if queryVec == nil {
			log.Warn().Msg("search: no query vector available")
		} else {
			vecBlob := float32SliceToBytes(queryVec)
			// Adaptive k: scale with MinSimilarity — higher threshold needs fewer candidates.
			kLimit := limit * 5
			if q.MinSimilarity > 0.7 {
				kLimit = limit * 2
			} else if q.MinSimilarity > 0.5 {
				kLimit = limit * 3
			}
			rows, err := idx.db.Query(
				`SELECT f.path, (1.0 - fv.distance) as similarity
				 FROM facts_vec fv
				 JOIN facts f ON f.rowid = fv.rowid
				 WHERE fv.embedding MATCH ? AND fv.k = ?
				 ORDER BY fv.distance ASC`,
				vecBlob, kLimit,
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
		for path, score := range idx.graphExpandSearch(vecSimByPath, graphHops) {
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
	pathArgs := make([]any, len(candidatePaths))
	for i, p := range candidatePaths {
		pathArgs[i] = p
	}

	metaRows, err := idx.db.Query(
		`SELECT f.path, f.title, f.blob_hash, f.type, f.domain, f.entities, f.confidence, f.sources, f.refs, f.commit_hash, f.evidence_weight
		 FROM facts f
		 WHERE f.path IN (`+pathPH[:len(pathPH)-1]+`)`+flt.SQL(),
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
	bodyArgs = append(bodyArgs, BlobObjectType)
	for _, c := range candidates {
		bodyArgs = append(bodyArgs, c.rec.BlobHash)
	}
	bodyRows, err := idx.db.Query(
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
	return out, nil
}

// containsAll reports whether haystack contains all elements of needles
// (case-insensitive exact match).
func containsAll(haystack []string, needles []string) bool {
	for _, needle := range needles {
		found := false
		for _, h := range haystack {
			if strings.EqualFold(h, needle) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// domainPrefixMatch checks if each query domain is a prefix of at least one fact domain.
func domainPrefixMatch(factDomains, queryDomains []string) bool {
	for _, qd := range queryDomains {
		found := false
		for _, fd := range factDomains {
			if fd == qd || strings.HasPrefix(fd, qd+"/") {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// matchesFilters reports whether rec satisfies the non-text filter fields in q
// (entities, domain, path prefix, minimum confidence).
func matchesFilters(rec FactRecord, q SearchQuery) bool {
	if len(q.Entities) > 0 && !containsAll(rec.Entities, q.Entities) {
		return false
	}
	if len(q.Domain) > 0 && !domainPrefixMatch(rec.Domain, q.Domain) {
		return false
	}
	if q.Path != "" && !strings.HasPrefix(rec.Path, q.Path) {
		return false
	}
	if q.MinConfidence > 0 && rec.Confidence < q.MinConfidence {
		return false
	}
	if len(q.IncludeTypes) > 0 {
		found := false
		for _, t := range q.IncludeTypes {
			if rec.Type == t {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(q.ExcludeTypes) > 0 {
		for _, t := range q.ExcludeTypes {
			if rec.Type == t {
				return false
			}
		}
	}
	return true
}

