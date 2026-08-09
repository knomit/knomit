package store

import (
	"context"
	"fmt"
	"strings"

	"knomit/internal/fact"
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

// excludedTypePlaceholders returns the "?,?,...,?" SQL placeholder list sized
// to highlightExcludedTypes, so highlights() and separationCounts() build
// their NOT IN clauses identically instead of each repeating the
// strings.Repeat incantation.
func excludedTypePlaceholders() string {
	return strings.TrimSuffix(strings.Repeat("?,", len(highlightExcludedTypes)), ",")
}

// MaxHighlights is the top-N returned per repo, and per mount before the lens
// union re-ranks and truncates to the same N. Exported because the lens
// handler truncates the merged list to the same bound.
const MaxHighlights = 10

// Highlight is one row of the overview's highlights list.
//
// Impact is the count of DISTINCT facts this one was derived from — i.e. the
// number of distinct target paths reached by its outgoing DERIVED_FROM edges,
// not the edge row count. The two differ: graphAddDerivedFromAtCommitTx
// (derived_from.go) writes one edge PER REF PER source_commit, deliberately —
// graphDerivedFromEdgeExists dedups on (src, tgt, source_commit,
// target_commit) because edges are immutable lineage assertions at a commit.
// When the same blob is re-indexed at a second commit (unchanged content),
// its lineage is asserted a second time: a second edge per ref, same target.
// Counting edge rows there would double the number a fact's own connections
// panel shows (which groups by target path), breaking the "verifiable by
// opening the fact's connections" property the ranking depends on. It is
// deliberately GLOBAL: never filtered by pathPrefix, so the same fact reports
// the same number from the repo root and from its own folder. There is
// deliberately NO commit_hash on the wire — highlights list live facts and
// open live, like a Library row.
type Highlight struct {
	Path        string  `json:"path"`
	Title       string  `json:"title"`
	Type        string  `json:"type"`
	Confidence  float64 `json:"confidence"`
	Impact      int     `json:"impact"`
	CommittedAt int64   `json:"committed_at"`
}

// liveFactNodeCTE returns the "WITH live AS (...), node AS (...), outd AS
// (...)" fragment that maps live facts on a branch to their graph node ids
// and out-degree, plus the args for its placeholders in order.
//
// The join through branch_facts is LOAD-BEARING: the graph is temporal and
// holds one node per fact VERSION, keyed by (path, blob_hash) — see
// graphNodeIDByBlob. Reading node_props_text directly returns historical
// versions, including near-duplicates a prior review already subsumed.
// branch_facts is UNIQUE(branch_id, path), so this yields exactly the live set
// with no collapse-by-path step.
//
// pathPrefix scoping uses the same conditional-AND idiom as the pre-existing
// Stats aggregates (index.go: `if pathPrefix != "" { q += " AND ..." }`)
// rather than a `(? = "" OR ...)` inline form, so the two idioms in this file
// don't drift from the rest of the package (see the M3 finding on the
// 2026-08-04 overview-highlights review). This fragment is a function rather
// than a constant precisely so both callers (highlights() and
// separationCounts()) can share the same idiom despite one of them (via
// separationCounts) always passing pathPrefix == "" — a constant string
// couldn't vary its own placeholder count.
//
// outd counts DISTINCT target PATHS per source node, not edge rows: a
// DERIVED_FROM edge is written once per ref per source_commit (see the
// Highlight.Impact doc comment), so the same lineage re-asserted at a second
// commit must not double the count.
func liveFactNodeCTE(branchID int64, pathPrefix string) (string, []any) {
	q := `
		WITH live AS (
		    SELECT f.path, f.title, f.type, f.confidence, f.blob_hash, bf.commit_hash
		      FROM branch_facts bf
		      JOIN facts f ON f.id = bf.fact_id
		     WHERE bf.branch_id = ?`
	args := []any{branchID}
	if pathPrefix != "" {
		q += ` AND f.path LIKE ?`
		args = append(args, pathPrefix+"%")
	}
	q += `
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
		    SELECT e.source_id AS nid, COUNT(DISTINCT tp.value) AS d
		      FROM edges e
		      JOIN node_props_text tp ON tp.node_id = e.target_id
		      JOIN property_keys ktp ON ktp.id = tp.key_id AND ktp.key = 'path'
		     WHERE e.type = 'DERIVED_FROM'
		     GROUP BY e.source_id
		)`
	return q, args
}

// highlights returns the top-N facts for the branch, path-scoped, ranked by
// the requested axis.
//
// Excluded types never appear — UNLESS they are all the scope has. The
// exclusion exists to stop the substrate burying the distilled layer (on core,
// 1,186 live observations against 128 syntheses); in a folder holding only
// observations there is no distilled layer to bury, so the exclusion protects
// nothing and merely deletes the section. A scope like that gets its own top-N
// instead of an empty panel.
//
// The fallback fires only on an EMPTY result, so one eligible fact anywhere in
// scope is enough to keep the excluded types out — it can never dilute a list
// that has something to show.
//
// The second return value reports whether the fallback fired. It exists because
// "is there a distilled layer to bury here" is a question about a SCOPE, and a
// lens union is a scope this function cannot see: a mount that is pure
// observation answers "no" for itself and would carry its observations into a
// merge with mounts that do have one. Only the caller assembling the union
// knows that, so it needs to be told which lists are fallbacks — see
// handleHALLensStats.
func (fq *factQuery) highlights(ctx context.Context, branchID int64, pathPrefix, axis string) ([]Highlight, bool, error) {
	out, err := fq.highlightRows(ctx, branchID, pathPrefix, axis, true)
	if err != nil {
		return nil, false, err
	}
	if len(out) > 0 {
		return out, false, nil
	}
	// Nothing eligible in this scope — see the exclusion note above.
	out, err = fq.highlightRows(ctx, branchID, pathPrefix, axis, false)
	if err != nil {
		return nil, false, err
	}
	// An empty scope is not a fallback: there was nothing to fall back TO, and
	// calling it one would let an empty mount suppress nothing while looking
	// like it had something to suppress.
	return out, len(out) > 0, nil
}

// highlightRows runs the ranked top-N query, optionally applying the type
// exclusion. Same ORDER BY either way, so a fallback list is ranked by the
// requested axis rather than arriving in whatever order the rows came back.
func (fq *factQuery) highlightRows(ctx context.Context, branchID int64, pathPrefix, axis string, exclude bool) ([]Highlight, error) {
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

	cte, args := liveFactNodeCTE(branchID, pathPrefix)
	where := ``
	if exclude {
		// Built from highlightExcludedTypes rather than a literal list so the
		// exclusion has one source of truth.
		where = ` WHERE live.type NOT IN (` + excludedTypePlaceholders() + `)`
		for _, t := range highlightExcludedTypes {
			args = append(args, t)
		}
	}
	q := cte + `
		SELECT live.path, live.title, live.type, live.confidence,
		       COALESCE(outd.d, 0) AS impact,
		       COALESCE(cl.committed_at, 0)
		  FROM live
		  LEFT JOIN node ON node.pa = live.path AND node.bh = live.blob_hash
		  LEFT JOIN outd ON outd.nid = node.nid
		  LEFT JOIN commit_log cl
		         ON cl.commit_hash = live.commit_hash AND cl.path = live.path
		` + where + `
		` + order + `
		 LIMIT ?`

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

// AxisFromSeparation is the SINGLE definition of the impact/confidence
// separation rule: given the pooled fact count and total out-degree of the
// top (distilled, non-excluded) layer and of observations, it decides
// whether impact ranking discriminates.
//
// The test is the ratio of MEAN out-degree between the two groups — NOT the
// fraction of facts carrying any edge. That simpler rule was tried and got
// two of four corpora backwards: knomit-kb (26% with edges) would have
// defaulted to impact and agentic-engineering (18%) to confidence, both
// wrong.
//
// Callers pass COUNTS and SUMS (not pre-divided means) specifically so this
// can be used as a POOLING primitive: separationCounts (below) computes them
// repo-scoped for a single repo, and the lens union handler
// (handlers_lenses_stats.go) sums them across mounts before calling this —
// averaging pre-computed per-mount means would weight a 5-fact mount the same
// as a 5000-fact one, and a mount with zero eligible facts would corrupt an
// average instead of correctly contributing (0, 0) to a sum.
func AxisFromSeparation(topFacts, topEdges, obsFacts, obsEdges int) string {
	// No distilled layer at all — nothing for impact to rank.
	if topFacts <= 0 {
		return AxisConfidence
	}
	topMean := float64(topEdges) / float64(topFacts)
	if topMean <= 0 {
		return AxisConfidence
	}
	// Observations carry zero out-degree: infinite separation. This is the
	// agentic-engineering shape (all edges concentrated in 28 syntheses).
	if obsFacts <= 0 {
		return AxisImpact
	}
	obsMean := float64(obsEdges) / float64(obsFacts)
	if obsMean == 0 {
		return AxisImpact
	}
	if topMean/obsMean >= separationThreshold {
		return AxisImpact
	}
	return AxisConfidence
}

// separationCounts computes the four pooled counters AxisFromSeparation
// needs: the live, non-excluded (top-layer) fact count and total out-degree,
// and the same two numbers restricted to observations.
//
// Repo-scoped by design: no pathPrefix. Per-folder ratios are noisy on small
// samples (core ranges 42x to 143x to undefined across its top-level folders)
// and a folder dipping below the threshold would flip the control while the
// user navigates.
func (fq *factQuery) separationCounts(ctx context.Context, branchID int64) (topFacts, topEdges, obsFacts, obsEdges int, err error) {
	excluded := excludedTypePlaceholders()
	q := `
		SELECT
		    COUNT(CASE WHEN j.type NOT IN (` + excluded + `) THEN 1 END),
		    COALESCE(SUM(CASE WHEN j.type NOT IN (` + excluded + `) THEN j.d END), 0),
		    COUNT(CASE WHEN j.type = ? THEN 1 END),
		    COALESCE(SUM(CASE WHEN j.type = ? THEN j.d END), 0)
		  FROM (
		    SELECT live.type AS type, COALESCE(outd.d, 0) AS d
		      FROM live
		      LEFT JOIN node ON node.pa = live.path AND node.bh = live.blob_hash
		      LEFT JOIN outd ON outd.nid = node.nid
		  ) j`
	cte, args := liveFactNodeCTE(branchID, "")
	for _, t := range highlightExcludedTypes {
		args = append(args, t)
	}
	for _, t := range highlightExcludedTypes {
		args = append(args, t)
	}
	args = append(args, string(fact.Observation), string(fact.Observation))
	err = conn(ctx, fq.rh.db).QueryRowContext(ctx, cte+q, args...).
		Scan(&topFacts, &topEdges, &obsFacts, &obsEdges)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("separationCounts: %w", err)
	}
	return topFacts, topEdges, obsFacts, obsEdges, nil
}

// typeCounts returns live fact counts per epistemic type, path-scoped. Unlike
// highlights this includes observations — it drives the type pills, which
// describe the whole folder.
//
// Uses the same conditional-AND path-scoping idiom as the pre-existing Stats
// aggregates (index.go) rather than a `(? = "" OR ...)` inline form — see the
// liveFactNodeCTE doc comment for why the two idioms coexist in this file.
func (fq *factQuery) typeCounts(ctx context.Context, branchID int64, pathPrefix string) (map[string]int, error) {
	res := make(map[string]int)
	q := `SELECT f.type, COUNT(*)
	        FROM branch_facts bf
	        JOIN facts f ON f.id = bf.fact_id
	       WHERE bf.branch_id = ?`
	args := []any{branchID}
	if pathPrefix != "" {
		q += ` AND f.path LIKE ?`
		args = append(args, pathPrefix+"%")
	}
	q += ` GROUP BY f.type`
	rows, err := conn(ctx, fq.rh.db).QueryContext(ctx, q, args...)
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
