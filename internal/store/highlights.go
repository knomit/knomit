package store

import (
	"context"
	"fmt"
	"strings"
)

// Axis names for Highlights ranking. All three order the SQL; Recent is
// requestable but is never returned as the server's default recommendation.
const (
	AxisImpact     = "impact"
	AxisConfidence = "confidence"
	AxisRecent     = "recent"
)

// NormalizeAxis maps a caller-supplied axis to a known one, falling back to
// the supplied default. Unknown values never reach the ORDER BY.
func NormalizeAxis(requested, fallback string) string {
	switch requested {
	case AxisImpact, AxisConfidence, AxisRecent:
		return requested
	default:
		return fallback
	}
}

// highlightExcludedTypes are the epistemic types that never appear in
// highlights. Observations and references are the substrate the distilled
// layer is built FROM; surfacing them would bury it (on core, 1134 live
// observations against 124 syntheses). This is the single source of truth
// for the exclusion — highlights() builds its SQL NOT IN list from this
// slice rather than repeating the literals, so the two cannot drift.
var highlightExcludedTypes = []string{"observation", "reference"}

// MaxHighlights is the top-N returned per repo, and per mount before the lens
// union re-ranks and truncates to the same N. Exported because the lens
// handler truncates the merged list to the same bound.
const MaxHighlights = 10

// Highlight is one row of the overview's highlights list.
//
// Impact is the count of outgoing DERIVED_FROM edges — how many facts this one
// was derived from. It is deliberately GLOBAL: never filtered by pathPrefix, so
// the same fact reports the same number from the repo root and from its own
// folder. There is deliberately NO commit_hash on the wire — highlights list
// live facts and open live, like a Library row.
type Highlight struct {
	Path        string  `json:"path"`
	Title       string  `json:"title"`
	Type        string  `json:"type"`
	Confidence  float64 `json:"confidence"`
	Impact      int     `json:"impact"`
	CommittedAt int64   `json:"committed_at"`
}

// liveFactNodeCTE maps live facts on a branch to their graph node ids.
//
// The join through branch_facts is LOAD-BEARING: the graph is temporal and
// holds one node per fact VERSION, keyed by (path, blob_hash) — see
// graphNodeIDByBlob. Reading node_props_text directly returns historical
// versions, including near-duplicates a prior review already subsumed.
// branch_facts is UNIQUE(branch_id, path), so this yields exactly the live set
// with no collapse-by-path step.
//
// Placeholders, in order: branchID, pathPrefix, pathPrefix.
const liveFactNodeCTE = `
	WITH live AS (
	    SELECT f.path, f.title, f.type, f.confidence, f.blob_hash, bf.commit_hash
	      FROM branch_facts bf
	      JOIN facts f ON f.id = bf.fact_id
	     WHERE bf.branch_id = ?
	       AND (? = '' OR f.path LIKE ? || '%')
	),
	node AS (
	    SELECT npp.value AS pa, npb.value AS bh, npp.node_id AS nid
	      FROM node_props_text npp
	      JOIN property_keys kp ON kp.id = npp.key_id AND kp.key = 'path'
	      JOIN node_props_text npb ON npb.node_id = npp.node_id
	      JOIN property_keys kb ON kb.id = npb.key_id AND kb.key = 'blob_hash'
	      JOIN node_labels nl ON nl.node_id = npp.node_id AND nl.label = 'Fact'
	),
	outd AS (
	    SELECT source_id AS nid, COUNT(*) AS d
	      FROM edges
	     WHERE type = 'DERIVED_FROM'
	     GROUP BY source_id
	)`

