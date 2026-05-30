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

// TestStemDomainToken pins the minimal, symmetric, match-only plural stemmer.
func TestStemDomainToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{"vulnerabilities", "vulnerability"},
		{"agents", "agent"},
		{"models", "model"},
		{"products", "product"},
		{"class", "class"}, // ss preserved
		{"ai", "ai"},       // len guard (<=3)
		{"aws", "aws"},     // len guard
		{"batches", "batch"},
	}
	for _, c := range cases {
		if got := stemDomainToken(c.in); got != c.want {
			t.Errorf("stemDomainToken(%q) = %q, want %q", c.in, got, c.want)
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
	}
	for _, c := range cases {
		got := domainTokens(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("domainTokens(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
