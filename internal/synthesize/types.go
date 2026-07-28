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

// maxDeliveredItemBytes bounds ONE delivered work item — the whole marshalled
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

// renderOverheadFixedBytes and renderOverheadPercent convert the delivered
// budget above into a budget on the compact fact JSON the chunker can actually
// measure. Both are measured, not guessed, from the rejected payloads: the
// prompt preamble, response schema and envelope keys are a fixed cost, and
// indentation applied by the delivering MarshalIndent is a proportional one.
//
// Keeping the conversion explicit is what stops the two budgets collapsing back
// into one number. TestDistillWorkItem_MaxChunkFitsDeliveredCap renders a
// full-size chunk and checks the real delivered result, so if the preamble or
// schema grows, the test fails rather than the constant silently going stale.
const (
	renderOverheadFixedBytes = 6 * 1024
	renderOverheadPercent    = 106
)

// maxDistillChunkBytes bounds the compact fact JSON of a single distill work
// item, derived so that the rendered item fits maxDeliveredItemBytes.
//
// Deliberately a const rather than a config knob: there is no evidence yet that
// anyone needs to tune this per-repo, and promoting it into the [synthesize]
// config section later is a one-line change. Adding the knob now would mean
// shipping a tunable nobody has a reason to turn.
//
// Distill only — prune clusters are NOT chunked; see StartSession.
const maxDistillChunkBytes = (maxDeliveredItemBytes - renderOverheadFixedBytes) * 100 / renderOverheadPercent

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
	// Facts is the item's payload, carried as structural JSON rather than
	// serialized into Prompt. RawMessage and not string on purpose: as a
	// string every quote in the payload is escaped a second time on the wire,
	// which is pure cost on the exact items that are already too large.
	// Mirrors HypothesizeItem.Fact, which has always shipped this way.
	Facts json.RawMessage `json:"facts,omitempty"`
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
