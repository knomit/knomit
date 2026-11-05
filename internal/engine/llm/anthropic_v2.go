package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

const (
	anthropicProvider             = "anthropic"
	defaultAnthropicContextWindow = 200000
	// anthropicMaxBreakpoints is the API's per-request cache_control limit.
	anthropicMaxBreakpoints = 4
)

// Effort levels (output_config.effort). Which levels a model accepts, and
// which is its default, is per model; see ModelProfile.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
	EffortXHigh  = "xhigh"
	EffortMax    = "max"
)

// Thinking display values (thinking.display).
const (
	DisplaySummarized = "summarized"
	DisplayOmitted    = "omitted"
)

// ModelProfile is the per-model request defaults the caller supplies (from
// the provider config, PR5). Zero fields take DefaultModelProfile's values.
//
// Thinking is always on: the adapter sends thinking {type: adaptive} on
// every request and never "disabled" or budget_tokens, which current models
// (Haiku 5.5, Sonnet 5.5, Opus 5.5) reject or restrict. Effort is THE control
// for how much the model thinks, and it is always sent explicitly, so a
// request never depends on a model's own default (medium on Haiku 5.5 and
// Opus 5.5, high on Sonnet 5.5). Keep one Effort per conversation: changing
// the top-level effort between requests invalidates the prompt cache.
//
// MaxOutput is max_tokens. Thinking counts against it, so it must leave room
// for the reasoning as well as the reply.
type ModelProfile struct {
	Effort          string // EffortLow … EffortMax
	MaxOutput       int    // max_tokens
	ThinkingDisplay string // DisplaySummarized or DisplayOmitted
}

// DefaultModelProfile is the chat default: medium effort, a generous
// max_tokens (64000, the current guidance for long streamed turns), and
// summarized thinking so a chat UI is not a silent pause while the model
// thinks.
var DefaultModelProfile = ModelProfile{Effort: EffortMedium, MaxOutput: 64000, ThinkingDisplay: DisplaySummarized}

func (p ModelProfile) withDefaults() ModelProfile {
	if p.Effort == "" {
		p.Effort = DefaultModelProfile.Effort
	}
	if p.MaxOutput <= 0 {
		p.MaxOutput = DefaultModelProfile.MaxOutput
	}
	if p.ThinkingDisplay == "" {
		p.ThinkingDisplay = DefaultModelProfile.ThinkingDisplay
	}
	return p
}

func (p ModelProfile) validate() error {
	switch p.Effort {
	case EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
	default:
		return fmt.Errorf("anthropic: unknown effort %q", p.Effort)
	}
	switch p.ThinkingDisplay {
	case DisplaySummarized, DisplayOmitted:
	default:
		return fmt.Errorf("anthropic: unknown thinking display %q", p.ThinkingDisplay)
	}
	return nil
}

// AnthropicOptions configures an AnthropicChat. ContextWindow defaults to
// 200000; Profile fields default per DefaultModelProfile. An empty APIKey
// leaves the SDK's own credential resolution (ANTHROPIC_API_KEY and friends)
// in charge; an empty BaseURL is the public API.
//
// The request shape is the same for every Anthropic model: no tool_choice
// (so never a forced one, a 400 on Sonnet 5.5 and Opus 5.5), no temperature,
// top_p or top_k, no budget_tokens, never thinking disabled.
type AnthropicOptions struct {
	APIKey, BaseURL string
	ContextWindow   int
	Profile         ModelProfile
}

func (o AnthropicOptions) withDefaults() AnthropicOptions {
	if o.ContextWindow <= 0 {
		o.ContextWindow = defaultAnthropicContextWindow
	}
	o.Profile = o.Profile.withDefaults()
	return o
}

// AnthropicChat is the v2 Anthropic Messages adapter: streaming, tools, and
// prompt caching at hook boundaries (see renderAnthropic).
type AnthropicChat struct {
	client anthropic.Client
	model  string
	o      AnthropicOptions
}

var _ ChatModel = (*AnthropicChat)(nil)

