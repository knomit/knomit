package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// TestSearch_DomainContainmentMatching exercises the de-hyphenize + token
// containment domain match: canonicalisation unifies case/space/hyphen variants,
// single-token queries match the whole facet family, multi-token queries are
// order-independent and require ALL tokens, and plurals are stemmed.
func TestSearch_DomainContainmentMatching(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	mk := func(path, title string, domains []string) {
		f := fact.NewFact("placeholder.md")
		f.Title = title
		f.Confidence = 0.9
		f.Sources = 1
		f.Domain = domains
		f.Entities = []string{"x"}
		f.Type = fact.Observation
		out, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, branch, path, out, "init", "")
		require.NoError(t, err)
	}
	mk("kb/gov1.md", "Gov One", []string{"ai-governance"}) // canonical: "ai governance"
	mk("kb/gov2.md", "Gov Two", []string{"AI Governance"}) // canonical: "ai governance"
	mk("kb/safety.md", "Safety", []string{"ai safety"})    // canonical: "ai safety"
	mk("kb/vuln.md", "Vuln", []string{"vulnerabilities"})  // stems to "vulnerability"

	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	paths := func(opts SearchOptions) map[string]bool {
		res, err := svc.Search().Search(ctx, branch, opts)
		require.NoError(t, err)
		m := map[string]bool{}
		for _, r := range res {
			m[r.Path] = true
		}
		return m
	}

	// Single token "ai" → the ai facet family (gov1, gov2, safety), NOT the
	// vulnerabilities fact (no "ai" token).
	got := paths(SearchOptions{Domain: []string{"ai"}})
	require.True(t, got["kb/gov1.md"] && got["kb/gov2.md"] && got["kb/safety.md"],
		"domain=ai must match all ai-tagged facts (containment), got %v", got)
	require.False(t, got["kb/vuln.md"], "domain=ai must NOT match the vulnerabilities fact, got %v", got)

	// Multi-token, order-independent, ALL required: "governance ai" → only the
	// two governance facts, not safety.
	got = paths(SearchOptions{Domain: []string{"governance ai"}})
	require.True(t, got["kb/gov1.md"] && got["kb/gov2.md"], "governance ai must match both gov facts, got %v", got)
	require.False(t, got["kb/safety.md"], "governance ai must NOT match ai safety, got %v", got)

	// Case/hyphen-insensitive: "AI-Governance" canonicalises identically.
	got = paths(SearchOptions{Domain: []string{"AI-Governance"}})
	require.True(t, got["kb/gov1.md"] && got["kb/gov2.md"], "AI-Governance must match both gov facts, got %v", got)
	require.False(t, got["kb/safety.md"], "AI-Governance must NOT match ai safety, got %v", got)

	// Plural stemming: querying singular "vulnerability" matches "vulnerabilities".
	got = paths(SearchOptions{Domain: []string{"vulnerability"}})
	require.True(t, got["kb/vuln.md"], "vulnerability must match the 'vulnerabilities' fact via stemming, got %v", got)
}

