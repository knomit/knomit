package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

// This file is the v2 Ollama adapter: hand-written HTTP against /api/chat
// (NDJSON streaming, tools), like the v1 OllamaAdapter in ollama.go, which
// stays untouched until the v1 path is deleted. The capability probe below is
// a COPY of the v1 /api/show probe's request, not a move: whoever deletes
// ollama.go must keep this one.
//
// Ollama has no cache breakpoints. Its prompt cache is the runner's KV cache
// for the loaded model, so the two things that make it hit are (1) a request
// prefix that is byte-stable up to the newest turn, which renderOllama
// preserves by putting everything shared into one system message and the
// tools in name order, and (2) keep_alive, which keeps the model loaded
// between turns. Cache use is therefore only visible through the prompt-eval
// count Ollama reports, which is what Usage.PromptEval and the estimated
// Usage.CacheRead are built from (see usageFromFinal).

const (
	ollamaProvider         = "ollama"
	defaultOllamaKeepAlive = "30m"
	// defaultOllamaNumCtx is sent on EVERY request. Without num_ctx Ollama
	// loads the model with its own (small) default context and silently drops
	// the oldest messages of a prompt that does not fit, with no error.
	defaultOllamaNumCtx = 32768
	defaultOllamaHost   = "http://localhost:11434"
	// ollamaUnlimited is num_predict -1: no output cap (see ollamaUncapped in
	// ollama.go for why a cap buys nothing on a local model).
	ollamaUnlimited = -1
)

// OllamaOptions configures an OllamaChat. Host defaults to OLLAMA_HOST, then
// http://localhost:11434. KeepAlive defaults to "30m" and NumCtx to 32768;
// NumCtx is also the context window the loop budgets against (Caps).
// MaxOutput <= 0 means no output cap.
type OllamaOptions struct {
	Host, KeepAlive   string
	NumCtx, MaxOutput int
}

func (o OllamaOptions) withDefaults() OllamaOptions {
	if o.Host == "" {
		o.Host = os.Getenv("OLLAMA_HOST")
	}
	if o.Host == "" {
		o.Host = defaultOllamaHost
	}
	if !strings.Contains(o.Host, "://") {
		o.Host = "http://" + o.Host
	}
	o.Host = strings.TrimRight(o.Host, "/")
	if o.KeepAlive == "" {
		o.KeepAlive = defaultOllamaKeepAlive
	}
	if o.NumCtx <= 0 {
		o.NumCtx = defaultOllamaNumCtx
	}
	return o
}

// OllamaChat is the v2 Ollama adapter.
type OllamaChat struct {
	client *http.Client
	model  string
	o      OllamaOptions

	tools  bool   // the model advertises the "tools" capability
	thinks bool   // the model advertises "thinking"
	reason string // why the model is unusable, "" when it is usable
}

var _ ChatModel = (*OllamaChat)(nil)

// NewOllamaChat builds the adapter and probes /api/show for the model's
// capabilities. The probe FAILS CLOSED (planner call P7): when it cannot be
// answered, or the model lacks "tools", the adapter is still returned but is
// unusable: Caps().Tools is false, UnusableReason says why, and Stream
// refuses. It never falls back to assuming tool support. An error is returned
// only for a request that cannot be built at all.
func NewOllamaChat(ctx context.Context, model string, o OllamaOptions) (*OllamaChat, error) {
	if model == "" {
		return nil, errors.New("ollama: no model name")
	}
	c := &OllamaChat{client: &http.Client{}, model: model, o: o.withDefaults()}
	caps, err := c.probe(ctx)
	switch {
	case err != nil:
		c.reason = "capability probe failed: " + err.Error()
	case !slices.Contains(caps, "tools"):
		c.reason = "model does not support tool calling (capabilities: " + strings.Join(caps, ", ") + ")"
	default:
		c.tools = true
	}
	c.thinks = slices.Contains(caps, "thinking")
	return c, nil
}

// probe asks /api/show for the model's capabilities.
func (c *OllamaChat) probe(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body, err := json.Marshal(map[string]string{"model": c.model})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.o.Host+"/api/show", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	var show struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&show); err != nil {
		return nil, fmt.Errorf("unreadable response: %w", err)
	}
	return show.Capabilities, nil
}

