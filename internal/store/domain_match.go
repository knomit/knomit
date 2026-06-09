package store

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/gertd/go-pluralize"
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

// domainPluralizer singularizes plural tokens to a match key. go-pluralize is a
// port of the widely-used JS `pluralize`, with a real irregular/exception table
// (not naive suffix rules), so it is symmetric and idempotent on the irregulars
// a hand-rolled or Porter/Snowball stemmer breaks: analyses≡analysis,
// indices≡index, matrices≡matrix, theses≡thesis. Allocated once; configured for
// the technical vocabulary in stemDomainToken's guards. Pure-Go, zero deps.
var domainPluralizer = pluralize.NewClient()

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

// stemDomainToken normalises a token to its singular form as a MATCH-ONLY key —
// the result is never displayed or stored as the canonical tag, only used so a
// query token and a stored token collapse to the same key ("vulnerabilities" ≡
// "vulnerability"). It delegates to go-pluralize, which is symmetric and
// idempotent on the irregulars a hand-rolled or Porter/Snowball stemmer breaks
// (analyses≡analysis, indices≡index, matrices≡matrix, theses≡thesis), verified
// against the real domain corpus.
//
// Two guards prevent over-singularizing non-plurals that merely end in 's'
// (any pluralizer treats a trailing 's' as plural, which is what we DON'T want
// for these — confirmed against real knomit domains):
//   - len <= 3: acronyms/identifiers (ai, aws, llm, tls) are never plurals.
//   - "...ics": -ics field/mass nouns (economics, robotics, metrics, ethics)
//     are singular; stripping to -ic would be wrong and asymmetric.
//
// Both guards are symmetric (applied identically at index and query time), so
// they never break matching; they only avoid mangling internal keys.
func stemDomainToken(t string) string {
	if len(t) <= 3 || strings.HasSuffix(t, "ics") {
		return t
	}
	return domainPluralizer.Singular(t)
}

// domainTokens returns the stemmed, de-duplicated token set of a canonical
// domain, split on whitespace AND slash. Splitting on '/' too means a
// hierarchical tag's individual segments are each searchable as a word
// ("multi-tenant/auth" → ["multi","tenant","auth"]) — without it, a segment
// glued to a slash ("tenant/auth") is unreachable by its own middle word and
// the slash-hierarchy branch only matches whole prefixes. Order follows first
// appearance; callers treat it as a set. Pass the output of canonicalizeDomain.
func domainTokens(canonical string) []string {
	fields := strings.FieldsFunc(canonical, func(r rune) bool {
		return r == '/' || unicode.IsSpace(r)
	})
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
