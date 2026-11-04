package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

// This file is the agent loop: send the context, stream the reply, run the
// tool calls the model asked for, append the results, and repeat until the
// model answers. It reaches tools only through ToolHost and decisions only
// through CallGate, so it knows nothing of MCP, HTTP or the chat UI.
//
// HISTORY IS APPEND-ONLY. The loop never edits, trims or replaces anything in
// the conversation hook (see history.go). A turn is built in the turn hook
// (HookTurn, hook 7), which IS allowed to change, and when the turn ends its
// entries are Added to the conversation hook (hook 6) in order and removed
// from hook 7. Nothing calls Builder.Replace or Builder.Remove on hook 6; the
// Builder would refuse. A long conversation is measured (tokens, estimated
// cost), warned about once near the price cliff, and refused before sending
// when it would not fit the context window; it is never shortened.

// ToolHost runs tool calls. The chat layer's in-process MCP session
// implements it.
type ToolHost interface {
	CallTool(ctx context.Context, name string, args json.RawMessage) (ToolOutcome, error)
}

// ToolOutcome is a tool's result as the model sees it.
type ToolOutcome struct {
	Content string
	IsError bool
}

// CallGate decides whether a tool call may run. The chat layer owns the
// policy and any user interaction; the loop only obeys. Decide blocks until
// decided or ctx is done.
type CallGate interface {
	Decide(ctx context.Context, call ToolUse) (Decision, error)
}

// Decision is a gate's answer. Kind is auto | allow | allow_session | decline
// and is recorded verbatim; an empty Kind is recorded as allow or decline.
type Decision struct {
	Allow bool
	Kind  string
}

// Texts the model reads back. They are part of the loop's contract with the
// model, so they are exported constants rather than inline strings.
const (
	DeclinedResult  = "The user declined this call. Do not retry it unless the user asks."
	CancelledResult = "Cancelled by the user."
	// SkippedResult answers a tool_use that was never run because the turn
	// ended first for a reason other than the user cancelling it.
	SkippedResult = "This call was not run because the turn ended."
)

// DefaultMaxCalls is the runaway guard (plan P20): tool calls per user turn.
const DefaultMaxCalls = 500

var (
	// ErrTooManyCalls: the turn asked for more tool calls than Loop.MaxCalls.
	ErrTooManyCalls = errors.New("llm: the turn exceeded its tool call limit")
	// ErrCancelled: the caller's context ended the turn.
	ErrCancelled = errors.New("llm: the turn was cancelled")
)

// turnError carries the sentence the user is shown while still matching the
// sentinel with errors.Is.
type turnError struct {
	msg string
	err error
}

func (e *turnError) Error() string { return e.msg }
func (e *turnError) Unwrap() error { return e.err }

// PriceCard prices one model's usage. It is injected by the caller (from
// config); the loop holds no model names or prices. All prices are USD per
// million tokens. Input and output rates up to TierTokens prompt tokens are
// InputPerMTok/OutputPerMTok; a request whose prompt (input + cache read +
// cache write) is larger uses the High rates. TierTokens 0 means one tier.
// The tier is chosen on ALL prompt tokens, cache reads and writes included.
type PriceCard struct {
	InputPerMTok  float64
	OutputPerMTok float64

	TierTokens        int
	HighInputPerMTok  float64
	HighOutputPerMTok float64
	CacheReadMult     float64 // multiple of the input rate
	CacheWrite5mMult  float64
	CacheWrite1hMult  float64
	WarnTokens        int // the prompt size that triggers the one warning; 0 disables it
}

// Cost is an estimate in USD. Usage does not say which cache TTL a write
// used, so Estimate prices every cache write at the 5m multiplier (the lower
// bound) and Upper at the 1h multiplier.
type Cost struct {
	Estimate float64
	Upper    float64
}

// Estimate prices u. A nil card has no prices and returns the zero Cost.
func (c *PriceCard) Estimate(u Usage) Cost {
	if c == nil {
		return Cost{}
	}
	in, out := c.InputPerMTok, c.OutputPerMTok
	if c.TierTokens > 0 && promptTokens(u) > int64(c.TierTokens) {
		in, out = c.HighInputPerMTok, c.HighOutputPerMTok
	}
	base := float64(u.Input)*in + float64(u.CacheRead)*in*c.CacheReadMult + float64(u.Output)*out
	return Cost{
		Estimate: (base + float64(u.CacheWrite)*in*c.CacheWrite5mMult) / 1e6,
		Upper:    (base + float64(u.CacheWrite)*in*c.CacheWrite1hMult) / 1e6,
	}
}

