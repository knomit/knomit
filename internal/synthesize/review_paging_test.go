package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// Work-item paging.
//
// The MCP tool-result cap bounds ONE tool result, not the conversation. Before
// paging, those were the same thing — an item shipped in a single response, so
// the transport limit doubled as a limit on how much one synthesis decision
// could be built from, and the only lever for an undeliverable item was to show
// the model less. Paging severs that: the item stays whole, the agent
// accumulates every page into its context, and answers once at the end.
//
// The contract has a hard edge, and it is the reason these tests exist. An
// agent that answers on page 1 must be REJECTED, not accepted. Merely
// instructing it to page until exhausted would repeat the defect fixed in
// fef554d1, where a response_schema's `required` list was inert because nothing
// probed for it — an advisory rule on a write path is not a rule. So the last
// page carries a token the agent can only hold by having received that page,
// and a multi-page item answered without it fails loudly, before the claim, so
// the item stays retryable.

// pagingCorpus seeds one cluster large enough to span several pages and returns
// the reviewer, store, and session.
func pagingCorpus(t *testing.T, facts int, bodyBytes int) (*Reviewer, *store.Service, string) {
	t.Helper()
	r, svc, _, _ := seedDistillCorpusSized(t, facts, bodyBytes)
	res, err := r.StartSession(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)
	return r, svc, res.SessionID
}

// currentDistillItem returns the first unanswered distill item on the session,
// draining anything ahead of it.
func currentDistillItem(t *testing.T, svc *store.Service, sessionID string) *store.PipelineWorkItem {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		item, err := svc.Pipeline().NextPipelineWorkItem(ctx, sessionID)
		require.NoError(t, err)
		require.NotNil(t, item, "ran out of items before reaching a distill item")
		if item.StepType == "distill" {
			return item
		}
		claimed, aerr := svc.Pipeline().AnswerPipelineWorkItem(ctx, item.ID, "{}")
		require.NoError(t, aerr)
		require.True(t, claimed)
	}
	t.Fatal("no distill item found")
	return nil
}

// TestPaging_ItemIsServedInPagesThatPartitionItsFacts is the core read-path
// contract. Every fact appears on exactly one page, in order, and the last page
// — and only the last — carries the completion token.
//
// Partitioning is asserted as an equality rather than a subset for the same
// reason as everywhere else in this package: a page boundary that drops a fact
// would be invisible, and the agent would synthesize over a silently incomplete
// cluster while believing it had seen everything.
func TestPaging_ItemIsServedInPagesThatPartitionItsFacts(t *testing.T) {
	r, svc, sessionID := pagingCorpus(t, 60, 3*1024)
	ctx := context.Background()

	item := currentDistillItem(t, svc, sessionID)

	var stored []factForLLM
	require.NoError(t, json.Unmarshal([]byte(item.FactsJSON), &stored))
	require.Greater(t, len(stored), 1)

	first, err := r.PageItem(ctx, sessionID, item.ID, 1)
	require.NoError(t, err)
	require.NotNil(t, first.Item)
	require.Greater(t, first.Item.Pages, 1,
		"precondition: the corpus must be large enough to span more than one page")

	var seen []string
	pages := first.Item.Pages
	for p := 1; p <= pages; p++ {
		res, perr := r.PageItem(ctx, sessionID, item.ID, p)
		require.NoErrorf(t, perr, "page %d", p)
		require.Equal(t, p, res.Item.Page)
		require.Equal(t, pages, res.Item.Pages)

		var pf []factForLLM
		require.NoError(t, json.Unmarshal(res.Item.Facts, &pf))
		require.NotEmptyf(t, pf, "page %d is empty", p)
		for _, f := range pf {
			seen = append(seen, f.File)
		}

		last := p == pages
		require.Equalf(t, !last, res.Item.MoreAvailable, "more_available on page %d", p)
		if last {
			require.NotEmpty(t, res.Item.CompletionToken,
				"the final page must carry the token that proves the agent reached it")
		} else {
			require.Empty(t, res.Item.CompletionToken,
				"an intermediate page must not carry the token — it is the proof of completion")
		}
	}

	want := make([]string, 0, len(stored))
	for _, f := range stored {
		want = append(want, f.File)
	}
	require.Equal(t, want, seen,
		"pages must partition the item's facts exactly, in order — no fact dropped, none duplicated")
}

