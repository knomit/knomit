package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

// ErrWindowWithStablePrefix is returned by RunTurn when Loop.Window is set for a
// model whose Caps().StablePrefix is true: the window changes the tools block
// between requests, which would break that provider's prompt cache.
var ErrWindowWithStablePrefix = errors.New("llm: Loop.Window is set for a model with a stable prefix (Caps.StablePrefix); the window must be nil for it")

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
//
// ADAPTIVE PROVIDERS (Loop.Window != nil: Caps.StablePrefix false, no prompt
// cache to protect). The loop still edits nothing: the Builder keeps every
// tool and the whole history. What changes is the SENT view. The loop sends
// Window.Project(Builder.Snapshot()), a copy whose tools block is the loader
// (load_tools) plus the tools loaded so far, and estimates, budgets and logs
// that same copy. The loader and a call to a tool that is not loaded are
// answered in the loop before the approval gate (they run nothing); every call
// to a loaded tool still goes through CallGate.Decide. Loaded tools are evicted
// only between requests and only when the request would not fit, never a tool
// called or loaded in the current turn, so a pending call never meets an
// eviction. With Window nil the snapshot sent is Builder.Snapshot(), byte for
// byte.
//
// For the same providers the SENT history is also shorter than the stored one,
// a deliberate carve-out from "never trimmed" (user decision, 2026-11-05): the
// projection replaces each tool_result of an ended turn (hook 6) with a stub,
// a header and a short head excerpt, and sends the running turn (hook 7) whole.
// The stored entries, the Builder's hook 6 and the session log's turn records
// stay whole; the stub never removes a tool_use or a tool_result, so the
// pairing holds in every request. A stub offers a re-call only for a call the
// effects table marks Replay. Anthropic (Caps.StablePrefix) never gets a
// window and keeps its full history: its cache and thinking signatures depend
// on a stable prefix.

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

// RepeatFailureLimit is how many times in a row, within one turn, the model
// may make the same failing tool call (same tool, byte-identical arguments,
// same error text) before the turn is ended. It is a policy, not a measurement.
// The first failure is information and the second is a fair retry after
// reading the error; a third identical failure shows the model is not acting
// on what the tool said, and each further attempt only spends tokens and
// repeats whatever the call does. Three is the smallest count that allows one
// retry.
const RepeatFailureLimit = 3

// RepeatedFailureMessage is the sentence the user is shown when the turn
// is ended by the repeat guard.
const RepeatedFailureMessage = "The model kept making the same failing tool call (%s, %d times with the same error), so the turn was stopped. The last error was: %s"