// NewAnthropicChat builds the adapter for model.
func NewAnthropicChat(model string, o AnthropicOptions) *AnthropicChat {
	o = o.withDefaults()
	var opts []option.RequestOption
	if o.APIKey != "" {
		opts = append(opts, option.WithAPIKey(o.APIKey))
	}
	if o.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(o.BaseURL))
	}
	return &AnthropicChat{client: anthropic.NewClient(opts...), model: model, o: o}
}

func (a *AnthropicChat) Provider() string { return anthropicProvider }
func (a *AnthropicChat) Model() string    { return a.model }
func (a *AnthropicChat) Caps() Caps {
	return Caps{Tools: true, MaxBreakpoints: anthropicMaxBreakpoints, ContextWindow: a.o.ContextWindow, MaxOutput: a.o.Profile.MaxOutput}
}

var (
	cacheTTL1h = anthropic.CacheControlEphemeralParam{TTL: anthropic.CacheControlEphemeralTTLTTL1h}
	cacheTTL5m = anthropic.CacheControlEphemeralParam{TTL: anthropic.CacheControlEphemeralTTLTTL5m}
)

// renderAnthropic lays a snapshot out as a Messages request. The API caches
// in the order tools → system → messages, so the hook order maps onto it
// directly:
//
//   - tools: hook 1, in snapshot (name) order. Each input_schema is the
//     canonical schema bytes verbatim.
//   - system: hook 2, hook 3, hook 4, hook 5 text blocks, in that order. The
//     last hook-3 block (or the last hook-2 block when hook 3 is empty)
//     carries cache_control ttl 1h: that one breakpoint caches tools + system
//     prompt + instructions, the prefix shared per permission set. The last
//     hook-4 block, when there is one, carries a second 1h breakpoint. Hook 5
//     gets none. It renders into system, so changing it mid-conversation
//     is an edit to history: it invalidates every later thinking block and
//     restarts the cache. See the append-only rule (ErrHistoryAppendOnly,
//     builder.go), which guards hook 6.
//   - messages: hook 6 then hook 7, consecutive same-role entries merged.
//     Every entry goes out unchanged, Anthropic reasoning included whichever
//     Anthropic model produced it (ReplayFor drops only other providers'
//     reasoning); the API drops the thinking blocks the target model cannot
//     read. The last cacheable block of the last message carries the rolling
//     ttl 5m breakpoint.
//
// That is at most three breakpoints (the API allows four), and every 1h
// breakpoint precedes the 5m one, which the API requires of mixed TTLs.
func renderAnthropic(s Snapshot, model string, o AnthropicOptions) anthropic.MessageNewParams {
	o = o.withDefaults()
	// SDK v1.26.0's ThinkingConfigAdaptiveParam has no display field, so it
	// goes in as the one extra field (one key: no map-order instability).
	adaptive := anthropic.ThinkingConfigAdaptiveParam{}
	adaptive.SetExtraFields(map[string]any{"display": o.Profile.ThinkingDisplay})
	p := anthropic.MessageNewParams{
		Model:        anthropic.Model(model),
		MaxTokens:    int64(o.Profile.MaxOutput),
		Thinking:     anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive},
		OutputConfig: anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(o.Profile.Effort)},
	}

	for _, it := range s.Hooks[HookTools] {
		t := anthropic.ToolParam{
			Name:        it.Tool.Name,
			InputSchema: param.Override[anthropic.ToolInputSchemaParam](it.Tool.Schema),
		}
		if it.Tool.Description != "" {
			t.Description = param.NewOpt(it.Tool.Description)
		}
		p.Tools = append(p.Tools, anthropic.ToolUnionParam{OfTool: &t})
	}

	sharedEnd, frameEnd := -1, -1
	for _, h := range []Hook{HookSystem, HookRepo, HookFrame, HookWorkingSet} {
		for _, it := range s.Hooks[h] {
			if it.Text == "" {
				continue // the API rejects empty text blocks
			}
			p.System = append(p.System, anthropic.TextBlockParam{Text: it.Text})
			switch h {
			case HookSystem, HookRepo:
				sharedEnd = len(p.System) - 1
			case HookFrame:
				frameEnd = len(p.System) - 1
			}
		}
	}
	if sharedEnd >= 0 {
		p.System[sharedEnd].CacheControl = cacheTTL1h
	}
	if frameEnd >= 0 {
		p.System[frameEnd].CacheControl = cacheTTL1h
	}

	for _, h := range []Hook{HookConversation, HookTurn} {
		for _, it := range s.Hooks[h] {
			role, blocks := anthropicItem(it)
			if len(blocks) == 0 {
				continue
			}
			if n := len(p.Messages); n > 0 && p.Messages[n-1].Role == role {
				p.Messages[n-1].Content = append(p.Messages[n-1].Content, blocks...)
				continue
			}
			p.Messages = append(p.Messages, anthropic.MessageParam{Role: role, Content: blocks})
		}
	}
	if n := len(p.Messages); n > 0 {
		last := p.Messages[n-1].Content
		for i := len(last) - 1; i >= 0; i-- {
			if setCacheControl(&last[i], cacheTTL5m) {
				break
			}
		}
	}
	return p
}

