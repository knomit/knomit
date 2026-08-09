package synthesize

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Payload-delivery contract for review work items.
//
// A distill work item is useless if the client cannot receive it. Seven real
// payloads from sessions 2b56c148-… (2026-07-26) and 284faa87-… (2026-07-27)
// were rejected by the harness and spilled to disk, so the agent never saw the
// work at all. Measured, every one of them sat just under maxDistillChunkBytes
// in COMPACT fact JSON — the chunker did exactly what it was told — and landed
// at 68.6–73.9 KB once delivered. The bound was applied to an artifact 1.13–1.14×
// smaller than the one that ships.
//
// Two defects, one constant:
//
//  1. maxDistillChunkBytes measures len(json.Marshal(f)) — compact fact JSON —
//     while what ships is MarshalIndent inside a prompt string inside a
//     JSON-encoded envelope, where every quote in the facts is escaped again.
//  2. Its stated rationale is a MODEL CONTEXT budget ("~16K tokens: comfortably
//     inside every hosting model's context window"). The model's context never
//     bound. The client's MCP tool-result cap did.
//
// TestReviewer_DistillItemsAreChunked pins len(item.FactsJSON) — the STORED
// form. Nothing pinned the DELIVERED form, which is why this shipped.
//
// The tests below therefore measure the delivered artifact — and measure it
// across the range of fact shapes, which is the second half of the same lesson.
// A budget that converted compact bytes to delivered bytes through a PERCENTAGE
// was still wrong, because indentation cost is per JSON token: the same 25 KB of
// compact facts delivers 32 KB as eleven 2 KiB-body facts and 46 KB as ninety-nine
// 50 B-body ones. A test pinned to a single body size cannot see that, so
// bodySizes below is deliberately a sweep.

// maxMethodologySection builds the largest methodology block the distill path
// can inject. Production calls distillMethodologySection() into this prompt;
// rendering without it would measure a prompt smaller than any real session
// sends and leave the margin unverified against the path that consumes it.
// Sized at the ceiling: methodologyTopK entries, each a long title and a deep
// path.
func maxMethodologySection() string {
	var b strings.Builder
	for i := 0; i < methodologyTopK; i++ {
		fmt.Fprintf(&b, "• score=0.87  %s  (kb/meta/reasoning/%s/%08d.md)\n",
			strings.Repeat("long methodology title ", 8), strings.Repeat("deep-segment/", 4), i)
	}
	return b.String()
}

// sizedFacts builds facts of a given body size. Everything but the body is held
// at a realistic ceiling — a deep path, two domains, two entities — because
// those fields are what indentation expands hardest.
func sizedFacts(n, bodySize int) []factForLLM {
	facts := make([]factForLLM, 0, n)
	for i := 0; i < n; i++ {
		facts = append(facts, factForLLM{
			File:       fmt.Sprintf("kb/architecture/subsystem/area/fact-%05d.md", i),
			Title:      fmt.Sprintf("fact %05d", i),
			Body:       strings.Repeat("x", bodySize),
			Type:       "observation",
			Domain:     []string{"architecture", "test"},
			Entities:   []string{"alpha", "beta"},
			Confidence: 0.8,
			Sources:    1,
		})
	}
	return facts
}

// bodySizes spans the range of fact shapes a real corpus produces, and exists
// because the constant this test guards used to be calibrated on exactly one of
// them. Indentation cost is per JSON token, not per byte, so the delivered size
// of a full page RISES as facts get SMALLER — the previous 106% multiplier held
// at 2 KiB bodies and failed everywhere below: a full page delivered 33.5 KB at
// 1 KiB, 40.6 KB at 200 B, and 46.4 KB at 50 B, against a 32 KiB cap. Testing
// one body size is what let that ship.
var bodySizes = []int{50, 100, 200, 500, 1024, 2 * 1024, 4 * 1024}