var (
	// ErrRepeatedFailure: the model repeated one failing call RepeatFailureLimit times in a row.
	ErrRepeatedFailure = errors.New("llm: the model repeated the same failing tool call")
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

// Cost is one exact figure in USD. Cache writes are priced at the TTL they
// used: Usage.CacheWrite5m at CacheWrite5mMult and Usage.CacheWrite1h at
// CacheWrite1hMult. Any part of Usage.CacheWrite the adapter did not split is
// priced at the 5m multiplier, the API's default TTL.
type Cost struct {
	USD float64
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
	unsplit := max(u.CacheWrite-u.CacheWrite5m-u.CacheWrite1h, 0)
	usd := float64(u.Input)*in +
		float64(u.CacheRead)*in*c.CacheReadMult +
		float64(u.CacheWrite5m+unsplit)*in*c.CacheWrite5mMult +
		float64(u.CacheWrite1h)*in*c.CacheWrite1hMult +
		float64(u.Output)*out
	return Cost{USD: usd / 1e6}
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
	// LoopToolsChanged: the loaded tool set changed; Tools is set. The reason
	// and the names are data; the loop carries no wording.
	LoopToolsChanged LoopEventKind = "tools_changed"
)

// LoopEvent is what the loop tells its sink as a turn runs.
type LoopEvent struct {
	Kind    LoopEventKind
	Text    string
	ToolUse *ToolUse
	Result  *ToolResult
	Usage   *Usage
	Cost    *Cost // nil when the loop has no price card
	Warning *PriceWarning
	Tools   *ToolsChange
}

// PriceWarning is the structured near-the-price-cliff warning: the prompt
// reached Threshold (PriceCard.WarnTokens). TierTokens is the price tier
// boundary (0: one tier). BeforeSend says PromptTokens is the pre-send
// estimate rather than a reported count.
type PriceWarning struct {
	Threshold    int
	PromptTokens int64
	TierTokens   int
	Model        string
	BeforeSend   bool
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
	// Window, when set, makes the loop adaptive: it sends the loader plus the
	// loaded tools instead of every tool (see the file comment). Nil is the
	// static path: every tool, every request. Set it only for a model whose
	// Caps().StablePrefix is false.
	Window *ToolWindow

	warned     bool
	haveUsage  bool
	lastTokens int64 // prompt + output of the last response: what the next request starts from
	lastCount  int   // conversation entries (hooks 6+7) that last response had accounted for
	lastBytes  int   // adaptive only: bytes of the whole sent view when lastCount was set
}

// TurnResult summarises one RunTurn. It is returned alongside an error too,
// holding whatever happened before the turn ended.
type TurnResult struct {
	StopReason string
	Text       string // the final assistant text
	Usage      Usage  // summed over the turn's requests
	Cost       Cost   // summed; zero without a price card
	Requests   int
	ToolCalls  int // every tool call answered, the loader's included
	Loads      int // tools loaded into the window this turn
	Evictions  int // tools evicted from the window this turn
	// Preloaded are the tools the ranker's guess loaded before the first
	// request, in rank order; PreloadHits are those the model then called this
	// turn. RankerError is set when the guess failed (the turn went on).
	Preloaded   []string
	PreloadHits []string
	RankerError string
	Entries     []Entry // appended to the conversation by this turn
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
// hook 6. The one exception is a turn that got no assistant entry (refused
// before its first request, a first-request error of any kind, a refusal or
// truncation with no text, a cancel before any reply): it is dropped from hook
// 7 and hook 6 is untouched, so the user text of a turn the model never
// answered is never resent. A turn in which tool calls already ran is never
// dropped: its results stay paired with their calls and are flushed.
func (l *Loop) RunTurn(ctx context.Context, text string) (*TurnResult, error) {
	res := &TurnResult{}
	if l.Window != nil && l.Model.Caps().StablePrefix {
		return res, ErrWindowWithStablePrefix
	}
	user := Entry{ID: l.newID(), Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: text}}}
	base := l.baseline()
	if l.Window != nil {
		l.Window.BeginTurn()
	}
	if err := l.addTurn(user); err != nil {
		return res, err
	}
	l.preload(ctx, text, res)
	var runErr error
	pauses := 0
	guard := &repeatGuard{}

	maxCalls := l.MaxCalls
	if maxCalls <= 0 {
		maxCalls = DefaultMaxCalls
	}
	for {
		snap := l.view()
		est := l.estimate(snap)
		if l.Window != nil {
			snap, est = l.relieve(res, snap, est)
		}
		if err := l.checkBudget(snap, est); err != nil {
			runErr = err
			break
		}
		l.maybeWarn(int64(est), true)
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
				if caps := l.Model.Caps(); isOpening(snap) && caps.ContextWindow > 0 {
					runErr = l.openingRefused(est, caps.ContextWindow, err)
				} else {
					runErr = &turnError{msg: contextFullMessage, err: err}
				}
			} else {
				runErr = err
			}
			return res, l.settle(res, base, runErr)
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
					runErr = err
					break
				}
			}
			res.StopReason = stop.Reason
			res.Text = blocksText(assistant.Blocks)
			switch stop.Reason {
			case StopRefusal:
				msg := "The model declined this request"
				if stop.Category != "" {
					msg += " (category: " + stop.Category + ")"
				}
				runErr = &turnError{msg: msg + ".", err: err}
			case StopContextWindowExceeded:
				runErr = &turnError{msg: contextFullMessage, err: err}
			default:
				runErr = &turnError{msg: "The reply hit the output limit.", err: err}
			}
			break
		}
		if err := l.addTurn(assistant); err != nil {
			runErr = err
			break
		}
		l.lastCount = l.entryCount()
		l.lastBytes = viewBytes(l.view())
		res.StopReason = resp.StopReason
		res.Text = blocksText(assistant.Blocks)

		calls := toolUses(assistant.Blocks)
		if len(calls) == 0 {
			if resp.StopReason == StopPauseTurn {
				// A paused server-side loop: send the conversation again with
				// this response appended; never a new user message. Each
				// continuation draws on the same per-turn budget as a tool
				// call (MaxCalls), so a provider that pauses forever ends the
				// turn instead of looping.
				pauses++
				if res.ToolCalls+pauses > maxCalls {
					runErr = &turnError{
						msg: fmt.Sprintf("The turn stopped after %d tool calls, the limit for one turn.", maxCalls),
						err: ErrTooManyCalls,
					}
					break
				}
				continue
			}
			break
		}
		results, approvals, err := l.runCalls(ctx, calls, res, maxCalls, guard)
		if ferr := l.finishResults(results, approvals); ferr != nil {
			runErr = ferr
			break
		}
		if err != nil {
			runErr = err
			break
		}
	}
	return res, l.settle(res, base, runErr)
}

