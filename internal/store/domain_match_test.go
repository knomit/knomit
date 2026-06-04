package store

import (
	"reflect"
	"testing"
)

// TestCanonicalizeDomain pins the deterministic write-time canonicalisation:
// NFC + case-fold + de-hyphenize (hyphen→space) + collapse whitespace + trim,
// with underscores preserved (identifier-like tags stay atomic).
func TestCanonicalizeDomain(t *testing.T) {
	cases := []struct{ in, want string }{
		{"AI-Governance", "ai governance"},
		{"enterprise ai", "enterprise ai"},
		{"enterprise-ai", "enterprise ai"},
		{"  Supply--Chain  ", "supply chain"},
		{"commit_log", "commit_log"}, // underscore NOT split
		{"Vulnerabilities", "vulnerabilities"},
		{"AI", "ai"},
	}
	for _, c := range cases {
		if got := canonicalizeDomain(c.in); got != c.want {
			t.Errorf("canonicalizeDomain(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Idempotent.
	if a, b := canonicalizeDomain("AI-Safety"), canonicalizeDomain(canonicalizeDomain("AI-Safety")); a != b {
		t.Errorf("not idempotent: %q vs %q", a, b)
	}
	// NFC: decomposed and composed forms must canonicalise equal.
	composed := canonicalizeDomain("caf\u00e9")     // é precomposed
	decomposed := canonicalizeDomain("cafe\u0301") // e + combining acute
	if composed != decomposed {
		t.Errorf("NFC mismatch: %q vs %q", composed, decomposed)
	}
}

// TestStemDomainToken pins the match-only singularizer: it normalises a token
// to its singular key (via go-pluralize), with short-token and -ics guards.
func TestStemDomainToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{"vulnerabilities", "vulnerability"},
		{"agents", "agent"},
		{"models", "model"},
		{"products", "product"},
		{"class", "class"},         // already singular, unchanged
		{"ai", "ai"},               // len guard (<=3)
		{"aws", "aws"},             // len guard — acronym, NOT treated as plural
		{"llm", "llm"},             // len guard
		{"economics", "economics"}, // -ics guard — field noun, NOT a plural
		{"robotics", "robotics"},   // -ics guard
		{"metrics", "metrics"},     // -ics guard
		{"batches", "batch"},
	}
	for _, c := range cases {
		if got := stemDomainToken(c.in); got != c.want {
			t.Errorf("stemDomainToken(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStemDomainToken_SymmetricAndIdempotent is the real contract: a word and
// its plural must collapse to the SAME match key (so a singular query finds a
// pluralised tag and vice-versa), and stemming a key again must be a fixpoint.
// This is exactly what the previous hand-rolled stemmer — and Porter/Snowball —
// get WRONG on -is/-es and irregular plurals.
func TestStemDomainToken_SymmetricAndIdempotent(t *testing.T) {
	pairs := []struct{ singular, plural string }{
		{"vulnerability", "vulnerabilities"},
		{"model", "models"},
		{"analysis", "analyses"},
		{"thesis", "theses"},
		{"index", "indices"},
		{"matrix", "matrices"},
		{"category", "categories"},
	}
	for _, p := range pairs {
		gs, gp := stemDomainToken(p.singular), stemDomainToken(p.plural)
		if gs != gp {
			t.Errorf("not symmetric: stem(%q)=%q != stem(%q)=%q", p.singular, gs, p.plural, gp)
		}
		// Idempotent: the key is a fixpoint.
		if got := stemDomainToken(gs); got != gs {
			t.Errorf("not idempotent: stem(stem(%q))=%q != %q", p.singular, got, gs)
		}
	}
}

// TestDomainTokens pins tokenisation: split the canonical form on whitespace,
// stem each token, dedupe. Order is irrelevant (it's a set).
func TestDomainTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"ai governance", []string{"ai", "governance"}},
		{"ai models", []string{"ai", "model"}},
		{"vulnerabilities", []string{"vulnerability"}},
		{"commit_log", []string{"commit_log"}},
		{"ai ai", []string{"ai"}}, // dedupe
		// Slash splits like whitespace: each hierarchy segment is its own token
		// (so a middle segment is searchable as a word). "multi tenant" comes from
		// the de-hyphenized "multi-tenant" segment.
		{"store/resolver", []string{"store", "resolver"}},
		{"multi tenant/auth", []string{"multi", "tenant", "auth"}},
	}
	for _, c := range cases {
		got := domainTokens(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("domainTokens(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