// promptTokens is everything the request sent as input.
func promptTokens(u Usage) int64 { return u.Input + u.CacheRead + u.CacheWrite }

// LoopEventKind names a LoopEvent.
type LoopEventKind string

const (
	LoopText       LoopEventKind = "text_delta"
	LoopReasoning  LoopEventKind = "reasoning"
	LoopToolUse    LoopEventKind = "tool_use"    // the model's call is complete; ToolUse is set
	LoopToolResult LoopEventKind = "tool_result" // Result is set; the call ran, was declined, or was refused
	LoopUsage      LoopEventKind = "usage"       // one request's Usage and Cost
	LoopWarning    LoopEventKind = "warning"     // Warning is set
)

// LoopEvent is what the loop tells its sink as a turn runs.
type LoopEvent struct {
	Kind    LoopEventKind
	Text    string
	ToolUse *ToolUse
	Result  *ToolResult
	Usage   *Usage
	Cost    *Cost // nil when the loop has no price card
	Warning string
}

// RequestLog is one request as the session log records it (plan P23): which
// items each hook held with their estimated tokens (the embedded
// RequestRecord), and the usage the provider reported for it.
type RequestLog struct {
	Provider, Model string
	RequestRecord
	Usage Usage
}

// ToolCallLog is one tool call and its outcome.
type ToolCallLog struct {
	CallID  string
	Name    string
	Args    json.RawMessage // what the model sent
	Content string
	IsError bool
}

// ApprovalLog is one gate decision. Decision is the gate's Kind.
type ApprovalLog struct {
	CallID, Tool, Decision string
}

// SessionLog receives the durable record of a chat. The chat layer implements
// it; a nil Loop.Log is a no-op.
type SessionLog interface {
	Request(RequestLog)
	Turn(Entry)
	ToolCall(ToolCallLog)
	Approval(ApprovalLog)
}

// Loop runs user turns against a Builder. One Loop is one chat: it holds the
// conversation (in Builder), the once-per-chat warning state and the last
// measured prompt size. It is not safe for concurrent RunTurn calls.
type Loop struct {
	Model   ChatModel
	Builder *Builder // hooks 1-3 (and 4-5) loaded by the caller; the loop owns 6 and 7
	Tools   ToolHost
	Gate    CallGate // nil: every call is allowed with Kind "auto"
	Sink    func(LoopEvent)
	// MaxCalls bounds tool calls per user turn; 0 means DefaultMaxCalls.
	MaxCalls int
	// NewID assigns history entry ids; nil uses a process-wide counter.
	NewID func() string
	// ArgsHook returns the arguments to SEND for a call (trace injection,
	// plan P17). The history keeps the arguments the model emitted. Nil sends
	// the model's own.
	ArgsHook func(ToolUse) json.RawMessage
	Log      SessionLog
	// Options are the per-request knobs, passed to the model untouched
	// (Progress feeds WrapChat's stall timer).
	Options StreamOptions
	// Price prices usage and carries the warn threshold; nil means no cost
	// estimate and no warning.
	Price *PriceCard

	warned     bool
	haveUsage  bool
	lastTokens int64 // prompt + output of the last response: what the next request starts from
	lastCount  int   // conversation entries (hooks 6+7) that last response had accounted for
}

// TurnResult summarises one RunTurn. It is returned alongside an error too,
// holding whatever happened before the turn ended.
type TurnResult struct {
	StopReason string
	Text       string // the final assistant text
	Usage      Usage  // summed over the turn's requests
	Cost       Cost   // summed; zero without a price card
	Requests   int
	ToolCalls  int
	Entries    []Entry // appended to the conversation by this turn
}

var entrySeq atomic.Int64

func (l *Loop) newID() string {
	if l.NewID != nil {
		return l.NewID()
	}
	return fmt.Sprintf("e_%d", entrySeq.Add(1))
}

func (l *Loop) emit(ev LoopEvent) {
	if l.Sink != nil {
		l.Sink(ev)
	}
}

