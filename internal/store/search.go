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
}

// SearchResult is a FactWithBody paired with a relevance score in [0, 100].
type SearchResult struct {
	FactWithBody
	Score float64 `json:"score"`
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
func (idx *Index) Search(q SearchQuery) ([]SearchResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	// ── Text-less path: return all facts matching filters with score 100 ──
	if q.Text == "" {
		rows, err := idx.db.Query(
			`SELECT f.path, f.title, f.blob_hash, f.type, f.domain, f.entities, f.confidence, f.sources, f.refs, f.commit_hash, o.data
			 FROM facts f
			 JOIN objects o ON o.hash = f.blob_hash AND o.type = ?`, BlobObjectType)
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
			if !matchesFilters(fb.FactRecord, q) {
				continue
			}
			out = append(out, SearchResult{FactWithBody: *fb, Score: 100})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(out) > limit {
			out = out[:limit]
		}
		return out, nil
	}

	// ── Vector (embedding) search ────────────────────────────────────────
	type candidate struct {
		rec   FactWithBody
		score float64
	}

	vecSimByPath := make(map[string]float64)
	if idx.embedder == nil && len(q.QueryVec) == 0 {
		log.Debug().Msg("search: no embedder configured, skipping vec search")
	} else {
		// Use pre-computed vector if provided, otherwise embed the text.
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
			// Single JOIN query: KNN + facts table → path + similarity.
			vecBlob := float32SliceToBytes(queryVec)
			rows, err := idx.db.Query(
				`SELECT f.path, (1.0 - fv.distance) as similarity
				 FROM facts_vec fv
				 JOIN facts f ON f.rowid = fv.rowid
				 WHERE fv.embedding MATCH ? AND fv.k = ?
				 ORDER BY fv.distance ASC`,
				vecBlob, limit*5,
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

	graphHops := q.GraphHops
	if graphHops < 0 {
		graphHops = 0
	}
	if graphHops > 0 && len(vecSimByPath) > 0 {
		expanded := idx.graphExpandSearch(vecSimByPath, graphHops)
		for path, score := range expanded {
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

	// Filter to paths above similarity threshold.
	candidatePaths := make([]string, 0, len(vecSimByPath))
	for path, cosine := range vecSimByPath {
		if cosine > minSim {
			candidatePaths = append(candidatePaths, path)
		}
	}
	if len(candidatePaths) == 0 {
		return nil, nil
	}

	// Batch-fetch all candidate facts+bodies in a single query with SQL-side filters.
	placeholders := make([]string, len(candidatePaths))
	args := make([]any, 0, len(candidatePaths)+4)
	args = append(args, BlobObjectType)
	for i, p := range candidatePaths {
		placeholders[i] = "?"
		args = append(args, p)
	}

	filterSQL := ""
	if q.MinConfidence > 0 {
		filterSQL += " AND f.confidence >= ?"
		args = append(args, q.MinConfidence)
	}
	if q.Path != "" {
		filterSQL += " AND f.path LIKE ?"
		args = append(args, q.Path+"%")
	}

	batchQuery := `SELECT f.path, f.title, f.blob_hash, f.type, f.domain, f.entities, f.confidence, f.sources, f.refs, f.commit_hash, o.data
		 FROM facts f
		 JOIN objects o ON o.hash = f.blob_hash AND o.type = ?
		 WHERE f.path IN (` + strings.Join(placeholders, ",") + `)` + filterSQL

	batchRows, err := idx.db.Query(batchQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("search: batch fetch: %w", err)
	}
	defer batchRows.Close()

	var candidates []candidate
	for batchRows.Next() {
		fb, err := scanFactWithBodyFromRows(batchRows)
		if err != nil {
			return nil, err
		}
		// Entity and domain filters still applied in Go (json_each would require
		// dynamic subqueries per filter element which adds complexity for little gain
		// on the typically small candidate set).
		if len(q.Entities) > 0 && !containsAll(fb.Entities, q.Entities) {
			continue
		}
		if len(q.Domain) > 0 && !domainPrefixMatch(fb.Domain, q.Domain) {
			continue
		}
		candidates = append(candidates, candidate{rec: *fb, score: vecSimByPath[fb.Path]})
	}
	if err := batchRows.Err(); err != nil {
		return nil, err
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	// ── Sort by score descending (insertion sort, fine for small N) ───────
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].score > candidates[j-1].score; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}

	// ── Scale scores to [0, 100] ─────────────────────────────────────────
	var out []SearchResult
	for _, c := range candidates {
		out = append(out, SearchResult{FactWithBody: c.rec, Score: c.score * 100.0})
		if len(out) >= limit {
			break
		}
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
	return true
}

