package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/edge/mcp"
	"knomit/internal/engine/llm"
)

// chatStreamGrace is how long a chat lives with no events stream attached
// (plan P11). A package variable so tests can shorten it; production never
// changes it.
var chatStreamGrace = 60 * time.Second

// maxApprovalArgs bounds the arguments echoed in an approval event; a larger
// call is shown without them (the call file keeps them in full).
const maxApprovalArgs = 16 * 1024

// maxTurnEndWait bounds how long ending a chat waits for its turn goroutine to
// unwind after its context was cancelled.
const maxTurnEndWait = 10 * time.Second

// sessionContextID is the first item of hook 6 (plan P17): the chat id and
// today's UTC date, the only per-session bytes that belong in a prefix-shaped
// request.
const sessionContextID = "session-context"

// viewItem is one row of the transcript the page renders. Tool results are not
// here: they are fetched lazily from the call file (GET .../calls/{callId}).
type viewItem struct {
	Kind      string          `json:"kind"` // user | assistant | tool_call
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	State     string          `json:"state,omitempty"` // running | done
	IsError   bool            `json:"is_error,omitempty"`
	// On a "tool_call" item, once done: how long the tool ran and a small
	// summary of its result when one is cheap to compute.
	DurationMS int64        `json:"duration_ms,omitempty"`
	Summary    *callSummary `json:"summary,omitempty"`

	// On a "user" item only: what the page shows for the turn it opens, so a
	// snapshot is the whole truth about the turn and the page patches nothing.
	Usage   *turnUsage `json:"usage,omitempty"`    // summed over the turn's requests
	CostUSD *float64   `json:"cost_usd,omitempty"` // summed; absent without a price card
	Warning string     `json:"warning,omitempty"`  // the price-tier warning, worded
	// WarningInfo is the same warning, structured, for the page to word its own line.
	WarningInfo *noticeView `json:"warning_info,omitempty"`
	// ErrorInfo is the structured detail of Error when it has one (context full).
	ErrorInfo *noticeView `json:"error_info,omitempty"`
	Error     string      `json:"error,omitempty"`     // how the turn ended, if it did not succeed
	Cancelled bool        `json:"cancelled,omitempty"` // Error is the person's own Stop
}

// callSummary is a finished call's result in one number: a query's result count.
type callSummary struct {
	Count int `json:"count"`
}

// noticeView is a notice's numbers, so the page words the line itself. Kind is
// "price_tier" (PromptTokens, Threshold, TierTokens, Mult, Model) or
// "context_full" (Needed, Window; Needed is 0 when only the provider knew).
type noticeView struct {
	Kind         string  `json:"kind"`
	PromptTokens int64   `json:"prompt_tokens,omitempty"`
	Threshold    int     `json:"threshold,omitempty"`
	TierTokens   int     `json:"tier_tokens,omitempty"`
	Mult         float64 `json:"mult,omitempty"`
	Model        string  `json:"model,omitempty"`
	Needed       int     `json:"needed,omitempty"`
	Window       int     `json:"window,omitempty"`
}

// summarize words a finished call's result as a number, when that is cheap:
// only knomit_query has one (the facts it returned).
func summarize(name, content string, isErr bool) *callSummary {
	if isErr || name != "knomit_query" {
		return nil
	}
	var r struct {
		Facts []json.RawMessage `json:"facts"`
	}
	if json.Unmarshal([]byte(content), &r) != nil || r.Facts == nil {
		return nil
	}
	return &callSummary{Count: len(r.Facts)}
}

// turnUsage is a turn's token counts, summed over its requests.
type turnUsage struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
}

