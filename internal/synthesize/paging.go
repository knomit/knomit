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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
)

// factPages splits a work item's stored fact array into the pages it will be
// delivered in. Returns one page per tool result, each already marshalled.
//
// A payload that is not a fact array yields a single page carrying it
// unchanged: prune, reflect and discover interpolate their payloads into the
// prompt rather than shipping them beside it, so they are single-page by
// construction and must not be reshaped here.
func factPages(factsJSON string) ([]json.RawMessage, error) {
	var facts []factForLLM
	if err := json.Unmarshal([]byte(factsJSON), &facts); err != nil {
		return []json.RawMessage{json.RawMessage(factsJSON)}, nil
	}
	if len(facts) == 0 {
		return []json.RawMessage{json.RawMessage(factsJSON)}, nil
	}

	chunks := chunkFacts(facts, maxPageBytes)
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
func pageCountFor(factsJSON string) int {
	pages, err := factPages(factsJSON)
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
func requireCompletionToken(itemID int64, factsJSON, supplied string) error {
	pages := pageCountFor(factsJSON)
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
