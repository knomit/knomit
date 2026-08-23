package store

import (
	"context"
	"database/sql"
	"fmt"
)

// TokenDF returns the number of facts LIVE on `branch` at HEAD that carry the
// given domain, entity or motif tag — the document-frequency input to
// discovery's `spec` (rarity) signal. It is a plain indexed COUNT over the
// junction tables joined to branch_facts for liveness; it never reads or
// tokenizes fact body text. kind must be "domain", "entity" or "motif".
//
//   - "domain": token is the CANONICAL tag (canonicalizeDomain form). fact_domains
//     stores the canonical form, so this is an exact indexed match.
//
//   - "entity": token is matched case-insensitively (fact_entities is COLLATE
//     NOCASE); pass the authored entity form. Entities are not de-hyphenized.
//
//   - "motif": token is a CANONICAL MOTIF ID, and df is counted over the whole
//     alias cluster it names — every live fact carrying any member spelling,
//     counted once each (blueprint §3.1, "TokenDF and all matching key on
//     canonical ids"). Matching is case-insensitive (fact_motifs is COLLATE
//     NOCASE), but motifs are validated lowercase, so that only ever forgives
//     a hand-edited file.
//
//     Resolution happens HERE rather than in the caller, and that is
//     load-bearing rather than stylistic: a caller cannot get this right by
//     summing per-spelling counts, because a fact carrying two spellings of
//     one mechanism would be counted twice — inflating df exactly where the
//     Phase-3 band is tightest. One definition of motif df, in the function
//     bridge scoring already calls.
//
//     On a corpus with no alias rows this degrades exactly to the pre-alias
//     behaviour: every spelling is its own singleton cluster, so the count is
//     what a plain match on the as-written string would have returned.
//
//     Storage is unaffected (MN3): the alias table is derived state, and a
//     motif is still stored exactly as its author typed it.
//
// Liveness + branch scoping come from branch_facts (UNIQUE(branch_id, path) =>
// one row per live path per branch), exactly as BlastRadius does.
func (gs *graphStore) TokenDF(ctx context.Context, branch, token, kind string) (int, error) {
	var table, col string
	switch kind {
	case "domain":
		table, col = "fact_domains", "domain"
	case "entity":
		table, col = "fact_entities", "entity"
	case "motif":
		table, col = "fact_motifs", "motif"
	default:
		return 0, fmt.Errorf("TokenDF: invalid kind %q: must be \"domain\", \"entity\" or \"motif\"", kind)
	}
	branchID, err := gs.rh.branchID(ctx, branch)
	if err != nil {
		return 0, fmt.Errorf("TokenDF: branchID: %w", err)
	}
	// Motifs resolve through the alias table: the token names a CLUSTER, so
	// every member spelling counts, and COUNT(DISTINCT bf.path) makes a fact
	// carrying several of them one carrier rather than several.
	//
	// READ IT THE WAY THE NEXT REBUILD WILL (designer ruling, 2026-08-23).
	//
	// Both sides key by CLUSTER — the mechanical grouping key as the floor, the
	// stored alias overlay on top — rather than by canonical id. The grouping is
	// a PURE FUNCTION of the spellings: "silent-fallback" and "silent-fallbacks"
	// stem to the same key whether or not anything has written that down, so the
	// rebuild CACHES the answer and does not create it. No reader may give a
	// different number according to whether the cache job has run.
	//
	// Matching COALESCE(a.canonical_id, j.motif) did exactly that. On a corpus
	// with facts written since the last rebuild — which is any live corpus, most
	// of the time — the two spellings reported df 1 each here while Clusters and
	// VocabularyHealth reported one cluster with df 2. The file header's own Q5
	// rationale already ruled that out ("a df counting one spelling would bench a
	// motif written three ways at df=1 three times"); this query simply had a
	// window where it did that anyway.
	//
	// The judge layer is untouched by this. Token-disjoint spellings still merge
	// only by a recorded judge decision, which arrives through a.cluster_key —
	// the overlay this expression prefers whenever it is present.
	//
	// The LEFT JOIN still matters for a different reason: an INNER JOIN would
	// return 0 for every motif until the first RebuildAliases, and that zero
	// reads as "no carriers" rather than "not resolved yet".
	//
	// The old posture, and why it is gone: it described the world before the
	// alias rebuild ran in every Plan. When the rebuild was reachable only from
	// the judge item's apply path (the C1 deadlock), "unresolved" was a lasting
	// state a reader had to have an answer for, and "each spelling is its own
	// cluster" was that answer. The rebuild is now unconditional at the top of
	// every medium-effort Plan, so unresolved is a window between a write and
	// the next session — not a state to design a second posture around.
	q := fmt.Sprintf(
		`SELECT COUNT(DISTINCT bf.path)
		   FROM branch_facts bf
		   JOIN %s j ON j.fact_id = bf.fact_id
		  WHERE bf.branch_id = ? AND j.%s = ?`, table, col)
	args := []any{branchID, token}
	if kind == "motif" {
		// The QUERY TOKEN resolves through the same three steps as each stored
		// spelling, so a caller may name any member and gets the cluster either
		// way. Written out rather than shared with motifClusterKeyExpr because
		// this side reads a parameter, not a joined column — the steps are the
		// same and the shapes are not.
		q = `SELECT COUNT(DISTINCT bf.path)
		       FROM branch_facts bf
		       JOIN fact_motifs j ON j.fact_id = bf.fact_id
		       LEFT JOIN motif_aliases a
		              ON a.branch_id = bf.branch_id AND a.motif = j.motif
		      WHERE bf.branch_id = ?
		        AND ` + motifClusterKeyExpr("j.motif") + ` = COALESCE(
		              NULLIF((SELECT a2.cluster_key FROM motif_aliases a2
		                       WHERE a2.branch_id = bf.branch_id AND a2.motif = ?), ''),
		              NULLIF(knomit_motif_key(?), ''), ?)`
		args = []any{branchID, token, token, token}
	}
	var n int
	err = conn(ctx, gs.rh.db).QueryRowContext(ctx, q, args...).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("TokenDF %s=%q: %w", kind, token, err)
	}
	return n, nil
}