// highlights returns the top-N facts for the branch, path-scoped, ranked by
// the requested axis. Excluded types never appear.
func (fq *factQuery) highlights(ctx context.Context, branchID int64, pathPrefix, axis string) ([]Highlight, error) {
	out := make([]Highlight, 0, MaxHighlights)

	var order string
	switch axis {
	case AxisConfidence:
		order = `ORDER BY live.confidence DESC, cl.committed_at DESC`
	case AxisRecent:
		order = `ORDER BY cl.committed_at DESC, live.confidence DESC`
	default:
		order = `ORDER BY impact DESC, live.confidence DESC`
	}

	// Built from highlightExcludedTypes rather than a literal list so the
	// exclusion has one source of truth.
	excludePlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(highlightExcludedTypes)), ",")
	q := liveFactNodeCTE + `
		SELECT live.path, live.title, live.type, live.confidence,
		       COALESCE(outd.d, 0) AS impact,
		       COALESCE(cl.committed_at, 0)
		  FROM live
		  LEFT JOIN node ON node.pa = live.path AND node.bh = live.blob_hash
		  LEFT JOIN outd ON outd.nid = node.nid
		  LEFT JOIN commit_log cl
		         ON cl.commit_hash = live.commit_hash AND cl.path = live.path
		 WHERE live.type NOT IN (` + excludePlaceholders + `)
		` + order + `
		 LIMIT ?`

	args := make([]any, 0, 3+len(highlightExcludedTypes)+1)
	args = append(args, branchID, pathPrefix, pathPrefix)
	for _, t := range highlightExcludedTypes {
		args = append(args, t)
	}
	args = append(args, MaxHighlights)

	rows, err := conn(ctx, fq.rh.db).QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("highlights: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var h Highlight
		if err := rows.Scan(&h.Path, &h.Title, &h.Type, &h.Confidence,
			&h.Impact, &h.CommittedAt); err != nil {
			return nil, fmt.Errorf("highlights scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// separationThreshold is the ratio above which impact ranking is judged to
// discriminate. Measured across four real corpora: core 32x, agentic-
// engineering infinite (zero observation out-degree), langchain-kb 1.0x,
// knomit-kb 0.9x. Nothing observed between 1.0x and 32x, so the exact value is
// unconstrained anywhere in roughly 1.5x-10x.
const separationThreshold = 3.0

// defaultAxis decides whether impact ranking discriminates for this repo.
//
// The test is the ratio of mean out-degree between the distilled layer and
// observations — NOT the fraction of facts carrying any edge. That simpler
// rule was tried and got two of four corpora backwards: knomit-kb (26% with
// edges) would have defaulted to impact and agentic-engineering (18%) to
// confidence, both wrong.
//
// Repo-scoped by design: no pathPrefix. Per-folder ratios are noisy on small
// samples (core ranges 42x to 143x to undefined across its top-level folders)
// and a folder dipping below the threshold would flip the control while the
// user navigates.
func (fq *factQuery) defaultAxis(ctx context.Context, branchID int64) (string, error) {
	var topMean, obsMean *float64
	q := `
		SELECT AVG(CASE WHEN j.type NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(highlightExcludedTypes)), ",") + `) THEN j.d END),
		       AVG(CASE WHEN j.type = 'observation' THEN j.d END)
		  FROM (
		    SELECT live.type AS type, COALESCE(outd.d, 0) AS d
		      FROM live
		      LEFT JOIN node ON node.pa = live.path AND node.bh = live.blob_hash
		      LEFT JOIN outd ON outd.nid = node.nid
		  ) j`
	args := make([]any, 0, 3+len(highlightExcludedTypes))
	args = append(args, branchID, "", "")
	for _, t := range highlightExcludedTypes {
		args = append(args, t)
	}
	err := conn(ctx, fq.rh.db).QueryRowContext(ctx, liveFactNodeCTE+q, args...).Scan(&topMean, &obsMean)
	if err != nil {
		return AxisConfidence, fmt.Errorf("defaultAxis: %w", err)
	}

	// No distilled layer at all — nothing for impact to rank.
	if topMean == nil || *topMean <= 0 {
		return AxisConfidence, nil
	}
	// Observations carry zero out-degree: infinite separation. This is the
	// agentic-engineering shape (all edges concentrated in 28 syntheses).
	if obsMean == nil || *obsMean == 0 {
		return AxisImpact, nil
	}
	if *topMean / *obsMean >= separationThreshold {
		return AxisImpact, nil
	}
	return AxisConfidence, nil
}

// typeCounts returns live fact counts per epistemic type, path-scoped. Unlike
// highlights this includes observations — it drives the type pills, which
// describe the whole folder.
func (fq *factQuery) typeCounts(ctx context.Context, branchID int64, pathPrefix string) (map[string]int, error) {
	res := make(map[string]int)
	q := `SELECT f.type, COUNT(*)
	        FROM branch_facts bf
	        JOIN facts f ON f.id = bf.fact_id
	       WHERE bf.branch_id = ?
	         AND (? = '' OR f.path LIKE ? || '%')
	       GROUP BY f.type`
	rows, err := conn(ctx, fq.rh.db).QueryContext(ctx, q, branchID, pathPrefix, pathPrefix)
	if err != nil {
		return nil, fmt.Errorf("typeCounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, fmt.Errorf("typeCounts scan: %w", err)
		}
		res[k] = n
	}
	return res, rows.Err()
}
