package synthesize

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/llm"
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
