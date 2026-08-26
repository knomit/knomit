package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// MethodologyMatch is one entry in RelevantMethodologyForFact's ranked result set.
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

// RelevantMethodologyForFact returns up to k methodology facts visible on
// branch, ranked by composite score (vectorWeight*VectorScore +
// tagWeight*TagOverlap), filtered to Score >= minScore. The source vector
// is fetched from facts_vec by (branch, factPath) — never re-embedded.
//
// Callers operating over multiple source facts invoke this once per fact
// and merge the results (keeping the highest score per methodology path).
// No body concatenation, no re-embedding, no truncation.
func (mm *methodologyMatcher) RelevantMethodologyForFact(
	ctx context.Context,
	branch string,
	factPath string,
	sourceDomains []string,
	sourceEntities []string,
	k int,
	minScore float64,
) ([]MethodologyMatch, error) {
	branchID, err := mm.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("RelevantMethodologyForFact: branchID: %w", err)
	}

	// Fetch the source fact's stored embedding from facts_vec. No Embed()
	// call runs on this code path — the vector was computed at index
	// time when the fact was written. If the row is absent (fact not yet
	// indexed, or no embedding produced), fall through to tag-only.
	var srcVec []byte
	err = conn(ctx, mm.rh.db).QueryRowContext(ctx, `
		SELECT fv.embedding
		FROM facts_vec fv
		JOIN branch_facts bf ON bf.fact_id = fv.rowid
		WHERE bf.branch_id = ? AND bf.path = ?`,
		branchID, factPath,
	).Scan(&srcVec)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("RelevantMethodologyForFact: fetch source embedding for %q: %w", factPath, err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		log.Warn().Str("branch", branch).Str("path", factPath).
			Msg("RelevantMethodologyForFact: no stored embedding for source fact; ranking tag-only")
		srcVec = nil
	}

	// Load methodology candidate set visible on this branch.
	//
	// The source fact is EXCLUDED from its own candidate set (#132). A source
	// fact may itself be a methodology fact — that is not a corner case but the
	// observed one: a distill cluster whose input facts include a methodology
	// fact retrieved kb/meta/reasoning/substrate-layer-dominance.md at 0.90 as
	// guidance for synthesizing that very fact. The score is genuine, because a
	// fact matches itself perfectly, and that is exactly what makes it
	// worthless: a perfect methodology score is a self-membership red flag, not
	// a strong recommendation, and here it cleared the 0.50 mandatory-read
	// threshold on nothing but its own identity.
	//
	// Excluded HERE, in the candidate set, rather than filtered from the
	// results: this query is the single authoritative definition of what is
	// being scored, so a later filter would leave the self-match to consume a
	// top-k slot before being dropped. The KNN query below is deliberately left
	// alone — it only populates similarity scores keyed by fact id, and an
	// entry for a fact absent from cands is never read.
	rows, err := conn(ctx, mm.rh.db).QueryContext(ctx, `
		SELECT f.path, f.title, f.id, f.blob_hash, f.domain, f.entities
		FROM facts f
		JOIN branch_facts bf ON bf.fact_id = f.id AND bf.branch_id = ?
		WHERE f.type = 'methodology' AND f.path <> ?
	`, branchID, factPath)
	if err != nil {
		return nil, fmt.Errorf("RelevantMethodologyForFact: query candidates: %w", err)
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
			return nil, fmt.Errorf("RelevantMethodologyForFact: scan candidate: %w", err)
		}
		if err := json.Unmarshal([]byte(domainJSON), &c.domains); err != nil {
			log.Warn().Err(err).Str("path", c.path).
				Msg("RelevantMethodologyForFact: candidate domain JSON malformed; treating as empty")
		}
		if err := json.Unmarshal([]byte(entitiesJSON), &c.entities); err != nil {
			log.Warn().Err(err).Str("path", c.path).
				Msg("RelevantMethodologyForFact: candidate entities JSON malformed; treating as empty")
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("RelevantMethodologyForFact: iterate candidates: %w", err)
	}
	if len(cands) == 0 {
		return nil, nil
	}

	srcDomSet := stringSet(sourceDomains)
	srcEntSet := stringSet(sourceEntities)

	// KNN against the source fact's embedding. sqlite-vec applies the
	// global top-k window BEFORE the WHERE filter, and facts_vec is keyed
	// by the global facts.id (one row per fact across all branches), so k
	// must cover the full vec table to guarantee every methodology
	// candidate gets a similarity score.
	//
	// DB-side prune: max possible composite for a candidate is
	// vectorWeight*vec + tagWeight*1.0; anything below that bound cannot
	// reach minScore even with full tag overlap. Filter at the SQL level
	// to skip work the Go side would discard anyway. At default
	// minScore=0.15 the bound is negative (no pruning); at higher
	// thresholds it trims the candidate set.
	simByID := make(map[int64]float64, len(cands))
	if len(srcVec) > 0 {
		var totalFacts int64
		if err := conn(ctx, mm.rh.db).QueryRowContext(ctx,
			`SELECT COUNT(*) FROM facts_vec`,
		).Scan(&totalFacts); err != nil {
			log.Warn().Err(err).Msg("RelevantMethodologyForFact: facts_vec count failed; falling back to tag-only ranking")
			totalFacts = 0
		}
		if totalFacts > 0 {
			minVec := (minScore - tagWeight) / vectorWeight
			if minVec < 0 {
				minVec = 0
			}
			knnRows, qErr := conn(ctx, mm.rh.db).QueryContext(ctx,
				`SELECT f.id, (1.0 - fv.distance) as similarity
				 FROM facts_vec fv
				 JOIN facts f ON f.id = fv.rowid
				 JOIN branch_facts bf ON bf.fact_id = f.id AND bf.branch_id = ?
				 WHERE fv.embedding MATCH ? AND fv.k = ? AND f.type = 'methodology'
				   AND (1.0 - fv.distance) >= ?`,
				branchID, srcVec, totalFacts, minVec,
			)
			if qErr != nil {
				log.Warn().Err(qErr).Int64("branch_id", branchID).
					Msg("RelevantMethodologyForFact: KNN query failed; falling back to tag-only ranking")
			} else {
				func() {
					defer knnRows.Close()
					for knnRows.Next() {
						var id int64
						var sim float64
						if err := knnRows.Scan(&id, &sim); err != nil {
							log.Warn().Err(err).Msg("RelevantMethodologyForFact: KNN row scan failed; skipping row")
							continue
						}
						simByID[id] = sim
					}
					if err := knnRows.Err(); err != nil {
						// Partial reads would leave non-uniform similarity
						// coverage across candidates — clear and degrade
						// to tag-only ranking instead.
						log.Warn().Err(err).Msg("RelevantMethodologyForFact: KNN row iteration error; clearing partial similarities, ranking tag-only")
						simByID = make(map[int64]float64, len(cands))
					}
				}()
			}
		}
	}

	out := make([]MethodologyMatch, 0, len(cands))
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("RelevantMethodologyForFact: %w", err)
		}
		body, err := mm.readFactBodyByBlobHash(ctx, c.blobHash)
		if err != nil {
			log.Warn().Err(err).Str("path", c.path).Str("blob_hash", c.blobHash).
				Msg("RelevantMethodologyForFact: skipping candidate, blob unreadable")
			continue
		}

		matchedDoms := intersectExcludingMarkers(c.domains, srcDomSet)
		matchedEnts := intersect(c.entities, srcEntSet)

		domOverlap := float64(len(matchedDoms)) / float64(max(1, len(sourceDomains)))
		entOverlap := float64(len(matchedEnts)) / float64(max(1, len(sourceEntities)))
		tagOverlap := (domOverlap + entOverlap) / 2.0
		vectorScore := simByID[c.id]
		composite := vectorWeight*vectorScore + tagWeight*tagOverlap

		if composite < minScore {
			continue
		}

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
// its `domain` array.
func IsMethodologyMarker(s string) bool {
	return s == "meta" || s == "reasoning" || s == "methodology"
}

