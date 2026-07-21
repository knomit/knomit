package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// Tests for behaviour the engine owns on behalf of EVERY strategy. They are
// parameterized over both strategies on purpose: before the extraction, review
// and hypothesize each carried their own copy of the completion/watermark
// logic, and the copies drifted (hypothesize grew a defensive re-read of the
// scoped flag that review never had). One test over both is what keeps a future
// third strategy honest, and what makes a divergence a failure rather than a
// silent asymmetry.

// allTools names every strategy the engine drives. A new strategy belongs
// here, which is the point: the subtests below then cover it for free.
var allTools = []string{reviewTool, hypothesizeTool}

// enginePipelineFor constructs the engine for one tool. Review is reached
// through its facade (Reviewer wraps the same Pipeline) so the test exercises
// the construction path production uses.
func enginePipelineFor(t *testing.T, tool string, ri *repos.RepoInstance, effort Effort, scope ScopeFilter) *Pipeline {
	t.Helper()
	switch tool {
	case reviewTool:
		return NewReviewerWithOptions(ri, nil, effort, scope).p
	case hypothesizeTool:
		return NewHypothesizer(ri, nil, effort, scope)
	default:
		t.Fatalf("no engine constructor for tool %q", tool)
		return nil
	}
}

// TestPipeline_ScopedEmptyPool_DoesNotAdvanceWatermark is the regression guard
// for watermark poisoning by a scoped run. A scoped session only ever looked at
// a slice of the corpus, so advancing the watermark to HEAD on its completion
// would permanently hide everything outside the scope from future unscoped
// sessions. This must hold on the zero-seed path too — that path is exactly
// where the bug lived (decisions/architecture/synthesize/scope-filter).
func TestPipeline_ScopedEmptyPool_DoesNotAdvanceWatermark(t *testing.T) {
	for _, tool := range allTools {
		t.Run(tool, func(t *testing.T) {
			svc, ri := newHypothesizeTestRepo(t)
			ctx := context.Background()

			// Empty corpus → empty seed pool for either strategy.
			p := enginePipelineFor(t, tool, ri, EffortNormal, ScopeFilter{Domain: []string{"auth"}})
			res, err := p.StartSession(ctx)
			require.NoError(t, err)
			require.True(t, res.Done, "empty pool → done immediately")

			wm, err := svc.Pipeline().GetPipelineWatermark(ctx, tool, "agent/test")
			require.NoError(t, err)
			require.Empty(t, wm,
				"a scoped run must not advance the watermark: out-of-scope facts would be "+
					"permanently hidden from future unscoped sessions")
		})
	}
}

// TestPipeline_UnscopedEmptyPool_AdvancesWatermark is the other half of the
// same coupling: an unscoped run that finds nothing to do still records that it
// saw HEAD, so the next run can be incremental.
func TestPipeline_UnscopedEmptyPool_AdvancesWatermark(t *testing.T) {
	for _, tool := range allTools {
		t.Run(tool, func(t *testing.T) {
			svc, ri := newHypothesizeTestRepo(t)
			ctx := context.Background()

			p := enginePipelineFor(t, tool, ri, EffortNormal, ScopeFilter{})
			res, err := p.StartSession(ctx)
			require.NoError(t, err)
			require.True(t, res.Done)

			wm, err := svc.Pipeline().GetPipelineWatermark(ctx, tool, "agent/test")
			require.NoError(t, err)
			require.NotEmpty(t, wm,
				"an unscoped no-op run must advance the watermark so the next run is incremental")
		})
	}
}

// TestPipeline_ScopedSessionRowMarked pins the mechanism the two tests above
// depend on: the scoped flag is written to the session ROW at start, not held
// in memory. The MCP handler rebuilds the engine with an empty scope on every
// continue call, so a completing turn can only learn "this was scoped" by
// reading it back off the row.
func TestPipeline_ScopedSessionRowMarked(t *testing.T) {
	for _, tool := range allTools {
		t.Run(tool, func(t *testing.T) {
			svc, ri := newHypothesizeTestRepo(t)
			ctx := context.Background()

			// A seed each strategy accepts, so the session survives past start.
			writeTestFact(t, svc, "kb/arch/a.md", "a", fact.Synthesis, "auth")
			writeTestFact(t, svc, "kb/arch/b.md", "b", fact.Observation, "auth")

			p := enginePipelineFor(t, tool, ri, EffortNormal, ScopeFilter{Domain: []string{"auth"}})
			res, err := p.StartSession(ctx)
			require.NoError(t, err)
			require.NotEmpty(t, res.SessionID)

			sess, err := svc.Pipeline().GetPipelineSession(ctx, res.SessionID)
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.True(t, sess.Scoped, "a scoped run must persist the flag on the session row")
		})
	}
}

// TestPipeline_UnknownSession_TouchesNothing covers the read side of the same
// safety property the deleted internal/mcp mock test guarded: when the engine
// cannot read a session, it errors out rather than proceeding on a guessed
// scoped flag. Completion — and therefore the watermark advance — is never
// reached.
func TestPipeline_UnknownSession_TouchesNothing(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()

	p := NewHypothesizer(ri, nil, EffortNormal, ScopeFilter{})
	_, err := p.ContinueSession(ctx, "no-such-session", "ack")
	require.Error(t, err)

	wm, err := svc.Pipeline().GetPipelineWatermark(ctx, "hypothesize", "agent/test")
	require.NoError(t, err)
	require.Empty(t, wm, "an unreadable session must never advance the watermark")
}

// TestFactFromSearchResult_PropagatesOrigin is the regression guard for the
// first-run idempotency leak, moved here from internal/mcp when the engine's
// factFromSearchResult became the ONLY search-hit → fact.Fact projection.
//
// The full-scan seed path used to build each seed from a search result without
// copying Origin, so a discovered-origin fact slipped past the §7 idempotency
// filter in the bridge engine (which excludes only origin=discovered) and
// seeded its own discovery. Origin MUST survive the projection.
func TestFactFromSearchResult_PropagatesOrigin(t *testing.T) {
	mk := func(origin string) store.SearchResult {
		var r store.SearchResult
		r.Path = "kb/x/p.md"
		r.Title = "P"
		r.Body = "body"
		r.Type = string(fact.Synthesis)
		r.Domain = []string{"auth"}
		r.Entities = []string{"shared"}
		r.Confidence = 0.9
		r.Sources = 1
		r.Origin = origin
		return r
	}

	discovered := factFromSearchResult(mk("discovered"))
	require.Equal(t, fact.Discovered, discovered.Origin,
		"discovered-origin facts must carry Origin so bridge seeding can exclude them (§7 idempotency)")
	require.Equal(t, fact.Authored, factFromSearchResult(mk("authored")).Origin)

	// The other index-sourced fields must survive the projection too.
	require.Equal(t, fact.Synthesis, discovered.Type)
	require.Equal(t, []string{"auth"}, discovered.Domain)
	require.Equal(t, []string{"shared"}, discovered.Entities)
	require.Equal(t, 0.9, discovered.Confidence)
	require.Equal(t, 1, discovered.Sources)
}
