package llm

import "encoding/json"

// The neutral history: knomit's own, provider-independent record of a
// conversation. Each adapter converts it to its native shape on every request
// and the stored form never takes a provider's shape (no cache_control, no
// provider tool-call ids, no tool-name matching). That is what makes a
// mid-chat provider or model switch possible later.

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
// provider's.
type ToolUse struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// ToolResult answers the ToolUse whose ID it names. Elided marks a result the
// trimmer replaced with a stub; the full result is kept outside the context.
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
	Elided    bool   `json:"elided,omitempty"`
}

// Reasoning is a provider-bound reasoning artifact (an Anthropic thinking
// block with its signature, say). Native is the provider's own encoding and is
// replayed only to the exact Provider+Model that produced it.
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
// PromptEval is a raw provider count where one exists (Ollama). Estimated
// marks numbers computed locally rather than reported.
type Usage struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	PromptEval int64 `json:"prompt_eval,omitempty"`
	Estimated  bool  `json:"estimated,omitempty"`
}

// ReplayFor returns e as it may be sent to provider/model: reasoning produced
// by any other provider or model is dropped, everything else is kept in
// order. The receiver is not modified.
func (e Entry) ReplayFor(provider, model string) Entry {
	out := e
	out.Blocks = make([]Block, 0, len(e.Blocks))
	for _, b := range e.Blocks {
		if b.Kind == BlockReasoning && (b.Reasoning == nil || b.Reasoning.Provider != provider || b.Reasoning.Model != model) {
			continue
		}
		out.Blocks = append(out.Blocks, b)
	}
	return out
}