const contextFullMessage = "This conversation no longer fits the model's context window, so the request was not sent. Start a new chat to continue."

// runCalls answers every call in order and returns the tool_result blocks. It
// stops early (still answering the rest) on cancellation, the call limit or a
// gate failure, and returns the error that ended the turn.
func (l *Loop) runCalls(ctx context.Context, calls []ToolUse, res *TurnResult, maxCalls int, guard *repeatGuard) ([]Block, []Approval, error) {
	if l.Window != nil {
		l.Window.BeginBatch()
	}
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
			// Malformed JSON repeated byte for byte is the same runaway as a
			// failing call: keyed by the raw bytes and a fixed error.
			if n := guard.observeKey(c.Name+"\x00"+string(c.RawArgs)+"\x00invalid JSON", true); n >= RepeatFailureLimit {
				abort(i+1, SkippedResult)
				return results, approvals, repeatedFailure(c.Name, n, "the arguments are not valid JSON")
			}
			continue
		}

		if l.Window != nil {
			if l.Window.Handles(c.Name) {
				// The loader, or a call to a tool the model had not loaded:
				// answered here, run by nobody, so it needs no approval. The
				// loader's decision is recorded as auto; a not-run call has none.
				out := l.Window.Run(c, l.Builder.Snapshot(), l.roomLeft, l.pinnedTools())
				l.emitToolChanges(res)
				if c.Name == LoadToolsName {
					approvals = append(approvals, Approval{ToolUseID: c.ID, Decision: "auto"})
					if l.Log != nil {
						l.Log.Approval(ApprovalLog{CallID: c.ID, Tool: c.Name, Decision: "auto"})
					}
				}
				answer(c, out.Content, out.IsError)
				l.logCall(c, out.Content, out.IsError)
				if n := guard.observe(c, out); n >= RepeatFailureLimit {
					abort(i+1, SkippedResult)
					return results, approvals, repeatedFailure(c.Name, n, truncateForMessage(out.Content, 300))
				}
				continue
			}
			l.Window.Touch(c.Name)
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
				guard.reset() // a decline is new information: the model was told no
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
		if n := guard.observe(c, out); n >= RepeatFailureLimit {
			// End the turn the way the call limit does: answer the rest of
			// this reply's calls so every tool_use stays paired (settle then
			// flushes the turn).
			abort(i+1, SkippedResult)
			return results, approvals, repeatedFailure(c.Name, n, truncateForMessage(out.Content, 300))
		}
	}
	return results, approvals, nil
}