// TestDistillWorkItem_EveryPageFitsDeliveredCap is the regression the incident
// needed. It takes the largest item the planner will emit (chunkFacts at
// maxItemBytes), renders it through the real prompt path, and measures what the
// MCP handler actually returns — a MarshalIndent of the whole ReviewResult
// (internal/mcp/review.go) — for EVERY page of it.
//
// Three things it deliberately does that its predecessor did not:
//
//   - It sweeps body sizes. The bug it now guards was invisible at 2 KiB.
//   - It assembles pages through reviewResultPage, so page/pages/more_available
//     /next/completion_token are present exactly as they ship. Hand-building a
//     ReviewItem left ~180 bytes of real payload out of a ~400-byte margin.
//   - It checks every page, not just the first. Page 1 carries the prompt, but
//     later pages carry more facts for it.
func TestDistillWorkItem_EveryPageFitsDeliveredCap(t *testing.T) {
	for _, bodySize := range bodySizes {
		t.Run(fmt.Sprintf("body=%d", bodySize), func(t *testing.T) {
			// Enough facts to overflow a whole ITEM, so chunkFacts closes one
			// and we measure the largest item a session can actually produce.
			facts := sizedFacts(3*maxItemBytes/(bodySize+200), bodySize)
			chunks := chunkFacts(facts, maxItemBytes)
			require.Greater(t, len(chunks), 1,
				"precondition: the corpus must be large enough that the chunker closes a full item")

			content, err := RenderDistillWorkItem(chunks[0], "kb", maxMethodologySection())
			require.NoError(t, err)

			res := &PipelineResult{
				SessionID: "00000000-0000-0000-0000-000000000000",
				Item: &PipelineItem{
					ID: 1, Type: "distill",
					Prompt: content.Prompt, ResponseSchema: content.ResponseSchema,
					Facts: content.Facts, FactsJSON: content.Facts,
				},
				Progress: &ReviewProgress{Completed: 3, Remaining: 17},
			}

			first, err := reviewResultPage(res, 1)
			require.NoError(t, err)
			require.NotEmpty(t, first.Item.Facts, "precondition: the item must carry its payload")

			for p := 1; p <= first.Item.Pages; p++ {
				out, perr := reviewResultPage(res, p)
				require.NoError(t, perr)

				// MarshalIndent, not Marshal: this is what ReviewHandler ships.
				delivered, merr := json.MarshalIndent(out, "", "  ")
				require.NoError(t, merr)

				var onPage []factForLLM
				require.NoError(t, json.Unmarshal(out.Item.Facts, &onPage))

				require.LessOrEqualf(t, len(delivered), maxDeliveredItemBytes,
					"body=%d: page %d/%d holds %d facts and delivers %d bytes, over the %d-byte cap; "+
						"the page budget bounds a different artifact than the one that ships",
					bodySize, p, first.Item.Pages, len(onPage), len(delivered), maxDeliveredItemBytes)
			}
		})
	}
}

// TestDeliveredPage_EnvelopeFitsItsReserve pins pageEnvelopeReserveBytes to the
// thing it reserves for.
//
// maxPageFactBytes is maxDeliveredItemBytes minus this reserve, so the whole
// guarantee rests on the non-facts part of a page — prompt, response schema,
// paging fields, envelope — actually fitting inside it. That part is NOT under
// the pager's control: distill's prompt grows with the methodology section, and
// either prompt file can be edited. Without this test the reserve goes stale
// silently and the page budget starts over-promising again.
func TestDeliveredPage_EnvelopeFitsItsReserve(t *testing.T) {
	one := sizedFacts(1, 16)

	for _, tc := range []struct {
		name    string
		content func() (*WorkItemContent, error)
	}{
		{"distill", func() (*WorkItemContent, error) {
			return RenderDistillWorkItem(one, "kb", maxMethodologySection())
		}},
		{"prune", func() (*WorkItemContent, error) { return RenderPruneWorkItem(one, "kb") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content, err := tc.content()
			require.NoError(t, err)

			res := &PipelineResult{
				SessionID: "00000000-0000-0000-0000-000000000000",
				Item: &PipelineItem{
					ID: 999999, Type: tc.name,
					Prompt: content.Prompt, ResponseSchema: content.ResponseSchema,
					Facts: content.Facts, FactsJSON: content.Facts,
				},
				Progress: &ReviewProgress{Completed: 3, Remaining: 17},
			}
			out, err := reviewResultPage(res, 1)
			require.NoError(t, err)

			delivered, err := json.MarshalIndent(out, "", "  ")
			require.NoError(t, err)
			envelope := len(delivered) - deliveredFactsLen(out.Item.Facts)

			require.LessOrEqualf(t, envelope, pageEnvelopeReserveBytes,
				"%s: the non-facts part of a delivered page is %d bytes, over the %d reserved for it; "+
					"maxPageFactBytes is derived by subtracting that reserve, so it now over-promises",
				tc.name, envelope, pageEnvelopeReserveBytes)
		})
	}
}

