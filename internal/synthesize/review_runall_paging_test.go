package synthesize

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/llm"
	"knomit/internal/store"
)

// RunAll and paging.
//
// Paging exists because ONE MCP tool result cannot carry a large work item. It
// is a property of that transport and of nothing else. RunAll is the other
// consumer of the same engine — the in-process synthesis job in
// internal/web/handlers_jobs.go, driving a local LLM adapter — and it receives
// the item whole, in a Go value, with no size limit and no pages.
//
// So the accumulate-then-respond guard must not fire for it. When it did, every
// distill item larger than a single page (~24 KiB delivered, against a 256 KiB
// item budget — i.e. most real items) was permanently unanswerable: RunAll had
// no token to supply, no way to obtain one, and aborted the whole session on
// the first oversized item. The guard meant to prevent a synthesis over partial
// input instead prevented synthesis entirely.
//
// The fix is itemDelivery: the guard asks about the DELIVERY, not the item.

// wholeItemAdapter is an llm.LLMAdapter that answers every work item with a
// response valid for its step type, and records what it was shown.
//
// It asserts as it goes rather than only at the end: the failure this guards
// against is the model being handed instructions about facts that are not in
// the message, so "the facts arrived" is checked on every call, not inferred
// from the session completing.
type wholeItemAdapter struct {
	t         *testing.T
	calls     atomic.Int64
	maxSeen   atomic.Int64
	sawFactor bool
}

func (a *wholeItemAdapter) Model() string { return "test/whole-item" }

func (a *wholeItemAdapter) Complete(_ context.Context, _ string, msgs []llm.Message, _ llm.CompletionOptions, _ func(string)) (string, error) {
	a.calls.Add(1)
	require.Len(a.t, msgs, 1)
	content := msgs[0].Content

	if n := int64(len(content)); n > a.maxSeen.Load() {
		a.maxSeen.Store(n)
	}
	if strings.Contains(content, "Facts in scope:") {
		a.sawFactor = true
	}

	// A superset response: each step probes only for its own required key, and
	// unknown keys are ignored, so one string answers every step type.
	return `{"decisions": [], "synthesize": [], "retract": [], "methodologies": []}`, nil
}

// TestRunAll_AnswersItemsThatSpanSeveralPages is the regression guard on the
// blocking defect.
//
// The corpus is deliberately the same shape the paging tests use, and the
// precondition asserts the item really is multi-page — without that, the test
// would pass against the broken build for the uninteresting reason that
// nothing triggered the guard.
func TestRunAll_AnswersItemsThatSpanSeveralPages(t *testing.T) {
	r, _, _, _ := seedDistillCorpusSized(t, 60, 3*1024)
	ctx := context.Background()

	adapter := &wholeItemAdapter{t: t}
	require.NoError(t, r.RunAll(ctx, adapter),
		"RunAll receives every item whole and must not be asked for a completion token it can never obtain")

	require.Greater(t, adapter.calls.Load(), int64(0), "RunAll must have driven at least one item")
	require.True(t, adapter.sawFactor,
		"a step whose payload ships beside the prompt must have it recombined into the message")

	// The precondition, asserted after the fact rather than by starting a
	// second session: at least one message RunAll sent was larger than a page.
	// Without this the test would pass against the broken build for the
	// uninteresting reason that nothing ever triggered the guard.
	require.Greater(t, adapter.maxSeen.Load(), int64(maxPageFactBytes),
		"precondition: RunAll must have been handed an item too large for one page")
}

// TestRunAll_StillRejectsNothing_WireStillEnforces pins the other half: the
// exemption is for the in-process delivery only. An answer arriving over the
// wire path for the same item is still refused without a token, or the fix
// would have quietly deleted the guard instead of scoping it.
func TestRunAll_WirePathStillEnforcesTheToken(t *testing.T) {
	r, svc, sessionID := pagingCorpus(t, 60, 3*1024)
	ctx := context.Background()
	item := currentDistillItem(t, svc, sessionID)

	first, err := r.PageItem(ctx, sessionID, item.ID, 1)
	require.NoError(t, err)
	require.Greater(t, first.Item.Pages, 1, "precondition: item must be multi-page")

	_, err = r.ContinueSessionForItemPaged(ctx, sessionID,
		`{"synthesize": [], "retract": []}`, item.ID, "")
	require.Error(t, err, "the wire path must still demand proof every page was read")
	require.Contains(t, err.Error(), "completion_token")
}

