package synthesize

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// seedFactInDomain writes one observation fact in the given domain on branch.
func seedFactInDomain(t *testing.T, svc *store.Service, branch, slug, domain string) {
	t.Helper()
	f := fact.NewFact("kb/" + domain + "/" + slug + ".md")
	f.Title = slug
	f.Body = "body of " + slug
	f.Type = fact.Observation
	f.Domain = []string{domain}
	f.Confidence = 0.5
	f.Sources = 1
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

// TestScopeFilter_FirstRun_RestrictsToDomain confirms the first-run seed
// search (no watermark) applies the scope filter's domain — only facts in
// the requested domain become seeds.
func TestScopeFilter_FirstRun_RestrictsToDomain(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	seedFactInDomain(t, svc, branch, "a", "auth")
	seedFactInDomain(t, svc, branch, "b", "auth")
	seedFactInDomain(t, svc, branch, "c", "billing")
	seedFactInDomain(t, svc, branch, "d", "billing")

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  branch,
		Svc:          svc,
		OntologyRoot: "kb",
	})

	r := NewReviewerWithOptions(ri, nil, EffortNormal, ScopeFilter{Domain: []string{"auth"}})

	gs, idx, pipelineIdx, _ := r.storeIndices()
	seeds, err := r.dirtyFacts(context.Background(), branch, gs, idx, pipelineIdx)
	require.NoError(t, err)
	require.Len(t, seeds, 2, "scope=auth must restrict the seed pool")
	for _, s := range seeds {
		require.Contains(t, s.Domain, "auth", "seed %q must be in domain auth", s.File)
	}
}

// seedFactWith writes one observation fact carrying the given domains and
// entities on branch.
func seedFactWith(t *testing.T, svc *store.Service, branch, slug string, domains, entities []string) {
	t.Helper()
	f := fact.NewFact("kb/scope/" + slug + ".md")
	f.Title = slug
	f.Body = "body of " + slug
	f.Type = fact.Observation
	f.Domain = domains
	f.Entities = entities
	f.Confidence = 0.5
	f.Sources = 1
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

// TestScopeFilter_FirstRun_UnionAcrossDomainAndEntity is the regression guard
// for the first-run/incremental scope divergence. A scope carrying BOTH a
// domain and an entity must seed any fact that touches EITHER (union), matching
// ScopeFilter.Matches and the incremental seed path. The earlier first-run code
// pushed scope.Domain/Entities into store.Search, which ANDs the two clauses
// (intersection) — so this same scope produced an EMPTY first-run pool while a
// later incremental run produced two seeds. Both runs must now agree.
func TestScopeFilter_FirstRun_UnionAcrossDomainAndEntity(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	// a: matches on domain only. b: matches on entity only. c: matches neither.
	seedFactWith(t, svc, branch, "a", []string{"auth"}, nil)
	seedFactWith(t, svc, branch, "b", []string{"billing"}, []string{"alice"})
	seedFactWith(t, svc, branch, "c", []string{"ops"}, []string{"bob"})

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  branch,
		Svc:          svc,
		OntologyRoot: "kb",
	})

	scope := ScopeFilter{Domain: []string{"auth"}, Entities: []string{"alice"}}
	r := NewReviewerWithOptions(ri, nil, EffortNormal, scope)

	gs, idx, pipelineIdx, _ := r.storeIndices()
	seeds, err := r.dirtyFacts(context.Background(), branch, gs, idx, pipelineIdx)
	require.NoError(t, err)

	got := map[string]bool{}
	for _, s := range seeds {
		got[s.File] = true
	}
	require.Len(t, seeds, 2, "domain OR entity must seed both touching facts (union), got %v", seeds)
	require.True(t, got["kb/scope/a.md"], "fact matched on domain must be seeded")
	require.True(t, got["kb/scope/b.md"], "fact matched on entity must be seeded")
	require.False(t, got["kb/scope/c.md"], "fact matching neither axis must be excluded")
}