// TestPaging_MoreAvailableIsStatedOnEveryPage pins more_available in the
// DELIVERED form rather than on the Go struct.
//
// The distinction is the whole lesson of this area. Every other assertion about
// more_available reads res.Item.MoreAvailable, which is `false` whether the
// field ships or is omitted — so an `omitempty` on it is invisible to all of
// them, while on the wire it deletes the field from the final page entirely.
// That is the page the agent is waiting on: the tool description, both paged
// prompt templates, and the `page` argument all phrase the protocol as "until
// more_available is false", and absent is not false to a reader told to look
// for false.
//
// Marshalled exactly as internal/mcp/review.go ships it, because the struct is
// not the artifact — the same reason TestDistillWorkItem_EveryPageFitsDeliveredCap
// measures MarshalIndent instead of trusting a byte count.
func TestPaging_MoreAvailableIsStatedOnEveryPage(t *testing.T) {
	r, svc, sessionID := pagingCorpus(t, 60, 3*1024)
	ctx := context.Background()
	item := currentDistillItem(t, svc, sessionID)

	first, err := r.PageItem(ctx, sessionID, item.ID, 1)
	require.NoError(t, err)
	require.Greater(t, first.Item.Pages, 1, "precondition: the item must be multi-page")

	for p := 1; p <= first.Item.Pages; p++ {
		res, perr := r.PageItem(ctx, sessionID, item.ID, p)
		require.NoErrorf(t, perr, "page %d", p)

		delivered, merr := json.MarshalIndent(res, "", "  ")
		require.NoError(t, merr)

		var wire struct {
			Item map[string]json.RawMessage `json:"item"`
		}
		require.NoError(t, json.Unmarshal(delivered, &wire))

		raw, present := wire.Item["more_available"]
		require.Truef(t, present,
			"page %d of %d omits more_available from the delivered result; the agent is told to "+
				"page until it is false, so a page that never states it cannot end the loop",
			p, first.Item.Pages)
		require.JSONEqf(t, fmt.Sprint(p < first.Item.Pages), string(raw),
			"more_available on page %d of %d", p, first.Item.Pages)
	}
}

// TestPaging_PromptOnlyOnFirstPage keeps the repeated cost off later pages: the
// instructions are already in the agent's context by then, and re-sending them
// per page would spend the budget this feature exists to protect.
func TestPaging_PromptOnlyOnFirstPage(t *testing.T) {
	r, svc, sessionID := pagingCorpus(t, 60, 3*1024)
	ctx := context.Background()
	item := currentDistillItem(t, svc, sessionID)

	first, err := r.PageItem(ctx, sessionID, item.ID, 1)
	require.NoError(t, err)
	require.NotEmpty(t, first.Item.Prompt)
	require.NotEmpty(t, first.Item.ResponseSchema)

	second, err := r.PageItem(ctx, sessionID, item.ID, 2)
	require.NoError(t, err)
	require.Empty(t, second.Item.Prompt, "later pages must not repeat the prompt")
	require.Empty(t, second.Item.ResponseSchema, "later pages must not repeat the schema")
}

// TestPaging_IsAPureReadAndNeverClaims pins that paging happens entirely before
// the claim CAS. Fetching pages must leave the item unanswered and re-fetchable,
// or an agent that died mid-page would lose the work — and
// invariants/synthesize/work-item-claim-protocol would be violated by a read.
func TestPaging_IsAPureReadAndNeverClaims(t *testing.T) {
	r, svc, sessionID := pagingCorpus(t, 60, 3*1024)
	ctx := context.Background()
	item := currentDistillItem(t, svc, sessionID)

	a, err := r.PageItem(ctx, sessionID, item.ID, 2)
	require.NoError(t, err)
	b, err := r.PageItem(ctx, sessionID, item.ID, 2)
	require.NoError(t, err)
	require.Equal(t, a.Item.Facts, b.Item.Facts, "re-fetching a page must be identical")

	still, err := svc.Pipeline().NextPipelineWorkItem(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, still)
	require.Equal(t, item.ID, still.ID, "paging must leave the item unanswered and current")
}