// TestFactPages_SizeModelNeverUnderCounts pins the DIRECTION of packFactPages'
// estimate.
//
// It sizes a page as the sum of its facts' individually indented lengths, which
// over-counts the real array by a few bytes per join. Over-counting is what
// makes the budget safe — a page lands slightly under the cap, never over — so
// the property to guard is the inequality, not the exact arithmetic. If a
// future Go changes json.Indent's formatting such that the sum UNDER-counts,
// every page silently starts overshooting again.
func TestFactPages_SizeModelNeverUnderCounts(t *testing.T) {
	for _, bodySize := range bodySizes {
		for _, n := range []int{1, 2, 5, 50, 200} {
			facts := sizedFacts(n, bodySize)

			whole, err := json.Marshal(facts)
			require.NoError(t, err)
			actual := deliveredFactsLen(whole)

			estimate := 0
			for _, f := range facts {
				one, merr := json.Marshal([]factForLLM{f})
				require.NoError(t, merr)
				estimate += deliveredFactsLen(one)
			}

			require.GreaterOrEqualf(t, estimate, actual,
				"body=%d n=%d: the page-size estimate (%d) under-counts the delivered array (%d); "+
					"packFactPages would overfill every page",
				bodySize, n, estimate, actual)
		}
	}
}

// TestRenderDistillWorkItem_FactsAreStructuralNotSerializedIntoThePrompt pins
// the shape paging needs.
//
// Today the facts are a JSON array serialized INSIDE the prompt string, between
// the instruction preamble and the response template. Two consequences the
// incident report documented: the payload cannot be windowed without regex
// surgery on `prompt`, and the whole array arrives as a handful of enormous
// lines, so line-based readers cannot chunk it without truncating mid-fact.
//
// Carrying facts as their own field is also strictly cheaper on the wire —
// embedded in a string every quote is escaped, structurally none are — and it
// is what hypothesize already does (HypothesizeItem.Fact, internal/mcp/
// hypothesize.go). Review is the outlier.
func TestRenderDistillWorkItem_FactsAreStructuralNotSerializedIntoThePrompt(t *testing.T) {
	facts := []factForLLM{{
		File:       "kb/test/only.md",
		Title:      "the only fact",
		Body:       "SENTINEL-BODY-must-not-appear-inside-the-prompt",
		Type:       "observation",
		Domain:     []string{"test"},
		Confidence: 0.9,
		Sources:    1,
	}}

	content, err := RenderDistillWorkItem(facts, "kb", "")
	require.NoError(t, err)

	require.NotContains(t, content.Prompt, "SENTINEL-BODY-must-not-appear-inside-the-prompt",
		"fact bodies must not be serialized into the prompt string — paging cannot slice them there")

	require.NotEmpty(t, content.Facts,
		"the rendered item must carry its facts as a structured field")

	var round []factForLLM
	require.NoError(t, json.Unmarshal([]byte(content.Facts), &round),
		"the structured facts field must be a well-formed fact array")
	require.Len(t, round, 1)
	require.Equal(t, "SENTINEL-BODY-must-not-appear-inside-the-prompt", round[0].Body,
		"splitting facts out of the prompt must not drop or alter them")
}