// repeatGuard counts consecutive identical failing calls within one turn.
// Identity is the tool name, the byte-identical arguments and the error text.
// Any success, a different call or a different error starts the count over.
type repeatGuard struct {
	key string
	n   int
}

// observe records an executed call's outcome and returns the current streak.
func (g *repeatGuard) observe(c ToolUse, out ToolOutcome) int {
	return g.observeKey(c.Name+"\x00"+string(c.Args)+"\x00"+out.Content, out.IsError)
}

// observeKey counts key as the next identical failure, or resets on success.
func (g *repeatGuard) observeKey(key string, isErr bool) int {
	if !isErr {
		g.reset()
		return 0
	}
	if key == g.key {
		g.n++
	} else {
		g.key, g.n = key, 1
	}
	return g.n
}

func (g *repeatGuard) reset() { g.key, g.n = "", 0 }

func repeatedFailure(tool string, n int, lastErr string) error {
	return &turnError{
		msg: fmt.Sprintf(RepeatedFailureMessage, tool, n, lastErr),
		err: ErrRepeatedFailure,
	}
}

func truncateForMessage(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "..."
}

func (l *Loop) logCall(c ToolUse, content string, isErr bool) {
	if l.Log != nil {
		l.Log.ToolCall(ToolCallLog{CallID: c.ID, Name: c.Name, Args: c.Args, Content: content, IsError: isErr})
	}
}

// finishResults appends the single user entry that answers an assistant
// entry's tool_use blocks. Anthropic requires all results in one message.
//
// A failure (a caller-supplied NewID that repeats an id) is returned and ends
// the turn: the results are not in the history, so settle removes the
// assistant entry whose tool_use they answer.
func (l *Loop) finishResults(results []Block, approvals []Approval) error {
	if len(results) == 0 {
		return nil
	}
	e := Entry{ID: l.newID(), Role: RoleUser, Blocks: results, Approvals: approvals}
	if err := l.addTurn(e); err != nil {
		return fmt.Errorf("llm: recording tool results: %w", err)
	}
	return nil
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
	res.Usage.CacheWrite5m += u.CacheWrite5m
	res.Usage.CacheWrite1h += u.CacheWrite1h
	if l.Price != nil {
		c := l.Price.Estimate(u)
		res.Cost.USD += c.USD
		ev.Cost = &c
	}
	if l.Log != nil {
		l.Log.Request(RequestLog{Provider: l.Model.Provider(), Model: l.Model.Model(), RequestRecord: snap.Record(), Usage: u})
	}
	l.emit(ev)

	prompt := promptTokens(u)
	l.haveUsage = true
	l.lastTokens = prompt + u.Output
	l.maybeWarn(prompt, false)
}

// maybeWarn emits the one structured warning per Loop when a prompt reaches
// PriceCard.WarnTokens: after a response (the reported prompt) and before a
// send (the same estimate the budget uses), so a single large tool result
// cannot carry the chat over the threshold unwarned. The loop carries no
// wording; the chat layer turns the event into text.
func (l *Loop) maybeWarn(prompt int64, beforeSend bool) {
	if l.Price == nil || l.Price.WarnTokens <= 0 || l.warned || prompt < int64(l.Price.WarnTokens) {
		return
	}
	l.warned = true
	l.emit(LoopEvent{Kind: LoopWarning, Warning: &PriceWarning{
		Threshold:    l.Price.WarnTokens,
		PromptTokens: prompt,
		TierTokens:   l.Price.TierTokens,
		Model:        l.Model.Model(),
		BeforeSend:   beforeSend,
	}})
}