// anthropicItem converts one hook-6/7 item. A text item is a user message.
func anthropicItem(it Item) (anthropic.MessageParamRole, []anthropic.ContentBlockParamUnion) {
	if it.Entry == nil {
		if it.Text == "" {
			return anthropic.MessageParamRoleUser, nil
		}
		return anthropic.MessageParamRoleUser, []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(it.Text)}
	}
	e := it.Entry.ReplayFor(anthropicProvider)
	role := anthropic.MessageParamRoleUser
	if e.Role == RoleAssistant {
		role = anthropic.MessageParamRoleAssistant
	}
	var out []anthropic.ContentBlockParamUnion
	for _, b := range e.Blocks {
		switch b.Kind {
		case BlockText:
			if b.Text != "" {
				out = append(out, anthropic.NewTextBlock(b.Text))
			}
		case BlockToolUse:
			args := b.ToolUse.Args
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			out = append(out, anthropic.ContentBlockParamUnion{OfToolUse: &anthropic.ToolUseBlockParam{
				ID: b.ToolUse.ID, Name: b.ToolUse.Name, Input: args,
			}})
		case BlockToolResult:
			r := anthropic.ToolResultBlockParam{
				ToolUseID: b.ToolResult.ToolUseID,
				Content:   []anthropic.ToolResultBlockParamContentUnion{{OfText: &anthropic.TextBlockParam{Text: b.ToolResult.Content}}},
			}
			if b.ToolResult.IsError {
				r.IsError = param.NewOpt(true)
			}
			out = append(out, anthropic.ContentBlockParamUnion{OfToolResult: &r})
		case BlockReasoning:
			if blk, ok := anthropicReasoning(b.Reasoning); ok {
				out = append(out, blk)
			}
		}
	}
	return role, out
}

// anthropicNative is the stored Native form of an Anthropic reasoning block.
// An empty thinking string (display "omitted") is a real block and is kept:
// it is replayed as "thinking":"" with its signature.
type anthropicNative struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

func anthropicReasoning(r *Reasoning) (anthropic.ContentBlockParamUnion, bool) {
	var n anthropicNative
	if r == nil || json.Unmarshal(r.Native, &n) != nil {
		return anthropic.ContentBlockParamUnion{}, false
	}
	switch n.Type {
	case "thinking":
		return anthropic.ContentBlockParamUnion{OfThinking: &anthropic.ThinkingBlockParam{Thinking: n.Thinking, Signature: n.Signature}}, true
	case "redacted_thinking":
		return anthropic.ContentBlockParamUnion{OfRedactedThinking: &anthropic.RedactedThinkingBlockParam{Data: n.Data}}, true
	}
	return anthropic.ContentBlockParamUnion{}, false
}

