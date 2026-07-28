// Package synthesize — shared types and helpers used across synthesis steps
// (prune, distill, decision, dedup, validation, review).
package synthesize

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ProgressEvent carries progress information from the pipeline to the caller.
type ProgressEvent struct {
	Phase   string
	Message string
}

// flexStrings unmarshals from either a JSON string ("x") or array (["x","y"]).
// Small LLMs sometimes return a bare string where an array is expected.
type flexStrings []string

func (f *flexStrings) UnmarshalJSON(b []byte) error {
	// Try array first.
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		*f = arr
		return nil
	}
	// Fall back to single string.
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*f = []string{}
	} else {
		*f = []string{s}
	}
	return nil
}

// PruneDecision is the LLM's decision for a single fact.
type PruneDecision struct {
	Path       string  `json:"path"`
	Action     string  `json:"action"` // "keep" | "retract" | "update"
	Confidence float64 `json:"confidence,omitempty"`
}

// mergedFact is the embedded merged fact object from the LLM response.
type mergedFact struct {
	Path       string      `json:"path"`
	Title      string      `json:"title"`
	Body       string      `json:"body"`
	Type       string      `json:"type"`
	Domain     flexStrings `json:"domain"`
	Confidence float64     `json:"confidence"`
	Sources    int         `json:"sources"`
	Entities   flexStrings `json:"entities"`
	Refs       flexStrings `json:"refs"`
}

// MergeEntry groups source paths with the merged replacement fact.
type MergeEntry struct {
	Paths  []string   `json:"paths"`
	Merged mergedFact `json:"merged"`
}

// PruneResult is the full JSON response from the LLM prune call.
type PruneResult struct {
	Decisions []PruneDecision `json:"decisions"`
	Merges    []MergeEntry    `json:"merges"`
}

// factForLLM is the subset of fact fields sent to the LLM.
// The Path field uses json:"path" to match the output schema (PruneDecision.Path,
// distillFact.Path) so small models don't have to map between field names.
//
// Origin (Plan 01 fact.Origin) is included so the bridge-seeding pass can
// exclude facts that were themselves discovered — Plan 03 §7 idempotency: a
// discovered fact must never become the seed for another discovery, otherwise
// the engine drifts away from the human-authored substrate.
type factForLLM struct {
	File       string   `json:"path"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Type       string   `json:"type"`
	Domain     []string `json:"domain"`
	Entities   []string `json:"entities"`
	Confidence float64  `json:"confidence"`
	Sources    int      `json:"sources"`
	Origin     string   `json:"origin,omitempty"`
}

// extractJSON strips optional markdown code fences from LLM output.
func extractJSON(text string) string {
	text = strings.TrimSpace(text)
	// Strip <think>...</think> blocks (used by small models for chain-of-thought)
	if idx := strings.Index(text, "</think>"); idx >= 0 {
		text = strings.TrimSpace(text[idx+len("</think>"):])
	}
	// Strip ```json ... ``` or ``` ... ```
	if strings.HasPrefix(text, "```") {
		end := strings.LastIndex(text, "```")
		if end > 3 {
			inner := text[3:end]
			if idx := strings.IndexByte(inner, '\n'); idx >= 0 {
				inner = inner[idx+1:]
			}
			return strings.TrimSpace(inner)
		}
	}
	return text
}

// requireResponseKey enforces the `required` list that a work item's
// response_schema already advertises. encoding/json silently drops keys it
// does not recognise, so a response that carried its content under the wrong
// name — {"facts": [...]} against a distill item, say — unmarshalled into a
// zero-valued result, passed the path validators (which only inspect fields
// that were never populated), and applied as a no-op. The item advanced, no
// error surfaced, and the work was gone. Checking presence on the raw object
// is what turns that into a loud, retryable failure.
//
// Presence, not non-emptiness: an explicitly empty array is a legitimate
// "nothing to do" for every step, and rejecting it would wedge any session
// whose agent honestly had nothing to contribute.
//
// A raw payload that is not a JSON object at all yields nil here; the typed
// unmarshal the caller already ran is the authority on malformed input, and
// its message is the more useful one.
func requireResponseKey(raw, key string) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return nil
	}
	if _, ok := probe[key]; ok {
		return nil
	}
	if len(probe) == 0 {
		return fmt.Errorf("response object is empty: required key %q is missing", key)
	}
	got := make([]string, 0, len(probe))
	for k := range probe {
		got = append(got, k)
	}
	sort.Strings(got)
	return fmt.Errorf("response is missing required key %q; got: %s", key, strings.Join(got, ", "))
}

