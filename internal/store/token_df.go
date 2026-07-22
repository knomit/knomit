package store

import (
	"context"
	"database/sql"
	"fmt"
)

// TokenDF returns the number of facts LIVE on `branch` at HEAD that carry the
// given domain or entity tag — the document-frequency input to discovery's
// `spec` (rarity) signal. It is a plain indexed COUNT over the junction tables
// joined to branch_facts for liveness; it never reads or tokenizes fact body
// text. kind must be "domain" or "entity".
//
//   - "domain": token is the CANONICAL tag (canonicalizeDomain form). fact_domains
//     stores the canonical form, so this is an exact indexed match.
//   - "entity": token is matched case-insensitively (fact_entities is COLLATE
//     NOCASE); pass the authored entity form. Entities are not de-hyphenized.
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
	default:
		return 0, fmt.Errorf("TokenDF: invalid kind %q: must be \"domain\" or \"entity\"", kind)
	}
	branchID, err := gs.rh.branchID(ctx, branch)
	if err != nil {
		return 0, fmt.Errorf("TokenDF: branchID: %w", err)
	}
	q := fmt.Sprintf(
		`SELECT COUNT(DISTINCT bf.path)
		   FROM branch_facts bf
		   JOIN %s j ON j.fact_id = bf.fact_id
		  WHERE bf.branch_id = ? AND j.%s = ?`, table, col)
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
