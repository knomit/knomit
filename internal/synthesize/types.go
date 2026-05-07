// Package synthesize — shared types and helpers used across synthesis steps
// (prune, distill, decision, dedup, validation, review).
package synthesize

import (
	"encoding/json"
	"fmt"
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
type factForLLM struct {
	File       string   `json:"path"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Type       string   `json:"type"`
	Domain     []string `json:"domain"`
	Entities   []string `json:"entities"`
	Confidence float64  `json:"confidence"`
	Sources    int      `json:"sources"`
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

// parsePruneResponse parses the LLM JSON response for a prune step.
func parsePruneResponse(text string) (PruneResult, error) {
	raw := extractJSON(text)
	var result PruneResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return PruneResult{}, fmt.Errorf("parsePruneResponse: %w (raw: %.200s)", err, raw)
	}
	return result, nil
}

// chunkFacts splits facts into groups where each group's JSON is ≤ maxBytes.
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
	Type           string `json:"type"` // "prune", "distill", or "reflect"
	Prompt         string `json:"prompt"`
	ResponseSchema string `json:"response_schema"`
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
	Reasoning string             `json:"reasoning"`
	Reinforce []ReinforceEntry   `json:"reinforce"`
	Propose   []ProposeEntry     `json:"propose"`
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
