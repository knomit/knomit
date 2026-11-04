package llm

import (
	"context"
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
	// synchronously, on the calling goroutine, for each event as it arrives.
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

// Response is one complete model response.
type Response struct {
	Blocks     []Block
	StopReason string
	Usage      Usage
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