// parsePruneResponse parses the LLM JSON response for a prune step.
func parsePruneResponse(text string) (PruneResult, error) {
	raw := extractJSON(text)
	var result PruneResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return PruneResult{}, fmt.Errorf("parsePruneResponse: %w (raw: %.200s)", err, raw)
	}
	if err := requireResponseKey(raw, "decisions"); err != nil {
		return PruneResult{}, fmt.Errorf("parsePruneResponse: %w", err)
	}
	return result, nil
}

// maxDeliveredItemBytes bounds ONE delivered PAGE — the whole marshalled
// ReviewResult as internal/mcp/review.go returns it, not just its facts.
//
// This is the budget that actually binds, and naming it is the point. The
// previous single constant was derived from a MODEL CONTEXT rationale ("~16K
// tokens: comfortably inside every hosting model's context window"), which was
// true and irrelevant: the model's context never rejected anything. The
// CLIENT's MCP tool-result cap did. Seven real distill payloads (sessions
// 2b56c148-…, 284faa87-…) each sat just under the old 64 KiB fact budget, were
// delivered at 68.6–73.9 KB, and were spilled to disk unread.
//
// 32 KiB against a limit known only by observation — Claude Code defaults
// MAX_MCP_OUTPUT_TOKENS to 25,000 — leaves headroom for fact bodies that grow
// over time. Raising MAX_MCP_OUTPUT_TOKENS is a per-user harness setting, never
// something this package may assume.
const maxDeliveredItemBytes = 32 * 1024

// pageEnvelopeReserveBytes reserves everything on a delivered page that is not
// facts: the prompt, the response schema, the paging fields, and the
// ReviewResult envelope around them. Page 1 is the tight case — it alone
// carries the prompt and schema — so later pages simply run with more slack.
//
// Measured, and pinned: TestDeliveredPage_EnvelopeFitsItsReserve renders every
// paging step type with a full-size methodology section (the largest prompt the
// distill path can produce) and fails if the envelope outgrows this. The
// measured worst case at the time of writing is 5,893 bytes.
const pageEnvelopeReserveBytes = 8 * 1024

// maxPageFactBytes bounds the facts carried on ONE page, measured AS DELIVERED
// — after the indentation json.MarshalIndent applies in internal/mcp/review.go,
// not in the compact form the store holds.
//
// The unit is the whole point, and it is why this is a subtraction rather than
// a percentage of the compact size. Indentation adds a newline plus indent per
// JSON TOKEN, not per byte, so the expansion factor is a function of how MANY
// facts a page holds, not how large each one is. A ratio calibrated on the
// incident's ~2 KiB-body facts held only there: at 1 KiB bodies a full page
// delivered 33.5 KB, at 200 B bodies 40.6 KB, at 50 B bodies 46.4 KB — all
// against a 32 KiB cap, which is the original defect reproduced at a different
// fact size. deliveredFactsLen measures the artifact instead of predicting it.
const maxPageFactBytes = maxDeliveredItemBytes - pageEnvelopeReserveBytes