// TestPaging_MultiPageAnswerWithoutTokenIsRejected is the enforcement test —
// the whole reason the token exists.
//
// Without this, an agent reasonably answers on page 1 and the server accepts a
// synthesis built over a fraction of the cluster. That is strictly worse than
// the bug being fixed: a loud transport failure would become a silent quality
// loss, with a summary that reads as success.
func TestPaging_MultiPageAnswerWithoutTokenIsRejected(t *testing.T) {
	r, svc, sessionID := pagingCorpus(t, 60, 3*1024)
	ctx := context.Background()
	item := currentDistillItem(t, svc, sessionID)

	first, err := r.PageItem(ctx, sessionID, item.ID, 1)
	require.NoError(t, err)
	require.Greater(t, first.Item.Pages, 1, "precondition: item must be multi-page")

	_, err = r.ContinueSessionForItemPaged(ctx, sessionID,
		`{"synthesize": [], "retract": []}`, item.ID, "")
	require.Error(t, err, "a multi-page item answered without the completion token must be rejected")
	require.Contains(t, err.Error(), "completion_token",
		"the error must name what is missing")
	require.Contains(t, err.Error(), fmt.Sprint(first.Item.Pages),
		"the error must say how many pages the agent needs to read")

	// Decode never ran, so the item is untouched and a corrected answer works.
	still, err := svc.Pipeline().NextPipelineWorkItem(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, item.ID, still.ID, "a rejected answer must leave the item retryable")
}

// TestPaging_WrongTokenIsRejected closes the obvious bypass: a token that is
// present but not this item's proves nothing.
func TestPaging_WrongTokenIsRejected(t *testing.T) {
	r, svc, sessionID := pagingCorpus(t, 60, 3*1024)
	ctx := context.Background()
	item := currentDistillItem(t, svc, sessionID)

	_, err := r.ContinueSessionForItemPaged(ctx, sessionID,
		`{"synthesize": [], "retract": []}`, item.ID, "not-the-right-token")
	require.Error(t, err)
	require.Contains(t, err.Error(), "completion_token")
}

// TestPaging_CorrectTokenApplies is the happy path: page to the end, echo the
// token, and the answer is accepted normally.
func TestPaging_CorrectTokenApplies(t *testing.T) {
	r, svc, sessionID := pagingCorpus(t, 60, 3*1024)
	ctx := context.Background()
	item := currentDistillItem(t, svc, sessionID)

	first, err := r.PageItem(ctx, sessionID, item.ID, 1)
	require.NoError(t, err)
	last, err := r.PageItem(ctx, sessionID, item.ID, first.Item.Pages)
	require.NoError(t, err)
	require.NotEmpty(t, last.Item.CompletionToken)

	_, err = r.ContinueSessionForItemPaged(ctx, sessionID,
		`{"synthesize": [], "retract": []}`, item.ID, last.Item.CompletionToken)
	require.NoError(t, err, "the token from the last page must be accepted")

	next, err := svc.Pipeline().NextPipelineWorkItem(ctx, sessionID)
	require.NoError(t, err)
	if next != nil {
		require.NotEqual(t, item.ID, next.ID, "the answered item must not be served again")
	}
}

