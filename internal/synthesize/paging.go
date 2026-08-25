// Package synthesize — work-item paging.
//
// A work item can hold more facts than one MCP tool result may carry. Paging is
// how the agent still sees all of them: the server serves the item in pages,
// the agent accumulates every page into its context, and answers ONCE at the
// end. The unit of the cap is a tool result, not a conversation, and this file
// is where those two stop being the same number.
//
// Three properties make this cheap and safe here:
//
//   - No new state. PipelineWorkItem.FactsJSON already holds the whole payload,
//     so a page is a deterministic slice of an existing column. Nothing is
//     written, no column is added, and the same (item, page) always yields the
//     same bytes.
//   - No interaction with the claim protocol. Paging is a pure read that runs
//     entirely before the claim CAS, so
//     invariants/synthesize/work-item-claim-protocol is untouched: the CAS still
//     fires exactly once, on the final response.
//   - Idempotent under re-serve. An agent that dies mid-page simply starts over
//     when NextPipelineWorkItem hands it the still-unanswered item.
package synthesize

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
)

// deliveredIndentPrefix and deliveredIndentUnit reproduce the indentation the
// facts array receives inside a delivered result. internal/mcp/review.go ships
// json.MarshalIndent(result, "", "  ") and `facts` sits two objects deep
// (result → item → facts), so passing that depth as json.Indent's prefix
// renders an array byte-for-byte as it will appear on the wire.
const (
	deliveredIndentPrefix = "    "
	deliveredIndentUnit   = "  "
)

// deliveredFactsLen reports how many bytes a compact JSON payload occupies once
// the delivering MarshalIndent has expanded it at the depth `facts` sits at.
//
// This is the measurement the page budget is expressed in. Indentation cost is
// per JSON token — a newline plus indent for every field and every array
// element — so it cannot be predicted from the compact byte count, only from
// the payload's shape. Measuring it is cheap; guessing it was the bug.
//
// A payload json.Indent rejects is not a fact array and is never paged, so
// falling back to its compact length is the honest answer rather than a
// silently wrong one.
func deliveredFactsLen(compact []byte) int {
	var buf bytes.Buffer
	if err := json.Indent(&buf, compact, deliveredIndentPrefix, deliveredIndentUnit); err != nil {
		return len(compact)
	}
	return buf.Len()
}

// packFactPages greedily fills pages with facts, sizing each candidate page by
// what it will DELIVER rather than by its compact bytes.
//
// A page's delivered length is estimated as the sum of its facts' individually
// indented lengths. That over-counts a real page by a small fixed amount per
// fact — joining two elements is shorter than the two singletons — and
// over-counting is the safe direction for a budget: a page comes out slightly
// under the cap, never over. TestFactPages_SizeModelNeverUnderCounts pins the
// direction so a future change to Go's formatting cannot invert it.
//
// A single fact larger than the budget still gets its own page, since it cannot
// be split; the bound is best-effort for a pathological fact exactly as
// chunkFacts is for the item budget.
func packFactPages(facts []factForLLM, maxBytes int) [][]factForLLM {
	var pages [][]factForLLM
	var current []factForLLM
	currentSize := 0

	for _, f := range facts {
		one, err := json.Marshal([]factForLLM{f})
		if err != nil {
			continue
		}
		size := deliveredFactsLen(one)
		if currentSize+size > maxBytes && len(current) > 0 {
			pages = append(pages, current)
			current = nil
			currentSize = 0
		}
		current = append(current, f)
		currentSize += size
	}
	if len(current) > 0 {
		pages = append(pages, current)
	}
	return pages
}