// maxItemBytes bounds what ONE work item holds — i.e. what the agent must
// accumulate across pages before answering. This is the second of the two
// budgets whose conflation was the original defect: maxPageFactBytes is a
// TRANSPORT limit (client-side, what fits in one tool result), maxItemBytes is
// a COGNITION limit (model-side, what can be reasoned over at once).
//
// Paging is what let them separate. Before it, an item shipped in a single
// response, so the transport limit doubled as the item limit and the only lever
// for an undeliverable item was to show the model less.
//
// It is a backstop, not a routine constraint. Since depth-0 distill groups by
// cluster (see distillGroups), item size is normally bounded by the community's
// own size; this catches the pathological mega-community, and — until every
// step type pages — keeps a first run over a large corpus from becoming one
// prompt. It is the knob most likely to want per-repo tuning, because unlike
// the transport limit it genuinely differs per deployment: this package
// accommodates small local models that cannot hold what a long-context hosted
// model can, however small each page is.
const maxItemBytes = 256 * 1024

// chunkFacts splits facts into groups where each group's JSON is ≤ maxBytes.
//
// A single fact larger than maxBytes still gets its own chunk (it cannot be
// split further), so the bound is best-effort for pathological facts.
func chunkFacts(facts []factForLLM, maxBytes int) [][]factForLLM {
	var chunks [][]factForLLM
	var current []factForLLM
	currentSize := 0

	for _, f := range facts {
		b, _ := json.Marshal(f)
		size := len(b)
		if currentSize+size > maxBytes && len(current) > 0 {
			chunks = append(chunks, current)
			current = nil
			currentSize = 0
		}
		current = append(current, f)
		currentSize += size
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}

// ReviewResult is returned from StartSession and ContinueSession.
type ReviewResult struct {
	SessionID string          `json:"session_id"`
	Item      *ReviewItem     `json:"item,omitempty"`
	Done      bool            `json:"done,omitempty"`
	Summary   *ReviewStats    `json:"summary,omitempty"`
	Progress  *ReviewProgress `json:"progress,omitempty"`
}

// ReviewItem describes a single work item for the hosting model.
type ReviewItem struct {
	// ID identifies this specific work item. Clients should echo it back as
	// `item_id` on the continue call: the queue can grow between render and
	// answer (applying a distill item enqueues RAPTOR follow-ups), so echoing
	// the id is what proves the response is for the item that was rendered.
	// Additive and optional — omitting it preserves the pre-D2 behaviour of
	// answering whatever item is current.
	ID             int64  `json:"id"`
	Type           string `json:"type"` // "prune", "distill", or "reflect"
	Prompt         string `json:"prompt"`
	ResponseSchema string `json:"response_schema"`
	// Facts is THIS PAGE of the item's payload, carried as structural JSON
	// rather than serialized into Prompt. RawMessage and not string on purpose:
	// as a string every quote in the payload is escaped a second time on the
	// wire, which is pure cost on the exact items that are already too large.
	// Mirrors HypothesizeItem.Fact, which has always shipped this way.
	Facts json.RawMessage `json:"facts,omitempty"`

	// Paging. An item whose facts exceed one tool result is served across
	// several; the agent accumulates every page and answers once at the end.
	// Page/Pages are 1-based. Prompt and ResponseSchema appear on page 1 only —
	// they are already in context by the time later pages arrive.
	Page  int `json:"page,omitempty"`
	Pages int `json:"pages,omitempty"`
	// MoreAvailable is true while pages remain. An answer submitted before it
	// goes false is rejected; see CompletionToken.
	//
	// Deliberately NOT omitempty, unlike every other optional field here. The
	// tool description, both paged prompt templates, and the `page` argument's
	// own documentation all tell the agent to keep paging "until more_available
	// is false" — and omitempty makes the final page carry no such field at all,
	// so the one condition the protocol is expressed in terms of never appears.
	// Absent is not false to a reader that was told to look for false. Twenty-two
	// bytes on the final page against a ~3 KB margin is not a trade worth making
	// on the single field the accumulate-then-respond contract turns on.
	MoreAvailable bool `json:"more_available"`
	// CompletionToken appears on the FINAL page only and must be echoed back
	// with the response. Emitting it solely on the last page is what makes it
	// proof the agent got there — the server can check that a multi-page item
	// was actually read, rather than asking politely and hoping.
	CompletionToken string `json:"completion_token,omitempty"`
	// Next is the human-readable instruction for what to do with this page,
	// carried beside the value it refers to so the agent does not have to
	// reconstruct the protocol from the tool description mid-item.
	Next string `json:"next,omitempty"`
}

// ReviewProgress tracks completed/remaining counts.
type ReviewProgress struct {
	Completed int `json:"completed"`
	Remaining int `json:"remaining"`
}

// DistillResult is the LLM JSON response for a distill step.
type DistillResult struct {
	Synthesize []distillFact `json:"synthesize"`
	Retract    []string      `json:"retract"`
}

// distillFact is a synthesized fact returned by the LLM in a distill step.
type distillFact struct {
	Path       string      `json:"path"`
	Title      string      `json:"title"`
	Body       string      `json:"body"`
	Type       string      `json:"type"`
	Domain     flexStrings `json:"domain"`
	Confidence float64     `json:"confidence"`
	Entities   flexStrings `json:"entities"`
	Refs       flexStrings `json:"refs"`
}

// parseDistillResponse parses the LLM JSON response for a distill step.
func parseDistillResponse(text string) (DistillResult, error) {
	raw := extractJSON(text)
	var result DistillResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return DistillResult{}, fmt.Errorf("parseDistillResponse: %w (raw: %.200s)", err, raw)
	}
	if err := requireResponseKey(raw, "synthesize"); err != nil {
		return DistillResult{}, fmt.Errorf("parseDistillResponse: %w", err)
	}
	return result, nil
}