func (c *OllamaChat) Provider() string { return ollamaProvider }
func (c *OllamaChat) Model() string    { return c.model }
func (c *OllamaChat) Caps() Caps {
	return Caps{Tools: c.tools, MaxBreakpoints: 0, ContextWindow: c.o.NumCtx, MaxOutput: c.o.MaxOutput}
}

// UnusableReason is "" for a usable model, and otherwise says why the model
// cannot run a chat (the picker shows it).
func (c *OllamaChat) UnusableReason() string { return c.reason }

// ---- rendering ----

type ollamaToolFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ollamaToolV2 struct {
	Type     string       `json:"type"`
	Function ollamaToolFn `json:"function"`
}

type ollamaCallFn struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ollamaCall struct {
	Function ollamaCallFn `json:"function"`
}

// ollamaMsgV2 is one request message. Content has no omitempty: an assistant
// message that only calls tools is sent with "content":"" as in the API docs.
type ollamaMsgV2 struct {
	Role      string       `json:"role"`
	Content   string       `json:"content"`
	Thinking  string       `json:"thinking,omitempty"`
	ToolCalls []ollamaCall `json:"tool_calls,omitempty"`
	ToolName  string       `json:"tool_name,omitempty"`
}

type ollamaOptsV2 struct {
	NumCtx     int `json:"num_ctx"`
	NumPredict int `json:"num_predict"`
}

type ollamaChatRequestV2 struct {
	Model     string         `json:"model"`
	Messages  []ollamaMsgV2  `json:"messages"`
	Tools     []ollamaToolV2 `json:"tools,omitempty"`
	Think     *bool          `json:"think,omitempty"`
	Stream    bool           `json:"stream"`
	KeepAlive string         `json:"keep_alive"`
	Options   ollamaOptsV2   `json:"options"`
}

// renderOllama lays a snapshot out as an /api/chat request:
//
//   - one system message: hooks 2, 3, 4 and 5 (non-empty texts) joined by a
//     blank line, in that order; none when all four are empty.
//   - tools: hook 1 in snapshot (name) order, each schema verbatim.
//   - messages: hook 6 then hook 7. Tool results are {role:"tool",
//     tool_name, content}: Ollama matches a result to its call by tool NAME,
//     so the knomit id is mapped to the name of the tool_use it answers and
//     never sent. Reasoning from another provider is dropped (ReplayFor).
//   - keep_alive and options.num_ctx always set.
//
// It renders a tool result whose tool_use is not in the snapshot with an empty
// tool_name; Stream refuses such a snapshot before sending (renderOllamaChecked's
// error), so that form never reaches the wire.
func renderOllama(s Snapshot, model string, o OllamaOptions) ollamaChatRequestV2 {
	req, _ := renderOllamaChecked(s, model, o, false)
	return req
}

func renderOllamaChecked(s Snapshot, model string, o OllamaOptions, thinks bool) (ollamaChatRequestV2, error) {
	o = o.withDefaults()
	numPredict := ollamaUnlimited
	if o.MaxOutput > 0 {
		numPredict = o.MaxOutput
	}
	req := ollamaChatRequestV2{
		Model: model, Stream: true, KeepAlive: o.KeepAlive,
		Options: ollamaOptsV2{NumCtx: o.NumCtx, NumPredict: numPredict},
	}
	if thinks {
		t := true
		req.Think = &t
	}
	for _, it := range s.Hooks[HookTools] {
		req.Tools = append(req.Tools, ollamaToolV2{Type: "function", Function: ollamaToolFn{
			Name: it.Tool.Name, Description: it.Tool.Description, Parameters: it.Tool.Schema,
		}})
	}
	var sys []string
	for _, h := range []Hook{HookSystem, HookRepo, HookFrame, HookWorkingSet} {
		for _, it := range s.Hooks[h] {
			if it.Text != "" {
				sys = append(sys, it.Text)
			}
		}
	}
	if len(sys) > 0 {
		req.Messages = append(req.Messages, ollamaMsgV2{Role: "system", Content: strings.Join(sys, "\n\n")})
	}

	names := map[string]string{} // knomit tool_use id -> tool name
	var firstErr error
	for _, h := range []Hook{HookConversation, HookTurn} {
		for _, it := range s.Hooks[h] {
			if it.Entry == nil {
				if it.Text != "" {
					req.Messages = append(req.Messages, ollamaMsgV2{Role: "user", Content: it.Text})
				}
				continue
			}
			e := it.Entry.ReplayFor(ollamaProvider)
			if e.Role == RoleAssistant {
				req.Messages = append(req.Messages, ollamaAssistant(e, names))
				continue
			}
			var text []string
			for _, b := range e.Blocks {
				switch b.Kind {
				case BlockText:
					if b.Text != "" {
						text = append(text, b.Text)
					}
				case BlockToolResult:
					name, ok := names[b.ToolResult.ToolUseID]
					if !ok && firstErr == nil {
						firstErr = fmt.Errorf("ollama: tool result %q answers no tool call in the conversation", b.ToolResult.ToolUseID)
					}
					req.Messages = append(req.Messages, ollamaMsgV2{Role: "tool", ToolName: name, Content: b.ToolResult.Content})
				}
			}
			if len(text) > 0 {
				req.Messages = append(req.Messages, ollamaMsgV2{Role: "user", Content: strings.Join(text, "\n\n")})
			}
		}
	}
	return req, firstErr
}

