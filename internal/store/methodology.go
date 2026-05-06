package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"
)

// Composite-score coefficients: Score = vectorWeight*VectorScore + tagWeight*TagOverlap.
const (
	vectorWeight = 0.6
	tagWeight    = 0.4
)

// MethodologyMatch is one entry in RelevantMethodology's ranked result set.
type MethodologyMatch struct {
	Path            string   `json:"path"`
	Title           string   `json:"title"`
	Body            string   `json:"body"`
	Score           float64  `json:"score"`
	VectorScore     float64  `json:"vector_score"`
	TagOverlap      float64  `json:"tag_overlap"`
	MatchedDomains  []string `json:"matched_domains,omitempty"`
	MatchedEntities []string `json:"matched_entities,omitempty"`
}

// RelevantMethodology returns up to k methodology facts visible on branch,
// ranked by a weighted combination of vector similarity and tag overlap
// (see vectorWeight / tagWeight).
func (si *searchIndex) RelevantMethodology(ctx context.Context, branch, sourceBody string, sourceDomains, sourceEntities []string, k int) ([]MethodologyMatch, error) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("RelevantMethodology: branchID: %w", err)
	}

	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, `
		SELECT f.path, f.title, f.id, f.blob_hash, f.domain, f.entities
		FROM facts f
		JOIN branch_facts bf ON bf.fact_id = f.id AND bf.branch_id = ?
		WHERE f.type = 'methodology'
	`, branchID)
	if err != nil {
		return nil, fmt.Errorf("RelevantMethodology: query: %w", err)
	}
	defer rows.Close()

	type candRow struct {
		path     string
		title    string
		id       int64
		blobHash string
		domains  []string
		entities []string
	}
	var cands []candRow
	for rows.Next() {
		var c candRow
		var domainJSON, entitiesJSON string
		if err := rows.Scan(&c.path, &c.title, &c.id, &c.blobHash, &domainJSON, &entitiesJSON); err != nil {
			return nil, fmt.Errorf("RelevantMethodology: scan: %w", err)
		}
		if err := json.Unmarshal([]byte(domainJSON), &c.domains); err != nil {
			log.Warn().Err(err).Str("path", c.path).
				Msg("RelevantMethodology: candidate domain JSON malformed; treating as empty")
		}
		if err := json.Unmarshal([]byte(entitiesJSON), &c.entities); err != nil {
			log.Warn().Err(err).Str("path", c.path).
				Msg("RelevantMethodology: candidate entities JSON malformed; treating as empty")
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("RelevantMethodology: rows: %w", err)
	}

	srcDomSet := stringSet(sourceDomains)
	srcEntSet := stringSet(sourceEntities)

	// Compute vector similarities once via a single sqlite-vec KNN.
	// sqlite-vec applies the global top-k window BEFORE the WHERE filter,
	// and facts_vec is keyed by the global facts.id (one row per fact
	// across all branches). To guarantee every methodology candidate gets
	// a similarity score even when sibling branches are present in the
	// same DB, k must cover the full vec table — not just this branch.
	simByID := make(map[int64]float64, len(cands))
	if emb := si.rh.getEmbedder(); emb != nil && sourceBody != "" && len(cands) > 0 {
		var totalFacts int64
		if err := conn(ctx, si.rh.db).QueryRowContext(ctx,
			`SELECT COUNT(*) FROM facts_vec`,
		).Scan(&totalFacts); err != nil {
			log.Warn().Err(err).Msg("RelevantMethodology: facts_vec count failed; falling back to tag-only ranking")
			totalFacts = 0
		}
		if totalFacts > 0 {
			v, embErr := emb.Embed(sourceBody)
			switch {
			case embErr != nil:
				log.Warn().Err(embErr).Msg("RelevantMethodology: embed failed; falling back to tag-only ranking")
			case len(v) == 0:
				log.Warn().Msg("RelevantMethodology: embedder returned empty vector; falling back to tag-only ranking")
			default:
				vecBlob := float32SliceToBytes(v)
				knnRows, qErr := conn(ctx, si.rh.db).QueryContext(ctx,
					`SELECT f.id, (1.0 - fv.distance) as similarity
					 FROM facts_vec fv
					 JOIN facts f ON f.id = fv.rowid
					 JOIN branch_facts bf ON bf.fact_id = f.id AND bf.branch_id = ?
					 WHERE fv.embedding MATCH ? AND fv.k = ? AND f.type = 'methodology'`,
					branchID, vecBlob, totalFacts,
				)
				if qErr != nil {
					log.Warn().Err(qErr).Int64("branch_id", branchID).
						Msg("RelevantMethodology: KNN query failed; falling back to tag-only ranking")
				} else {
					func() {
						defer knnRows.Close()
						for knnRows.Next() {
							var id int64
							var sim float64
							if err := knnRows.Scan(&id, &sim); err != nil {
								log.Warn().Err(err).Msg("RelevantMethodology: KNN row scan failed; skipping row")
								continue
							}
							simByID[id] = sim
						}
						if err := knnRows.Err(); err != nil {
							// Partial reads would leave non-uniform similarity
							// coverage across candidates — clear and degrade
							// to tag-only ranking instead.
							log.Warn().Err(err).Msg("RelevantMethodology: KNN row iteration error; clearing partial similarities, ranking tag-only")
							simByID = make(map[int64]float64, len(cands))
						}
					}()
				}
			}
		}
	}

	out := make([]MethodologyMatch, 0, len(cands))
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("RelevantMethodology: %w", err)
		}
		body, err := si.readFactBodyByBlobHash(ctx, c.blobHash)
		if err != nil {
			log.Warn().Err(err).Str("path", c.path).Str("blob_hash", c.blobHash).
				Msg("RelevantMethodology: skipping candidate, blob unreadable")
			continue
		}

		matchedDoms := intersectExcludingMarkers(c.domains, srcDomSet)
		matchedEnts := intersect(c.entities, srcEntSet)

		domOverlap := float64(len(matchedDoms)) / float64(max(1, len(sourceDomains)))
		entOverlap := float64(len(matchedEnts)) / float64(max(1, len(sourceEntities)))
		tagOverlap := (domOverlap + entOverlap) / 2.0
		vectorScore := simByID[c.id]
		composite := vectorWeight*vectorScore + tagWeight*tagOverlap

		out = append(out, MethodologyMatch{
			Path:            c.path,
			Title:           c.title,
			Body:            body,
			VectorScore:     vectorScore,
			TagOverlap:      tagOverlap,
			Score:           composite,
			MatchedDomains:  matchedDoms,
			MatchedEntities: matchedEnts,
		})
	}

	// SliceStable: SQL row order is the deterministic tiebreaker for equal scores.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })

	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// stringSet builds a set from a slice of strings; nil-safe.
