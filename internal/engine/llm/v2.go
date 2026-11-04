package llm

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

// This file is the v2 model interface: streaming, tool-calling, cache-aware.
// It coexists with the v1 LLMAdapter/Complete path until that is deleted; the
// v2 names (ChatModel, Entry, Block) are chosen so the two never collide.
//
// A request is never a flat message list. It is a Snapshot of the context
// Builder's fixed hooks (builder.go), which each adapter renders into its own
// native request, placing cache breakpoints at hook boundaries. The
// conversation is kept in the provider-neutral form in history.go and is
// converted on every request; it never takes a provider's native shape.

// ChatModel is one provider model behind the v2 interface.
type ChatModel interface {
	Provider() string
	Model() string
	Caps() Caps
	// Stream renders s for this provider, sends it, and calls sink
	// synchronously, on the calling goroutine, for each event as it arrives;
	// sink may be nil.
	// The returned Response holds the complete assistant blocks and the usage
	// the provider reported.
	Stream(ctx context.Context, s Snapshot, o StreamOptions, sink func(Event)) (*Response, error)
}

// Caps describes what a model can do, for the loop and the trimmer.
type Caps struct {
	Tools          bool // can call tools
	MaxBreakpoints int  // cache breakpoints the provider accepts per request (0: none)
	ContextWindow  int  // input tokens
	MaxOutput      int  // output tokens requested per response
}

// StreamOptions are per-request knobs that are not part of the context.
type StreamOptions struct {
	// MaxOutput overrides the model's default output budget when > 0.
	MaxOutput int
	// NewID assigns the knomit id of each tool_use the model emits. Nil uses a
	// process-wide counter (NextToolUseID). The provider's own id never enters
	// the neutral history.
	NewID func() string
	// Progress, when set, is called synchronously for EVERY stream event the
	// provider sends, including those that never reach the sink: thinking and
	// signature deltas, tool-argument (input_json) deltas, message_start and
	// message_delta. It is the liveness signal WrapChat's stall timer uses, so
	// a model that thinks for minutes before its first text is not taken for
	// a stalled stream. It carries no data and must not block.
	Progress func()
}

func (o StreamOptions) progress() {
	if o.Progress != nil {
		o.Progress()
	}
}

// EventKind names a streamed event.
type EventKind string

const (
	EventTextDelta EventKind = "text_delta" // Text holds the delta
	EventToolUse   EventKind = "tool_use"   // ToolUse is complete (args fully streamed)
	EventReasoning EventKind = "reasoning"  // Reasoning is complete (signature included)
	EventDone      EventKind = "done"       // the response is complete
)

// Event is one streamed item.
type Event struct {
	Kind      EventKind
	Text      string
	ToolUse   *ToolUse
	Reasoning *Reasoning
}

// Response is one complete model response. StopReason is the provider's
// own value; the Stop* constants name the ones the loop distinguishes.
type Response struct {
	Blocks     []Block
	StopReason string
	Usage      Usage
}

// Stop reasons a Response can carry. max_tokens and refusal never come back
// as a plain Response: the adapter returns a *StopError for them instead (see
// there), so a cut-off or declined answer cannot pass for a finished one.
const (
	StopEndTurn   = "end_turn"
	StopToolUse   = "tool_use"
	StopSequence  = "stop_sequence"
	StopMaxTokens = "max_tokens"
	StopRefusal   = "refusal"
	// StopPauseTurn is a server-side tool loop that paused. It is returned as
	// a normal Response; the caller continues by sending the conversation
	// again with this response appended, never with a new user message.
	// knomit declares no server-side tools, so it is not expected in chat.
	StopPauseTurn = "pause_turn"
)

var (
	// ErrTruncated: the response stopped at max_tokens. Thinking counts
	// against max_tokens, so this can happen before any text.
	ErrTruncated = errors.New("llm: the response hit the output token limit before it finished")
	// ErrContextFull: the prompt does not fit the model's context window and
	// the provider refused it instead of cutting history (Ollama with
	// truncate:false). Retrying the same request gets the same answer; the
	// caller must shrink the prompt.
	ErrContextFull = errors.New("llm: the prompt does not fit the context window")
	// ErrRefused: the provider's safety classifiers declined the request
	// (stop_reason "refusal"). Retrying the same request gets the same answer.
	ErrRefused = errors.New("llm: the model declined this request")
)

// StopError is a response that ended without a usable answer: max_tokens
// (Is ErrTruncated) or refusal (Is ErrRefused). Response holds whatever was
// streamed before the stop, usage included, so the caller can show the
// partial text and account for the tokens. It is not retryable.
type StopError struct {
	Reason   string    // StopMaxTokens or StopRefusal
	Category string    // refusal only: the provider's stop_details category, "" if none
	Response *Response // never nil
}

func (e *StopError) Error() string {
	switch e.Reason {
	case StopRefusal:
		msg := ErrRefused.Error()
		if e.Category != "" {
			msg += " (category: " + e.Category + ")"
		}
		return msg
	case StopMaxTokens:
		return ErrTruncated.Error()
	}
	return "llm: response stopped: " + e.Reason
}

// Is makes errors.Is(err, ErrRefused) and errors.Is(err, ErrTruncated) work.
func (e *StopError) Is(target error) bool {
	return (target == ErrRefused && e.Reason == StopRefusal) || (target == ErrTruncated && e.Reason == StopMaxTokens)
}

// toolUseSeq backs NextToolUseID.
var toolUseSeq atomic.Int64

// NextToolUseID returns a process-unique knomit tool_use id, "tu_<n>".
func NextToolUseID() string { return fmt.Sprintf("tu_%d", toolUseSeq.Add(1)) }

func (o StreamOptions) newID() string {
	if o.NewID != nil {
		return o.NewID()
	}
	return NextToolUseID()
}