// estimate is the prompt size, in tokens, the next request will have: the
// larger of (a) the whole snapshot at bytes/4 (Snapshot.Record) and (b) the
// last response's reported prompt plus output tokens, plus bytes/4 for the
// entries added since. (b) is exact up to the new content, (a) covers the
// first request of a chat and cannot be lower than what is really there by
// much. The budget and the pre-send warning both use it.
func (l *Loop) estimate(snap Snapshot) int {
	rec := snap.Record()
	est := 0
	for h := HookTools; h <= HookTurn; h++ {
		est += rec.Tokens[h]
	}
	if l.haveUsage {
		known := int(l.lastTokens)
		extra := 0
		if l.Window != nil {
			// The sent view can shrink between requests (an eviction), so what
			// changed since the last response is a signed byte difference of
			// the whole view, not the bytes of the entries added.
			extra = (viewBytes(snap) - l.lastBytes) / 4
		} else {
			for i, it := range conversationItems(snap) {
				if i >= l.lastCount {
					extra += itemBytes(it) / 4
				}
			}
		}
		est = max(est, known+extra)
	}
	return est
}

// replyReserve is the room the budget keeps for the reply. The window covers
// input and output together (Ollama's num_ctx does), so the reserve is the
// model's own Caps().MaxOutput. A model that reports none (Ollama with no
// cap), or one whose MaxOutput is not below its window (which would refuse
// every request), reserves a quarter of the window instead.
func replyReserve(c Caps) int {
	if c.MaxOutput <= 0 || c.MaxOutput >= c.ContextWindow {
		return c.ContextWindow / 4
	}
	return c.MaxOutput
}

// checkBudget refuses a request that cannot fit the context window, before
// anything is sent. The rule:
//
//	need = estimated prompt tokens + replyReserve(Caps())
//	refuse when need > Caps().ContextWindow
//
// A model whose Caps report no ContextWindow is not pre-checked; the adapter's
// ErrContextFull still ends the request.
func (l *Loop) checkBudget(snap Snapshot, est int) error {
	reserve, window := l.budget()
	if window <= 0 {
		return nil
	}
	if est+reserve > window {
		if isOpening(snap) {
			return l.openingTooBig(est, reserve, window)
		}
		return &turnError{
			msg: fmt.Sprintf("%s (about %d prompt tokens plus %d reserved for the reply, window %d)",
				contextFullMessage, est, reserve, window),
			err: ErrContextFull,
		}
	}
	return nil
}

// budget is the reply reserve and the context window the request must fit;
// window 0 means the model reports none and nothing is pre-checked.
func (l *Loop) budget() (reserve, window int) {
	caps := l.Model.Caps()
	if caps.ContextWindow <= 0 {
		return 0, 0
	}
	if l.Options.MaxOutput > 0 {
		// The request's own cap wins over the model's default.
		caps.MaxOutput = l.Options.MaxOutput
	}
	return replyReserve(caps), caps.ContextWindow
}

// view is the snapshot a request carries: the Builder's own, or, for an
// adaptive provider, its projection through the ToolWindow.
func (l *Loop) view() Snapshot {
	snap := l.Builder.Snapshot()
	if l.Window != nil {
		p := l.Window.Project(snap)
		// Ended turns shrink to stubs; the running turn is sent whole. With no
		// known window there is no room to derive the excerpt from: no stubs.
		reserve, window := l.budget()
		return l.Window.StubEnded(p, window-reserve)
	}
	return snap
}

// viewBytes is the size in bytes of everything a snapshot sends.
func viewBytes(s Snapshot) int {
	n := 0
	for h := HookTools; h <= HookTurn; h++ {
		for _, it := range s.Hooks[h] {
			n += itemBytes(it)
		}
	}
	return n
}

// roomLeft is the tokens the next request has room for: the window less the
// reserve less the estimate of the view as it is now (negative when over). A
// model that reports no window has unbounded room.
func (l *Loop) roomLeft() int {
	reserve, window := l.budget()
	if window <= 0 {
		return unboundedRoom
	}
	return window - reserve - l.estimate(l.view())
}