// RunTurn adds text as a user entry and runs the model and its tool calls
// until the model answers or the turn fails. On every outcome the Builder is
// left valid: each tool_use has its tool_result, and the turn's entries are in
// hook 6. The one exception is a turn refused before its first request
// (ErrContextFull): it is dropped from hook 7 and hook 6 is untouched.
func (l *Loop) RunTurn(ctx context.Context, text string) (*TurnResult, error) {
	res := &TurnResult{}
	user := Entry{ID: l.newID(), Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: text}}}
	if err := l.addTurn(user); err != nil {
		return res, err
	}
	var runErr error

	maxCalls := l.MaxCalls
	if maxCalls <= 0 {
		maxCalls = DefaultMaxCalls
	}
	for {
		snap := l.Builder.Snapshot()
		if err := l.checkBudget(snap); err != nil {
			if res.Requests == 0 {
				l.dropTurn()
				return res, err
			}
			runErr = err
			break
		}
		var streamed strings.Builder
		resp, err := l.Model.Stream(ctx, snap, l.Options, func(ev Event) {
			switch ev.Kind {
			case EventTextDelta:
				streamed.WriteString(ev.Text)
				l.emit(LoopEvent{Kind: LoopText, Text: ev.Text})
			case EventReasoning:
				l.emit(LoopEvent{Kind: LoopReasoning})
			case EventToolUse:
				l.emit(LoopEvent{Kind: LoopToolUse, ToolUse: ev.ToolUse})
			}
		})
		var stop *StopError
		switch {
		case errors.As(err, &stop):
			resp = stop.Response
		case err != nil:
			if ctx.Err() != nil {
				l.keepPartialText(streamed.String())
				runErr = &turnError{msg: "The turn was cancelled.", err: ErrCancelled}
			} else if errors.Is(err, ErrContextFull) {
				if res.Requests == 0 {
					l.dropTurn()
					return res, &turnError{msg: contextFullMessage, err: err}
				}
				runErr = &turnError{msg: contextFullMessage, err: err}
			} else {
				runErr = err
			}
			l.flush(res)
			return res, runErr
		}

		res.Requests++
		l.account(res, snap, resp)

		assistant := Entry{ID: l.newID(), Role: RoleAssistant, Blocks: resp.Blocks, Usage: &resp.Usage}
		if stop != nil {
			// A cut-off or declined reply: keep the text that did arrive, but
			// never a tool_use that would need a result (the turn ends here),
			// and never reasoning blocks that no later request builds on.
			assistant.Blocks = textBlocks(resp.Blocks)
			if stop.Reason == StopRefusal || len(assistant.Blocks) == 0 {
				assistant = Entry{}
			}
			if assistant.ID != "" {
				if err := l.addTurn(assistant); err != nil {
					return res, err
				}
			}
			res.StopReason = stop.Reason
			res.Text = blocksText(assistant.Blocks)
			if stop.Reason == StopRefusal {
				msg := "The model declined this request"
				if stop.Category != "" {
					msg += " (category: " + stop.Category + ")"
				}
				runErr = &turnError{msg: msg + ".", err: err}
			} else {
				runErr = &turnError{msg: "The reply hit the output limit.", err: err}
			}
			break
		}
		if err := l.addTurn(assistant); err != nil {
			return res, err
		}
		l.lastCount = l.entryCount()
		res.StopReason = resp.StopReason
		res.Text = blocksText(assistant.Blocks)

		calls := toolUses(assistant.Blocks)
		if len(calls) == 0 {
			if resp.StopReason == StopPauseTurn {
				// A paused server-side loop: send the conversation again with
				// this response appended; never a new user message.
				continue
			}
			break
		}
		results, approvals, err := l.runCalls(ctx, calls, res, maxCalls)
		l.finishResults(results, approvals)
		if err != nil {
			runErr = err
			break
		}
	}
	l.flush(res)
	return res, runErr
}

const contextFullMessage = "This conversation no longer fits the model's context window, so the request was not sent. Start a new chat to continue."

