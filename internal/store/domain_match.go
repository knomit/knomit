package store

import (
	"context"
	"fmt"
	"strings"

	"knomit/internal/textnorm"
)

// Domain-tag matching: a domain tag is canonicalised deterministically at the
// index-build path (NFC + case-fold + de-hyphenize), then tokenised + stemmed
// for "containment" matching (a query matches a tag when ALL its tokens are
// present in the tag's token set). This unifies case/hyphen/space and plural
// variants and makes matching word-order-independent, with no FTS5/embeddings.
// Authored tags stay in git; this canonical/token form is derived index state.

// The three normalizers below moved to internal/textnorm so internal/fact can
// use the SAME definition of "the same token" for the motif subject-word strip
// without inverting the store -> fact dependency (internal/fact imports no
// internal package, by design). Two stemmers are two things that drift, and a
// drifted stemmer lets a motif that renames its own fact's subject past the
// strip.
//
// They stay as unexported names here because every call site in this package
// reads better with the domain-specific name, and because renaming ~30 call
// sites would bury a mechanical extraction inside an unrelated diff. The
// existing domain-match suite is the zero-diff gate: no test was edited.
func canonicalizeDomain(s string) string     { return textnorm.Canonicalize(s) }
func stemDomainToken(t string) string        { return textnorm.Stem(t) }
func domainTokens(canonical string) []string { return textnorm.Tokens(canonical) }

// DomainTagMatches reports whether queryTag matches factTag under the SAME default
// semantics SearchOptions.Domain uses (search_query.go:339-373): canonical
// slash-hierarchy descendant-or-equal, OR canonical token containment (all of
// queryTag's stemmed/de-hyphenized tokens present in factTag's token set). This is
// the single in-memory definition; SQL paths use fact_domain_tokens for the same.
func DomainTagMatches(factTag, queryTag string) bool {
	fc, qc := canonicalizeDomain(factTag), canonicalizeDomain(queryTag)
	if fc == qc {
		return true
	}
	// slash-hierarchy descendant-or-equal: "store" matches "store/sqlite".
	if strings.HasPrefix(fc, qc+"/") {
		return true
	}
	// token containment: every query token must appear in the fact's token set.
	qt := domainTokens(qc)
	if len(qt) == 0 {
		return false
	}
	fset := make(map[string]struct{})
	for _, t := range domainTokens(fc) {
		fset[t] = struct{}{}
	}
	for _, t := range qt {
		if _, ok := fset[t]; !ok {
			return false
		}
	}
	return true
}

// EntityTagMatches reports case-folded equality, mirroring the COLLATE NOCASE
// fact_entities junction. Entities are not tokenized (no entity token table).
func EntityTagMatches(factEntity, queryEntity string) bool {
	return textnorm.Fold(factEntity) == textnorm.Fold(queryEntity)
}

// CanonicalizeTag exposes canonicalizeDomain for consumers that must group tags
// by their canonical form (e.g. bridge seeding). Same NFC+case-fold+de-hyphenize
// rule as domain-tag matching — case/hyphen variants of a tag collapse to the
// same key, so "Store" and "store" unify before grouping.
func CanonicalizeTag(s string) string { return canonicalizeDomain(s) }

// repopulateDomainTokens rebuilds fact_domain_tokens for EVERY fact version —
// HEAD and historical (superseded) alike — from the immutable authored domains
// (facts.domain JSON), canonicalised. This deliberately covers all versions, not
// just _rebuild_entries: historical fact_domains rows are left untouched
// (immutable artifacts we never rewrite), but their derived tokens are populated
// so token containment can reach them if a future caller wants cross-version
// matching. The token's `domain` is the CANONICAL form even when the historical
// fact_domains value is still raw — the token table is self-contained for
// containment (it is never joined back to fact_domains).
//
// Clear-then-repopulate keeps the table consistent with the current
// canonicalisation logic on every rebuild (historical fact rows keep stable
// rowids, so they receive no CASCADE cleanup of their own). Rows are collected
// before writing so the read cursor is closed first (single-connection safety).
func (si *searchIndex) repopulateDomainTokens(ctx context.Context) error {
	db := conn(ctx, si.rh.db)
	if _, err := db.ExecContext(ctx, `DELETE FROM fact_domain_tokens`); err != nil {
		return fmt.Errorf("repopulateDomainTokens: clear: %w", err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT f.id, j.value
		FROM facts f
		JOIN json_each(f.domain) j
		WHERE j.value IS NOT NULL AND j.value != ''`)
	if err != nil {
		return fmt.Errorf("repopulateDomainTokens: query: %w", err)
	}
	type row struct {
		factID int64
		raw    string
	}
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.factID, &r.raw); err != nil {
			rows.Close()
			return fmt.Errorf("repopulateDomainTokens: scan: %w", err)
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("repopulateDomainTokens: rows: %w", err)
	}
	for _, r := range pending {
		canon := canonicalizeDomain(r.raw)
		if canon == "" {
			continue
		}
		for _, tok := range domainTokens(canon) {
			if _, err := db.ExecContext(ctx,
				`INSERT OR IGNORE INTO fact_domain_tokens(fact_id, domain, token) VALUES (?, ?, ?)`,
				r.factID, canon, tok); err != nil {
				return fmt.Errorf("repopulateDomainTokens: insert: %w", err)
			}
		}
	}
	return nil
}
