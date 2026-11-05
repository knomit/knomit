package llm

import (
	"encoding/json"
	"errors"
	"fmt"
)

// The neutral history: knomit's own, provider-independent record of a
// conversation. Each adapter converts it to its native shape on every request
// and the stored form never takes a provider's shape (no cache_control, no
// provider tool-call ids, no tool-name matching). That is what makes a
// mid-chat provider or model switch possible later.
//
// The history is APPEND-ONLY (user decision, 2026-11-04). Nothing edits,
// reorders, trims or removes an earlier entry, its tool results included:
// current Anthropic models bind each thinking block to the exact system,
// tools and earlier messages it was produced after, and on accounts created
// on or after 2026-08-31 replaying a block after such an edit is a 400. The
// Builder refuses Replace and Remove in HookConversation for that reason.
// Shrinking a long conversation is left to the provider's server-side context
// editing or compaction, which do not count as edits (deferred work).
//
// A provider with no prompt cache and no signatures to protect
// (Caps.StablePrefix false) is adaptive: the loop sends a PROJECTION of the
// stored history and tool list (ToolWindow.Project), a copy. The stored history
// is still whole and still append-only; only the view sent differs. Today the
// projection changes the tools block and shrinks each tool_result of an ended
// turn to a stub (projection.go); the running turn is sent whole.

// Role is the speaker of an Entry.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockKind names the one payload a Block carries.
type BlockKind string

const (
	BlockText       BlockKind = "text"
	BlockToolUse    BlockKind = "tool_use"
	BlockToolResult BlockKind = "tool_result"
	BlockReasoning  BlockKind = "reasoning"
)

// Entry is one turn of the conversation.
type Entry struct {
	ID        string     `json:"id"`
	Role      Role       `json:"role"`
	Blocks    []Block    `json:"blocks"`
	Usage     *Usage     `json:"usage,omitempty"`
	Approvals []Approval `json:"approvals,omitempty"`
}

// Block is one piece of an Entry; exactly the field named by Kind is set.
type Block struct {
	Kind       BlockKind   `json:"kind"`
	Text       string      `json:"text,omitempty"`
	ToolUse    *ToolUse    `json:"tool_use,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
	Reasoning  *Reasoning  `json:"reasoning,omitempty"`
}

// ToolUse is a model's tool call. ID is knomit-assigned ("tu_<n>"), never the
// provider's; adapters send it back to the provider as the call id. Args is
// always valid JSON. When the model streamed arguments that are not valid
// JSON, the adapter stores Args as {} and keeps the bytes it received in
// RawArgs, so the entry still marshals and renders; ArgsErr then reports the
// problem, and the caller answers the call with a tool error instead of
// running it.
type ToolUse struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Args    json.RawMessage `json:"args"`
	RawArgs string          `json:"raw_args,omitempty"`
}

// ErrInvalidToolArgs is what an *InvalidToolArgsError Is.
var ErrInvalidToolArgs = errors.New("llm: tool arguments are not valid JSON")

// InvalidToolArgsError names a tool call whose streamed arguments were not
// valid JSON. Raw is what the model sent.
type InvalidToolArgsError struct {
	ToolUseID, Name, Raw string
}

func (e *InvalidToolArgsError) Error() string {
	return fmt.Sprintf("llm: tool call %s (%s): arguments are not valid JSON: %q", e.ToolUseID, e.Name, e.Raw)
}

func (e *InvalidToolArgsError) Unwrap() error { return ErrInvalidToolArgs }

// ArgsErr is nil for a call whose arguments parsed, and an
// *InvalidToolArgsError otherwise.
func (t ToolUse) ArgsErr() error {
	if t.RawArgs == "" {
		return nil
	}
	return &InvalidToolArgsError{ToolUseID: t.ID, Name: t.Name, Raw: t.RawArgs}
}

// ToolResult answers the ToolUse whose ID it names. Elided marks the rendered
// view of a result whose stored content is whole: an adaptive provider's
// projection (ToolWindow.StubEnded) sends a stub of an ended turn's result and
// sets it. It is never set on a stored entry, so the history stays append-only
// and whole; a stub is not a stored result.
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
	Elided    bool   `json:"elided,omitempty"`
}

// Reasoning is a provider-bound reasoning artifact (an Anthropic thinking
// block with its signature, say). Native is the provider's own encoding.
// Model records which model produced it; it does not limit where it is
// replayed (see ReplayFor).
type Reasoning struct {
	Provider string          `json:"provider"`
	Model    string          `json:"model"`
	Native   json.RawMessage `json:"native"`
}

// Approval records how a tool call was decided: allow | decline |
// allow_session | auto.
type Approval struct {
	ToolUseID string `json:"tool_use_id"`
	Decision  string `json:"decision"`
}

// Usage is one response's token accounting, as reported by the provider.
// Input is uncached input; CacheRead and CacheWrite are the cached parts.
// PromptEval is a raw provider count where one exists (Ollama): ALL prompt
// tokens, cached ones included, so Input + CacheRead == PromptEval there.
// Estimated is true when any field was computed locally rather than reported;
// EstimatedFields then names those fields by their JSON name (e.g.
// "cache_read"), and every field not named is the provider's own count
// (user decision, 2026-11-04).
type Usage struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	// CacheWrite5m and CacheWrite1h split CacheWrite by the TTL it used, when
	// the provider says (Anthropic's cache_creation). They sum to CacheWrite
	// or to less of it; the rest is unsplit. Ollama has no cache writes.
	CacheWrite5m int64 `json:"cache_write_5m,omitempty"`
	CacheWrite1h int64 `json:"cache_write_1h,omitempty"`
	Output       int64 `json:"output"`
	PromptEval   int64 `json:"prompt_eval,omitempty"`
	Estimated    bool  `json:"estimated,omitempty"`

	EstimatedFields []string `json:"estimated_fields,omitempty"`
}

// ReplayFor returns e as it may be sent to provider: reasoning produced by a
// DIFFERENT provider is dropped, everything else is kept unchanged and in
// order. The receiver is not modified.
//
// Reasoning from the same provider is replayed to every model of it, whichever
// model produced it (user decision, 2026-11-04). Anthropic's API reads the
// thinking blocks the target model can read and drops the rest itself,
// silently and unbilled; stripping them here would lose reasoning the target
// could have read (Haiku 5.5 → Sonnet 5.5 → Opus 5.5 read upward) and edit the
// history the later blocks are bound to.
func (e Entry) ReplayFor(provider string) Entry {
	out := e
	out.Blocks = make([]Block, 0, len(e.Blocks))
	for _, b := range e.Blocks {
		if b.Kind == BlockReasoning && (b.Reasoning == nil || b.Reasoning.Provider != provider) {
			continue
		}
		out.Blocks = append(out.Blocks, b)
	}
	return out
}
