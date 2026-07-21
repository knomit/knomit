package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/synthesize"
)

// mcpToolRequest builds a CallToolRequest from the given key-value arguments.
func mcpToolRequest(t *testing.T, params map[string]interface{}) mcpgo.CallToolRequest {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = params
	return req
}

// synthFactContent returns a valid serialized synthesis fact body for use in
// hypothesize tests. Uses fact.SerializeFact to guarantee the content round-
// trips through ParseFact (body requires a # heading; raw YAML strings don't).
func synthFactContent(t *testing.T, path, title string) string {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = "synthesis body"
	f.Type = fact.Synthesis
	f.Origin = fact.Distilled
	f.Confidence = 0.8
	f.Sources = 1
	f.Domain = []string{"x"}
	out, err := fact.SerializeFact(f)
	require.NoError(t, err)
	return out
}

// openHypothesizeTestStore opens a fresh store, initialises the agent branch,
// and returns a (Service, RepoInstance, mcpStore) triple ready for hypothesize
// tests. No embedder: search paths exercised here are SQL-only.
func openHypothesizeTestStore(t *testing.T) (*store.Service, *repos.RepoInstance, mcpStore) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  "agent/test",
		Svc:          svc,
		OntologyRoot: "kb",
	})

	s := mcpStore{
		facts:    svc.Facts(),
		search:   svc.Search(),
		pipeline: svc.Pipeline(),
		branches: svc.Branches(),
	}
	return svc, ri, s
}

// TestScopedHypothesizeStart_EmptyPool_DoesNotAdvanceWatermark is the
// regression guard for the early-return watermark-poisoning bug. When
// hypothesizeStart finds zero synthesis facts and a scope filter is active,
// it must NOT advance the watermark to HEAD. Before the fix, the early return
// unconditionally called SetPipelineWatermark, permanently hiding all
// out-of-scope synthesis facts from future unscoped sessions.
func TestScopedHypothesizeStart_EmptyPool_DoesNotAdvanceWatermark(t *testing.T) {
	svc, ri, s := openHypothesizeTestStore(t)
	ctx := context.Background()
	agentBranch := "agent/test"

	// No synthesis facts exist → the early-return path is taken.
	scope := synthesize.ScopeFilter{Domain: []string{"auth"}}
	result, err := hypothesizeStart(ctx, ri, s, agentBranch, synthesize.EffortNormal, scope)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Done, "empty pool → Done immediately")

	// Watermark must NOT have advanced.
	watermark, err := svc.Pipeline().GetPipelineWatermark(ctx, "hypothesize", agentBranch)
	require.NoError(t, err)
	require.Empty(t, watermark,
		"scoped hypothesizeStart with empty pool must not advance the watermark: "+
			"out-of-scope synthesis facts would be permanently hidden from future unscoped sessions")
}

// TestUnscopedHypothesizeStart_EmptyPool_AdvancesWatermark confirms the
// unscoped early-return path still advances the watermark (the desired
// behaviour: nothing to do, mark progress).
func TestUnscopedHypothesizeStart_EmptyPool_AdvancesWatermark(t *testing.T) {
	svc, ri, s := openHypothesizeTestStore(t)
	ctx := context.Background()
	agentBranch := "agent/test"

	result, err := hypothesizeStart(ctx, ri, s, agentBranch, synthesize.EffortNormal, synthesize.ScopeFilter{})
	require.NoError(t, err)
	require.True(t, result.Done)

	// Unscoped: watermark SHOULD advance so the next run is incremental.
	watermark, err := svc.Pipeline().GetPipelineWatermark(ctx, "hypothesize", agentBranch)
	require.NoError(t, err)
	require.NotEmpty(t, watermark,
		"unscoped hypothesizeStart with empty pool must advance watermark for incremental next run")
}

