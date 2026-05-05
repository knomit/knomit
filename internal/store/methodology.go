package store

import (
	"context"
	"fmt"
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
		SELECT f.path, f.title, f.id, f.blob_hash
		FROM facts f
		JOIN branch_facts bf ON bf.fact_id = f.id AND bf.branch_id = ?
		WHERE f.type = 'methodology'
		ORDER BY f.path
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
	}
	var cands []candRow
	for rows.Next() {
		var c candRow
		if err := rows.Scan(&c.path, &c.title, &c.id, &c.blobHash); err != nil {
			return nil, fmt.Errorf("RelevantMethodology: scan: %w", err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("RelevantMethodology: rows: %w", err)
	}

	// Hydrate body for each candidate via the git blob store.
	out := make([]MethodologyMatch, 0, len(cands))
	for _, c := range cands {
		body, err := si.readFactBodyByBlobHash(ctx, c.blobHash)
		if err != nil {
			log.Warn().Err(err).Str("path", c.path).Str("blob_hash", c.blobHash).
				Msg("RelevantMethodology: skipping candidate, blob unreadable")
			continue
		}
		out = append(out, MethodologyMatch{
			Path:  c.path,
			Title: c.title,
			Body:  body,
		})
	}
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out, nil
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