// ReflectResult is the LLM JSON response for a reflect step. The contract
// is a forced choice between reinforcing existing methodologies (the
// default action) and proposing a new one (capped, dedup-checked, requires
// a novelty argument). The server applies both arms — reinforce appends
// the methodology fact's path to each cited transition fact's refs (so
// reinforcement count is "facts that ref it"); propose writes a new
// methodology fact via the standard fact-write path.
type ReflectResult struct {
	Reasoning string           `json:"reasoning"`
	Reinforce []ReinforceEntry `json:"reinforce"`
	Propose   []ProposeEntry   `json:"propose"`
}

// ReinforceEntry binds existing methodologies to the transitions they
// explain. transition_paths must be a non-empty subset of the session's
// recorded transitions; methodology_path must resolve to an existing
// type=methodology fact on the branch.
type ReinforceEntry struct {
	MethodologyPath string   `json:"methodology_path"`
	TransitionPaths []string `json:"transition_paths"`
	Rationale       string   `json:"rationale"`
}

// ProposeEntry describes a brand-new methodology fact the agent wants the
// server to write. NoveltyArgument is the agent's case for why no existing
// methodology already captures this lesson — required because the prompt
// already injects a candidate-existing-methodologies section, so the agent
// has been shown what's available before proposing.
//
// Type is implicitly "methodology" — the server stamps it; agent input is
// ignored here to keep the contract crisp.
type ProposeEntry struct {
	Title           string      `json:"title"`
	Body            string      `json:"body"`
	Domain          flexStrings `json:"domain"`
	Entities        flexStrings `json:"entities"`
	TopicPath       string      `json:"topic_path"`
	Confidence      float64     `json:"confidence"`
	Refs            flexStrings `json:"refs"`
	TransitionPaths []string    `json:"transition_paths"`
	NoveltyArgument string      `json:"novelty_argument"`
}

// parseReflectResponse parses the LLM JSON response for a reflect step.
// It accepts code-fenced output (extractJSON strips fences and <think>
// blocks) and returns a structurally-typed result; semantic validation
// (cap, transition-path scope, novelty, etc.) lives in
// validateReflectResponse and ApplyReflectDecisions.
func parseReflectResponse(text string) (ReflectResult, error) {
	raw := extractJSON(text)
	var result ReflectResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return ReflectResult{}, fmt.Errorf("parseReflectResponse: %w (raw: %.200s)", err, raw)
	}
	return result, nil
}
