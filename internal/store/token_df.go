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
	// The LEFT JOIN plus the `OR` is what makes an unresolved corpus behave
	// exactly as it did before aliases existed — a spelling with no alias row
	// matches only itself. An INNER JOIN here would silently return 0 for
	// every motif until the first RebuildAliases, which is the kind of
	// zero that reads as "no carriers" rather than "not resolved yet".
	//
	// OPEN QUESTION, raised by the 2026-08-23 review remediation and NOT
	// resolved here. This matches on canonical id, so on an UNRESOLVED corpus
	// two spellings of one mechanism each report df 1, while Clusters and
	// VocabularyHealth — which key by cluster (motifClusterKeyExpr) — report
	// one cluster with df 2. Both readings are defensible: this one is "a
	// spelling nothing has resolved is its own cluster" and is pinned by
	// TestTokenDF_Motif_UnresolvedCorpusBehavesAsBefore; theirs is "read it the
	// way the next rebuild will". They disagree only in the window between a
	// fact being written and the next session's rebuild. Changing this one is a
	// contract change with an explicit test asserting the current answer, so it
	// is the designer's call, not a remediation's.
	q := fmt.Sprintf(
		`SELECT COUNT(DISTINCT bf.path)
		   FROM branch_facts bf
		   JOIN %s j ON j.fact_id = bf.fact_id
		  WHERE bf.branch_id = ? AND j.%s = ?`, table, col)
	if kind == "motif" {
		q = `SELECT COUNT(DISTINCT bf.path)
		       FROM branch_facts bf
		       JOIN fact_motifs j ON j.fact_id = bf.fact_id
		       LEFT JOIN motif_aliases a
		              ON a.branch_id = bf.branch_id AND a.motif = j.motif
		      WHERE bf.branch_id = ?
		        AND COALESCE(a.canonical_id, j.motif) = ?`
	}
	var n int
	err = conn(ctx, gs.rh.db).QueryRowContext(ctx, q, branchID, token).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("TokenDF %s=%q: %w", kind, token, err)
	}
	return n, nil
}
