package synthesize

import (
	"context"
	"sync/atomic"
	"testing"

	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/engine/store/model"

	"github.com/stretchr/testify/require"
)

// Payload-only page rendering. Pages after the first are built by
// RenderPayload instead of Render; these tests pin that the two agree and that
// later pages do not re-render the prompt. The wire-path token guard itself is
// pinned by TestPaging_MultiPageAnswerWithoutTokenIsRejected in
// review_paging_test.go.

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

func (s renderCountingStrategy) Render(ctx context.Context, d step.Deps, sess *model.PipelineSession, item *model.PipelineWorkItem) (*step.WorkItemView, error) {
	s.renders.Add(1)
	return s.reviewStrategy.Render(ctx, d, sess, item)
}

func (s renderCountingStrategy) RenderPayload(item *model.PipelineWorkItem) (string, error) {
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
	p := NewPipeline(r.ri, nil, step.DefaultEffort, step.ScopeFilter{},
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
		got, err := reviewStrategy{}.RenderPayload(&model.PipelineWorkItem{
			ID: 3, StepType: step, FactsJSON: `[{"path":"kb/a.md"}]`,
		})
		require.NoErrorf(t, err, "step %s", step)
		require.Emptyf(t, got, "%s interpolates its payload into the prompt and has nothing to serve beside it", step)
	}
}
