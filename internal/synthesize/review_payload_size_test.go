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

// renderMaxDistillChunk builds the worst case a session can actually produce:
// facts packed by the real chunker until it closes a chunk, then rendered
// through the real prompt path. Returns the rendered item and the chunk it came
// from.
func renderMaxDistillChunk(t *testing.T) (*WorkItemContent, []factForLLM) {
	t.Helper()

	// Bodies sized so many facts fit in one chunk, matching the real payloads
	// (15–29 facts, ~53 KB of body text between them).
	const (
		numFacts = 200
		bodySize = 2 * 1024
	)
	facts := make([]factForLLM, 0, numFacts)
	for i := 0; i < numFacts; i++ {
		facts = append(facts, factForLLM{
			File:       fmt.Sprintf("kb/test/fact-%03d.md", i),
			Title:      fmt.Sprintf("fact %03d", i),
			Body:       strings.Repeat("x", bodySize),
			Type:       "observation",
			Domain:     []string{"test"},
			Entities:   []string{"alpha", "beta"},
			Confidence: 0.8,
			Sources:    1,
		})
	}

	chunks := chunkFacts(facts, maxPageBytes)
	require.Greater(t, len(chunks), 1,
		"precondition: the corpus must be large enough that the chunker closes a full chunk")
	full := chunks[0]

	// Render with a methodology section, not an empty one. Production injects
	// distillMethodologySection() into this prompt; rendering without it would
	// measure a prompt smaller than any real session ever sends and leave the
	// margin unverified against the path that actually consumes it. Sized at
	// the ceiling: methodologyTopK entries, each a long title and a deep path.
	var methodology strings.Builder
	for i := 0; i < methodologyTopK; i++ {
		fmt.Fprintf(&methodology, "• score=0.87  %s  (kb/meta/reasoning/%s/%08d.md)\n",
			strings.Repeat("long methodology title ", 8), strings.Repeat("deep-segment/", 4), i)
	}

	content, err := RenderDistillWorkItem(full, "kb", methodology.String())
	require.NoError(t, err)
	return content, full
}

// TestDistillWorkItem_MaxChunkFitsDeliveredCap is the regression the incident
// needed. It measures what the MCP handler actually returns — a MarshalIndent
// of the whole ReviewResult (internal/mcp/review.go) — for the largest chunk
// the chunker will emit.
//
// The failure this catches: a chunk budget that is honoured exactly and still
// produces an undeliverable result, because the budget names a different
// artifact than the transport carries.
func TestDistillWorkItem_MaxChunkFitsDeliveredCap(t *testing.T) {
	content, facts := renderMaxDistillChunk(t)

	// Assembled exactly as reviewResult() does, Facts included. Omitting Facts
	// here would measure a result with no payload in it and pass trivially —
	// the payload is the entire thing under test.
	result := &ReviewResult{
		SessionID: "00000000-0000-0000-0000-000000000000",
		Item: &ReviewItem{
			ID:             1,
			Type:           "distill",
			Prompt:         content.Prompt,
			ResponseSchema: content.ResponseSchema,
			Facts:          json.RawMessage(content.Facts),
		},
		Progress: &ReviewProgress{Completed: 3, Remaining: 17},
	}
	require.NotEmpty(t, result.Item.Facts, "precondition: the item must carry its payload")

	// MarshalIndent, not Marshal: this is what ReviewHandler ships.
	delivered, err := json.MarshalIndent(result, "", "  ")
	require.NoError(t, err)

	require.LessOrEqualf(t, len(delivered), maxDeliveredItemBytes,
		"a max-size distill chunk (%d facts) delivers %d bytes, over the %d-byte cap; "+
			"the chunk budget bounds a smaller artifact than the one that ships",
		len(facts), len(delivered), maxDeliveredItemBytes)
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