// TestScopeFilter_Empty_WholeCorpus asserts the empty filter is whole-corpus —
// not a hidden "match nothing" footgun.
func TestScopeFilter_Empty_WholeCorpus(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	seedFactInDomain(t, svc, branch, "a", "auth")
	seedFactInDomain(t, svc, branch, "b", "billing")
	seedFactInDomain(t, svc, branch, "c", "ops")

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  branch,
		Svc:          svc,
		OntologyRoot: "kb",
	})

	r := NewReviewerWithOptions(ri, nil, EffortNormal, ScopeFilter{}) // empty

	gs, idx, pipelineIdx, _ := r.storeIndices()
	seeds, err := r.dirtyFacts(context.Background(), branch, gs, idx, pipelineIdx)
	require.NoError(t, err)
	require.Len(t, seeds, 3, "empty scope = whole-corpus search")
}

// TestScopeFilter_IsEmpty asserts the predicate is the union zero.
func TestScopeFilter_IsEmpty(t *testing.T) {
	require.True(t, ScopeFilter{}.IsEmpty())
	require.False(t, ScopeFilter{Domain: []string{"x"}}.IsEmpty())
	require.False(t, ScopeFilter{Entities: []string{"y"}}.IsEmpty())
}

// TestScopedReview_ZeroSeeds_DoesNotAdvanceWatermark is the regression guard for
// the watermark-poisoning bug: a scoped review that finds zero seeds in its scope
// called completeSession, which unconditionally advanced the watermark to HEAD.
// This permanently hid all out-of-scope facts from future unscoped sessions.
// After the fix, completeSession skips watermark advancement when a scope filter
// is active.
func TestScopedReview_ZeroSeeds_DoesNotAdvanceWatermark(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	// Seed facts only in "billing" domain — auth scope will find zero.
	seedFactInDomain(t, svc, branch, "b1", "billing")
	seedFactInDomain(t, svc, branch, "b2", "billing")

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  branch,
		Svc:          svc,
		OntologyRoot: "kb",
	})

	// Review scoped to "auth": zero seeds → completeSession is called.
	r := NewReviewerWithOptions(ri, func(ProgressEvent) {}, EffortNormal, ScopeFilter{Domain: []string{"auth"}})
	result, err := r.StartSession(context.Background())
	require.NoError(t, err)
	require.NotNil(t, result, "StartSession must return a result")
	require.True(t, result.Done, "zero seeds → session done immediately")

	// Watermark must NOT have advanced: out-of-scope facts would be permanently
	// hidden from future unscoped sessions if it had.
	watermark, err := svc.Pipeline().GetPipelineWatermark(context.Background(), "review", branch)
	require.NoError(t, err)
	require.Empty(t, watermark, "scoped review with zero seeds must not advance the watermark")
}

// TestScopedReview_NonEmptyWatermark_StillSeedsInScope is the regression guard
// for the read-side watermark gating bug. A scoped review must re-examine its
// whole scope regardless of the shared "review" watermark — its purpose is an
// on-demand pass over a slice, independent of incremental change-tracking.
//
// Before the fix, dirtyFacts chose first-run (full scan) vs incremental
// (DiffFiles since watermark) purely on watermark=="". So once a prior UNSCOPED
// review advanced the watermark to HEAD, every scoped review took the
// incremental path, DiffFiles returned nothing (no commits since HEAD), and the
// scope filter ran over an empty set → zero seeds → "nothing found" even though
// the scope was full of facts. Scoped reviews don't ADVANCE the watermark
// (TestScopedReview_*DoesNotAdvanceWatermark), so they must not be BLOCKED by it
// either — the read and write sides must agree.
func TestScopedReview_NonEmptyWatermark_StillSeedsInScope(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	seedFactInDomain(t, svc, branch, "a", "auth")
	seedFactInDomain(t, svc, branch, "b", "auth")
	seedFactInDomain(t, svc, branch, "c", "billing")

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  branch,
		Svc:          svc,
		OntologyRoot: "kb",
	})

	// Simulate a prior unscoped review having advanced the watermark to HEAD.
	head, err := svc.Branches().HeadCommit(context.Background(), branch)
	require.NoError(t, err)
	require.NotEmpty(t, head)
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(context.Background(), "review", branch, head))

	r := NewReviewerWithOptions(ri, nil, EffortNormal, ScopeFilter{Domain: []string{"auth"}})
	gs, idx, pipelineIdx, _ := r.storeIndices()
	seeds, err := r.dirtyFacts(context.Background(), branch, gs, idx, pipelineIdx)
	require.NoError(t, err)
	require.Len(t, seeds, 2,
		"scoped review must seed its whole scope even when the watermark is at HEAD; "+
			"the watermark must not block a scoped re-examination")
	for _, s := range seeds {
		require.Contains(t, s.Domain, "auth")
	}
}