// ollamaAssistant converts one assistant entry; it records each tool_use's
// name under its knomit id for the results that follow.
func ollamaAssistant(e Entry, names map[string]string) ollamaMsgV2 {
	m := ollamaMsgV2{Role: "assistant"}
	var text []string
	for _, b := range e.Blocks {
		switch b.Kind {
		case BlockText:
			if b.Text != "" {
				text = append(text, b.Text)
			}
		case BlockReasoning:
			var n struct {
				Thinking string `json:"thinking"`
			}
			if b.Reasoning != nil && json.Unmarshal(b.Reasoning.Native, &n) == nil {
				m.Thinking += n.Thinking
			}
		case BlockToolUse:
			names[b.ToolUse.ID] = b.ToolUse.Name
			args := b.ToolUse.Args
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			m.ToolCalls = append(m.ToolCalls, ollamaCall{Function: ollamaCallFn{Name: b.ToolUse.Name, Arguments: args}})
		}
	}
	m.Content = strings.Join(text, "")
	return m
}

// OllamaPrefixJSON is the cached part of the Ollama request for s, as
// {"system":<the system message or null>,"tools":<tools or null>}, exactly as
// renderOllama marshals them. It exists so the byte-identity tests outside
// this package compare the bytes Ollama actually receives.
func OllamaPrefixJSON(s Snapshot, model string) ([]byte, error) {
	req := renderOllama(s, model, OllamaOptions{})
	var system *ollamaMsgV2
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		system = &req.Messages[0]
	}
	return encodeCanonical(struct {
		System *ollamaMsgV2   `json:"system"`
		Tools  []ollamaToolV2 `json:"tools"`
	}{system, req.Tools})
}

// ---- streaming ----