// TestHypothesizeStart_MarkScopedFails_ReturnsError is the regression guard for
// the swallowed MarkPipelineSessionScoped error. Before the fix, the error was
// logged and the session continued with Scoped=false in the DB, causing the
// watermark to advance at session completion and permanently hiding out-of-scope
// facts from future unscoped sessions.
func TestHypothesizeStart_MarkScopedFails_ReturnsError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	_, ri, realS := openHypothesizeTestStore(t)
	ctx := context.Background()

	// Seed one synthesis fact (domain=auth) so hypothesizeStart doesn't
	// short-circuit when the scope filter Domain=["auth"] is applied.
	seedFact := fact.NewFact("kb/arch/a.md")
	seedFact.Title = "T"
	seedFact.Body = "synthesis body"
	seedFact.Type = fact.Synthesis
	seedFact.Origin = fact.Distilled
	seedFact.Confidence = 0.8
	seedFact.Sources = 1
	seedFact.Domain = []string{"auth"}
	seedContent, serErr := fact.SerializeFact(seedFact)
	require.NoError(t, serErr)
	_, err := realS.facts.WriteFact(ctx, "agent/test", "kb/arch/a.md",
		seedContent, "seed", "")
	require.NoError(t, err)

	// Mock PipelineIndex: MarkPipelineSessionScoped returns an error.
	mp := NewMockPipelineIndex(ctrl)
	mp.EXPECT().GetPipelineWatermark(gomock.Any(), "hypothesize", "agent/test").Return("", nil)
	mp.EXPECT().CreatePipelineSession(gomock.Any(), "hypothesize", "agent/test").
		DoAndReturn(func(_ context.Context, _, _ string) (*store.PipelineSession, error) {
			return realS.pipeline.CreatePipelineSession(ctx, "hypothesize", "agent/test")
		})
	mp.EXPECT().MarkPipelineSessionScoped(gomock.Any(), gomock.Any()).
		Return(fmt.Errorf("db write error"))

	s := mcpStore{
		facts:    realS.facts,
		search:   realS.search,
		pipeline: mp,
		branches: realS.branches,
	}

	scope := synthesize.ScopeFilter{Domain: []string{"auth"}}
	_, err = hypothesizeStart(ctx, ri, s, "agent/test", synthesize.EffortNormal, scope)
	require.Error(t, err, "hypothesizeStart must return error when MarkPipelineSessionScoped fails")
	require.Contains(t, err.Error(), "mark session scoped")
}