func stringSet(s []string) map[string]struct{} {
	m := make(map[string]struct{}, len(s))
	for _, v := range s {
		m[v] = struct{}{}
	}
	return m
}

// intersect returns the elements of a present in set, in their original order.
func intersect(a []string, set map[string]struct{}) []string {
	out := make([]string, 0, len(a))
	for _, v := range a {
		if _, ok := set[v]; ok {
			out = append(out, v)
		}
	}
	return out
}

// IsMethodologyMarker reports whether s is one of the universal markers
// (meta, reasoning, methodology) that every methodology fact carries in
// its `domain` array. Used to filter these out before tag overlap
// scoring so they don't generate automatic intersection.
func IsMethodologyMarker(s string) bool {
	return s == "meta" || s == "reasoning" || s == "methodology"
}

// intersectExcludingMarkers is intersect for the candidate-side domain
// list, with the universal methodology markers (meta, reasoning,
// methodology) stripped from the candidate set BEFORE intersection. This
// prevents every methodology fact from getting an automatic "domain
// match" via the markers.
func intersectExcludingMarkers(candidateDomains []string, srcSet map[string]struct{}) []string {
	out := make([]string, 0, len(candidateDomains))
	for _, v := range candidateDomains {
		if IsMethodologyMarker(v) {
			continue
		}
		if _, ok := srcSet[v]; ok {
			out = append(out, v)
		}
	}
	return out
}

// readFactBodyByBlobHash reads the markdown body for a fact identified by
// its blob_hash via the git object store. Used by RelevantMethodology to
// hydrate match bodies for prompt injection.
func (si *searchIndex) readFactBodyByBlobHash(ctx context.Context, blobHash string) (string, error) {
	var raw []byte
	err := conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT data FROM objects WHERE hash = ? AND type = ?`,
		blobHash, blobObjectType,
	).Scan(&raw)
	if err != nil {
		return "", fmt.Errorf("readFactBodyByBlobHash: %w", err)
	}
	return extractBody(raw), nil
}

// FormatMethodologySection renders ranked methodology candidates as
// title/path/score bullet lines. Drops matches with Score < minScore.
// Returns "" when no matches survive filtering, so callers can omit the
// surrounding heading entirely.
//
// The bullets are heading-less; each call site supplies its own framing
// (e.g. "Applicable methodology" for distill/hypothesize, "Existing
// methodology" for reflect) so wording can vary by audience.
func FormatMethodologySection(matches []MethodologyMatch, minScore float64) string {
	if len(matches) == 0 {
		return ""
	}
	kept := make([]MethodologyMatch, 0, len(matches))
	for _, m := range matches {
		if m.Score < minScore {
			continue
		}
		kept = append(kept, m)
	}
	if len(kept) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, m := range kept {
		if i > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "• score=%.2f  %s  (%s)", m.Score, m.Title, m.Path)
	}
	sb.WriteByte('\n')
	return sb.String()
}
