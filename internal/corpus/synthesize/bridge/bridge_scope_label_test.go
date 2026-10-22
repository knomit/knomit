package bridge

import (
	"strings"
	"testing"

	"knomit/internal/corpus/synthesize/step"
)

// --- ScopeLabel tests ---

// TestScopeLabel_EmptyFilter returns "" for an empty scope filter.
func TestScopeLabel_EmptyFilter(t *testing.T) {
	got := ScopeLabel(step.ScopeFilter{})
	if got != "" {
		t.Errorf("empty filter: want \"\", got %q", got)
	}
}

// TestScopeLabel_DomainOnly formats domain-only scopes.
func TestScopeLabel_DomainOnly(t *testing.T) {
	got := ScopeLabel(step.ScopeFilter{Domain: []string{"auth"}})
	if got == "" {
		t.Error("domain-only scope must return non-empty label")
	}
	if !strings.Contains(got, "auth") {
		t.Errorf("domain-only scope label must contain 'auth', got %q", got)
	}
}

// TestScopeLabel_MultiDomainAndEntity formats a full scope with domain+entities.
func TestScopeLabel_MultiDomainAndEntity(t *testing.T) {
	s := step.ScopeFilter{Domain: []string{"auth"}, Entities: []string{"permissions"}}
	got := ScopeLabel(s)
	if !strings.Contains(got, "auth") {
		t.Errorf("label must contain 'auth', got %q", got)
	}
	if !strings.Contains(got, "permissions") {
		t.Errorf("label must contain 'permissions', got %q", got)
	}
}

// TestScopeLabel_Deterministic confirms the same scope returns the same string.
func TestScopeLabel_Deterministic(t *testing.T) {
	s := step.ScopeFilter{Domain: []string{"auth", "billing"}, Entities: []string{"alice"}}
	a := ScopeLabel(s)
	b := ScopeLabel(s)
	if a != b {
		t.Errorf("ScopeLabel must be deterministic: %q != %q", a, b)
	}
}
