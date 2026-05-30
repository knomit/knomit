package store

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Domain-tag matching: a domain tag is canonicalised deterministically at the
// index-build path (NFC + case-fold + de-hyphenize), then tokenised + stemmed
// for "containment" matching (a query matches a tag when ALL its tokens are
// present in the tag's token set). This unifies case/hyphen/space and plural
// variants and makes matching word-order-independent, with no FTS5/embeddings.
// Authored tags stay in git; this canonical/token form is derived index state.

// domainCaser performs Unicode case folding (language-neutral). Allocated once.
var domainCaser = cases.Fold()

// canonicalizeDomain normalises a domain tag for matching and junction storage:
// NFC → case-fold → replace hyphens and Unicode whitespace with a single space →
// trim. Underscores are PRESERVED so identifier-like tags (commit_log) stay one
// token. Pure and idempotent.
func canonicalizeDomain(s string) string {
	s = norm.NFC.String(s)
	s = domainCaser.String(s)
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true // leading-trim: suppress leading spaces
	for _, r := range s {
		if r == '-' || unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimRight(b.String(), " ")
}

// stemDomainToken applies a minimal, symmetric, English plural stemmer for
// MATCH-ONLY use — the stem is never displayed or stored as the canonical tag,
// so its quirks (analysis→analysi) are harmless because both query and stored
// tokens are stemmed identically. Rules: ies→y; strip (s|x|ch|sh)es; strip a
// trailing s (not ss, len>3).
func stemDomainToken(t string) string {
	if len(t) > 4 && strings.HasSuffix(t, "ies") {
		return t[:len(t)-3] + "y"
	}
	for _, suf := range []string{"ses", "xes", "ches", "shes"} {
		if strings.HasSuffix(t, suf) {
			return t[:len(t)-2]
		}
	}
	if len(t) > 3 && strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "ss") {
		return t[:len(t)-1]
	}
	return t
}

// domainTokens returns the stemmed, de-duplicated token set of a canonical
// domain (split on whitespace). Order follows first appearance; callers treat it
// as a set. Pass the output of canonicalizeDomain.
func domainTokens(canonical string) []string {
	fields := strings.Fields(canonical)
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		st := stemDomainToken(f)
		if _, ok := seen[st]; ok {
			continue
		}
		seen[st] = struct{}{}
		out = append(out, st)
	}
	return out
}

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