// pinnedTools are the tools that must stay loaded: every tool the model has
// called in this turn, this response's calls included (they are in hook 7
// before they are answered).
func (l *Loop) pinnedTools() Pinned {
	pinned := Pinned{}
	for _, it := range l.Builder.Snapshot().Hooks[HookTurn] {
		if it.Entry != nil {
			for _, c := range toolUses(it.Entry.Blocks) {
				pinned[c.Name] = true
			}
		}
	}
	return pinned
}

// relieve evicts loaded tools, least recently used first, while the request
// would not fit, and stops when it fits or nothing may be evicted (then
// checkBudget refuses). It runs between requests only: every call of the turn
// so far has been answered, and the pinned set keeps it that way if the loop
// order ever changes.
func (l *Loop) relieve(res *TurnResult, snap Snapshot, est int) (Snapshot, int) {
	reserve, window := l.budget()
	if window <= 0 {
		return snap, est
	}
	for est+reserve > window {
		if _, ok := l.Window.EvictOne(l.pinnedTools()); !ok {
			break
		}
		l.emitToolChanges(res)
		snap = l.view()
		est = l.estimate(snap)
	}
	return snap, est
}

// preload loads the ranker's guesses for this turn's message before the first
// request. It adds to the window within its budget and never fails the turn.
func (l *Loop) preload(ctx context.Context, text string, res *TurnResult) {
	if l.Window == nil {
		return
	}
	reserve, window := l.budget()
	if window <= 0 {
		return // no window reported: P cannot be derived, so nothing is guessed
	}
	l.Window.Preload(ctx, text, l.Builder.Snapshot(), window-reserve, l.roomLeft())
	for _, ch := range l.Window.changes {
		if ch.RankerError != "" {
			res.RankerError = ch.RankerError
		}
	}
	res.Preloaded = l.Window.Picks()
	l.emitToolChanges(res)
}

// reportPreloadHits records which preloaded tools the model called this turn.
func (l *Loop) reportPreloadHits(res *TurnResult) {
	if l.Window == nil || len(res.Preloaded) == 0 {
		return
	}
	called := l.pinnedTools()
	for _, n := range res.Preloaded {
		if called[n] {
			res.PreloadHits = append(res.PreloadHits, n)
		}
	}
}

// emitToolChanges reports the window's pending changes to the sink and counts
// them in the turn result.
func (l *Loop) emitToolChanges(res *TurnResult) {
	for _, ch := range l.Window.TakeChanges() {
		res.Loads += len(ch.Loaded)
		res.Evictions += len(ch.Evicted)
		c := ch
		l.emit(LoopEvent{Kind: LoopToolsChanged, Tools: &c})
	}
}

// OpeningTooBigMessage is shown when the very first request of a chat
// (tool definitions, instructions and the first message, with nothing said
// yet) does not fit the window. The arguments are the estimated opening size,
// the window, the reply reserve and the room left after it.
const OpeningTooBigMessage = "The opening of this chat (the tool definitions, the repo instructions and your message) needs about %d tokens, but the context window of %s is %d and %d of it is reserved for the reply, which leaves %d. Use a model with a larger window%s."

// OllamaRaiseNumCtxHint is appended for an Ollama provider.
const OllamaRaiseNumCtxHint = ", or raise num_ctx for this provider in knomit.toml"

// isOpening reports whether snap is a chat's first request: nothing in the
// conversation hook and only the first user entry in the turn hook.
func isOpening(snap Snapshot) bool {
	return len(snap.Hooks[HookConversation]) == 0 && len(snap.Hooks[HookTurn]) <= 1
}

// openingTooBig is the pre-send error for an opening whose estimate plus the
// reply reserve exceeds the window. It names the sizes so the user knows what
// to change; it never suggests a number, because the toolset's size varies
// with the repos and the permissions.
func (l *Loop) openingTooBig(est, reserve, window int) error {
	hint := ""
	if l.Model.Provider() == "ollama" {
		hint = OllamaRaiseNumCtxHint
	}
	left := max(window-reserve, 0)
	return &turnError{
		msg: fmt.Sprintf(OpeningTooBigMessage, est, l.Model.Model(), window, reserve, left, hint),
		err: ErrContextFull,
	}
}