// factPages splits a work item's stored fact array into the pages it will be
// delivered in. Returns one page per tool result, each already marshalled.
//
// A payload that is not a fact array yields a single page carrying it
// unchanged: reflect and discover interpolate their payloads into the prompt
// rather than shipping them beside it, so they are single-page by construction
// and must not be reshaped here.
func factPages(stepType, factsJSON string) ([]json.RawMessage, error) {
	// Only page the step types that actually page. Everything else ships
	// verbatim, as one page.
	//
	// This gate is load-bearing, not an optimisation. Unmarshalling into
	// []factForLLM SUCCEEDS for any JSON array of objects — Go ignores unknown
	// fields — so a payload of a different shape decodes to a slice of EMPTY
	// factForLLM structs, and re-marshalling them REPLACES the real payload
	// with blanks. The motif alias and define payloads are arrays, and both
	// were being destroyed on the way to the model; backfill survived only
	// because its payload happens to be an object, where the unmarshal fails
	// and the verbatim fall-through catches it.
	//
	// The failure is silent at every layer: Render returns the right payload,
	// the item stores the right payload, and only what the ENGINE serves is
	// wrong.
	if !pagedStepTypes[stepType] {
		return []json.RawMessage{json.RawMessage(factsJSON)}, nil
	}
	var facts []factForLLM
	if err := json.Unmarshal([]byte(factsJSON), &facts); err != nil {
		return []json.RawMessage{json.RawMessage(factsJSON)}, nil
	}
	if len(facts) == 0 {
		return []json.RawMessage{json.RawMessage(factsJSON)}, nil
	}

	chunks := packFactPages(facts, maxPageFactBytes)
	pages := make([]json.RawMessage, 0, len(chunks))
	for i, c := range chunks {
		b, err := json.Marshal(c)
		if err != nil {
			return nil, fmt.Errorf("marshal page %d: %w", i+1, err)
		}
		pages = append(pages, json.RawMessage(b))
	}
	return pages, nil
}

// pageCountFor reports how many pages an item will be served in.
func pageCountFor(stepType, factsJSON string) int {
	pages, err := factPages(stepType, factsJSON)
	if err != nil || len(pages) == 0 {
		return 1
	}
	return len(pages)
}

// completionTokenFor derives the proof that an agent reached an item's final
// page. It is a pure function of the item, so it needs no storage and cannot
// drift from what was served — and because it is only ever emitted ON the last
// page, holding it is equivalent to having received that page.
//
// Not a security boundary, and not meant to be: this is the same trust model as
// item_id, which the agent has always echoed back. It exists to make "I read
// the whole item" checkable instead of merely requested — the distinction that
// fef554d1 established when a response_schema's `required` list turned out to
// be inert because nothing probed for it.
func completionTokenFor(itemID int64, factsJSON string) string {
	h := sha256.Sum256([]byte(strconv.FormatInt(itemID, 10) + ":" + factsJSON))
	return hex.EncodeToString(h[:])[:16]
}

// requireCompletionToken enforces the accumulate-then-respond contract for a
// multi-page item.
//
// Called before Decode and therefore before the claim, so a rejected answer
// leaves the item fully retryable — the agent pages properly and resubmits.
// Single-page items are exempt: the requirement must track the actual need, or
// every existing caller breaks for no gain.
//
// The error names the token, the page count, and how to obtain it, because the
// agent that hits this has already produced a synthesis it is about to lose.
func requireCompletionToken(stepType string, itemID int64, factsJSON, supplied string) error {
	pages := pageCountFor(stepType, factsJSON)
	if pages <= 1 {
		return nil
	}
	want := completionTokenFor(itemID, factsJSON)
	if supplied == want {
		return nil
	}
	if supplied == "" {
		return fmt.Errorf("work item %d spans %d pages and no completion_token was supplied; "+
			"read every page (call knomit_review with page=1..%d until more_available is false), "+
			"then resubmit with the completion_token from the final page",
			itemID, pages, pages)
	}
	return fmt.Errorf("work item %d spans %d pages and the supplied completion_token is not valid for it; "+
		"re-read the final page (page=%d) and echo the completion_token it returns",
		itemID, pages, pages)
}