// setCacheControl marks b; thinking blocks cannot carry cache_control, so it
// reports false for them and the caller moves to the previous block.
func setCacheControl(b *anthropic.ContentBlockParamUnion, cc anthropic.CacheControlEphemeralParam) bool {
	switch {
	case b.OfText != nil:
		b.OfText.CacheControl = cc
	case b.OfToolUse != nil:
		b.OfToolUse.CacheControl = cc
	case b.OfToolResult != nil:
		b.OfToolResult.CacheControl = cc
	default:
		return false
	}
	return true
}

// pendingBlock accumulates one streamed content block by index.
type pendingBlock struct {
	typ                       string
	text, args, thinking, sig strings.Builder
	id, name, data            string
}

// Stream sends the rendered snapshot as a streaming Messages request.
//
// Every stream event calls o.Progress, whether or not it reaches the sink.
//
// Usage: message_start carries input, cache_read_input_tokens and
// cache_creation_input_tokens (and a first output count); message_delta
// carries the CUMULATIVE counts, output always and, on current API versions,
// the input and cache counts too. Each field is therefore the larger of the
// two. The SDK's own Message.Accumulate copies only output_tokens from
// message_delta, which is why this does not use it for usage.
//
// Stop reasons: max_tokens and refusal return a *StopError holding the
// partial Response (and no EventDone); every other reason, pause_turn
// included, returns the Response.
func (a *AnthropicChat) Stream(ctx context.Context, s Snapshot, o StreamOptions, sink func(Event)) (*Response, error) {
	if err := a.o.Profile.validate(); err != nil {
		return nil, err
	}
	params := renderAnthropic(s, a.model, a.o)
	if o.MaxOutput > 0 {
		params.MaxTokens = int64(o.MaxOutput)
	}
	if sink == nil {
		sink = func(Event) {}
	}

	stream := a.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	resp := &Response{}
	var refusalCategory string
	open := map[int64]*pendingBlock{}
	for stream.Next() {
		ev := stream.Current()
		o.progress()
		switch ev.Type {
		case "message_start":
			u := ev.Message.Usage
			// cache_creation splits the write by TTL; only message_start
			// carries it (writes happen while the prompt is read).
			resp.Usage = Usage{
				Input: u.InputTokens, CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens, Output: u.OutputTokens,
				CacheWrite5m: u.CacheCreation.Ephemeral5mInputTokens, CacheWrite1h: u.CacheCreation.Ephemeral1hInputTokens,
			}
		case "content_block_start":
			cb := ev.ContentBlock
			pb := &pendingBlock{typ: cb.Type, id: cb.ID, name: cb.Name, data: cb.Data}
			pb.thinking.WriteString(cb.Thinking)
			pb.sig.WriteString(cb.Signature)
			if cb.Text != "" {
				pb.text.WriteString(cb.Text)
				sink(Event{Kind: EventTextDelta, Text: cb.Text})
			}
			open[ev.Index] = pb
		case "content_block_delta":
			pb := open[ev.Index]
			if pb == nil {
				return nil, fmt.Errorf("anthropic: delta for unopened content block %d", ev.Index)
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					pb.text.WriteString(ev.Delta.Text)
					sink(Event{Kind: EventTextDelta, Text: ev.Delta.Text})
				}
			case "input_json_delta":
				pb.args.WriteString(ev.Delta.PartialJSON)
			case "thinking_delta":
				pb.thinking.WriteString(ev.Delta.Thinking)
			case "signature_delta":
				pb.sig.WriteString(ev.Delta.Signature)
			}
		case "content_block_stop":
			pb := open[ev.Index]
			delete(open, ev.Index)
			if pb == nil {
				continue
			}
			if b, ok := a.finishBlock(pb, o, sink); ok {
				resp.Blocks = append(resp.Blocks, b)
			}
		case "message_delta":
			resp.StopReason = string(ev.Delta.StopReason)
			if resp.StopReason == StopRefusal {
				refusalCategory = stopDetailsCategory(ev.RawJSON())
			}
			u := ev.Usage
			resp.Usage.Input = max(resp.Usage.Input, u.InputTokens)
			resp.Usage.CacheRead = max(resp.Usage.CacheRead, u.CacheReadInputTokens)
			resp.Usage.CacheWrite = max(resp.Usage.CacheWrite, u.CacheCreationInputTokens)
			resp.Usage.Output = max(resp.Usage.Output, u.OutputTokens)
		}
	}
	if err := stream.Err(); err != nil {
		err = redactKey(err, a.o.APIKey)
		if anthropicPromptTooLong(err) {
			return nil, fmt.Errorf("anthropic: %w: %w", err, ErrContextFull)
		}
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	switch resp.StopReason {
	case StopMaxTokens, StopRefusal, StopContextWindowExceeded:
		return nil, &StopError{Reason: resp.StopReason, Category: refusalCategory, Response: resp}
	}
	sink(Event{Kind: EventDone})
	return resp, nil
}