// TestRebuild_BackfillsTokensForHistoricalVersions pins that a full rebuild
// populates fact_domain_tokens for EVERY fact version (HEAD + historical), so
// token containment can reach superseded versions if needed. Historical
// fact_domains rows are left as-is (immutable); only the derived token table is
// (re)built — from the canonicalised authored domain.
func TestRebuild_BackfillsTokensForHistoricalVersions(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	branch := "main"

	mk := func(title string, domains []string) string {
		f := fact.NewFact("placeholder.md")
		f.Title = title
		f.Confidence = 0.9
		f.Sources = 1
		f.Domain = domains
		f.Entities = []string{"x"}
		f.Type = fact.Observation
		out, err := fact.SerializeFact(f)
		require.NoError(t, err)
		return out
	}
	// v1 then v2 at the same path → v1 becomes a historical (non-branch) version.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/x.md", mk("V1", []string{"AI-Governance"}), "v1", "")
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Sync(ctx, branch))
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/x.md", mk("V2 different body entirely", []string{"AI-Governance"}), "v2", "")
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Sync(ctx, branch))

	si := svc.si
	// The historical version: a facts row for kb/x.md not pointed to by branch_facts.
	var histID int64
	require.NoError(t, si.rh.db.QueryRowContext(ctx, `
		SELECT f.id FROM facts f
		WHERE f.path='kb/x.md'
		  AND f.id NOT IN (SELECT fact_id FROM branch_facts)`).Scan(&histID),
		"there must be a historical (non-branch) version of kb/x.md")

	// Simulate a version indexed BEFORE the token table existed: drop its tokens.
	_, err = si.rh.db.ExecContext(ctx, `DELETE FROM fact_domain_tokens WHERE fact_id=?`, histID)
	require.NoError(t, err)

	// A full rebuild must backfill the historical version's tokens.
	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	var n int
	require.NoError(t, si.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM fact_domain_tokens WHERE fact_id=?`, histID).Scan(&n))
	require.Positive(t, n, "rebuild must backfill tokens for the historical version")
	// Tokens are the canonicalised authored domain ("AI-Governance" -> ai, governance).
	var toks []string
	rows, err := si.rh.db.QueryContext(ctx, `SELECT token FROM fact_domain_tokens WHERE fact_id=? ORDER BY token`, histID)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		toks = append(toks, s)
	}
	require.Equal(t, []string{"ai", "governance"}, toks)
}

// TestCompletions_DomainCanonicalizesPrefix pins that domain autocomplete
// canonicalises the typed prefix, so a raw "AI-Gov" matches the stored canonical
// "ai governance" (case + hyphen folded). Entities are intentionally NOT
// canonicalised (proper nouns / identifiers), so this applies to domain only.
func TestCompletions_DomainCanonicalizesPrefix(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	branch := "main"

	f := fact.NewFact("placeholder.md")
	f.Title = "Gov"
	f.Confidence = 0.9
	f.Sources = 1
	f.Domain = []string{"AI-Governance"} // canonical: "ai governance"
	f.Entities = []string{"x"}
	f.Type = fact.Observation
	out, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/g.md", out, "init", "")
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	for _, prefix := range []string{"AI-Gov", "ai gov", "AI gov"} {
		got, err := svc.Search().Completions(ctx, branch, "domain", prefix, 10)
		require.NoError(t, err)
		require.Contains(t, got, "ai governance",
			"domain completion for prefix %q must surface canonical 'ai governance', got %v", prefix, got)
	}
}

// TestSearch_DomainMiddleSegmentSearchable regresses the bug where a token glued
// to a slash ("tenant/auth") was unreachable by its own middle word: tokens are
// now split on '/' too, so a hierarchical/hyphenated tag's interior segment
// matches as a word, while the slash-hierarchy prefix match is unaffected.
func TestSearch_DomainMiddleSegmentSearchable(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	branch := "main"

	mk := func(path, title string, domains []string) {
		f := fact.NewFact("placeholder.md")
		f.Title = title
		f.Confidence = 0.9
		f.Sources = 1
		f.Domain = domains
		f.Entities = []string{"x"}
		f.Type = fact.Observation
		out, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, branch, path, out, "init", "")
		require.NoError(t, err)
	}
	mk("kb/mt.md", "Multi-Tenant Auth", []string{"multi-tenant/auth"}) // canonical: "multi tenant/auth"
	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	paths := func(opts SearchOptions) map[string]bool {
		res, err := svc.Search().Search(ctx, branch, opts)
		require.NoError(t, err)
		m := map[string]bool{}
		for _, r := range res {
			m[r.Path] = true
		}
		return m
	}

	for _, term := range []string{"tenant", "auth", "multi"} {
		got := paths(SearchOptions{Domain: []string{term}})
		require.True(t, got["kb/mt.md"],
			"domain=%q must match 'multi-tenant/auth' via its interior token, got %v", term, got)
	}
	// Slash-hierarchy descendant match still works for the leading segment.
	got := paths(SearchOptions{Domain: []string{"multi tenant"}, DomainExact: false})
	require.True(t, got["kb/mt.md"], "multi tenant must still match, got %v", got)
}

// TestSearch_DegenerateDomainFilterIsNoOp regresses the bug where a domain term
// that canonicalises to "" (junk like "---") emitted `domain = ”`, matching
// zero facts and making the whole query return nothing. Such a filter must be a
// no-op so the other filters (here: text) still return their hits.
func TestSearch_DegenerateDomainFilterIsNoOp(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	branch := "main"

	f := fact.NewFact("placeholder.md")
	f.Title = "Real Fact"
	f.Confidence = 0.9
	f.Sources = 1
	f.Domain = []string{"ai"}
	f.Entities = []string{"x"}
	f.Type = fact.Observation
	out, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/r.md", out, "init", "")
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	for _, junk := range []string{"---", "-", "   "} {
		res, err := svc.Search().Search(ctx, branch, SearchOptions{Domain: []string{junk}})
		require.NoError(t, err)
		found := false
		for _, r := range res {
			if r.Path == "kb/r.md" {
				found = true
			}
		}
		require.True(t, found,
			"a junk domain filter %q must be ignored (not emit domain=''), so kb/r.md still returns; got %d results", junk, len(res))
	}
}

// TestCompletions_JunkDomainPrefixReturnsNothing regresses PR #70 review finding
// #3: a domain-autocomplete prefix that canonicalises to "" (junk like "---")
// fell through to `LIKE '%'` and returned every domain. A junk prefix must yield
// no completions, while empty input still lists everything and a real prefix
// filters.
func TestCompletions_JunkDomainPrefixReturnsNothing(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	branch := "main"

	mk := func(path string, domains []string) {
		f := fact.NewFact("placeholder.md")
		f.Title = "T"
		f.Confidence = 0.9
		f.Sources = 1
		f.Domain = domains
		f.Entities = []string{"x"}
		f.Type = fact.Observation
		out, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, branch, path, out, "init", "")
		require.NoError(t, err)
	}
	mk("kb/a.md", []string{"ai-governance"}) // canonical: "ai governance"
	mk("kb/b.md", []string{"security"})
	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	// Junk prefix → no completions (the bug returned all domains).
	for _, junk := range []string{"---", "-", "   "} {
		got, err := svc.Search().Completions(ctx, branch, "domain", junk, 50)
		require.NoError(t, err)
		require.Empty(t, got, "junk domain prefix %q must return no completions", junk)
	}

	// Empty input still lists everything (the intended "nothing typed yet"
	// behaviour).
	all, err := svc.Search().Completions(ctx, branch, "domain", "", 50)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"ai governance", "security"}, all)

	// A real prefix filters to the matching canonical domain.
	one, err := svc.Search().Completions(ctx, branch, "domain", "ai", 50)
	require.NoError(t, err)
	require.Equal(t, []string{"ai governance"}, one)
}