// OpeningRefusedByServerMessage is shown when the server refused an
// opening that the local estimate says fits the window the model reports: the
// estimate and the window are therefore not what the server enforces. The
// arguments are the estimate, the model, the reported window and a hint.
const OpeningRefusedByServerMessage = "The server refused the opening of this chat even though its estimated size (about %d tokens) fits the context window reported for %s (%d)%s."

// OllamaServerContextHint is appended for an Ollama provider.
const OllamaServerContextHint = ". The server's context (num_ctx) is likely smaller than the opening, so raise num_ctx for this provider in knomit.toml"

// openingRefused is the error for an opening the adapter (the server) refused
// with ErrContextFull although the local estimate fit. It makes no claim about
// room left, because by the estimate there is room.
func (l *Loop) openingRefused(est, window int, cause error) error {
	hint := ""
	if l.Model.Provider() == "ollama" {
		hint = OllamaServerContextHint
	}
	return &turnError{
		msg: fmt.Sprintf(OpeningRefusedByServerMessage, est, l.Model.Model(), window, hint),
		err: cause,
	}
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

// baselineState is what the budget measures from; settle restores it when a
// turn is dropped, so the dropped text is not counted again.
type baselineState struct {
	haveUsage  bool
	lastTokens int64
	lastCount  int
	lastBytes  int
}

func (l *Loop) baseline() baselineState {
	return baselineState{l.haveUsage, l.lastTokens, l.lastCount, l.lastBytes}
}

// settle ends a turn: it makes hook 7 valid, then flushes it to hook 6, or
// drops it when the model never answered. Valid means every tool_use has its
// tool_result, so a trailing assistant entry whose results could not be
// recorded is removed. A turn left with no assistant entry (refused or failed
// on its first request, cancelled before any reply) is dropped whole: its user
// text would otherwise sit in the history and ride along on every later
// request, which for a refusal resends the very content the provider declined.
// err is the turn's error; a flush failure is returned in its place only when
// there is none.
func (l *Loop) settle(res *TurnResult, base baselineState, err error) error {
	l.reportPreloadHits(res)
	turn := l.Builder.Snapshot().Hooks[HookTurn]
	if n := len(turn); n > 0 {
		if last := turn[n-1].Entry; last != nil && last.Role == RoleAssistant && len(toolUses(last.Blocks)) > 0 {
			_ = l.Builder.Remove(turn[n-1].ID)
			turn = turn[:n-1]
		}
	}
	answered := false
	for _, it := range turn {
		if it.Entry != nil && it.Entry.Role == RoleAssistant {
			answered = true
		}
	}
	if !answered {
		l.dropTurn()
		l.haveUsage, l.lastTokens, l.lastCount, l.lastBytes = base.haveUsage, base.lastTokens, base.lastCount, base.lastBytes
		return err
	}
	if ferr := l.flush(res); ferr != nil && err == nil {
		return ferr
	}
	return err
}

// flush moves the finished turn from hook 7 into hook 6: Remove from hook 7,
// then Add to hook 6. Hook 6 only ever grows. The first failure is returned.
func (l *Loop) flush(res *TurnResult) error {
	var first error
	for _, it := range l.Builder.Snapshot().Hooks[HookTurn] {
		if err := l.Builder.Remove(it.ID); err != nil {
			first = cmpFirst(first, err)
			continue
		}
		moved := it
		moved.Hook = HookConversation
		if err := l.Builder.Add(moved); err != nil {
			first = cmpFirst(first, err)
			continue
		}
		res.Entries = append(res.Entries, *it.Entry)
		if l.Log != nil {
			l.Log.Turn(*it.Entry)
		}
	}
	// lastCount counted hooks 6+7 together, which flushing does not change.
	return first
}

func cmpFirst(first, err error) error {
	if first != nil {
		return first
	}
	return fmt.Errorf("llm: moving the turn into the conversation: %w", err)
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