// TestPaging_SinglePageItemNeedsNoToken keeps the common case unchanged. Most
// items fit one page, and requiring a token there would break every existing
// caller for no gain — presence of the requirement must track the actual need.
func TestPaging_SinglePageItemNeedsNoToken(t *testing.T) {
	r, svc, sessionID := pagingCorpus(t, 4, 256)
	ctx := context.Background()
	item := currentDistillItem(t, svc, sessionID)

	res, err := r.PageItem(ctx, sessionID, item.ID, 1)
	require.NoError(t, err)
	require.Equal(t, 1, res.Item.Pages, "precondition: this item must fit one page")
	require.False(t, res.Item.MoreAvailable)

	_, err = r.ContinueSessionForItemPaged(ctx, sessionID,
		`{"synthesize": [], "retract": []}`, item.ID, "")
	require.NoError(t, err, "a single-page item must not require a token")
}

// TestPaging_EveryPageFitsTheDeliveredCap ties paging back to the constraint
// that started all of this: whatever the item's total size, every page must be
// deliverable.
//
// This is the same property TestDistillWorkItem_EveryPageFitsDeliveredCap
// asserts, reached through the live session path — StartSession, the planner,
// and PageItem — rather than by assembling a PipelineResult by hand. Keeping
// both is deliberate: the unit test sweeps fact SHAPES cheaply, this one proves
// the shapes it sweeps are the ones a real session actually delivers.
//
// Small bodies as well as large. The cap is hardest to meet when facts are
// SMALL, because indentation cost is per JSON token and a page then holds many
// more of them — the regime the original single-size test never entered.
func TestPaging_EveryPageFitsTheDeliveredCap(t *testing.T) {
	for _, tc := range []struct{ facts, bodyBytes int }{
		{600, 100},
		{300, 512},
		{120, 3 * 1024},
	} {
		t.Run(fmt.Sprintf("body=%d", tc.bodyBytes), func(t *testing.T) {
			r, svc, sessionID := pagingCorpus(t, tc.facts, tc.bodyBytes)
			ctx := context.Background()
			item := currentDistillItem(t, svc, sessionID)

			first, err := r.PageItem(ctx, sessionID, item.ID, 1)
			require.NoError(t, err)
			require.Greater(t, first.Item.Pages, 2, "precondition: a genuinely large item")

			for p := 1; p <= first.Item.Pages; p++ {
				res, perr := r.PageItem(ctx, sessionID, item.ID, p)
				require.NoError(t, perr)

				delivered, merr := json.MarshalIndent(res, "", "  ")
				require.NoError(t, merr)

				var onPage []factForLLM
				require.NoError(t, json.Unmarshal(res.Item.Facts, &onPage))

				require.LessOrEqualf(t, len(delivered), maxDeliveredItemBytes,
					"body=%d: page %d of %d holds %d facts and delivers %d bytes, over the %d cap",
					tc.bodyBytes, p, first.Item.Pages, len(onPage), len(delivered), maxDeliveredItemBytes)
			}
		})
	}
}

// seedDistillCorpusSized writes `total` facts of a given body size into a fresh
// store and returns a reviewer over it. Separate from seedDistillCorpus because
// paging tests need to dial the payload to a specific page count rather than a
// specific cluster shape.
func seedDistillCorpusSized(t *testing.T, total, bodyBytes int) (*Reviewer, *store.Service, string, map[string]string) {
	t.Helper()

	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })

	branch := "agent/test"
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))
	ctx := context.Background()

	body := strings.Repeat("x", bodyBytes)
	paths := map[string]string{}
	for i := 0; i < total; i++ {
		f := fact.NewFact(fmt.Sprintf("kb/architecture/paging/seed-%03d.md", i))
		f.Title = fmt.Sprintf("paging seed %03d", i)
		f.Body = body
		f.Type = fact.Observation
		f.Domain = []string{"architecture", "paging"}
		f.Confidence = 0.7
		f.Sources = 1
		serialized, serr := fact.SerializeFact(f)
		require.NoError(t, serr)
		_, werr := svc.Facts().WriteFact(ctx, branch, f.Path(), serialized, "seed", "")
		require.NoError(t, werr)
		paths[f.Path()] = "paging"
	}

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "test", AgentBranch: branch, Svc: svc, OntologyRoot: "kb",
	})
	return NewReviewer(ri, nil), svc, branch, paths
}