// runCalls answers every call in order and returns the tool_result blocks. It
// stops early (still answering the rest) on cancellation, the call limit or a
// gate failure, and returns the error that ended the turn.
func (l *Loop) runCalls(ctx context.Context, calls []ToolUse, res *TurnResult, maxCalls int) ([]Block, []Approval, error) {
	var results []Block
	var approvals []Approval
	answer := func(c ToolUse, content string, isErr bool) {
		tr := &ToolResult{ToolUseID: c.ID, Content: content, IsError: isErr}
		results = append(results, Block{Kind: BlockToolResult, ToolResult: tr})
		l.emit(LoopEvent{Kind: LoopToolResult, Result: tr})
	}
	abort := func(from int, content string) {
		for _, c := range calls[from:] {
			answer(c, content, true)
		}
	}
	for i, c := range calls {
		if ctx.Err() != nil {
			abort(i, CancelledResult)
			return results, approvals, &turnError{msg: "The turn was cancelled.", err: ErrCancelled}
		}
		if res.ToolCalls >= maxCalls {
			abort(i, SkippedResult)
			return results, approvals, &turnError{
				msg: fmt.Sprintf("The turn stopped after %d tool calls, the limit for one turn.", maxCalls),
				err: ErrTooManyCalls,
			}
		}
		res.ToolCalls++

		if err := c.ArgsErr(); err != nil {
			msg := fmt.Sprintf("The arguments for %s are not valid JSON, so the call was not run. Received: %s", c.Name, c.RawArgs)
			answer(c, msg, true)
			l.logCall(c, msg, true)
			continue
		}

		if l.Gate != nil {
			d, err := l.Gate.Decide(ctx, c)
			if err != nil {
				if ctx.Err() != nil {
					abort(i, CancelledResult)
					return results, approvals, &turnError{msg: "The turn was cancelled.", err: ErrCancelled}
				}
				abort(i, SkippedResult)
				return results, approvals, fmt.Errorf("approval for %s failed: %w", c.Name, err)
			}
			kind := d.Kind
			if kind == "" {
				kind = map[bool]string{true: "allow", false: "decline"}[d.Allow]
			}
			approvals = append(approvals, Approval{ToolUseID: c.ID, Decision: kind})
			if l.Log != nil {
				l.Log.Approval(ApprovalLog{CallID: c.ID, Tool: c.Name, Decision: kind})
			}
			if !d.Allow {
				answer(c, DeclinedResult, true)
				l.logCall(c, DeclinedResult, true)
				continue
			}
		}

		args := c.Args
		if l.ArgsHook != nil {
			args = l.ArgsHook(c)
		}
		out, err := l.Tools.CallTool(ctx, c.Name, args)
		if err != nil {
			if ctx.Err() != nil {
				abort(i, CancelledResult)
				return results, approvals, &turnError{msg: "The turn was cancelled.", err: ErrCancelled}
			}
			// Unknown tool, transport failure: the model reads it and carries on.
			out = ToolOutcome{Content: fmt.Sprintf("The call to %s failed: %v", c.Name, err), IsError: true}
		}
		answer(c, out.Content, out.IsError)
		l.logCall(c, out.Content, out.IsError)
	}
	return results, approvals, nil
}

func (l *Loop) logCall(c ToolUse, content string, isErr bool) {
	if l.Log != nil {
		l.Log.ToolCall(ToolCallLog{CallID: c.ID, Name: c.Name, Args: c.Args, Content: content, IsError: isErr})
	}
}

// finishResults appends the single user entry that answers an assistant
// entry's tool_use blocks. Anthropic requires all results in one message.
func (l *Loop) finishResults(results []Block, approvals []Approval) {
	if len(results) == 0 {
		return
	}
	e := Entry{ID: l.newID(), Role: RoleUser, Blocks: results, Approvals: approvals}
	if err := l.addTurn(e); err != nil {
		// Entry ids are generated here and unique; a duplicate means a
		// caller-supplied NewID repeats. Surfacing it as a panic would kill the
		// server; the next addTurn reports it as the turn error.
		return
	}
}

// keepPartialText stores text that streamed before a cancellation as an
// assistant entry so the user's view and the history agree.
func (l *Loop) keepPartialText(text string) {
	if text == "" {
		return
	}
	_ = l.addTurn(Entry{ID: l.newID(), Role: RoleAssistant, Blocks: []Block{{Kind: BlockText, Text: text}}})
}

// account records one response: usage and cost to the sink and the log, the
// once-per-chat warning, and the baseline the next budget check starts from.
func (l *Loop) account(res *TurnResult, snap Snapshot, resp *Response) {
	u := resp.Usage
	res.Usage.Input += u.Input
	res.Usage.CacheRead += u.CacheRead
	res.Usage.CacheWrite += u.CacheWrite
	res.Usage.Output += u.Output
	res.Usage.PromptEval += u.PromptEval
	res.Usage.Estimated = res.Usage.Estimated || u.Estimated

	ev := LoopEvent{Kind: LoopUsage, Usage: &u}
	if l.Price != nil {
		c := l.Price.Estimate(u)
		res.Cost.Estimate += c.Estimate
		res.Cost.Upper += c.Upper
		ev.Cost = &c
	}
	if l.Log != nil {
		l.Log.Request(RequestLog{Provider: l.Model.Provider(), Model: l.Model.Model(), RequestRecord: snap.Record(), Usage: u})
	}
	l.emit(ev)

	prompt := promptTokens(u)
	l.haveUsage = true
	l.lastTokens = prompt + u.Output
	if l.Price != nil && l.Price.WarnTokens > 0 && !l.warned && prompt >= int64(l.Price.WarnTokens) {
		l.warned = true
		msg := fmt.Sprintf("This chat's prompt has reached %d tokens.", prompt)
		if l.Price.TierTokens > 0 {
			msg += fmt.Sprintf(" This model's price per token rises above %d prompt tokens; consider starting a new chat.", l.Price.TierTokens)
		}
		l.emit(LoopEvent{Kind: LoopWarning, Warning: msg})
	}
}