// anthropicPromptTooLong recognises the API's context-overflow error: HTTP 400
// invalid_request_error "prompt is too long: N tokens > M maximum", returned on
// every model when the input alone exceeds the window. (When input plus
// max_tokens exceeds it the API accepts the request on current models and the
// reply ends with stop_reason model_context_window_exceeded instead.)
func anthropicPromptTooLong(err error) bool {
	var apiErr *anthropic.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(apiErr.Error()), "prompt is too long")
}

// stopDetailsCategory reads delta.stop_details.category from a raw
// message_delta event. SDK v1.26.0 has no stop_details field; the category is
// informational (branch on stop_reason, never on it), so a missing or
// malformed value is "".
func stopDetailsCategory(raw string) string {
	var ev struct {
		Delta struct {
			StopDetails *struct {
				Category *string `json:"category"`
			} `json:"stop_details"`
		} `json:"delta"`
	}
	if json.Unmarshal([]byte(raw), &ev) != nil || ev.Delta.StopDetails == nil || ev.Delta.StopDetails.Category == nil {
		return ""
	}
	return *ev.Delta.StopDetails.Category
}

// finishBlock turns a completed stream block into a neutral Block, emitting
// the tool_use or reasoning event for it. The provider's tool_use id is
// replaced by a knomit id. Tool arguments that are not valid JSON are kept in
// RawArgs with Args {} (ToolUse.ArgsErr), so no invalid JSON ever enters the
// neutral history, where it would fail CanonicalPrefix and rendering.
func (a *AnthropicChat) finishBlock(pb *pendingBlock, o StreamOptions, sink func(Event)) (Block, bool) {
	switch pb.typ {
	case "text":
		if pb.text.Len() == 0 {
			return Block{}, false
		}
		return Block{Kind: BlockText, Text: pb.text.String()}, true
	case "tool_use":
		tu := &ToolUse{ID: o.newID(), Name: pb.name, Args: json.RawMessage(`{}`)}
		if raw := pb.args.String(); raw != "" {
			if json.Valid([]byte(raw)) {
				tu.Args = json.RawMessage(raw)
			} else {
				tu.RawArgs = raw
			}
		}
		sink(Event{Kind: EventToolUse, ToolUse: tu})
		return Block{Kind: BlockToolUse, ToolUse: tu}, true
	case "thinking", "redacted_thinking":
		n := anthropicNative{Type: pb.typ, Thinking: pb.thinking.String(), Signature: pb.sig.String(), Data: pb.data}
		raw, _ := json.Marshal(n)
		r := &Reasoning{Provider: anthropicProvider, Model: a.model, Native: raw}
		sink(Event{Kind: EventReasoning, Reasoning: r})
		return Block{Kind: BlockReasoning, Reasoning: r}, true
	}
	return Block{}, false
}

// AnthropicPrefixJSON is the cached part of the Anthropic request for s: the
// "tools" and "system" fields exactly as renderAnthropic marshals them, as
// {"system":...,"tools":...}. It exists so the byte-identity tests outside
// this package compare the bytes the API actually receives.
func AnthropicPrefixJSON(s Snapshot, model string) ([]byte, error) {
	raw, err := json.Marshal(renderAnthropic(s, model, AnthropicOptions{}))
	if err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	return encodeCanonical(struct {
		System json.RawMessage `json:"system"`
		Tools  json.RawMessage `json:"tools"`
	}{all["system"], all["tools"]})
}