// TestHypothesizeNextItem_GetSessionError_SuppressesWatermark is the regression
// guard for the discarded GetPipelineSession error. Before the fix, a DB error
// returned (nil, err); sess was nil; sess==nil triggered the watermark-advance
// branch unconditionally.
func TestHypothesizeNextItem_GetSessionError_SuppressesWatermark(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	_, ri, realS := openHypothesizeTestStore(t)
	ctx := context.Background()

	// Create an active session with no work items so hypothesizeNextItem
	// takes the "no more items → complete" path.
	sess, err := realS.pipeline.CreatePipelineSession(ctx, "hypothesize", "agent/test")
	require.NoError(t, err)

	// Mock PipelineIndex: GetPipelineSession returns an error for this session.
	mp := NewMockPipelineIndex(ctrl)
	mp.EXPECT().NextPipelineWorkItem(gomock.Any(), sess.ID).Return(nil, nil)
	mp.EXPECT().GetPipelineSession(gomock.Any(), sess.ID).Return(nil, fmt.Errorf("db error"))
	mp.EXPECT().CompletePipelineSession(gomock.Any(), sess.ID).Return(nil)
	// SetPipelineWatermark must NOT be called.
	mp.EXPECT().SetPipelineWatermark(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Times(0)
	mp.EXPECT().PipelineWorkItemStats(gomock.Any(), gomock.Any()).Return(0, 0, nil).AnyTimes()

	s := mcpStore{
		facts:    realS.facts,
		search:   realS.search,
		pipeline: mp,
		branches: realS.branches,
	}

	result, err := hypothesizeNextItem(ctx, ri, s, "agent/test", sess.ID)
	require.NoError(t, err)
	require.True(t, result.Done)
	// gomock will fail the test if SetPipelineWatermark was called (Times(0)).
}

// TestHypothesizeContinue_MarkAnsweredBeforeApply confirms that when
// SetPipelineWorkItemResponse fails, ApplyDiscoveredProposals was never called
// and no facts were written. This guards against the double-write bug where
// facts were written first; a crash/error before marking the item answered
// would cause the item to be re-presented and facts duplicated on retry.
func TestHypothesizeContinue_MarkAnsweredBeforeApply(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	_, ri, realS := openHypothesizeTestStore(t)
	ctx := context.Background()
	branch := "agent/test"

	// Create session with one discover work item.
	sess, err := realS.pipeline.CreatePipelineSession(ctx, "hypothesize", branch)
	require.NoError(t, err)

	// Use forward direction so the blast-radius gate (backward-only) is not
	// triggered. BridgeSeedSet.Members is []factForLLM (unexported), so build
	// the payload as raw JSON to avoid referencing the unexported type.
	payloadJSON := []byte(`{"direction":"forward","bridge":{"token":"auth","kind":"entity","members":[]}}`)
	// Sanity-check: the JSON must round-trip into DiscoverWorkPayload.
	var check synthesize.DiscoverWorkPayload
	require.NoError(t, json.Unmarshal(payloadJSON, &check))

	require.NoError(t, realS.pipeline.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   "discover",
		ClusterKey: "fwd-0",
		FactsJSON:  string(payloadJSON),
		Priority:   -100,
	}))

	// Mock PipelineIndex: intercept the real pipeline but make
	// SetPipelineWorkItemResponse fail. Everything else delegates to realS.
	mp := NewMockPipelineIndex(ctrl)
	mp.EXPECT().GetPipelineSession(gomock.Any(), sess.ID).
		DoAndReturn(func(ctx context.Context, id string) (*store.PipelineSession, error) {
			return realS.pipeline.GetPipelineSession(ctx, id)
		})
	mp.EXPECT().NextPipelineWorkItem(gomock.Any(), sess.ID).
		DoAndReturn(func(ctx context.Context, id string) (*store.PipelineWorkItem, error) {
			return realS.pipeline.NextPipelineWorkItem(ctx, id)
		})
	mp.EXPECT().SetPipelineWorkItemResponse(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(fmt.Errorf("db write error"))

	s := mcpStore{
		facts:    realS.facts,
		search:   realS.search,
		pipeline: mp,
		branches: realS.branches,
	}

	// A valid forward-discover response with one synthesis proposal. The path
	// must be under the ontology root ("kb") and use type="synthesis" to match
	// the forward direction. With empty bridge members, refs=[] satisfies the
	// refsCoverSeeds check, and confidence 0.9 exceeds the 0.5 threshold.
	response := `{"proposals":[{"path":"kb/x/p.md","title":"P","body":"B","type":"synthesis","domain":["auth"],"confidence":0.9,"entities":[],"refs":[]}]}`
	_, err = hypothesizeContinue(ctx, ri, s, branch, sess.ID, response)
	require.Error(t, err, "hypothesizeContinue must propagate SetPipelineWorkItemResponse error")

	// Facts index must be empty — ApplyDiscoveredProposals must not have run.
	results, searchErr := realS.search.Search(ctx, branch, store.SearchOptions{Limit: 10})
	require.NoError(t, searchErr)
	require.Empty(t, results, "no facts must be written when SetPipelineWorkItemResponse fails")
}