// ollamaStreamLineV2 is one NDJSON frame. Error is the mid-stream error
// object ({"error":"…"}) the API docs describe; its HTTP status is already 200.
type ollamaStreamLineV2 struct {
	Message struct {
		Content   string `json:"content"`
		Thinking  string `json:"thinking"`
		ToolCalls []struct {
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int64  `json:"prompt_eval_count"`
	EvalCount       int64  `json:"eval_count"`
	Error           string `json:"error"`
}

// Stream sends the rendered snapshot and reads the NDJSON stream.
//
// Every stream line calls o.Progress, text or not, so the stall timer counts
// a model that is only thinking.
//
// done_reason "length" (the output limit, or the context filled up while
// generating) returns a *StopError (Is ErrTruncated) holding the partial
// Response; it is never a success. A stream that ends without a done line
// (the connection dropped) and a mid-stream {"error":…} line are errors.
func (c *OllamaChat) Stream(ctx context.Context, s Snapshot, o StreamOptions, sink func(Event)) (*Response, error) {
	if c.reason != "" {
		return nil, fmt.Errorf("ollama: model %q is unusable: %s", c.model, c.reason)
	}
	oo := c.o
	if o.MaxOutput > 0 {
		oo.MaxOutput = o.MaxOutput
	}
	req, err := renderOllamaChecked(s, c.model, oo, c.thinks)
	if err != nil {
		return nil, err
	}
	if sink == nil {
		sink = func(Event) {}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal request: %w", err)
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.o.Host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: build request: %w", err)
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("ollama: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("ollama: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}

	var (
		text, thinking strings.Builder
		blocks         []Block
		reasoned       bool // the reasoning block has been emitted
		toolBlocks     []Block
		final          *ollamaStreamLineV2
	)
	flushReasoning := func() {
		if reasoned || thinking.Len() == 0 {
			return
		}
		reasoned = true
		raw, _ := json.Marshal(map[string]string{"thinking": thinking.String()})
		r := &Reasoning{Provider: ollamaProvider, Model: c.model, Native: raw}
		blocks = append(blocks, Block{Kind: BlockReasoning, Reasoning: r})
		sink(Event{Kind: EventReasoning, Reasoning: r})
	}

	dec := json.NewDecoder(resp.Body)
	for final == nil {
		var ln ollamaStreamLineV2
		if err := dec.Decode(&ln); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, errors.New("ollama: stream ended before the done line")
			}
			return nil, fmt.Errorf("ollama: read stream: %w", err)
		}
		o.progress()
		if ln.Error != "" {
			return nil, fmt.Errorf("ollama: %s", ln.Error)
		}
		thinking.WriteString(ln.Message.Thinking)
		if ln.Message.Content != "" || len(ln.Message.ToolCalls) > 0 {
			flushReasoning()
		}
		if ln.Message.Content != "" {
			text.WriteString(ln.Message.Content)
			sink(Event{Kind: EventTextDelta, Text: ln.Message.Content})
		}
		for _, tc := range ln.Message.ToolCalls {
			tu := &ToolUse{ID: o.newID(), Name: tc.Function.Name, Args: json.RawMessage(`{}`)}
			if raw := bytes.TrimSpace(tc.Function.Arguments); len(raw) > 0 && string(raw) != "null" {
				if raw[0] == '{' {
					var buf bytes.Buffer
					_ = json.Compact(&buf, raw) // raw came out of a JSON decode: valid
					tu.Args = json.RawMessage(buf.Bytes())
				} else {
					// Valid JSON but not an object: the model's arguments are
					// unusable. Keep them, store {} (ToolUse.ArgsErr).
					tu.RawArgs = string(raw)
				}
			}
			toolBlocks = append(toolBlocks, Block{Kind: BlockToolUse, ToolUse: tu})
			sink(Event{Kind: EventToolUse, ToolUse: tu})
		}
		if ln.Done {
			final = &ln
		}
	}
	flushReasoning()
	if text.Len() > 0 {
		blocks = append(blocks, Block{Kind: BlockText, Text: text.String()})
	}
	blocks = append(blocks, toolBlocks...)

	out := &Response{Blocks: blocks, StopReason: ollamaStopReason(final.DoneReason, len(toolBlocks) > 0), Usage: usageFromFinal(s, final)}
	if final.DoneReason == "length" {
		return nil, &StopError{Reason: StopMaxTokens, Response: out}
	}
	sink(Event{Kind: EventDone})
	return out, nil
}

// ollamaStopReason maps done_reason onto the Stop* constants. Ollama reports
// "stop" for a turn that ended in tool calls, so the calls decide.
func ollamaStopReason(doneReason string, calls bool) string {
	switch {
	case doneReason == "length":
		return StopMaxTokens
	case calls:
		return StopToolUse
	case doneReason == "stop" || doneReason == "":
		return StopEndTurn
	}
	return doneReason
}

// usageFromFinal is the response's token accounting. Ollama reports no cache
// counters: prompt_eval_count is the number of prompt tokens it actually
// EVALUATED, which is smaller than the prompt when the runner reused its KV
// cache. So PromptEval and Input are the raw count, Output is eval_count, and
// CacheRead is an ESTIMATE: the locally estimated prompt tokens (bytes/4 of
// the snapshot, see Snapshot.Record) minus the evaluated ones, never below 0.
// Estimated is therefore always true, and CacheRead is a hint of reuse, not a
// measurement.
func usageFromFinal(s Snapshot, f *ollamaStreamLineV2) Usage {
	est := 0
	for _, n := range s.Record().Tokens {
		est += n
	}
	return Usage{
		Input:      f.PromptEvalCount,
		PromptEval: f.PromptEvalCount,
		Output:     f.EvalCount,
		CacheRead:  max(0, int64(est)-f.PromptEvalCount),
		Estimated:  true,
	}
}
