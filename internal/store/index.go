// Package store implements the knomit knowledge base backed by SQLite, sqlite-vec,
// and a bare git repository. It provides vector similarity search and filtered
// search over git-backed fact files.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"knomit/internal/fact"
	storegit "knomit/internal/store/git"
)

// ────────────────────────────────────────────────────────────────────────────
// Domain types
// ────────────────────────────────────────────────────────────────────────────

// FactRecord is the stored record — no body, just a blob_hash pointer.
type FactRecord struct {
	Path           string   `json:"path"`
	Title          string   `json:"title"`
	BlobHash       string   `json:"blob_hash"`
	Kind           string   `json:"kind"`
	Type           string   `json:"type"`
	Domain         []string `json:"domain"`
	Entities       []string `json:"entities"`
	Confidence     float64  `json:"confidence"`
	Sources        int      `json:"sources"`
	Refs           []string `json:"refs"`
	EvidenceWeight float64  `json:"evidence_weight,omitempty"`
	SourceCommit   string   `json:"source_commit,omitempty"` // commit at which this version was written
}

// NewFactRecord constructs a FactRecord from a parsed fact and git metadata.
// blobHash is the blob SHA returned by WriteFile.
//
// Kind is normalized at this boundary: a zero-value fact.Kind (e.g. from a
// pre-Kind-aware caller) is canonicalized to fact.DefaultKind so the
// in-memory FactRecord always carries the same value the SQL row will
// carry under the column's DEFAULT.
func NewFactRecord(f fact.Fact, blobHash string) FactRecord {
	kind := f.Kind
	if kind == "" {
		kind = fact.DefaultKind
	}
	return FactRecord{
		Path:           f.Path(),
		Title:          f.Title,
		BlobHash:       blobHash,
		Kind:           string(kind),
		Type:           string(f.Type),
		Domain:         f.Domain,
		Entities:       f.Entities,
		Confidence:     f.Confidence,
		Sources:        f.Sources,
		Refs:           f.Refs,
		EvidenceWeight: f.EvidenceWeight,
	}
}

// FactWithBody is returned by read operations that hydrate the body from git objects.
type FactWithBody struct {
	FactRecord
	Body        string `json:"body"`
	CommitHash  string `json:"commit_hash,omitempty"`
	CommittedAt int64  `json:"committed_at,omitempty"`
}


func conn(ctx context.Context, db *sql.DB) storegit.CtxExecer {
	return storegit.Conn(ctx, db)
}

func beginTxIfNeeded(ctx context.Context, db *sql.DB) (context.Context, *sql.Tx, bool, error) {
	return storegit.BeginTxIfNeeded(ctx, db)
}

// extractBody strips YAML frontmatter from raw markdown and returns just the body.
// It assumes the format: ---\n...\n---\n# Title\n\nBody
func extractBody(raw []byte) string {
	content := string(raw)
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return content
	}
	afterFrontmatter := strings.TrimSpace(parts[2])
	// Skip the title line (first # heading)
	if idx := strings.Index(afterFrontmatter, "\n"); idx >= 0 {
		return strings.TrimSpace(afterFrontmatter[idx+1:])
	}
	return ""
}


// Completions returns autocomplete suggestions for a given filter category and prefix,
// scoped to the given branch.
// Supported categories: "domain", "entity", "type", "ep", "path".
func (si *searchIndex) Completions(ctx context.Context, branch, category, prefix string, limit int) ([]string, error) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("completions: %w", err)
	}

	switch category {
	case "domain":
		// Canonicalise the typed prefix (NFC + fold + de-hyphenize) so it matches
		// the canonical stored domains — "AI-Gov" → "ai gov" → "ai governance".
		// Entities (below) are NOT canonicalised: they are proper nouns/identifiers.
		canonPrefix := canonicalizeDomain(prefix)
		// A non-empty prefix that canonicalises away (pure separators/hyphens,
		// e.g. "---") must not fall through to `LIKE '%'`, which would return
		// every domain — turning a junk keystroke into the full domain list.
		// Empty input is still allowed through (prefix == "" → `LIKE '%'`), the
		// intended "nothing typed yet, list everything" behaviour.
		if prefix != "" && canonPrefix == "" {
			return []string{}, nil
		}
		return si.queryDistinct(ctx,
			`SELECT DISTINCT fd.domain FROM fact_domains fd
			 JOIN branch_facts bf ON bf.fact_id = fd.fact_id
			 WHERE bf.branch_id = ? AND fd.domain LIKE ? LIMIT ?`,
			branchID, canonPrefix+"%", limit)
	case "entity":
		return si.queryDistinct(ctx,
			`SELECT DISTINCT fe.entity FROM fact_entities fe
			 JOIN branch_facts bf ON bf.fact_id = fe.fact_id
			 WHERE bf.branch_id = ? AND fe.entity LIKE ? LIMIT ?`,
			branchID, prefix+"%", limit)
	case "type":
		types := append(fact.AllEpistemicTypes(), fact.AllPragmaticTypes()...)
		out := make([]string, len(types))
		for i, t := range types {
			out[i] = string(t)
		}
		return out, nil
	case "kind":
		return []string{"epistemic", "pragmatic"}, nil
	case "ep":
		return []string{"learn", "update", "retract", "subsume", "synthesize", "sync"}, nil
	case "path":
		rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
			`SELECT DISTINCT f.path
			 FROM branch_facts bf
			 JOIN facts f ON f.id = bf.fact_id
			 WHERE bf.branch_id = ? AND f.path LIKE ? LIMIT 500`,
			branchID, prefix+"%")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		dirs := map[string]bool{}
		prefixLen := len(prefix)
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				return nil, fmt.Errorf("completions: scan path: %w", err)
			}
			// Find the next '/' after the prefix to extract directory components
			rest := p[prefixLen:]
			if i := strings.Index(rest, "/"); i >= 0 {
				dirs[p[:prefixLen+i]] = true
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		vals := make([]string, 0, len(dirs))
		for d := range dirs {
			vals = append(vals, d)
		}
		sort.Strings(vals)
		if len(vals) > limit {
			vals = vals[:limit]
		}
		return vals, nil
	default:
		return nil, fmt.Errorf("unknown completion category: %s", category)
	}
}

