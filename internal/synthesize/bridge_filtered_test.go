package synthesize

import (
	"testing"
)

// --- specificityWithinScope tests ---

// TestSpecificityWithinScope_TokenOnOneFact verifies that a token carried by
// exactly 1 fact out of N yields spec = 1.0 (df=1, 1/1 = 1.0).
func TestSpecificityWithinScope_TokenOnOneFact(t *testing.T) {
	pool := []factForLLM{
		{File: "a.md", Domain: []string{"store"}, Entities: []string{}},
		{File: "b.md", Domain: []string{"query"}, Entities: []string{}},
		{File: "c.md", Domain: []string{"index"}, Entities: []string{}},
	}
	got := specificityWithinScope("store", BridgeDomain, pool)
	if got != 1.0 {
		t.Fatalf("want 1.0, got %f", got)
	}
}

// TestSpecificityWithinScope_TokenOnFourFacts verifies that a token carried by
// 4 pool facts yields spec = 0.25 (df=4, 1/4 = 0.25).
func TestSpecificityWithinScope_TokenOnFourFacts(t *testing.T) {
	pool := []factForLLM{
		{File: "a.md", Entities: []string{"Go"}},
		{File: "b.md", Entities: []string{"Go"}},
		{File: "c.md", Entities: []string{"Go"}},
		{File: "d.md", Entities: []string{"Go"}},
		{File: "e.md", Entities: []string{"Rust"}},
	}
	got := specificityWithinScope("Go", BridgeEntity, pool)
	want := 0.25
	if got != want {
		t.Fatalf("want %f, got %f", want, got)
	}
}

// TestSpecificityWithinScope_TokenAbsent verifies that a token absent from
// the pool yields 1.0 (max(df,1) = 1, 1/1 = 1.0).
func TestSpecificityWithinScope_TokenAbsent(t *testing.T) {
	pool := []factForLLM{
		{File: "a.md", Domain: []string{"store"}, Entities: []string{}},
	}
	got := specificityWithinScope("nosuchtoken", BridgeDomain, pool)
	if got != 1.0 {
		t.Fatalf("want 1.0, got %f", got)
	}
}

// TestSpecificityWithinScope_DomainHierarchyMatch verifies that a query token
// "store" matches a fact domain "store/sqlite" via DomainTagMatches hierarchy.
func TestSpecificityWithinScope_DomainHierarchyMatch(t *testing.T) {
	pool := []factForLLM{
		{File: "a.md", Domain: []string{"store/sqlite"}},
		{File: "b.md", Domain: []string{"store/postgres"}},
		{File: "c.md", Domain: []string{"query"}},
	}
	// "store" is parent of "store/sqlite" and "store/postgres" — df should be 2.
	got := specificityWithinScope("store", BridgeDomain, pool)
	want := 0.5 // 1/2
	if got != want {
		t.Fatalf("want %f, got %f", want, got)
	}
}

// TestSpecificityWithinScope_EntityCaseInsensitive verifies that entity
// matching is case-insensitive (EntityTagMatches uses case-fold equality).
func TestSpecificityWithinScope_EntityCaseInsensitive(t *testing.T) {
	pool := []factForLLM{
		{File: "a.md", Entities: []string{"Anthropic"}},
		{File: "b.md", Entities: []string{"anthropic"}},
		{File: "c.md", Entities: []string{"OpenAI"}},
	}
	// "Anthropic" matches both "Anthropic" and "anthropic" via case-fold — df=2.
	got := specificityWithinScope("Anthropic", BridgeEntity, pool)
	want := 0.5
	if got != want {
		t.Fatalf("want %f, got %f", want, got)
	}
}

// TestSpecificityWithinScope_BridgeBothMatchesBothAxes verifies that
// BridgeBoth (or "") matches on either domain or entity axis.
func TestSpecificityWithinScope_BridgeBothMatchesBothAxes(t *testing.T) {
	pool := []factForLLM{
		{File: "a.md", Domain: []string{"llm"}, Entities: []string{}},
		{File: "b.md", Domain: []string{}, Entities: []string{"llm"}},
		{File: "c.md", Domain: []string{"store"}, Entities: []string{}},
	}
	// "llm" appears once in domain (a.md) and once in entity (b.md) — df=2 for BridgeBoth.
	got := specificityWithinScope("llm", BridgeBoth, pool)
	want := 0.5
	if got != want {
		t.Fatalf("want %f, got %f", want, got)
	}
}

// --- sharedSubToken tests ---

// TestSharedSubToken_RarestSharedWins verifies that when members all share
// both "x" (df=2 in pool, rare) and "y" (df=10, common), the returned token
// is "x" with spec = 1/2 = 0.5.
func TestSharedSubToken_RarestSharedWins(t *testing.T) {
	// Build a pool where "x" appears in 2 facts and "y" in 10.
	pool := make([]factForLLM, 0, 12)
	pool = append(pool, factForLLM{File: "x1.md", Domain: []string{"x"}})
	pool = append(pool, factForLLM{File: "x2.md", Domain: []string{"x"}})
	for i := 0; i < 10; i++ {
		pool = append(pool, factForLLM{File: "y" + string(rune('0'+i)) + ".md", Domain: []string{"y"}})
	}

	// Members all carry both "x" and "y".
	members := []factForLLM{
		{File: "m1.md", Domain: []string{"x", "y"}},
		{File: "m2.md", Domain: []string{"x", "y"}},
		{File: "m3.md", Domain: []string{"x", "y"}},
	}

	tok, kind, spec := sharedSubToken(members, BridgeDomain, pool)
	if tok != "x" {
		t.Fatalf("want token \"x\", got %q", tok)
	}
	if kind != BridgeDomain {
		t.Fatalf("want kind BridgeDomain, got %q", kind)
	}
	want := 0.5 // 1/2
	if spec != want {
		t.Fatalf("want spec %f, got %f", want, spec)
	}
}