// TestRenderPayload_MatchesRenderExactly is the correctness condition of the
// payload-only page render.
//
// Pages after the first are built by RenderPayload instead of Render, to skip a
// prompt they would discard. That is only sound if the two produce identical
// bytes: the completion token is derived from the SERVED payload and validated
// against the STORED row, so a payload that differed between the page-1 render
// and the page-N render would issue a token that can never be accepted, and the
// item would be unanswerable with no error explaining why.
func TestRenderPayload_MatchesRenderExactly(t *testing.T) {
	r, svc, _, _ := seedDistillCorpusSized(t, 12, 512)
	ctx := context.Background()

	res, err := r.StartSession(ctx)
	require.NoError(t, err)
	require.NotNil(t, res.Item)

	item, err := svc.Pipeline().NextPipelineWorkItem(ctx, res.SessionID)
	require.NoError(t, err)
	require.NotNil(t, item)

	payload, err := reviewStrategy{}.RenderPayload(item)
	require.NoError(t, err)
	require.NotEmpty(t, payload)

	// res.Item.Facts is what the full Render produced for the same item. This
	// corpus is small enough to be single-page, so page 1 carries all of it.
	require.Equal(t, 1, res.Item.Pages, "precondition: a single-page item, so page 1 is the whole payload")
	require.Equal(t, string(res.Item.Facts), payload,
		"RenderPayload must reproduce Render's payload byte-for-byte, or paged items issue tokens that cannot validate")

	// And it must agree with the stored row, which is what RequireCompletion
	// hashes on the answer side.
	require.Equal(t, item.FactsJSON, payload,
		"the served payload and the stored row must hash to the same completion token")
}

// renderCountingStrategy is reviewStrategy with a tally on the two render
// entry points, so a test can assert which one a page went through.
type renderCountingStrategy struct {
	reviewStrategy
	renders  *atomic.Int64
	payloads *atomic.Int64
}

func (s renderCountingStrategy) Render(ctx context.Context, d Deps, sess *store.PipelineSession, item *store.PipelineWorkItem) (*WorkItemView, error) {
	s.renders.Add(1)
	return s.reviewStrategy.Render(ctx, d, sess, item)
}

func (s renderCountingStrategy) RenderPayload(item *store.PipelineWorkItem) (string, error) {
	s.payloads.Add(1)
	return s.reviewStrategy.RenderPayload(item)
}

// TestPaging_LaterPagesDoNotRerenderThePrompt is the regression guard on the
// cost of serving a page.
//
// Every page used to go through the strategy's full Render, and review's Render
// runs one methodology retrieval PER FACT in the item
// (distillMethodologySection). So an N-fact item served across P pages issued
// N×P store queries where N would do — and every one of the (P−1)×N extra
// queries built a prompt that reviewResultPage then dropped, since pages after
// the first carry facts only.
//
// Asserted on the render path rather than on a timing, because the property is
// structural: page 1 renders, later pages must not.
func TestPaging_LaterPagesDoNotRerenderThePrompt(t *testing.T) {
	r, svc, _, _ := seedDistillCorpusSized(t, 60, 3*1024)
	ctx := context.Background()

	var renders, payloads atomic.Int64
	p := NewPipeline(r.ri, nil, DefaultEffort, ScopeFilter{},
		renderCountingStrategy{renders: &renders, payloads: &payloads})

	res, err := p.StartSession(ctx)
	require.NoError(t, err)
	require.NotNil(t, res.Item)

	item, err := svc.Pipeline().NextPipelineWorkItem(ctx, res.SessionID)
	require.NoError(t, err)
	require.NotNil(t, item)

	pages, err := reviewResultPage(res, 1)
	require.NoError(t, err)
	require.Greater(t, pages.Item.Pages, 2,
		"precondition: the item must span several pages for this to mean anything")

	renders.Store(0)
	payloads.Store(0)

	for page := 2; page <= pages.Item.Pages; page++ {
		out, perr := p.CurrentItem(ctx, res.SessionID, item.ID, page)
		require.NoErrorf(t, perr, "page %d", page)
		require.NotEmptyf(t, out.Item.Facts, "page %d must still carry its payload", page)
	}

	require.Zero(t, renders.Load(),
		"pages after the first discard the prompt; rendering one costs a methodology retrieval per fact")
	require.Equal(t, int64(pages.Item.Pages-1), payloads.Load(),
		"each later page must be built by the cheap payload-only path")

	// Page 1 still pays for the prompt, because it is the page that carries it.
	first, err := p.CurrentItem(ctx, res.SessionID, item.ID, 1)
	require.NoError(t, err)
	require.NotEmpty(t, first.Item.Prompt)
	require.Equal(t, int64(1), renders.Load())
}

// TestRenderPayload_EmptyForStepsThatInterpolateTheirPayload keeps the
// fall-through honest: a step whose facts live inside its prompt has no
// payload to serve on its own, and the engine must fall back to a full render
// rather than emit a page with no content.
func TestRenderPayload_EmptyForStepsThatInterpolateTheirPayload(t *testing.T) {
	for _, step := range []string{"reflect", "discover"} {
		got, err := reviewStrategy{}.RenderPayload(&store.PipelineWorkItem{
			ID: 3, StepType: step, FactsJSON: `[{"path":"kb/a.md"}]`,
		})
		require.NoErrorf(t, err, "step %s", step)
		require.Emptyf(t, got, "%s interpolates its payload into the prompt and has nothing to serve beside it", step)
	}
}