func TestScopeFilterMatchesTokenized(t *testing.T) {
	// Domain: case-insensitive, hierarchy, plural — all must match now.
	f := ScopeFilter{Domain: []string{"Store"}}
	if !f.Matches([]string{"store/sqlite"}, nil) {
		t.Error("scope 'Store' should match fact domain 'store/sqlite' (case + hierarchy)")
	}
	if !(ScopeFilter{Domain: []string{"migrations"}}).Matches([]string{"migration"}, nil) {
		t.Error("scope 'migrations' should match fact domain 'migration' (stem)")
	}
	// Entity: case-insensitive.
	if !(ScopeFilter{Entities: []string{"anthropic"}}).Matches(nil, []string{"Anthropic"}) {
		t.Error("entity scope should be case-insensitive")
	}
	// Empty filter still matches everything.
	if !(ScopeFilter{}).Matches([]string{"x"}, []string{"y"}) {
		t.Error("empty filter must match all")
	}
	// Non-match still rejects.
	if (ScopeFilter{Domain: []string{"auth"}}).Matches([]string{"store"}, nil) {
		t.Error("unrelated domain must not match")
	}
}

// TestScopedReview_CompletionOnFreshReviewer_DoesNotAdvanceWatermark guards the
// real failure mode: the MCP review handler reconstructs a fresh Reviewer with
// EMPTY scope on every continue call, so the call that finally completes the
// session carries no domain/entities args. completeSession therefore must read
// the scoped flag off the persisted session row, NOT the in-memory r.scope —
// otherwise the watermark advances to HEAD and permanently hides out-of-scope
// facts from future unscoped sessions. The zero-seeds test above can't catch
// this because there completeSession runs inside the same scoped StartSession
// call; here the completing reviewer is unscoped, exactly as in production.
func TestScopedReview_CompletionOnFreshReviewer_DoesNotAdvanceWatermark(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	// Seed enough in-scope facts that StartSession builds a real (non-empty)
	// scoped session rather than short-circuiting on zero seeds.
	seedFactInDomain(t, svc, branch, "a1", "auth")
	seedFactInDomain(t, svc, branch, "a2", "auth")

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "test", AgentBranch: branch, Svc: svc, OntologyRoot: "kb",
	})

	// Start scoped: StartSession persists Scoped=true on the session row.
	scoped := NewReviewerWithOptions(ri, func(ProgressEvent) {}, EffortNormal, ScopeFilter{Domain: []string{"auth"}})
	res, err := scoped.StartSession(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID, "scoped review with seeds must open a session")

	// Simulate the completing continue call: a fresh, UNSCOPED Reviewer (this is
	// what the MCP handler builds when the client passes only session_id +
	// response). Drive completeSession directly with the persisted session.
	sess, err := svc.Pipeline().GetPipelineSession(context.Background(), res.SessionID)
	require.NoError(t, err)
	require.True(t, sess.Scoped, "StartSession must persist the scoped flag")

	fresh := NewReviewerWithOptions(ri, func(ProgressEvent) {}, EffortNormal, ScopeFilter{})
	_, err = fresh.completeSession(context.Background(), sess)
	require.NoError(t, err)

	watermark, err := svc.Pipeline().GetPipelineWatermark(context.Background(), "review", branch)
	require.NoError(t, err)
	require.Empty(t, watermark, "scoped session completed by an unscoped reviewer must not advance the watermark")
}