// TestScopedHypothesize_NonEmptyWatermark_StillSeedsInScope is the regression
// guard for the read-side watermark gating bug. A scoped hypothesize run must
// re-examine its whole scope regardless of the shared "hypothesize" watermark.
//
// Before the fix, hypothesizeStart chose first-run (full synthesis-fact scan)
// vs incremental (DiffFiles since watermark) purely on watermark=="". So once a
// prior UNSCOPED run advanced the watermark to HEAD, a scoped run took the
// incremental path, DiffFiles returned nothing, and the scope filter ran over an
// empty set → zero synth facts → Done immediately even though the scope held
// synthesis facts. Scoped runs don't ADVANCE the watermark, so they must not be
// BLOCKED by it either.
func TestScopedHypothesize_NonEmptyWatermark_StillSeedsInScope(t *testing.T) {
	_, ri, s := openHypothesizeTestStore(t)
	ctx := context.Background()
	branch := "agent/test"

	// Two synthesis facts in scope (auth) — enough to start a session.
	for _, slug := range []string{"a", "b"} {
		f := fact.NewFact("kb/arch/" + slug + ".md")
		f.Title = slug
		f.Body = "synthesis body"
		f.Type = fact.Synthesis
		f.Origin = fact.Distilled
		f.Confidence = 0.8
		f.Sources = 1
		f.Domain = []string{"auth"}
		content, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = s.facts.WriteFact(ctx, branch, f.Path(), content, "seed", "")
		require.NoError(t, err)
	}

	// Simulate a prior unscoped run having advanced the watermark to HEAD.
	head, err := s.branches.HeadCommit(ctx, branch)
	require.NoError(t, err)
	require.NotEmpty(t, head)
	require.NoError(t, s.pipeline.SetPipelineWatermark(ctx, "hypothesize", branch, head))

	scope := synthesize.ScopeFilter{Domain: []string{"auth"}}
	result, err := hypothesizeStart(ctx, ri, s, branch, synthesize.EffortNormal, scope)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Done,
		"scoped hypothesize must seed its whole scope even when the watermark is at HEAD; "+
			"the watermark must not block a scoped re-examination")
}

// TestHypothesizeStart_BackwardDiscovery_SingleFact_Silent checks that
// hypothesizeStart completes cleanly when effort=high but only 1 synthesis fact
// matches the scope (len(synthFacts) < 2 guard). Before the regression was found,
// this path was silent (no log, no error). This test confirms no crash and normal
// session creation — the log-emission is verified by code review.
func TestHypothesizeStart_BackwardDiscovery_SingleFact_Silent(t *testing.T) {
	_, ri, s := openHypothesizeTestStore(t)
	ctx := context.Background()
	branch := "agent/test"

	// Write exactly one synthesis fact in the scoped domain.
	f := fact.NewFact("kb/arch/only.md")
	f.Title = "Only"
	f.Body = "synthesis body"
	f.Type = fact.Synthesis
	f.Origin = fact.Distilled
	f.Confidence = 0.8
	f.Sources = 1
	f.Domain = []string{"auth"}
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = s.facts.WriteFact(ctx, branch, "kb/arch/only.md", content, "seed", "")
	require.NoError(t, err)

	scope := synthesize.ScopeFilter{Domain: []string{"auth"}}
	result, err := hypothesizeStart(ctx, ri, s, branch, synthesize.EffortHigh, scope)
	require.NoError(t, err)
	require.NotNil(t, result)
	// Session must start (1 synthesis fact → 1 hypothesize item), not crash.
	require.False(t, result.Done, "one synthesis fact in scope → session should start")
}

// TestHypothesizeHandler_EffortValidation_OnlyContinue checks that passing an
// invalid effort on a continue call does NOT block session advancement.
// Before the fix, effort was validated before the session_id branch, so
// effort="bogus" on a continue would return a tool error and leave the session stuck.
func TestHypothesizeHandler_EffortValidation_OnlyContinue(t *testing.T) {
	_, ri, s := openHypothesizeTestStore(t)
	ctx := repos.WithRepoInstance(context.Background(), ri)

	// Start a session with one synthesis fact present so session_id is issued.
	_, err := s.facts.WriteFact(ctx, "agent/test", "kb/arch/a.md",
		synthFactContent(t, "kb/arch/a.md", "T"),
		"seed", "")
	require.NoError(t, err)

	r1, err := hypothesizeStart(ctx, ri, s, "agent/test", synthesize.EffortNormal, synthesize.ScopeFilter{})
	require.NoError(t, err)
	require.False(t, r1.Done)
	sid := r1.SessionID

	// Continue with an invalid effort — must succeed (effort ignored on continue path).
	handler := HypothesizeHandler()
	req := mcpToolRequest(t, map[string]interface{}{
		"session_id": sid,
		"response":   "ack",
		"effort":     "bogus",
	})
	res, err := handler(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError,
		"continue with invalid effort must not error: agent cannot advance a stuck session")
}