// intersectExcludingMarkers is intersect for the candidate-side domain
// list, with the universal methodology markers stripped from the
// candidate set BEFORE intersection. Markers only ever appear in domain
// (not entities), so entity intersection need not filter them.
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
// its blob_hash via the git object store.
func (mm *methodologyMatcher) readFactBodyByBlobHash(ctx context.Context, blobHash string) (string, error) {
	var raw []byte
	err := conn(ctx, mm.rh.db).QueryRowContext(ctx,
		`SELECT data FROM objects WHERE hash = ? AND type = ?`,
		blobHash, blobObjectType,
	).Scan(&raw)
	if err != nil {
		return "", fmt.Errorf("readFactBodyByBlobHash: %w", err)
	}
	return extractBody(raw), nil
}

// FormatMethodologySection renders ranked methodology candidates as
// title/path/score bullet lines. Returns "" when matches is empty so
// callers can omit the surrounding heading entirely.
//
// Bullets are heading-less; each call site supplies its own framing
// (e.g. "Applicable methodology" for distill/hypothesize, "Existing
// methodology" for reflect). Score filtering happens upstream in
// RelevantMethodologyForFact — the formatter renders whatever it gets.
func FormatMethodologySection(matches []MethodologyMatch) string {
	if len(matches) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, m := range matches {
		if i > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "• score=%.2f  %s  (%s)", m.Score, m.Title, m.Path)
	}
	sb.WriteByte('\n')
	return sb.String()
}