// TestSharedSubToken_NoSharedToken verifies that when members share no token,
// the function returns ("", BridgeBoth, neutralSpec).
func TestSharedSubToken_NoSharedToken(t *testing.T) {
	pool := []factForLLM{
		{File: "p1.md", Domain: []string{"alpha"}},
		{File: "p2.md", Domain: []string{"beta"}},
	}
	members := []factForLLM{
		{File: "m1.md", Domain: []string{"alpha"}},
		{File: "m2.md", Domain: []string{"beta"}},
	}

	tok, _, spec := sharedSubToken(members, BridgeDomain, pool)
	if tok != "" {
		t.Fatalf("want empty token, got %q", tok)
	}
	if spec != neutralSpec {
		t.Fatalf("want neutralSpec %f, got %f", neutralSpec, spec)
	}
}

// TestSharedSubToken_CanonicalUnification verifies that "Store" and "store"
// are treated as the same token (canonical unification via CanonicalizeTag).
func TestSharedSubToken_CanonicalUnification(t *testing.T) {
	pool := []factForLLM{
		{File: "p1.md", Domain: []string{"store"}},
	}
	// m1 carries "Store", m2 carries "store" — canonical form is the same.
	members := []factForLLM{
		{File: "m1.md", Domain: []string{"Store"}},
		{File: "m2.md", Domain: []string{"store"}},
	}

	tok, kind, spec := sharedSubToken(members, BridgeDomain, pool)
	if tok == "" {
		t.Fatalf("want a shared token, got empty (canonical unification failed)")
	}
	if kind != BridgeDomain {
		t.Fatalf("want BridgeDomain, got %q", kind)
	}
	// df=1 (only p1.md carries "store" in pool, members are NOT in pool here)
	if spec != 1.0 {
		t.Fatalf("want spec 1.0 (df=1), got %f", spec)
	}
}

// TestSharedSubToken_DeterministicTieBreak verifies that when two shared tokens
// have equal df in pool, the one with the smallest canonical string is returned.
func TestSharedSubToken_DeterministicTieBreak(t *testing.T) {
	pool := []factForLLM{
		{File: "p1.md", Domain: []string{"aaa", "zzz"}},
	}
	// Both "aaa" and "zzz" appear in 1 fact each — equal df.
	// Members carry both. Tie-break: smallest canonical string = "aaa".
	members := []factForLLM{
		{File: "m1.md", Domain: []string{"aaa", "zzz"}},
		{File: "m2.md", Domain: []string{"aaa", "zzz"}},
	}

	tok, _, _ := sharedSubToken(members, BridgeDomain, pool)
	if tok != "aaa" {
		t.Fatalf("want \"aaa\" (tie-break: smallest canonical), got %q", tok)
	}
}

// TestSharedSubToken_EntityBeatesDomainOnBridgeBoth verifies that when a
// token appears on both entity and domain axes under BridgeBoth, the returned
// kind is BridgeEntity (entity beats domain, mirroring enumerateBridgeCandidates).
func TestSharedSubToken_EntityBeatesDomainOnBridgeBoth(t *testing.T) {
	pool := []factForLLM{
		{File: "p1.md", Entities: []string{"llm"}, Domain: []string{"llm"}},
	}
	members := []factForLLM{
		{File: "m1.md", Entities: []string{"llm"}, Domain: []string{"llm"}},
		{File: "m2.md", Entities: []string{"llm"}, Domain: []string{"llm"}},
	}

	_, kind, _ := sharedSubToken(members, BridgeBoth, pool)
	if kind != BridgeEntity {
		t.Fatalf("want BridgeEntity (entity beats domain under BridgeBoth), got %q", kind)
	}
}

// TestSharedSubToken_SingleMember verifies the len<2 edge: a single member
// yields neutral (no meaningful "shared" set for size-1 slices).
func TestSharedSubToken_SingleMember(t *testing.T) {
	pool := []factForLLM{
		{File: "p1.md", Domain: []string{"store"}},
	}
	members := []factForLLM{
		{File: "m1.md", Domain: []string{"store"}},
	}

	tok, _, spec := sharedSubToken(members, BridgeDomain, pool)
	// Single member: documented contract is the neutral result — empty token
	// and neutralSpec — because callers key on tok == "" for the neutral case.
	// The filtered generator only calls this for subsets of size >= 2 in practice.
	if tok != "" {
		t.Fatalf("want empty token for single member, got %q", tok)
	}
	if spec != neutralSpec {
		t.Fatalf("want neutralSpec for single member, got %f", spec)
	}
}

// TestSharedSubToken_EmptyMembers verifies that empty members returns neutral.
func TestSharedSubToken_EmptyMembers(t *testing.T) {
	pool := []factForLLM{{File: "p1.md", Domain: []string{"store"}}}
	tok, _, spec := sharedSubToken(nil, BridgeDomain, pool)
	if tok != "" {
		t.Fatalf("want empty token for empty members, got %q", tok)
	}
	if spec != neutralSpec {
		t.Fatalf("want neutralSpec for empty members, got %f", spec)
	}
}