// queryDistinct executes a query and returns distinct non-empty string values.
func (si *searchIndex) queryDistinct(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var vals []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("queryDistinct scan: %w", err)
		}
		if v != "" {
			vals = append(vals, v)
		}
	}
	return vals, rows.Err()
}

// StatsResult holds aggregate statistics computed from the facts table.
type StatsResult struct {
	Total         int            `json:"total"`
	AvgConfidence float64        `json:"avg_confidence"`
	Domains       map[string]int `json:"domains"`
	Entities      map[string]int `json:"entities"`
}

// Stats returns aggregate statistics over all indexed facts on a branch,
// optionally filtered to those whose path starts with pathPrefix.
func (si *searchIndex) Stats(ctx context.Context, branch, pathPrefix string) (StatsResult, error) {
	res := StatsResult{
		Domains:  make(map[string]int),
		Entities: make(map[string]int),
	}

	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return res, fmt.Errorf("stats: %w", err)
	}

	// Total count and average confidence.
	var avgConf *float64
	q := `SELECT COUNT(*), AVG(f.confidence)
	      FROM branch_facts bf
	      JOIN facts f ON f.id = bf.fact_id
	      WHERE bf.branch_id = ?`
	args := []any{branchID}
	if pathPrefix != "" {
		q += ` AND f.path LIKE ?`
		args = append(args, pathPrefix+"%")
	}
	if err := conn(ctx, si.rh.db).QueryRowContext(ctx, q, args...).Scan(&res.Total, &avgConf); err != nil {
		return res, err
	}
	if avgConf != nil {
		// Round to 2 decimal places.
		res.AvgConfidence = float64(int(*avgConf*100+0.5)) / 100
	}

	// Domain counts via json_each.
	dq := `SELECT d.value, COUNT(*)
	       FROM branch_facts bf
	       JOIN facts f ON f.id = bf.fact_id, json_each(f.domain) d
	       WHERE bf.branch_id = ? AND d.value IS NOT NULL`
	dargs := []any{branchID}
	if pathPrefix != "" {
		dq += ` AND f.path LIKE ?`
		dargs = append(dargs, pathPrefix+"%")
	}
	dq += ` GROUP BY d.value`
	drows, err := conn(ctx, si.rh.db).QueryContext(ctx, dq, dargs...)
	if err != nil {
		return res, err
	}
	defer drows.Close()
	for drows.Next() {
		var k string
		var n int
		if err := drows.Scan(&k, &n); err != nil {
			return res, err
		}
		res.Domains[k] = n
	}

	// Entity counts via json_each.
	eq := `SELECT e.value, COUNT(*)
	       FROM branch_facts bf
	       JOIN facts f ON f.id = bf.fact_id, json_each(f.entities) e
	       WHERE bf.branch_id = ? AND e.value IS NOT NULL`
	eargs := []any{branchID}
	if pathPrefix != "" {
		eq += ` AND f.path LIKE ?`
		eargs = append(eargs, pathPrefix+"%")
	}
	eq += ` GROUP BY e.value`
	erows, err := conn(ctx, si.rh.db).QueryContext(ctx, eq, eargs...)
	if err != nil {
		return res, err
	}
	defer erows.Close()
	for erows.Next() {
		var k string
		var n int
		if err := erows.Scan(&k, &n); err != nil {
			return res, err
		}
		res.Entities[k] = n
	}

	return res, nil
}
