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
	MinSimilarity float64 // cosine similarity threshold (0–1); 0 uses default 0.40
	Limit         int
}

// SearchResult is a FactRecord paired with a relevance score in [0, 100].
type SearchResult struct {
	FactRecord
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
			`SELECT path, title, body, domain, entities, confidence, sources, refs, commit_hash
			 FROM facts`)
		if err != nil {
			return nil, fmt.Errorf("search: list all: %w", err)
		}
		defer rows.Close()

		var out []SearchResult
		for rows.Next() {
			rec, err := scanFactRecordFromRows(rows)
			if err != nil {
				return nil, err
			}
			if !matchesFilters(*rec, q) {
				continue
			}
			out = append(out, SearchResult{FactRecord: *rec, Score: 100})
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
		rec   FactRecord
		score float64
	}

	vecSimByPath := make(map[string]float64)
	if idx.embedder == nil {
		log.Debug().Msg("search: no embedder configured, skipping vec search")
	} else {
		queryVec, embedErr := idx.embedder.Embed(q.Text)
		if embedErr != nil {
			log.Warn().Err(embedErr).Msg("search: embed query failed")
		} else if queryVec == nil {
			log.Warn().Msg("search: embedder returned nil vector")
		} else {
			vecBlob := float32SliceToBytes(queryVec)
			rows, err := idx.db.Query(
				`SELECT rowid, distance FROM facts_vec WHERE embedding MATCH ? AND k = ?`,
				vecBlob, limit*5,
			)
			if err != nil {
				log.Warn().Err(err).Msg("search: vec query failed")
			} else {
				type vecHit struct {
					rowid int64
					dist  float64
				}
				var hits []vecHit
				for rows.Next() {
					var h vecHit
					if err := rows.Scan(&h.rowid, &h.dist); err != nil {
						break
					}
					hits = append(hits, h)
				}
				rows.Close()

				// Convert rowid → path and store cosine similarity (1 - distance).
				for _, h := range hits {
					var path string
					err := idx.db.QueryRow(`SELECT path FROM facts WHERE rowid = ?`, h.rowid).Scan(&path)
					if err == nil {
						vecSimByPath[path] = 1.0 - h.dist
					}
				}
				log.Debug().Int("vec_hits", len(vecSimByPath)).Msg("vec search complete")
			}
		}
	}

	if len(vecSimByPath) == 0 {
		return nil, nil
	}

	// Build candidates from vec hits above the similarity threshold.
	candidates := make([]candidate, 0, len(vecSimByPath))

	minSim := q.MinSimilarity
	if minSim <= 0 {
		minSim = 0.40
	}
	for path, cosine := range vecSimByPath {
		if cosine <= minSim {
			continue
		}
		rec, err := idx.GetByPath(path)
		if err != nil || rec == nil {
			continue
		}
		candidates = append(candidates, candidate{rec: *rec, score: cosine})
	}

	// ── Apply post-retrieval filters ─────────────────────────────────────
	filtered := candidates[:0]
	for _, c := range candidates {
		if matchesFilters(c.rec, q) {
			filtered = append(filtered, c)
		}
	}
	candidates = filtered

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
		out = append(out, SearchResult{FactRecord: c.rec, Score: c.score * 100.0})
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

// matchesFilters reports whether rec satisfies the non-text filter fields in q
// (entities, domain, path prefix, minimum confidence).
func matchesFilters(rec FactRecord, q SearchQuery) bool {
	if len(q.Entities) > 0 && !containsAll(rec.Entities, q.Entities) {
		return false
	}
	if len(q.Domain) > 0 && !containsAll(rec.Domain, q.Domain) {
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