// pendingApproval is a tool call waiting for the person.
type pendingApproval struct {
	CallID      string          `json:"call_id"`
	Tool        string          `json:"tool"`
	Action      string          `json:"action"`
	Target      string          `json:"target,omitempty"`
	TargetKind  string          `json:"target_kind,omitempty"`
	Destructive bool            `json:"destructive"`
	Grantable   bool            `json:"grantable"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
	// Turn is the view index of the user item of the turn the call belongs to.
	Turn int `json:"turn"`

	pin string
	ch  chan string // the decision: allow | decline | allow_session
}

// grantKey is the approval-grant key (plan P16): the binding pin the call's
// handle resolves to, the tool and the action. The pin is what makes a grant
// for repo A not cover repo B, and two handles on one repo share one grant.
type grantKey struct{ pin, tool, action string }

// grantInfo is a grant as GET .../grants lists it.
type grantInfo struct {
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Tool   string `json:"tool"`
	Action string `json:"action"`
}

type callMeta struct{ action, pin string }

// subscriber is one attached events stream.
type subscriber struct {
	ch chan []byte
}

// chat is one live chat: its MCP session, loop, approvals, grants and temp
// files. Every field below mu is guarded by it.
type chat struct {
	id, provider, model string
	owner               string
	m                   *manager

	sess  *mcp.InProcessSession
	loop  *llm.Loop
	store *chatStore
	log   *sessionLog

	ctx    context.Context
	cancel context.CancelFunc

	traceTools map[string]bool // tools whose schema declares `trace`

	mu         sync.Mutex
	ended      bool
	running    bool
	turnCancel context.CancelFunc
	turnSend   string   // the send_id of the running turn, if it had one
	sends      []string // send ids seen, oldest first (bounded): see sendState
	sendState  map[string]string
	turnBegan  time.Time // when the running turn was accepted
	firstSeen  bool      // the running turn has shown its first sign of life
	lastPrompt int64     // the last request's prompt tokens (input + cache read + cache write)
	turns      sync.WaitGroup
	view       []viewItem
	turnAt     int // index in view of the running (or last) turn's user item
	calls      map[string]llm.ToolUse
	pending    map[string]*pendingApproval
	grants     map[grantKey]grantInfo
	meta       map[string]callMeta
	subs       map[*subscriber]struct{}
	attached   int
	grace      *time.Timer

	// Per-request flags the truncation wording needs: whether the last
	// request streamed text, and whether it streamed reasoning.
	reqText, reqReasoning   bool
	lastText, lastReasoning bool
	usageSeq                int // requests whose usage was reported, for the usage event's seq

	endOnce sync.Once
	done    chan struct{}
}

var _ llm.CallGate = (*chat)(nil)

// ---- the approval gate ----

// Decide implements llm.CallGate. mcp.ClassifyCall decides: a read-only call
// runs. A side-effecting call looks up the grant (pin, tool, action); with
// none it emits an approval event and blocks until the person answers, the
// turn is cancelled or the chat ends. A side-effecting call whose target does
// not resolve (no handle, a handle that names nothing, or a failed lookup)
// fails closed: it asks, and cannot be granted. An unknown tool or action
// always asks and is never grantable.
func (c *chat) Decide(ctx context.Context, call llm.ToolUse) (llm.Decision, error) {
	var args map[string]any
	_ = json.Unmarshal(call.Args, &args)
	eff, known := mcp.ClassifyCall(call.Name, args)
	if known && eff.ReadOnly {
		return llm.Decision{Allow: true, Kind: "auto"}, nil
	}
	action := eff.Action
	switch {
	case known && action == "":
		action = strings.TrimPrefix(call.Name, "knomit_")
	case !known:
		action, _ = args["action"].(string)
	}
	pin, pinOK, pinErr := c.sess.TargetPin(ctx, args)
	if err := ctx.Err(); err != nil {
		// A cancelled turn must not flash an approval card (TargetPin returns
		// the cancellation as its error).
		return llm.Decision{}, err
	}
	// Fail closed: a write whose target did not resolve (the lookup failed,
	// the call carries no handle, or the handle names nothing) asks and is not
	// grantable. It is never run unprompted: the later binding gate refusing it
	// is a second line, not the approval.
	unresolved := pinErr != nil || !pinOK
	if pinErr != nil {
		log.Warn().Err(pinErr).Str("chat", c.id).Str("tool", call.Name).Msg("chat: cannot resolve the call's binding handle; asking")
	} else if !pinOK {
		log.Warn().Str("chat", c.id).Str("tool", call.Name).Msg("chat: write call has no resolvable binding handle; asking")
	}
	var kind, name string
	if !unresolved {
		kind, name = c.m.targetOf(pin)
	}

	c.mu.Lock()
	c.meta[call.ID] = callMeta{action: action, pin: pin}
	// Unconditional: grants are only stored for grantable calls, so a key of
	// an unknown or unresolved call can never be present.
	if _, ok := c.grants[grantKey{pin, call.Name, action}]; ok {
		c.mu.Unlock()
		return llm.Decision{Allow: true, Kind: "grant"}, nil
	}
	p := &pendingApproval{
		CallID: call.ID, Tool: call.Name, Action: action, Target: name, TargetKind: kind,
		Destructive: known && eff.Destructive, Grantable: known && !unresolved, pin: pin,
		ch: make(chan string, 1),
	}
	if len(call.Args) <= maxApprovalArgs {
		p.Arguments = call.Args
	}
	p.Turn = c.turnAt
	c.pending[call.ID] = p
	c.mu.Unlock()

	c.emit("approval", p)
	select {
	case d := <-p.ch:
		switch d {
		case "decline":
			return llm.Decision{Allow: false, Kind: "decline"}, nil
		case "allow_session":
			return llm.Decision{Allow: true, Kind: "allow_session"}, nil
		}
		return llm.Decision{Allow: true, Kind: "allow"}, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, call.ID)
		c.mu.Unlock()
		return llm.Decision{}, ctx.Err()
	}
}

// answerApproval resolves a pending approval. It reports false when no such
// call is pending, and errNotGrantable when allow_session is asked of a call
// that may not be granted.
var errNotGrantable = errors.New("this call cannot be allowed for the session")

func (c *chat) answerApproval(callID, decision string) (bool, error) {
	c.mu.Lock()
	p := c.pending[callID]
	if p == nil {
		c.mu.Unlock()
		return false, nil
	}
	if decision == "allow_session" {
		if !p.Grantable {
			c.mu.Unlock()
			return true, errNotGrantable
		}
		c.grants[grantKey{p.pin, p.Tool, p.Action}] = grantInfo{Target: p.Target, Kind: p.TargetKind, Tool: p.Tool, Action: p.Action}
	}
	delete(c.pending, callID)
	c.mu.Unlock()
	p.ch <- decision
	return true, nil
}

// grantList is the chat's grants, sorted.
func (c *chat) grantList() []grantInfo {
	c.mu.Lock()
	out := make([]grantInfo, 0, len(c.grants))
	for _, g := range c.grants {
		out = append(out, g)
	}
	c.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		if a.Tool != b.Tool {
			return a.Tool < b.Tool
		}
		return a.Action < b.Action
	})
	return out
}

// revokeGrant removes the grant g describes (as grantList shows it) and
// reports whether there was one. Nothing else is touched: a grant for another
// target, tool or action stays in force, and a call waiting for approval is
// not decided by it.
func (c *chat) revokeGrant(g grantInfo) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	found := false
	for k, v := range c.grants {
		if v == g {
			delete(c.grants, k)
			found = true
		}
	}
	return found
}

func (c *chat) callMetaOf(callID string) (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.meta[callID]
	return m.action, m.pin
}

// ---- events ----

// emit sends one SSE event to every attached stream. A stream that cannot keep
// up is dropped (its channel closed); the client reconnects and refetches.
func (c *chat) emit(kind string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	frame := []byte("event: " + kind + "\ndata: " + string(b) + "\n\n")
	c.mu.Lock()
	defer c.mu.Unlock()
	for s := range c.subs {
		select {
		case s.ch <- frame:
		default:
			delete(c.subs, s)
			close(s.ch)
		}
	}
}

// attach registers an events stream and cancels the grace timer. ok is false
// when the chat has ended.
func (c *chat) attach() (*subscriber, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return nil, false
	}
	s := &subscriber{ch: make(chan []byte, 256)}
	c.subs[s] = struct{}{}
	c.attached++
	if c.grace != nil {
		c.grace.Stop()
		c.grace = nil
	}
	return s, true
}

// detach removes a stream; the last one out starts the grace timer.
func (c *chat) detach(s *subscriber) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.subs[s]; ok {
		delete(c.subs, s)
		close(s.ch)
	}
	c.attached--
	if c.attached <= 0 && !c.ended && c.grace == nil {
		c.grace = time.AfterFunc(chatStreamGrace, func() { c.end("stream_grace") })
	}
}

// ---- the turn ----

var errBusy = errors.New("a turn is already running")
var errWithdrawn = errors.New("the send was withdrawn before it was accepted")

// maxSends bounds how many send ids a chat remembers.
const maxSends = 256

// noteSend records a send id's state ("started" or "withdrawn"); c.mu is held.
func (c *chat) noteSend(id, state string) {
	if c.sendState == nil {
		c.sendState = map[string]string{}
	}
	if _, ok := c.sendState[id]; !ok {
		c.sends = append(c.sends, id)
		if len(c.sends) > maxSends {
			delete(c.sendState, c.sends[0])
			c.sends = c.sends[1:]
		}
	}
	c.sendState[id] = state
}

// cancelSend is Stop for a send the page can name. A turn that already runs
// under that id is cancelled. A send that has not been accepted yet (its POST
// is still on the way) is withdrawn: when it arrives it is refused, and no turn
// starts. A send whose turn already ended is left alone.
func (c *chat) cancelSend(id string) {
	c.mu.Lock()
	if c.running && c.turnSend == id {
		cancel := c.turnCancel
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	if _, seen := c.sendState[id]; !seen {
		c.noteSend(id, "withdrawn")
	}
	c.mu.Unlock()
}

var errEnded = errors.New("the chat has ended")

// startTurn runs one user turn on its own goroutine.
// It returns the view index of the turn's user item, which the 202 carries.
func (c *chat) startTurn(text, sendID string) (int, error) {
	c.mu.Lock()
	if c.ended {
		c.mu.Unlock()
		return 0, errEnded
	}
	if sendID != "" && c.sendState[sendID] == "withdrawn" {
		c.mu.Unlock()
		return 0, errWithdrawn
	}
	if c.running {
		c.mu.Unlock()
		return 0, errBusy
	}
	c.running = true
	ctx, cancel := context.WithCancel(c.ctx)
	c.turnCancel = cancel
	c.turnSend = sendID
	if sendID != "" {
		c.noteSend(sendID, "started")
	}
	c.turnBegan, c.firstSeen = time.Now(), false
	c.turnAt = len(c.view)
	turn := c.turnAt
	c.view = append(c.view, viewItem{Kind: "user", Text: text})
	c.turns.Add(1)
	c.mu.Unlock()
	c.store.appendTranscript(viewItem{Kind: "user", Text: text})

	go func() {
		defer c.turns.Done()
		defer cancel()
		res, err := c.loop.RunTurn(ctx, text)
		c.finishTurn(ctx, res, err)
	}()
	return turn, nil
}

func (c *chat) finishTurn(ctx context.Context, res *llm.TurnResult, err error) {
	// How the turn ended is worded once, recorded in the turn's view item in the
	// same step that ends it (a snapshot cut after `running` is false has it),
	// and then sent.
	msg, cancelled := "", false
	if err != nil {
		msg = err.Error()
		cancelled = errors.Is(err, llm.ErrCancelled) || ctx.Err() != nil
	}
	c.mu.Lock()
	turn := c.turnAt // before running is false: the next send moves turnAt
	c.running = false
	c.turnCancel = nil
	if err != nil && errors.Is(err, llm.ErrTruncated) && c.lastReasoning && !c.lastText {
		msg = reasoningTruncatedMessage
	}
	if err != nil && c.turnAt < len(c.view) {
		c.view[c.turnAt].Error, c.view[c.turnAt].Cancelled = msg, cancelled && msg == err.Error()
	}
	truncatedBare := msg == reasoningTruncatedMessage
	var errInfo *noticeView
	if err != nil && errors.Is(err, llm.ErrContextFull) {
		needed, window, _ := llm.ContextFullInfo(err)
		if window <= 0 {
			window, _, _ = contextOf(c.loop.Model)
		}
		errInfo = &noticeView{Kind: "context_full", Needed: needed, Window: window}
		if c.turnAt < len(c.view) {
			c.view[c.turnAt].ErrorInfo = errInfo
		}
	}
	ended := c.ended
	c.mu.Unlock()
	c.store.appendTranscript(map[string]any{"turn_end": true, "error": errString(err)})
	if ended {
		return
	}
	if err == nil {
		c.emit("turn_done", map[string]any{
			"turn": turn, "stop_reason": res.StopReason, "requests": res.Requests, "tool_calls": res.ToolCalls,
			"usage": usageView(res.Usage), "cost_usd": res.Cost.USD,
		})
		return
	}
	if !truncatedBare && cancelled {
		c.emit("turn_error", map[string]any{"message": msg, "cancelled": true, "turn": turn})
		return
	}
	out := map[string]any{"message": msg, "turn": turn}
	if errInfo != nil {
		out["error_info"] = errInfo
	}
	c.emit("turn_error", out)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func usageView(u llm.Usage) map[string]any { return usageFields(u) }

// cancelTurn cancels the running turn, if any.
func (c *chat) cancelTurn() {
	c.mu.Lock()
	cancel := c.turnCancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// onLoopEvent is the loop's sink: it keeps the transcript view and call files
// and forwards the event to attached streams. It runs on the turn goroutine.
func (c *chat) onLoopEvent(ev llm.LoopEvent) {
	switch ev.Kind {
	case llm.LoopText, llm.LoopThinking, llm.LoopReasoning, llm.LoopToolUse:
		c.noteFirstSign()
	}
	switch ev.Kind {
	case llm.LoopThinking:
		c.mu.Lock()
		turn := c.turnAt
		c.mu.Unlock()
		c.emit("thinking_delta", map[string]any{"text": ev.Text, "turn": turn})
	case llm.LoopText:
		c.mu.Lock()
		c.reqText = true
		if n := len(c.view); n > 0 && c.view[n-1].Kind == "assistant" {
			c.view[n-1].Text += ev.Text
		} else {
			c.view = append(c.view, viewItem{Kind: "assistant", Text: ev.Text})
		}
		turn := c.turnAt
		c.mu.Unlock()
		c.emit("text_delta", map[string]any{"text": ev.Text, "turn": turn})
	case llm.LoopReasoning:
		c.mu.Lock()
		first := !c.reqReasoning
		c.reqReasoning = true
		c.mu.Unlock()
		if first {
			c.emit("reasoning", struct{}{})
		}
	case llm.LoopToolUse:
		tu := *ev.ToolUse
		c.mu.Lock()
		c.calls[tu.ID] = tu
		c.view = append(c.view, viewItem{Kind: "tool_call", ID: tu.ID, Name: tu.Name, Arguments: tu.Args, State: "running"})
		turn := c.turnAt
		c.mu.Unlock()
		out := map[string]any{"id": tu.ID, "name": tu.Name, "state": "running", "turn": turn}
		if len(tu.Args) > 0 && len(tu.Args) <= maxApprovalArgs {
			out["arguments"] = tu.Args
		}
		c.emit("tool_call", out)
	case llm.LoopToolResult:
		r := ev.Result
		c.mu.Lock()
		tu := c.calls[r.ToolUseID]
		ms := ev.Took.Milliseconds()
		if ev.Took > 0 && ms == 0 {
			ms = 1
		}
		sum := summarize(tu.Name, r.Content, r.IsError)
		for i := range c.view {
			if c.view[i].Kind == "tool_call" && c.view[i].ID == r.ToolUseID {
				c.view[i].State, c.view[i].IsError = "done", r.IsError
				c.view[i].DurationMS, c.view[i].Summary = ms, sum
			}
		}
		turn := c.turnAt
		c.mu.Unlock()
		if err := c.store.putCall(r.ToolUseID, callRecord{Name: tu.Name, Arguments: tu.Args, Result: r.Content, IsError: r.IsError, Elided: r.Elided}); err != nil {
			log.Warn().Err(err).Str("chat", c.id).Msg("chat: could not store a tool call")
		}
		done := map[string]any{"id": r.ToolUseID, "name": tu.Name, "state": "done", "is_error": r.IsError, "turn": turn}
		if ms > 0 {
			done["duration_ms"] = ms
		}
		if sum != nil {
			done["summary"] = sum
		}
		c.emit("tool_call", done)
	case llm.LoopUsage:
		c.mu.Lock()
		c.lastPrompt = ev.Usage.Input + ev.Usage.CacheRead + ev.Usage.CacheWrite
		c.lastText, c.lastReasoning = c.reqText, c.reqReasoning
		c.reqText, c.reqReasoning = false, false
		c.usageSeq++
		seq := c.usageSeq
		if c.turnAt < len(c.view) {
			it := &c.view[c.turnAt]
			// A NEW value every time: GET /chats/{id} copies the item under
			// this lock but encodes it after releasing it, so what a view
			// item points at must never change.
			var u turnUsage
			if it.Usage != nil {
				u = *it.Usage
			}
			u.Input += ev.Usage.Input
			u.CacheRead += ev.Usage.CacheRead
			u.CacheWrite += ev.Usage.CacheWrite
			u.Output += ev.Usage.Output
			it.Usage = &u
			if ev.Cost != nil {
				usd := ev.Cost.USD
				if it.CostUSD != nil {
					usd += *it.CostUSD
				}
				it.CostUSD = &usd
			}
		}
		turn := c.turnAt
		c.mu.Unlock()
		// seq numbers the chat's requests (1, 2, ...): a client that sees the
		// same one twice has seen the same request's usage twice.
		out := map[string]any{"usage": usageView(*ev.Usage), "seq": seq, "turn": turn}
		if ev.Cost != nil {
			out["cost_usd"] = ev.Cost.USD
		}
		c.emit("usage", out)
	case llm.LoopWarning:
		text := priceWarningText(*ev.Warning)
		info := &noticeView{
			Kind: "price_tier", PromptTokens: ev.Warning.PromptTokens, Threshold: ev.Warning.Threshold,
			TierTokens: ev.Warning.TierTokens, Mult: tierMult(c.loop.Price), Model: ev.Warning.Model,
		}
		c.mu.Lock()
		if c.turnAt < len(c.view) {
			c.view[c.turnAt].Warning = text
			c.view[c.turnAt].WarningInfo = info
		}
		turn := c.turnAt
		c.mu.Unlock()
		c.emit("warning", map[string]any{
			"turn": turn, "message": text, "kind": info.Kind, "threshold": info.Threshold,
			"prompt_tokens": info.PromptTokens, "tier_tokens": info.TierTokens, "mult": info.Mult, "model": info.Model,
		})
	}
}

// noteFirstSign records, once per turn, how long the model took to show its
// first sign of life (reasoning, text or a tool call) since the turn was
// accepted. The manager keeps the slowest per model for this server run.
func (c *chat) noteFirstSign() {
	c.mu.Lock()
	if c.firstSeen || c.turnBegan.IsZero() {
		c.mu.Unlock()
		return
	}
	c.firstSeen = true
	d := time.Since(c.turnBegan)
	c.mu.Unlock()
	c.m.noteFirstToken(c.provider, c.model, d)
}

// tierMult is the factor the input price rises by past the price tier: tier 2
// input over tier 1 input. 0 when the card has no second tier.
func tierMult(p *llm.PriceCard) float64 {
	if p == nil || p.InputPerMTok <= 0 || p.HighInputPerMTok <= 0 {
		return 0
	}
	return p.HighInputPerMTok / p.InputPerMTok
}

// argsHook adds `trace: {Knomit-Trace: chat:<id>}` to a call whose tool
// declares a trace argument and whose arguments do not already name
// Knomit-Trace (plan P17). The history keeps the arguments the model sent.
func (c *chat) argsHook(tu llm.ToolUse) json.RawMessage {
	if !c.traceTools[tu.Name] {
		return tu.Args
	}
	var args map[string]any
	if len(tu.Args) > 0 {
		if err := json.Unmarshal(tu.Args, &args); err != nil {
			return tu.Args
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	trace, _ := args["trace"].(map[string]any)
	if _, has := trace["Knomit-Trace"]; has {
		return tu.Args
	}
	if trace == nil {
		trace = map[string]any{}
	}
	trace["Knomit-Trace"] = "chat:" + c.id
	args["trace"] = trace
	b, err := json.Marshal(args)
	if err != nil {
		return tu.Args
	}
	return b
}

// ---- ending ----

// end finishes the chat once, whatever called it (the stream grace timer, a
// DELETE, the server's lifetime): it forgets the chat, cancels its context so
// a blocked approval or a running turn unwinds, waits for the turn, records
// the end in the MCP session (ended_at), writes chat_end to the session log
// and removes the temp files. The session log file stays. Later callers wait
// for the first to finish.
func (c *chat) end(reason string) {
	c.endOnce.Do(func() {
		defer close(c.done)
		c.mu.Lock()
		c.ended = true
		if c.grace != nil {
			c.grace.Stop()
			c.grace = nil
		}
		c.mu.Unlock()
		c.m.forget(c.id)
		c.cancel()

		waited := make(chan struct{})
		go func() { c.turns.Wait(); close(waited) }()
		select {
		case <-waited:
		case <-time.After(maxTurnEndWait):
			log.Warn().Str("chat", c.id).Msg("chat: a turn did not unwind in time")
		}

		c.emit("chat_end", map[string]string{"reason": reason})
		c.mu.Lock()
		for s := range c.subs {
			delete(c.subs, s)
			close(s.ch)
		}
		c.mu.Unlock()

		if err := c.sess.Close(context.Background()); err != nil {
			log.Warn().Err(err).Str("chat", c.id).Msg("chat: closing the MCP session failed")
		}
		c.log.End(reason)
		c.store.remove()
	})
	<-c.done
}

func (c *chat) String() string { return fmt.Sprintf("chat %s", c.id) }