// checkBudget refuses a request that cannot fit the context window, before
// anything is sent. The rule:
//
//	need = estimated prompt tokens + Caps().MaxOutput
//	refuse when need > Caps().ContextWindow
//
// The estimate is the larger of (a) the whole snapshot at bytes/4
// (Snapshot.Record) and (b) the last response's reported prompt plus output
// tokens, plus bytes/4 for the entries added since. (b) is exact up to the new
// content, (a) covers the first request of a chat and cannot be lower than
// what is really there by much. MaxOutput is reserved because the window
// covers input and output together (Ollama's num_ctx does), and it comes from
// the model's own Caps rather than a constant. A model whose Caps report no
// ContextWindow is not pre-checked; the adapter's ErrContextFull still ends
// the request.
func (l *Loop) checkBudget(snap Snapshot) error {
	caps := l.Model.Caps()
	if caps.ContextWindow <= 0 {
		return nil
	}
	rec := snap.Record()
	est := 0
	for h := HookTools; h <= HookTurn; h++ {
		est += rec.Tokens[h]
	}
	if l.haveUsage {
		known := int(l.lastTokens)
		extra := 0
		for i, it := range conversationItems(snap) {
			if i >= l.lastCount {
				extra += itemBytes(it) / 4
			}
		}
		est = max(est, known+extra)
	}
	need := est + max(caps.MaxOutput, 0)
	if need > caps.ContextWindow {
		return &turnError{
			msg: fmt.Sprintf("%s (about %d prompt tokens plus %d reserved for the reply, window %d)",
				contextFullMessage, est, caps.MaxOutput, caps.ContextWindow),
			err: ErrContextFull,
		}
	}
	return nil
}

func conversationItems(s Snapshot) []Item {
	out := append([]Item(nil), s.Hooks[HookConversation]...)
	return append(out, s.Hooks[HookTurn]...)
}

func (l *Loop) entryCount() int {
	return len(conversationItems(l.Builder.Snapshot()))
}

// addTurn puts e in hook 7.
func (l *Loop) addTurn(e Entry) error {
	cp := e
	return l.Builder.Add(Item{ID: e.ID, Hook: HookTurn, Entry: &cp})
}

// dropTurn removes the turn's entries from hook 7 only. Hook 6 is untouched.
func (l *Loop) dropTurn() {
	for _, it := range l.Builder.Snapshot().Hooks[HookTurn] {
		_ = l.Builder.Remove(it.ID)
	}
}

// flush moves the finished turn from hook 7 into hook 6: Add to hook 6, then
// Remove from hook 7. Hook 6 only ever grows.
func (l *Loop) flush(res *TurnResult) {
	for _, it := range l.Builder.Snapshot().Hooks[HookTurn] {
		if err := l.Builder.Remove(it.ID); err != nil {
			continue
		}
		moved := it
		moved.Hook = HookConversation
		if err := l.Builder.Add(moved); err != nil {
			continue
		}
		res.Entries = append(res.Entries, *it.Entry)
		if l.Log != nil {
			l.Log.Turn(*it.Entry)
		}
	}
	// lastCount counted hooks 6+7 together, which flushing does not change.
}

func textBlocks(bs []Block) []Block {
	var out []Block
	for _, b := range bs {
		if b.Kind == BlockText && b.Text != "" {
			out = append(out, b)
		}
	}
	return out
}

func blocksText(bs []Block) string {
	var sb strings.Builder
	for _, b := range bs {
		if b.Kind == BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func toolUses(bs []Block) []ToolUse {
	var out []ToolUse
	for _, b := range bs {
		if b.Kind == BlockToolUse && b.ToolUse != nil {
			out = append(out, *b.ToolUse)
		}
	}
	return out
}
