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
	"sync"
	"sync/atomic"
	"time"
)

// This file is the Ollama adapter: hand-written HTTP against /api/chat
// (NDJSON streaming, tools). The capability probe below began as a copy of the
// request the earlier adapter made; that adapter is deleted and this is the only
// /api/show probe.
//
// Ollama has no cache breakpoints. Its prompt cache is the runner's KV cache
// for the loaded model, so the two things that make it hit are (1) a request
// prefix that is byte-stable up to the newest turn, which renderOllama
// preserves by putting everything shared into one system message and the
// tools in name order, and (2) keep_alive, which keeps the model loaded
// between turns. Cache use is visible through the counters Ollama reports,
// which usageFromFinal turns into Usage (prompt_eval_cached_count where the
// server sends it, an estimate where it does not).

const (
	ollamaProvider         = "ollama"
	defaultOllamaKeepAlive = "30m"
	// num_ctx is sent on EVERY request, together with truncate:false: without
	// it Ollama loads the model with its own default context, and without
	// truncate:false it silently drops the oldest messages of a prompt that
	// does not fit (see ollamaChatRequest). There is no default value here:
	// the configured num_ctx wins, otherwise numctx.go derives one.
	defaultOllamaHost = "http://localhost:11434"
	// ollamaUnlimited is num_predict -1: no output cap (see ollamaUncapped in
	// ollama.go for why a cap buys nothing on a local model).
	ollamaUnlimited = -1
)

// OllamaOptions configures an OllamaChat. Host defaults to OLLAMA_HOST, then
// http://localhost:11434. KeepAlive defaults to "30m". NumCtx is the
// configured context size and always wins; 0 means "derive it" (numctx.go).
// It is also the context window the loop budgets against (Caps), capped at
// the model's trained context when /api/show reports one.
// MaxOutput is an explicit reply cap: when > 0 it is sent as num_predict and
// the loop reserves it. 0 (and a negative value) means no cap: num_predict -1
// is sent, and the loop's pre-send reserve is a quarter of the effective
// window. That reserve is a check only; it never limits the reply.
type OllamaOptions struct {
	Host, KeepAlive   string
	NumCtx, MaxOutput int
	// Memory is a test seam: the memory usable for the KV cache after the
	// model weights, in bytes, and whether it is known. nil means detect it
	// (memlimit.Detect minus the weights' size from /api/tags). It is only
	// consulted for a loopback host with NumCtx unset.
	Memory func(ctx context.Context) (int64, bool)
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
	if o.NumCtx < 0 {
		o.NumCtx = 0
	}
	return o
}

// OllamaChat is the Ollama adapter.
type OllamaChat struct {
	client *http.Client
	model  string
	o      OllamaOptions

	trained int    // the model's trained context from /api/show, 0 when not reported
	tools   bool   // the model advertises the "tools" capability
	thinks  bool   // the model advertises "thinking"
	reason  string // why the model is unusable, "" when it is usable

	// ctxUnresolved: no num_ctx is configured and none could be derived
	// (reason says why). The model is then unavailable, not merely toolless.
	ctxUnresolved bool
	how           string // how the window was resolved: NumCtxHow*; "" when unresolved

	spillOnce sync.Once
	spill     atomic.Bool   // /api/ps showed layers on the CPU after the first request
	spillDone chan struct{} // closed when that read has finished; nil outside tests
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
	show, err := c.probe(ctx)
	switch {
	case err != nil:
		c.reason = "capability probe failed: " + err.Error()
	case !slices.Contains(show.caps, "tools"):
		c.reason = "model does not support tool calling (capabilities: " + strings.Join(show.caps, ", ") + ")"
	default:
		c.tools = true
	}
	c.thinks = slices.Contains(show.caps, "thinking")
	c.trained = show.trained
	c.resolveNumCtx(ctx, show, err == nil)
	return c, nil
}

// ollamaShow is what /api/show tells knomit about a model.
type ollamaShow struct {
	caps    []string
	trained int            // model_info["<arch>.context_length"], 0 when absent
	arch    string         // model_info["general.architecture"]
	info    map[string]any // the whole model_info
}

