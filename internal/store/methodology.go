package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"
)

// MethodologyMatch is one entry in RelevantMethodology's ranked result set.
// Fields beyond Path/Title/Body are populated only when scoring is in play
// (Tasks 2 and 3 fill them); Task 1 returns Score=0 for every entry.
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
// ranked by composite score (0.6·vector + 0.4·tag_overlap). See
// .claude/plans/2026-05-01-methodology-in-the-loop-design.md for the full
// retrieval algorithm.
//
// In Task 1 only the candidate-set filter is implemented; ranking and top-k
// land in Tasks 2 and 3.
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
		_ = json.Unmarshal([]byte(domainJSON), &c.domains)
		_ = json.Unmarshal([]byte(entitiesJSON), &c.entities)
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("RelevantMethodology: rows: %w", err)
	}

	// Score each candidate: tag-overlap component only (Task 2). Vector
	// score lands in Task 3.
	srcDomSet := stringSet(sourceDomains)
	srcEntSet := stringSet(sourceEntities)

	out := make([]MethodologyMatch, 0, len(cands))
	for _, c := range cands {
		body, err := si.readFactBodyByBlobHash(ctx, c.blobHash)
		if err != nil {
			log.Warn().Err(err).Str("path", c.path).Str("blob_hash", c.blobHash).
				Msg("RelevantMethodology: skipping candidate, blob unreadable")
			continue
		}

		matchedDoms := intersectExcludingMarkers(c.domains, srcDomSet)
		matchedEnts := intersect(c.entities, srcEntSet)

		domOverlap := safeDiv(float64(len(matchedDoms)), float64(max(1, len(sourceDomains))))
		entOverlap := safeDiv(float64(len(matchedEnts)), float64(max(1, len(sourceEntities))))
		tagOverlap := (domOverlap + entOverlap) / 2.0

		out = append(out, MethodologyMatch{
			Path:            c.path,
			Title:           c.title,
			Body:            body,
			TagOverlap:      tagOverlap,
			MatchedDomains:  matchedDoms,
			MatchedEntities: matchedEnts,
			Score:           tagOverlap, // composite still pending vector
		})
	}

	// Sort descending by Score; Task 3 will refine the score formula.
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

// intersectExcludingMarkers is intersect for the candidate-side domain
// list, with the universal methodology markers (meta, reasoning,
// methodology) stripped from the candidate set BEFORE intersection. This
// prevents every methodology fact from getting an automatic "domain
// match" via the markers.
func intersectExcludingMarkers(candidateDomains []string, srcSet map[string]struct{}) []string {
	out := make([]string, 0, len(candidateDomains))
	for _, v := range candidateDomains {
		if v == "meta" || v == "reasoning" || v == "methodology" {
			continue
		}
		if _, ok := srcSet[v]; ok {
			out = append(out, v)
		}
	}
	return out
}

// safeDiv returns a/b, or 0 if b == 0.
func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
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

// FormatMethodologySection renders a slice of MethodologyMatch as the
// "Applicable methodology" prompt section used by knomit_hypothesize,
// reflect, and distill. Pure function with no DB or context dependencies
// — exported so consumers in internal/mcp and internal/synthesize can
// share a single rendering. Returns "" for an empty/nil slice so callers
// can omit the section entirely.
func FormatMethodologySection(matches []MethodologyMatch) string {
	if len(matches) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Applicable methodology (top 3 by relevance — reasoning lessons from prior cycles):\n")
	for _, m := range matches {
		fmt.Fprintf(&sb, "\n• %s (%s)\n%s\n", m.Title, m.Path, m.Body)
	}
	return sb.String()
}
