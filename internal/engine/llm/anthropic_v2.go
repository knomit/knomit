package llm

import (
	"context"
	"encoding/json"
	"fmt"
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

// AnthropicOptions configures an AnthropicChat. Zero values take defaults:
// ContextWindow 200000, MaxOutput 8192. ThinkingBudget > 0 sends
// thinking {type: enabled, budget_tokens}; 0 sends no thinking parameter at
// all, which leaves the model's own default in force (current models think
// adaptively by default, and reject budget_tokens). An empty APIKey leaves
// the SDK's own credential resolution (ANTHROPIC_API_KEY and friends) in
// charge; an empty BaseURL is the public API.
type AnthropicOptions struct {
	APIKey, BaseURL                          string
	ContextWindow, MaxOutput, ThinkingBudget int
}

func (o AnthropicOptions) withDefaults() AnthropicOptions {
	if o.ContextWindow <= 0 {
		o.ContextWindow = defaultAnthropicContextWindow
	}
	if o.MaxOutput <= 0 {
		o.MaxOutput = defaultMaxTokens
	}
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
	return Caps{Tools: true, MaxBreakpoints: anthropicMaxBreakpoints, ContextWindow: a.o.ContextWindow, MaxOutput: a.o.MaxOutput}
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
//     gets none: the working set changes within a conversation.
//   - messages: hook 6 then hook 7, each entry replayed for this model only
//     (ReplayFor), consecutive same-role entries merged. The last cacheable
//     block of the last message carries the rolling ttl 5m breakpoint.
//
// That is at most three breakpoints (the API allows four), and every 1h
// breakpoint precedes the 5m one, which the API requires of mixed TTLs.
func renderAnthropic(s Snapshot, model string, o AnthropicOptions) anthropic.MessageNewParams {
	o = o.withDefaults()
	p := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: int64(o.MaxOutput),
	}
	if o.ThinkingBudget > 0 {
		p.Thinking = anthropic.ThinkingConfigParamOfEnabled(int64(o.ThinkingBudget))
		// max_tokens must exceed the thinking budget, and the budget is not
		// meant to eat the answer's own allowance.
		if p.MaxTokens <= int64(o.ThinkingBudget) {
			p.MaxTokens = int64(o.ThinkingBudget) + int64(o.MaxOutput)
		}
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
			role, blocks := anthropicItem(it, model)
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
func anthropicItem(it Item, model string) (anthropic.MessageParamRole, []anthropic.ContentBlockParamUnion) {
	if it.Entry == nil {
		if it.Text == "" {
			return anthropic.MessageParamRoleUser, nil
		}
		return anthropic.MessageParamRoleUser, []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(it.Text)}
	}
	e := it.Entry.ReplayFor(anthropicProvider, model)
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
// Usage: message_start carries input, cache_read_input_tokens and
// cache_creation_input_tokens (and a first output count); message_delta
// carries the CUMULATIVE counts, output always and, on current API versions,
// the input and cache counts too. Each field is therefore the larger of the
// two. The SDK's own Message.Accumulate copies only output_tokens from
// message_delta, which is why this does not use it for usage.
func (a *AnthropicChat) Stream(ctx context.Context, s Snapshot, o StreamOptions, sink func(Event)) (*Response, error) {
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
	open := map[int64]*pendingBlock{}
	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		case "message_start":
			u := ev.Message.Usage
			resp.Usage = Usage{Input: u.InputTokens, CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens, Output: u.OutputTokens}
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
			u := ev.Usage
			resp.Usage.Input = max(resp.Usage.Input, u.InputTokens)
			resp.Usage.CacheRead = max(resp.Usage.CacheRead, u.CacheReadInputTokens)
			resp.Usage.CacheWrite = max(resp.Usage.CacheWrite, u.CacheCreationInputTokens)
			resp.Usage.Output = max(resp.Usage.Output, u.OutputTokens)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	sink(Event{Kind: EventDone})
	return resp, nil
}

// finishBlock turns a completed stream block into a neutral Block, emitting
// the tool_use or reasoning event for it. The provider's tool_use id is
// replaced by a knomit id.
func (a *AnthropicChat) finishBlock(pb *pendingBlock, o StreamOptions, sink func(Event)) (Block, bool) {
	switch pb.typ {
	case "text":
		if pb.text.Len() == 0 {
			return Block{}, false
		}
		return Block{Kind: BlockText, Text: pb.text.String()}, true
	case "tool_use":
		args := json.RawMessage(pb.args.String())
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		tu := &ToolUse{ID: o.newID(), Name: pb.name, Args: args}
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