// probe asks /api/show for the model's capabilities, trained context length
// and the model_info keys numctx.go sizes the KV cache from.
func (c *OllamaChat) probe(ctx context.Context) (ollamaShow, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body, err := json.Marshal(map[string]string{"model": c.model})
	if err != nil {
		return ollamaShow{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.o.Host+"/api/show", bytes.NewReader(body))
	if err != nil {
		return ollamaShow{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return ollamaShow{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return ollamaShow{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	var show struct {
		Capabilities []string       `json:"capabilities"`
		ModelInfo    map[string]any `json:"model_info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&show); err != nil {
		return ollamaShow{}, fmt.Errorf("unreadable response: %w", err)
	}
	out := ollamaShow{caps: show.Capabilities, info: show.ModelInfo}
	if arch, ok := show.ModelInfo["general.architecture"].(string); ok {
		out.arch = arch
		if n, ok := show.ModelInfo[arch+".context_length"].(float64); ok && n > 0 {
			out.trained = int(n)
		}
	}
	return out, nil
}

func (c *OllamaChat) Provider() string { return ollamaProvider }
func (c *OllamaChat) Model() string    { return c.model }
func (c *OllamaChat) Caps() Caps {
	// Ollama runs a model with at most its trained context (server/sched.go
	// effectiveContext, v0.40.14), so a larger num_ctx buys no window.
	window := c.o.NumCtx
	if c.trained > 0 {
		window = min(window, c.trained)
	}
	return Caps{Tools: c.tools, MaxBreakpoints: 0, ContextWindow: window, MaxOutput: c.o.MaxOutput}
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

type ollamaTool struct {
	Type     string       `json:"type"`
	Function ollamaToolFn `json:"function"`
}

type ollamaCallFn struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ollamaCall struct {
	ID       string       `json:"id,omitempty"`
	Function ollamaCallFn `json:"function"`
}

// ollamaMsg is one request message. Content has no omitempty: an assistant
// message that only calls tools is sent with "content":"" as in the API docs.
type ollamaMsg struct {
	Role      string       `json:"role"`
	Content   string       `json:"content"`
	Thinking  string       `json:"thinking,omitempty"`
	ToolCalls []ollamaCall `json:"tool_calls,omitempty"`
	ToolName  string       `json:"tool_name,omitempty"`
	// ToolCallID answers the tool_calls[].ID it names (Ollama api.Message
	// tool_call_id). ToolName is sent too: it is what an Ollama that ignores
	// ids matches on.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type ollamaOpts struct {
	NumCtx     int `json:"num_ctx"`
	NumPredict int `json:"num_predict"`
}

type ollamaChatRequest struct {
	Model     string       `json:"model"`
	Messages  []ollamaMsg  `json:"messages"`
	Tools     []ollamaTool `json:"tools,omitempty"`
	Think     *bool        `json:"think,omitempty"`
	Stream    bool         `json:"stream"`
	KeepAlive string       `json:"keep_alive"`
	Options   ollamaOpts   `json:"options"`
	// Truncate is ALWAYS sent, as false (user decision, 2026-11-04). Ollama's
	// default drops the oldest messages of a chat that does not fit num_ctx
	// without an error (server/prompt.go chatPrompt, v0.40.14); false makes it
	// keep the whole conversation, so an over-long prompt fails instead of
	// silently becoming a different conversation. See ollamaContextFull.
	Truncate bool `json:"truncate"`
	// Shift is ALWAYS sent, as false (user decision, 2026-11-04). Ollama
	// launches llama-server with context shift ON by default; when prompt plus
	// output reaches num_ctx WHILE GENERATING, llama-server then silently
	// discards the middle of the KV cache (keeping only num_keep, default 4,
	// tokens) and the response still ends done_reason "stop". With shift:false
	// it stops with "length", which Stream maps to ErrTruncated. This does
	// NOT affect an over-long PROMPT: that is refused with a 400 either way
	// (see ollamaContextFull). Trade-off: shift is a runner launch attribute,
	// so another client of the same Ollama that sends a different shift value
	// forces a model reload.
	Shift bool `json:"shift"`
}

// renderOllama lays a snapshot out as an /api/chat request:
//
//   - one system message: hooks 2, 3, 4 and 5 (non-empty texts) joined by a
//     blank line, in that order; none when all four are empty.
//   - tools: hook 1 in snapshot (name) order, each schema verbatim.
//   - messages: hook 6 then hook 7. A replayed tool call carries its id
//     (always knomit's ToolUse.ID, whatever id the server sent) and a
//     tool result is {role:"tool", tool_name, tool_call_id, content} with the
//     same id and the name of the tool_use it answers, so parallel calls to
//     one tool are told apart by id and, on a server that ignores ids, by
//     name and order. The ids live in hooks 6 and 7, never in the shared
//     prefix. Reasoning from another provider is dropped (ReplayFor).
//   - keep_alive, options.num_ctx, truncate:false and shift:false always set.
//
// It renders a tool result whose tool_use is not in the snapshot with an empty
// tool_name; Stream refuses such a snapshot before sending (renderOllamaChecked's
// error), so that form never reaches the wire.
func renderOllama(s Snapshot, model string, o OllamaOptions) ollamaChatRequest {
	req, _ := renderOllamaChecked(s, model, o, false)
	return req
}

func renderOllamaChecked(s Snapshot, model string, o OllamaOptions, thinks bool) (ollamaChatRequest, error) {
	o = o.withDefaults()
	numPredict := ollamaUnlimited
	if o.MaxOutput > 0 {
		numPredict = o.MaxOutput
	}
	req := ollamaChatRequest{
		Model: model, Stream: true, KeepAlive: o.KeepAlive, Truncate: false, Shift: false,
		Options: ollamaOpts{NumCtx: o.NumCtx, NumPredict: numPredict},
	}
	if thinks {
		t := true
		req.Think = &t
	}
	for _, it := range s.Hooks[HookTools] {
		req.Tools = append(req.Tools, ollamaTool{Type: "function", Function: ollamaToolFn{
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
		req.Messages = append(req.Messages, ollamaMsg{Role: "system", Content: strings.Join(sys, "\n\n")})
	}

	names := map[string]ollamaCallRef{} // knomit tool_use id -> the call it answers
	var firstErr error
	for _, h := range []Hook{HookConversation, HookTurn} {
		for _, it := range s.Hooks[h] {
			if it.Entry == nil {
				if it.Text != "" {
					req.Messages = append(req.Messages, ollamaMsg{Role: "user", Content: it.Text})
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
					ref, ok := names[b.ToolResult.ToolUseID]
					if !ok && firstErr == nil {
						firstErr = fmt.Errorf("ollama: tool result %q answers no tool call in the conversation", b.ToolResult.ToolUseID)
					}
					req.Messages = append(req.Messages, ollamaMsg{Role: "tool", ToolName: ref.name, ToolCallID: ref.id, Content: b.ToolResult.Content})
				}
			}
			if len(text) > 0 {
				req.Messages = append(req.Messages, ollamaMsg{Role: "user", Content: strings.Join(text, "\n\n")})
			}
		}
	}
	return req, firstErr
}

// ollamaCallRef is what a tool result needs from the call it answers: the
// tool's name and the id sent on that call.
type ollamaCallRef struct{ name, id string }

// ollamaAssistant converts one assistant entry; it records each tool_use's
// name and wire id under its knomit id for the results that follow.
func ollamaAssistant(e Entry, names map[string]ollamaCallRef) ollamaMsg {
	m := ollamaMsg{Role: "assistant"}
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
			id := b.ToolUse.ID
			names[b.ToolUse.ID] = ollamaCallRef{name: b.ToolUse.Name, id: id}
			args := b.ToolUse.Args
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			m.ToolCalls = append(m.ToolCalls, ollamaCall{ID: id, Function: ollamaCallFn{Name: b.ToolUse.Name, Arguments: args}})
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
	var system *ollamaMsg
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		system = &req.Messages[0]
	}
	// json.Marshal, not encodeCanonical: the wire form escapes <, > and &, and
	// this export must be those exact bytes.
	return json.Marshal(struct {
		System *ollamaMsg   `json:"system"`
		Tools  []ollamaTool `json:"tools"`
	}{system, req.Tools})
}

// ---- streaming ----

// ollamaStreamLine is one NDJSON frame. Error is the mid-stream error
// object ({"error":"…"}) the API docs describe; its HTTP status is already 200.
type ollamaStreamLine struct {
	Message struct {
		Content   string `json:"content"`
		Thinking  string `json:"thinking"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int64  `json:"prompt_eval_count"`
	// PromptEvalCachedCount is a pointer so a server that does not send it
	// (Ollama before v0.35.0) differs from one that sends 0.
	PromptEvalCachedCount *int64 `json:"prompt_eval_cached_count"`
	EvalCount             int64  `json:"eval_count"`
	Error                 string `json:"error"`
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
		return nil, ollamaHTTPError(resp.StatusCode, string(bytes.TrimSpace(b)))
	}

	var (
		text, thinking strings.Builder
		blocks         []Block
		reasoned       bool // the reasoning block has been emitted
		toolBlocks     []Block
		final          *ollamaStreamLine
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
		var ln ollamaStreamLine
		if err := dec.Decode(&ln); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, errors.New("ollama: stream ended before the done line")
			}
			return nil, fmt.Errorf("ollama: read stream: %w", err)
		}
		o.progress()
		if ln.Error != "" {
			return nil, ollamaLineError(ln.Error)
		}
		thinking.WriteString(ln.Message.Thinking)
		if ln.Message.Thinking != "" {
			sink(Event{Kind: EventThinkingDelta, Text: ln.Message.Thinking})
		}
		if ln.Message.Content != "" || len(ln.Message.ToolCalls) > 0 {
			flushReasoning()
		}
		if ln.Message.Content != "" {
			text.WriteString(ln.Message.Content)
			sink(Event{Kind: EventTextDelta, Text: ln.Message.Content})
		}
		for _, tc := range ln.Message.ToolCalls {
			tu := &ToolUse{ID: o.newID(), Name: tc.Function.Name, Args: json.RawMessage(`{}`)}
			// Ollama always sends arguments as a JSON object: a model whose
			// arguments are not valid JSON makes the SERVER fail the request
			// (llm/llama_server.go, v0.40.14), which surfaces as an error
			// above, never as a tool call. So there is no malformed case here.
			if raw := bytes.TrimSpace(tc.Function.Arguments); len(raw) > 0 && string(raw) != "null" {
				var buf bytes.Buffer
				_ = json.Compact(&buf, raw) // raw came out of a JSON decode: valid
				tu.Args = json.RawMessage(buf.Bytes())
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
	c.checkSpillOnce(ctx)
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

// ollamaHTTPError formats a non-200 /api/chat answer as "ollama: HTTP <code>:
// <body>" so isRetryable still matches 429/503/529. A 400 or 413 whose message
// says the prompt exceeds the context is also ErrContextFull.
func ollamaHTTPError(status int, body string) error {
	err := fmt.Errorf("ollama: HTTP %d: %s", status, body)
	if (status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge) && ollamaContextFull(body) {
		return fmt.Errorf("%w: %w", err, ErrContextFull)
	}
	return err
}

// ollamaLineError is a mid-stream {"error":...} line, ErrContextFull when it
// says the prompt exceeds the context.
func ollamaLineError(msg string) error {
	err := fmt.Errorf("ollama: %s", msg)
	if ollamaContextFull(msg) {
		return fmt.Errorf("%w: %w", err, ErrContextFull)
	}
	return err
}

// ollamaContextFull recognises the "prompt does not fit" error text. With
// truncate:false the refusal a real Ollama v0.40.14 sends is llama.cpp b11963's
// HTTP 400 "request (N tokens) exceeds the available context size (M tokens),
// try increasing it" (tools/server/server-context.cpp:4080-4086, status from
// server-common.cpp:125-128), forwarded as {"error":"..."}; that is the
// "available context" alternative. The "context length" alternative is Ollama's
// own promptTooLongMessage (llm/llama_server.go), reachable only with
// truncate:true, kept as a cheap guard.
func ollamaContextFull(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "context length") || strings.Contains(m, "available context")
}

// usageFromFinal is the response's token accounting.
//
// Current Ollama (prompt_eval_cached_count exists from v0.35.0) reports
// prompt_eval_count as ALL prompt tokens and prompt_eval_cached_count as the
// part read from the KV cache. Then Input = count - cached (uncached, the same
// meaning as Anthropic's Input), CacheRead = cached, PromptEval = count, and
// nothing is estimated.
//
// A server that sends no cached count (older Ollama, where prompt_eval_count
// is only the tokens it evaluated) gets the fallback: Input and PromptEval are
// the raw count and CacheRead is ESTIMATED as the local bytes/4 prompt
// estimate (Snapshot.Record) minus that count, never below 0. Only then is
// Estimated set, with EstimatedFields naming "cache_read" alone
// (user decision, 2026-11-04).
func usageFromFinal(s Snapshot, f *ollamaStreamLine) Usage {
	if f.PromptEvalCachedCount != nil {
		cached := *f.PromptEvalCachedCount
		return Usage{
			Input:      max(0, f.PromptEvalCount-cached),
			PromptEval: f.PromptEvalCount,
			Output:     f.EvalCount,
			CacheRead:  cached,
		}
	}
	est := 0
	for _, n := range s.Record().Tokens {
		est += n
	}
	return Usage{
		Input:           f.PromptEvalCount,
		PromptEval:      f.PromptEvalCount,
		Output:          f.EvalCount,
		CacheRead:       max(0, int64(est)-f.PromptEvalCount),
		Estimated:       true,
		EstimatedFields: []string{"cache_read"},
	}
}
