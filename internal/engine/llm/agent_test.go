package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fakes ----

type fakeStep struct {
	resp   *Response
	err    error
	deltas []string
	block  bool // wait for ctx to end, then return its error
}

type fakeModel struct {
	caps  Caps
	steps []fakeStep
	snaps []Snapshot
}

func (m *fakeModel) Provider() string { return "fake" }
func (m *fakeModel) Model() string    { return "claude-test-x" }
func (m *fakeModel) Caps() Caps       { return m.caps }
func (m *fakeModel) Stream(ctx context.Context, s Snapshot, _ StreamOptions, sink func(Event)) (*Response, error) {
	m.snaps = append(m.snaps, s)
	if len(m.steps) == 0 {
		return nil, errors.New("fakeModel: no step left")
	}
	st := m.steps[0]
	m.steps = m.steps[1:]
	for _, d := range st.deltas {
		sink(Event{Kind: EventTextDelta, Text: d})
	}
	if st.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return st.resp, st.err
}

type hostCall struct {
	name string
	args string
}

type fakeHost struct {
	calls []hostCall
	out   map[string]ToolOutcome
}

func (h *fakeHost) CallTool(_ context.Context, name string, args json.RawMessage) (ToolOutcome, error) {
	h.calls = append(h.calls, hostCall{name, string(args)})
	o, ok := h.out[name]
	if !ok {
		return ToolOutcome{}, fmt.Errorf("unknown tool %q", name)
	}
	return o, nil
}

type gateFn func(ctx context.Context, c ToolUse) (Decision, error)

func (g gateFn) Decide(ctx context.Context, c ToolUse) (Decision, error) { return g(ctx, c) }

func allow(context.Context, ToolUse) (Decision, error) {
	return Decision{Allow: true, Kind: "allow"}, nil
}

func textResp(text string, u Usage) *Response {
	return &Response{Blocks: []Block{{Kind: BlockText, Text: text}}, StopReason: StopEndTurn, Usage: u}
}

func toolResp(id, name, args string, u Usage) *Response {
	return &Response{
		Blocks:     []Block{{Kind: BlockToolUse, ToolUse: &ToolUse{ID: id, Name: name, Args: json.RawMessage(args)}}},
		StopReason: StopToolUse, Usage: u,
	}
}

func newLoop(t *testing.T, m *fakeModel, h ToolHost) (*Loop, *Builder) {
	t.Helper()
	b := NewBuilder()
	if err := b.Add(Item{ID: "tool.knomit_repos", Hook: HookTools, Tool: &ToolDef{Name: "knomit_repos", Description: "d", Schema: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(Item{ID: "system", Hook: HookSystem, Text: "sys"}); err != nil {
		t.Fatal(err)
	}
	return &Loop{Model: m, Builder: b, Tools: h}, b
}

func history(b *Builder) []Entry {
	var out []Entry
	for _, it := range b.Snapshot().Hooks[HookConversation] {
		out = append(out, *it.Entry)
	}
	return out
}

// find returns the tool_result for a tool_use id in the model's request k.
func resultIn(s Snapshot, id string) *ToolResult {
	for _, it := range conversationItems(s) {
		for _, bl := range it.Entry.Blocks {
			if bl.Kind == BlockToolResult && bl.ToolResult.ToolUseID == id {
				return bl.ToolResult
			}
		}
	}
	return nil
}

// ---- Task 4.1 ----

func TestLoop_RunsToolThenAnswers(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{
		{resp: toolResp("tu_1", "knomit_repos", `{"q":"x"}`, Usage{})},
		{resp: textResp("done", Usage{})},
	}}
	h := &fakeHost{out: map[string]ToolOutcome{"knomit_repos": {Content: "alpha,beta"}}}
	l, b := newLoop(t, m, h)

	res, err := l.RunTurn(context.Background(), "what repos?")
	if err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 1 || h.calls[0].name != "knomit_repos" || h.calls[0].args != `{"q":"x"}` {
		t.Fatalf("host calls = %+v, want one knomit_repos with the model's args", h.calls)
	}
	got := history(b)
	if len(got) != 4 {
		t.Fatalf("hook 6 has %d entries, want 4: %+v", len(got), got)
	}
	if got[0].Role != RoleUser || got[0].Blocks[0].Text != "what repos?" {
		t.Fatalf("entry 0 = %+v, want the user text", got[0])
	}
	if got[1].Role != RoleAssistant || got[1].Blocks[0].Kind != BlockToolUse || got[1].Blocks[0].ToolUse.ID != "tu_1" {
		t.Fatalf("entry 1 = %+v, want the assistant tool_use tu_1", got[1])
	}
	if r := got[2].Blocks[0].ToolResult; got[2].Role != RoleUser || r == nil || r.ToolUseID != "tu_1" || r.Content != "alpha,beta" || r.IsError {
		t.Fatalf("entry 2 = %+v, want the user tool_result for tu_1", got[2])
	}
	if got[3].Role != RoleAssistant || got[3].Blocks[0].Text != "done" {
		t.Fatalf("entry 3 = %+v, want assistant done", got[3])
	}
	if res.Text != "done" || res.StopReason != StopEndTurn || len(res.Entries) != 4 {
		t.Fatalf("result = %+v", res)
	}
	if n := len(b.Snapshot().Hooks[HookTurn]); n != 0 {
		t.Fatalf("hook 7 still holds %d items after the turn", n)
	}
	// The second request saw the tool_result in the turn hook (hook 7), the
	// first saw only the user text there: the turn is built in hook 7.
	if n := len(m.snaps[0].Hooks[HookTurn]); n != 1 {
		t.Fatalf("first request: hook 7 has %d entries, want 1", n)
	}
	if n := len(m.snaps[1].Hooks[HookTurn]); n != 3 {
		t.Fatalf("second request: hook 7 has %d entries, want 3", n)
	}
}

func TestLoop_DeclineReturnsToolError(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{
		{resp: toolResp("tu_1", "knomit_learn", `{}`, Usage{})},
		{resp: textResp("ok, not writing", Usage{})},
	}}
	h := &fakeHost{out: map[string]ToolOutcome{"knomit_learn": {Content: "written"}}}
	l, _ := newLoop(t, m, h)
	l.Gate = gateFn(func(context.Context, ToolUse) (Decision, error) { return Decision{Allow: false, Kind: "decline"}, nil })

	res, err := l.RunTurn(context.Background(), "write it")
	if err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 0 {
		t.Fatalf("host was called %d times for a declined call", len(h.calls))
	}
	r := resultIn(m.snaps[1], "tu_1")
	if r == nil || !r.IsError || r.Content != "The user declined this call. Do not retry it unless the user asks." {
		t.Fatalf("second request's tool_result = %+v, want the decline text with IsError", r)
	}
	if res.StopReason != StopEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", res.StopReason)
	}
}

func TestLoop_BadArgsAndUnknownToolAreToolErrors(t *testing.T) {
	bad := &Response{StopReason: StopToolUse, Blocks: []Block{
		{Kind: BlockToolUse, ToolUse: &ToolUse{ID: "tu_1", Name: "knomit_repos", Args: json.RawMessage(`{}`), RawArgs: `{"q":`}},
		{Kind: BlockToolUse, ToolUse: &ToolUse{ID: "tu_2", Name: "knomit_nonesuch", Args: json.RawMessage(`{}`)}},
	}}
	m := &fakeModel{steps: []fakeStep{{resp: bad}, {resp: textResp("sorry", Usage{})}}}
	h := &fakeHost{out: map[string]ToolOutcome{"knomit_repos": {Content: "never"}}}
	l, b := newLoop(t, m, h)

	if _, err := l.RunTurn(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	r1 := resultIn(m.snaps[1], "tu_1")
	if r1 == nil || !r1.IsError || !strings.Contains(r1.Content, `{"q":`) || !strings.Contains(r1.Content, "knomit_repos") {
		t.Fatalf("bad-args result = %+v, want an error naming the tool and the raw bytes", r1)
	}
	r2 := resultIn(m.snaps[1], "tu_2")
	if r2 == nil || !r2.IsError || !strings.Contains(r2.Content, "knomit_nonesuch") {
		t.Fatalf("unknown-tool result = %+v, want an error naming the tool", r2)
	}
	if len(h.calls) != 1 || h.calls[0].name != "knomit_nonesuch" {
		t.Fatalf("host calls = %+v: the bad-args call must not reach the host, the unknown tool must", h.calls)
	}
	// Both results sit in ONE user entry, after the assistant entry.
	got := history(b)
	if len(got) != 4 || len(got[2].Blocks) != 2 {
		t.Fatalf("history = %d entries, results entry has %d blocks; want 4 and 2", len(got), len(got[2].Blocks))
	}
}

func TestLoop_CancelWhileGateBlocks(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{{resp: toolResp("tu_1", "knomit_learn", `{}`, Usage{})}}}
	l, b := newLoop(t, m, &fakeHost{})
	entered := make(chan struct{})
	l.Gate = gateFn(func(ctx context.Context, _ ToolUse) (Decision, error) {
		close(entered)
		<-ctx.Done()
		return Decision{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := l.RunTurn(ctx, "go"); done <- err }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("err = %v, want ErrCancelled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not return within 1s of cancel")
	}
	got := history(b)
	last := got[len(got)-1]
	if r := last.Blocks[0].ToolResult; last.Role != RoleUser || r == nil || r.ToolUseID != "tu_1" || r.Content != "Cancelled by the user." {
		t.Fatalf("last hook-6 entry = %+v, want tu_1 paired with the cancelled result", last)
	}
}

func TestLoop_CancelDuringStreamKeepsPartialText(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{{deltas: []string{"par", "tial"}, block: true}}}
	l, b := newLoop(t, m, &fakeHost{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := l.RunTurn(ctx, "go")
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
	got := history(b)
	if len(got) != 2 || got[1].Role != RoleAssistant || got[1].Blocks[0].Text != "partial" {
		t.Fatalf("history = %+v, want user then the assistant's partial text", got)
	}
}

func TestLoop_MaxCalls(t *testing.T) {
	var steps []fakeStep
	for i := 1; i <= 5; i++ {
		steps = append(steps, fakeStep{resp: toolResp(fmt.Sprintf("tu_%d", i), "knomit_repos", `{}`, Usage{})})
	}
	m := &fakeModel{steps: steps}
	h := &fakeHost{out: map[string]ToolOutcome{"knomit_repos": {Content: "r"}}}
	l, b := newLoop(t, m, h)
	l.MaxCalls = 3
	_, err := l.RunTurn(context.Background(), "loop")
	if !errors.Is(err, ErrTooManyCalls) {
		t.Fatalf("err = %v, want ErrTooManyCalls", err)
	}
	if len(h.calls) != 3 {
		t.Fatalf("host calls = %d, want exactly 3", len(h.calls))
	}
	// The 4th tool_use is answered, not left dangling.
	got := history(b)
	last := got[len(got)-1]
	if r := last.Blocks[0].ToolResult; r == nil || r.ToolUseID != "tu_4" || !r.IsError {
		t.Fatalf("last entry = %+v, want tu_4 answered with an error", last)
	}
}

func TestLoop_OllamaStyleServerErrorFailsTheTurnWithoutRetry(t *testing.T) {
	boom := errors.New("llama-server returned invalid tool call arguments")
	m := &fakeModel{steps: []fakeStep{{err: boom}, {resp: textResp("never reached", Usage{})}}}
	l, _ := newLoop(t, m, &fakeHost{})
	_, err := l.RunTurn(context.Background(), "go")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the adapter's error", err)
	}
	if len(m.snaps) != 1 {
		t.Fatalf("model was asked %d times, want 1 (a turn error is not retried)", len(m.snaps))
	}
}

func TestLoop_RefusalAndTruncationEndTheTurn(t *testing.T) {
	refused := &StopError{Reason: StopRefusal, Category: "cyber", Response: &Response{
		Blocks: []Block{{Kind: BlockText, Text: "I can't"}}, StopReason: StopRefusal, Usage: Usage{Input: 7},
	}}
	m := &fakeModel{steps: []fakeStep{{err: refused}}}
	l, b := newLoop(t, m, &fakeHost{})
	res, err := l.RunTurn(context.Background(), "do the bad thing")
	if !errors.Is(err, ErrRefused) || err.Error() != "The model declined this request (category: cyber)." {
		t.Fatalf("err = %q (%v), want the refusal sentence wrapping ErrRefused", err, err)
	}
	if res.Usage.Input != 7 || len(m.snaps) != 1 {
		t.Fatalf("usage input = %d, requests = %d; want 7 and 1 (no retry)", res.Usage.Input, len(m.snaps))
	}
	// A turn refused on its first request is dropped whole: the refused text
	// is in neither hook, so no later request resends it.
	if got := history(b); len(got) != 0 {
		t.Fatalf("history after a first-request refusal = %+v, want empty", got)
	}
	if n := len(b.Snapshot().Hooks[HookTurn]); n != 0 || len(res.Entries) != 0 {
		t.Fatalf("hook 7 holds %d items, result %d entries; want 0 and 0", n, len(res.Entries))
	}
	m.steps = []fakeStep{{resp: textResp("fine", Usage{})}}
	if _, err := l.RunTurn(context.Background(), "a harmless question"); err != nil {
		t.Fatal(err)
	}
	next := conversationItems(m.snaps[1])
	if len(next) != 1 || next[0].Entry.Blocks[0].Text != "a harmless question" {
		t.Fatalf("the next request carries %d conversation items %+v; want only the new question, not the refused text", len(next), next)
	}
	if got := history(b); len(got) != 2 {
		t.Fatalf("hook 6 = %+v, want the new question and its answer", got)
	}

	cut := &StopError{Reason: StopMaxTokens, Response: &Response{
		Blocks:     []Block{{Kind: BlockText, Text: "half an ans"}, {Kind: BlockToolUse, ToolUse: &ToolUse{ID: "tu_9", Name: "knomit_repos", Args: json.RawMessage(`{}`)}}},
		StopReason: StopMaxTokens,
	}}
	m2 := &fakeModel{steps: []fakeStep{{err: cut}}}
	l2, b2 := newLoop(t, m2, &fakeHost{})
	_, err = l2.RunTurn(context.Background(), "long")
	if !errors.Is(err, ErrTruncated) || err.Error() != "The reply hit the output limit." {
		t.Fatalf("err = %q, want the truncation sentence wrapping ErrTruncated", err)
	}
	got := history(b2)
	if len(got) != 2 || got[1].Blocks[0].Text != "half an ans" || len(got[1].Blocks) != 1 {
		t.Fatalf("history = %+v, want the partial text kept and no dangling tool_use", got)
	}
}

func TestLoop_PauseTurnResendsWithoutNewUserMessage(t *testing.T) {
	pause := &Response{Blocks: []Block{{Kind: BlockText, Text: "working"}}, StopReason: StopPauseTurn}
	m := &fakeModel{steps: []fakeStep{{resp: pause}, {resp: textResp("finished", Usage{})}}}
	l, b := newLoop(t, m, &fakeHost{})
	res, err := l.RunTurn(context.Background(), "long job")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.snaps) != 2 {
		t.Fatalf("requests = %d, want 2", len(m.snaps))
	}
	second := conversationItems(m.snaps[1])
	if len(second) != 2 || second[0].Entry.Role != RoleUser || second[1].Entry.Role != RoleAssistant || second[1].Entry.Blocks[0].Text != "working" {
		t.Fatalf("second request's conversation = %d entries; want [user, paused assistant] with no new user message", len(second))
	}
	if got := history(b); len(got) != 3 || res.Text != "finished" {
		t.Fatalf("history = %d entries, text %q; want 3 and finished", len(got), res.Text)
	}
}

type recLog struct {
	mu  sync.Mutex
	ev  []string
	req []RequestLog
	tc  []ToolCallLog
	ap  []ApprovalLog
}

func (r *recLog) add(s string) { r.mu.Lock(); r.ev = append(r.ev, s); r.mu.Unlock() }
func (r *recLog) Request(x RequestLog) {
	r.req = append(r.req, x)
	r.add("request")
}
func (r *recLog) Turn(e Entry)           { r.add("turn:" + string(e.Role)) }
func (r *recLog) ToolCall(c ToolCallLog) { r.tc = append(r.tc, c); r.add("toolcall:" + c.Name) }
func (r *recLog) Approval(a ApprovalLog) { r.ap = append(r.ap, a); r.add("approval:" + a.Decision) }

func TestLoop_LogsRequestsCallsApprovals(t *testing.T) {
	u := Usage{Input: 11, CacheRead: 22, CacheWrite: 33, Output: 44}
	m := &fakeModel{steps: []fakeStep{
		{resp: toolResp("tu_1", "knomit_learn", `{"a":1}`, u)},
		{resp: textResp("done", Usage{Input: 55})},
	}}
	h := &fakeHost{out: map[string]ToolOutcome{"knomit_learn": {Content: "ok"}}}
	l, _ := newLoop(t, m, h)
	l.Gate = gateFn(allow)
	lg := &recLog{}
	l.Log = lg
	if _, err := l.RunTurn(context.Background(), "write"); err != nil {
		t.Fatal(err)
	}
	want := []string{"request", "approval:allow", "toolcall:knomit_learn", "request", "turn:user", "turn:assistant", "turn:user", "turn:assistant"}
	if strings.Join(lg.ev, ",") != strings.Join(want, ",") {
		t.Fatalf("log order = %v\nwant       %v", lg.ev, want)
	}
	r := lg.req[0]
	if r.Model != "claude-test-x" || r.Provider != "fake" || !sameTokens(r.Usage, u) || len(r.Items[HookTools]) == 0 {
		t.Fatalf("first request log = %+v", r)
	}
	if lg.req[1].Usage.Input != 55 {
		t.Fatalf("second request log usage = %+v, want input 55", lg.req[1].Usage)
	}
	if c := lg.tc[0]; c.CallID != "tu_1" || c.IsError || c.Content != "ok" || string(c.Args) != `{"a":1}` {
		t.Fatalf("tool call log = %+v", c)
	}

	// A declined call logs the decline and an error call.
	m2 := &fakeModel{steps: []fakeStep{{resp: toolResp("tu_1", "knomit_learn", `{}`, Usage{})}, {resp: textResp("fine", Usage{})}}}
	l2, _ := newLoop(t, m2, &fakeHost{})
	l2.Gate = gateFn(func(context.Context, ToolUse) (Decision, error) { return Decision{Kind: "decline"}, nil })
	lg2 := &recLog{}
	l2.Log = lg2
	if _, err := l2.RunTurn(context.Background(), "write"); err != nil {
		t.Fatal(err)
	}
	if len(lg2.ap) != 1 || lg2.ap[0].Decision != "decline" || len(lg2.tc) != 1 || !lg2.tc[0].IsError {
		t.Fatalf("decline log = approvals %+v calls %+v", lg2.ap, lg2.tc)
	}

	// Log == nil: the same run completes.
	m3 := &fakeModel{steps: []fakeStep{{resp: toolResp("tu_1", "knomit_learn", `{}`, u)}, {resp: textResp("done", Usage{})}}}
	l3, _ := newLoop(t, m3, h)
	l3.Gate = gateFn(allow)
	res3, err := l3.RunTurn(context.Background(), "write")
	if err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 2 || res3.ToolCalls != 1 || len(res3.Entries) != 4 || res3.Text != "done" {
		t.Fatalf("Log == nil run: host calls %d (1 from the first run + 1), tool calls %d, entries %d, text %q; want 2, 1, 4, done",
			len(h.calls), res3.ToolCalls, len(res3.Entries), res3.Text)
	}
}

func TestLoop_ArgsHookChangesWhatIsSentNotWhatIsStored(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{{resp: toolResp("tu_1", "knomit_repos", `{"a":1}`, Usage{})}, {resp: textResp("ok", Usage{})}}}
	h := &fakeHost{out: map[string]ToolOutcome{"knomit_repos": {Content: "r"}}}
	l, b := newLoop(t, m, h)
	l.ArgsHook = func(ToolUse) json.RawMessage { return json.RawMessage(`{"a":1,"trace":"chat:c1"}`) }
	if _, err := l.RunTurn(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if h.calls[0].args != `{"a":1,"trace":"chat:c1"}` {
		t.Fatalf("host got %s, want the hook's args", h.calls[0].args)
	}
	if got := string(history(b)[1].Blocks[0].ToolUse.Args); got != `{"a":1}` {
		t.Fatalf("history args = %s, want the model's own", got)
	}
}

// ---- Task 4.2 ----

func sameTokens(a, b Usage) bool {
	return a.Input == b.Input && a.CacheRead == b.CacheRead && a.CacheWrite == b.CacheWrite && a.Output == b.Output &&
		a.CacheWrite5m == b.CacheWrite5m && a.CacheWrite1h == b.CacheWrite1h
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestLoop_ReportsTurnTokensAndCost(t *testing.T) {
	u := Usage{Input: 11, CacheRead: 22, CacheWrite: 33, CacheWrite5m: 20, CacheWrite1h: 13, Output: 44}
	card := &PriceCard{
		InputPerMTok: 0.10, OutputPerMTok: 0.50, TierTokens: 100_000, HighInputPerMTok: 0.50, HighOutputPerMTok: 2.50,
		CacheReadMult: 0.1, CacheWrite5mMult: 1.25, CacheWrite1hMult: 2,
	}
	run := func(card *PriceCard, u Usage) (*TurnResult, []LoopEvent) {
		m := &fakeModel{steps: []fakeStep{{resp: textResp("hi", u)}}}
		l, _ := newLoop(t, m, &fakeHost{})
		l.Price = card
		var evs []LoopEvent
		l.Sink = func(e LoopEvent) { evs = append(evs, e) }
		res, err := l.RunTurn(context.Background(), "q")
		if err != nil {
			t.Fatal(err)
		}
		return res, evs
	}
	usageEv := func(evs []LoopEvent) LoopEvent {
		for _, e := range evs {
			if e.Kind == LoopUsage {
				return e
			}
		}
		t.Fatal("no usage event")
		return LoopEvent{}
	}

	res, evs := run(card, u)
	ev := usageEv(evs)
	if !sameTokens(*ev.Usage, u) {
		t.Fatalf("usage event = %+v, want %+v", *ev.Usage, u)
	}
	// ONE exact figure, each write at its own TTL's rate:
	// (11*0.10 + 22*0.10*0.1 + 44*0.50 + 20*0.10*1.25 + 13*0.10*2) / 1e6 USD
	if !approx(ev.Cost.USD, 28.42e-6) {
		t.Fatalf("cost = %+v, want 28.42e-6", *ev.Cost)
	}
	if !approx(res.Cost.USD, 28.42e-6) || res.Usage.CacheWrite5m != 20 || res.Usage.CacheWrite1h != 13 {
		t.Fatalf("turn cost/usage = %+v / %+v", res.Cost, res.Usage)
	}

	// A write the adapter did not split is priced at the 5m multiplier.
	_, evs = run(card, Usage{Input: 11, CacheRead: 22, CacheWrite: 33, Output: 44})
	// (1.1 + 0.22 + 22 + 33*0.10*1.25) / 1e6
	if c := usageEv(evs).Cost; !approx(c.USD, 27.445e-6) {
		t.Fatalf("unsplit write cost = %+v, want 27.445e-6", *c)
	}
	// Part split, part not: 7 at 1h, the other 26 at 5m.
	_, evs = run(card, Usage{Input: 11, CacheRead: 22, CacheWrite: 33, CacheWrite1h: 7, Output: 44})
	// 1.1 + 0.22 + 22 + 26*0.10*1.25 + 7*0.10*2
	if c := usageEv(evs).Cost; !approx(c.USD, 27.97e-6) {
		t.Fatalf("part-split write cost = %+v, want 27.97e-6", *c)
	}

	// The Sonnet/Opus card: cache read at 0.05x.
	card2 := *card
	card2.CacheReadMult = 0.05
	_, evs = run(&card2, u)
	if c := usageEv(evs).Cost; !approx(c.USD, 28.31e-6) {
		t.Fatalf("0.05x cache-read cost = %+v, want 28.31e-6", *c)
	}

	// Above the tier the High rates apply, on ALL prompt tokens (cache included).
	big := Usage{Input: 1000, CacheRead: 99_001, Output: 10}
	_, evs = run(card, big)
	// 1000*0.50 + 99001*0.50*0.1 + 10*2.50 = 500 + 4950.05 + 25
	if c := usageEv(evs).Cost; !approx(c.USD, 5475.05e-6) {
		t.Fatalf("tiered cost = %+v, want 5475.05e-6", *c)
	}
	at := Usage{Input: 100_000, Output: 10} // exactly the tier: still the base rate
	_, evs = run(card, at)
	if c := usageEv(evs).Cost; !approx(c.USD, (100_000*0.10+10*0.50)/1e6) {
		t.Fatalf("cost at the tier = %+v, want base rates", *c)
	}

	// No card: usage, no cost.
	_, evs = run(nil, u)
	if e := usageEv(evs); e.Cost != nil || !sameTokens(*e.Usage, u) {
		t.Fatalf("nil card event = %+v", e)
	}
}

func TestLoop_WarnsNearPriceCliff(t *testing.T) {
	card := &PriceCard{InputPerMTok: 1, TierTokens: 100_000, WarnTokens: 95_000}
	prompts := []int64{94_999, 95_000, 96_000, 120_000}
	var steps []fakeStep
	for _, p := range prompts {
		steps = append(steps, fakeStep{resp: textResp("a", Usage{Input: p})})
	}
	m := &fakeModel{steps: steps}
	l, _ := newLoop(t, m, &fakeHost{})
	l.Price = card
	var warnings []PriceWarning
	l.Sink = func(e LoopEvent) {
		if e.Kind == LoopWarning {
			warnings = append(warnings, *e.Warning)
		}
	}
	var perTurn []int
	for range prompts {
		before := len(warnings)
		if _, err := l.RunTurn(context.Background(), "q"); err != nil {
			t.Fatal(err)
		}
		perTurn = append(perTurn, len(warnings)-before)
	}
	if fmt.Sprint(perTurn) != "[0 1 0 0]" {
		t.Fatalf("warnings per turn = %v, want [0 1 0 0]: none at 94,999, one at 95,000, never again", perTurn)
	}
	if w := warnings[0]; w.Threshold != 95_000 || w.TierTokens != 100_000 || w.Model != "claude-test-x" || w.PromptTokens < 95_000 || w.PromptTokens > 95_100 {
		t.Fatalf("warning = %+v, want threshold 95000, tier 100000, model claude-test-x, prompt about 95000", w)
	}

	// Cache tokens count toward the prompt; and without a card there is no warning.
	m2 := &fakeModel{steps: []fakeStep{{resp: textResp("a", Usage{Input: 1000, CacheRead: 60_000, CacheWrite: 34_000})}}}
	l2, _ := newLoop(t, m2, &fakeHost{})
	l2.Price = card
	n := 0
	l2.Sink = func(e LoopEvent) {
		if e.Kind == LoopWarning {
			n++
		}
	}
	if _, err := l2.RunTurn(context.Background(), "q"); err != nil || n != 1 {
		t.Fatalf("err %v, warnings %d: want 1 for a 95,000-token prompt made of input + cache read + cache write", err, n)
	}
	// Before sending: one large tool result takes the prompt from 90K to well
	// past 95K inside a single turn. The warning comes BEFORE the request that
	// carries it, with the pre-send estimate, and only once.
	huge := strings.Repeat("z", 40_000*4) // about 40,000 tokens
	m4 := &fakeModel{steps: []fakeStep{
		{resp: toolResp("tu_1", "knomit_repos", `{}`, Usage{Input: 90_000, Output: 10})},
		{resp: textResp("done", Usage{Input: 131_000, Output: 5})},
	}}
	h4 := &fakeHost{out: map[string]ToolOutcome{"knomit_repos": {Content: huge}}}
	l4, _ := newLoop(t, m4, h4)
	l4.Price = card
	var pre []PriceWarning
	var requestsAtWarning []int
	l4.Sink = func(e LoopEvent) {
		if e.Kind == LoopWarning {
			pre = append(pre, *e.Warning)
			requestsAtWarning = append(requestsAtWarning, len(m4.snaps))
		}
	}
	if _, err := l4.RunTurn(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if len(pre) != 1 || requestsAtWarning[0] != 1 || !pre[0].BeforeSend || pre[0].PromptTokens < 130_000 || pre[0].PromptTokens > 131_000 {
		t.Fatalf("warnings = %+v at requests %v; want exactly one, before the second request, BeforeSend, prompt about 130,010", pre, requestsAtWarning)
	}

	m3 := &fakeModel{steps: []fakeStep{{resp: textResp("a", Usage{Input: 200_000})}}}
	l3, _ := newLoop(t, m3, &fakeHost{})
	l3.Sink = func(e LoopEvent) {
		if e.Kind == LoopWarning {
			t.Fatal("warning without a price card")
		}
	}
	if _, err := l3.RunTurn(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
}

func TestLoop_RefusesWhenContextFull(t *testing.T) {
	m := &fakeModel{
		caps:  Caps{ContextWindow: 2000, MaxOutput: 1000},
		steps: []fakeStep{{resp: textResp("fine", Usage{Input: 30})}, {resp: textResp("never", Usage{})}},
	}
	l, b := newLoop(t, m, &fakeHost{})
	if _, err := l.RunTurn(context.Background(), "short"); err != nil {
		t.Fatalf("a short first turn must fit: %v", err)
	}
	before := b.Snapshot().Hooks[HookConversation]
	requestsBefore := len(m.snaps)

	// 4 bytes per token: 2,000 tokens of text leave no room for 1,000 of output.
	huge := strings.Repeat("x", 2000*4)
	res, err := l.RunTurn(context.Background(), huge)
	if !errors.Is(err, ErrContextFull) {
		t.Fatalf("err = %v, want ErrContextFull", err)
	}
	if !strings.Contains(err.Error(), "no longer fits the model's context window") {
		t.Fatalf("err text = %q, want the clear message", err.Error())
	}
	if len(m.snaps) != requestsBefore {
		t.Fatalf("the model received %d request(s) for a refused turn, want none", len(m.snaps)-requestsBefore)
	}
	after := b.Snapshot().Hooks[HookConversation]
	if len(after) != len(before) {
		t.Fatalf("hook 6 changed: %d -> %d entries", len(before), len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID {
			t.Fatalf("hook 6 item %d changed from %s to %s", i, before[i].ID, after[i].ID)
		}
	}
	if n := len(b.Snapshot().Hooks[HookTurn]); n != 0 || len(res.Entries) != 0 {
		t.Fatalf("hook 7 holds %d items and the result %d entries after a refused turn, want 0 and 0", n, len(res.Entries))
	}
}

// The estimate also starts from the provider's own count: the snapshot is tiny
// here, so only the last response's reported prompt can make the next turn not fit.
func TestLoop_RefusalUsesReportedUsage(t *testing.T) {
	m := &fakeModel{
		caps:  Caps{ContextWindow: 2000, MaxOutput: 1000},
		steps: []fakeStep{{resp: textResp("ok", Usage{Input: 900, Output: 5})}},
	}
	l, _ := newLoop(t, m, &fakeHost{})
	if _, err := l.RunTurn(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	// 905 known + a few new tokens + 1000 reserved > 2000.
	if _, err := l.RunTurn(context.Background(), strings.Repeat("y", 400)); !errors.Is(err, ErrContextFull) {
		t.Fatalf("err = %v, want ErrContextFull from the reported usage", err)
	}
	if len(m.snaps) != 1 {
		t.Fatalf("model requests = %d, want 1", len(m.snaps))
	}

	// No ContextWindow in the caps: no pre-check at all.
	m2 := &fakeModel{caps: Caps{MaxOutput: 1 << 30}, steps: []fakeStep{{resp: textResp("ok", Usage{})}}}
	l2, _ := newLoop(t, m2, &fakeHost{})
	res2, err := l2.RunTurn(context.Background(), "hi")
	if err != nil {
		t.Fatalf("a model with no reported window must not be pre-checked: %v", err)
	}
	if len(m2.snaps) != 1 || res2.Text != "ok" || res2.Requests != 1 {
		t.Fatalf("requests %d, text %q; want the one request sent and answered", len(m2.snaps), res2.Text)
	}
}

func TestLoop_AdapterContextFullEndsTurn(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{{err: fmt.Errorf("ollama: %w", ErrContextFull)}}}
	l, b := newLoop(t, m, &fakeHost{})
	_, err := l.RunTurn(context.Background(), "q")
	if !errors.Is(err, ErrContextFull) {
		t.Fatalf("err = %v", err)
	}
	if got := history(b); len(got) != 0 {
		t.Fatalf("hook 6 = %+v after a first request the provider refused, want empty", got)
	}
}

// ---- PR #760 follow-up ----

// text of about n tokens (4 bytes per token, the loop's estimate).
func tokensOfText(n int) string { return strings.Repeat("x", n*4) }

func TestLoop_ReplyReserve(t *testing.T) {
	type tc struct {
		name    string
		caps    Caps
		textTok int
		refused bool
	}
	// The snapshot's own tools and system prompt are well under 200 tokens.
	for _, c := range []tc{
		// MaxOutput unset: the loop reserves window/4 = 1000.
		{"unset reserves a quarter: fits", Caps{ContextWindow: 4000}, 2700, false},
		{"unset reserves a quarter: does not fit", Caps{ContextWindow: 4000}, 3000, true},
		{"negative is unset too", Caps{ContextWindow: 4000, MaxOutput: -1}, 3000, true},
		// A configured MaxOutput is honoured, whatever a quarter would be.
		{"configured reserve: fits", Caps{ContextWindow: 4000, MaxOutput: 500}, 3300, false},
		{"configured reserve: does not fit", Caps{ContextWindow: 4000, MaxOutput: 2500}, 1700, true},
		// MaxOutput >= window would refuse every request: clamp to window/4.
		{"equal to the window clamps: fits", Caps{ContextWindow: 4000, MaxOutput: 4000}, 2700, false},
		{"above the window clamps: fits", Caps{ContextWindow: 4000, MaxOutput: 9000}, 2700, false},
		{"above the window clamps: still refuses a full prompt", Caps{ContextWindow: 4000, MaxOutput: 9000}, 3000, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := &fakeModel{caps: c.caps, steps: []fakeStep{{resp: textResp("ok", Usage{})}}}
			l, _ := newLoop(t, m, &fakeHost{})
			_, err := l.RunTurn(context.Background(), tokensOfText(c.textTok))
			if c.refused {
				if !errors.Is(err, ErrContextFull) || len(m.snaps) != 0 {
					t.Fatalf("err = %v, requests = %d; want ErrContextFull and no request", err, len(m.snaps))
				}
				return
			}
			if err != nil || len(m.snaps) != 1 {
				t.Fatalf("err = %v, requests = %d; want the request sent", err, len(m.snaps))
			}
		})
	}
	// The refusal names the reserve it used.
	m := &fakeModel{caps: Caps{ContextWindow: 4000, MaxOutput: 9000}}
	l, _ := newLoop(t, m, &fakeHost{})
	_, err := l.RunTurn(context.Background(), tokensOfText(3000))
	if err == nil || !strings.Contains(err.Error(), "1000 reserved for the reply, window 4000") {
		t.Fatalf("err = %v, want the clamped 1000 reserve in the message", err)
	}
}

func TestLoop_FirstRequestErrorDropsTheTurn(t *testing.T) {
	for name, step := range map[string]fakeStep{
		"server error":       {err: errors.New("ollama: server error 500")},
		"truncated no text":  {err: &StopError{Reason: StopMaxTokens, Response: &Response{StopReason: StopMaxTokens}}},
		"cancelled, no text": {block: true},
	} {
		t.Run(name, func(t *testing.T) {
			m := &fakeModel{steps: []fakeStep{step, {resp: textResp("fine", Usage{})}}}
			l, b := newLoop(t, m, &fakeHost{})
			ctx, cancel := context.WithCancel(context.Background())
			if step.block {
				go func() { time.Sleep(20 * time.Millisecond); cancel() }()
			}
			_, err := l.RunTurn(ctx, "the failed question")
			cancel()
			if err == nil {
				t.Fatal("want an error")
			}
			if got := history(b); len(got) != 0 || len(b.Snapshot().Hooks[HookTurn]) != 0 {
				t.Fatalf("history %+v after a failed first request, want none", got)
			}
			if _, err := l.RunTurn(context.Background(), "the next question"); err != nil {
				t.Fatal(err)
			}
			// N5: no two user entries with text in a row, so the Ollama render
			// (which does not merge) never sends consecutive user messages.
			got := history(b)
			if len(got) != 2 || got[0].Blocks[0].Text != "the next question" {
				t.Fatalf("history = %+v, want the next question and its answer only", got)
			}
		})
	}
}

// N5: the conversation never holds two text user entries in a row, whichever
// way an earlier turn ended. A user entry that follows tool results is not a
// problem for Ollama (results render as tool messages).
func TestLoop_NoConsecutiveUserTextEntries(t *testing.T) {
	refusal := &StopError{Reason: StopRefusal, Response: &Response{StopReason: StopRefusal}}
	m := &fakeModel{steps: []fakeStep{
		{err: refusal},                  // dropped
		{err: errors.New("boom")},       // dropped
		{resp: textResp("a1", Usage{})}, // answered
		{resp: toolResp("tu_1", "knomit_repos", `{}`, Usage{})},
		{err: refusal}, // after a tool call: kept, ends with results
		{resp: textResp("a3", Usage{})},
	}}
	h := &fakeHost{out: map[string]ToolOutcome{"knomit_repos": {Content: "r"}}}
	l, b := newLoop(t, m, h)
	for _, q := range []string{"q1", "q2", "q3", "q4", "q5"} {
		_, _ = l.RunTurn(context.Background(), q)
	}
	var prevText bool
	for i, e := range history(b) {
		isText := e.Role == RoleUser && len(e.Blocks) > 0 && e.Blocks[0].Kind == BlockText
		if isText && prevText {
			t.Fatalf("entries %d and %d are both user text: %+v", i-1, i, history(b))
		}
		prevText = isText
	}
	// q4 ran a tool before the refusal, so it stays (paired); q1 and q2 do not.
	var texts []string
	for _, e := range history(b) {
		if e.Role == RoleUser && e.Blocks[0].Kind == BlockText {
			texts = append(texts, e.Blocks[0].Text)
		}
	}
	if fmt.Sprint(texts) != "[q3 q4 q5]" {
		t.Fatalf("user texts kept = %v, want [q3 q4 q5]", texts)
	}
}

func TestLoop_RefusalAfterToolCallsKeepsPairedResults(t *testing.T) {
	refusal := &StopError{Reason: StopRefusal, Response: &Response{StopReason: StopRefusal}}
	m := &fakeModel{steps: []fakeStep{{resp: toolResp("tu_1", "knomit_repos", `{}`, Usage{})}, {err: refusal}}}
	h := &fakeHost{out: map[string]ToolOutcome{"knomit_repos": {Content: "r"}}}
	l, b := newLoop(t, m, h)
	_, err := l.RunTurn(context.Background(), "look")
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v", err)
	}
	got := history(b)
	if len(got) != 3 || got[0].Blocks[0].Text != "look" || toolUses(got[1].Blocks)[0].ID != "tu_1" || got[2].Blocks[0].ToolResult.ToolUseID != "tu_1" {
		t.Fatalf("history = %+v, want user, tool_use, its result (the side effect stays visible)", got)
	}
}

func TestLoop_PauseTurnIsBounded(t *testing.T) {
	pause := &Response{Blocks: []Block{{Kind: BlockText, Text: "working"}}, StopReason: StopPauseTurn}
	var steps []fakeStep
	for i := 0; i < 50; i++ {
		steps = append(steps, fakeStep{resp: pause})
	}
	m := &fakeModel{steps: steps}
	l, b := newLoop(t, m, &fakeHost{})
	l.MaxCalls = 3
	res, err := l.RunTurn(context.Background(), "forever")
	if !errors.Is(err, ErrTooManyCalls) {
		t.Fatalf("err = %v, want ErrTooManyCalls", err)
	}
	if len(m.snaps) != 4 || res.Requests != 4 {
		t.Fatalf("requests = %d / %d, want 4: the first plus MaxCalls (3) continuations", len(m.snaps), res.Requests)
	}
	if got := history(b); len(got) != 5 {
		t.Fatalf("history has %d entries, want the user entry and 4 pause replies", len(got))
	}
	// Pauses and tool calls draw on one budget.
	m2 := &fakeModel{steps: []fakeStep{
		{resp: toolResp("tu_1", "knomit_repos", `{}`, Usage{})}, {resp: pause}, {resp: pause}, {resp: pause},
	}}
	h := &fakeHost{out: map[string]ToolOutcome{"knomit_repos": {Content: "r"}}}
	l2, _ := newLoop(t, m2, h)
	l2.MaxCalls = 2
	if _, err := l2.RunTurn(context.Background(), "x"); !errors.Is(err, ErrTooManyCalls) || len(m2.snaps) != 3 {
		t.Fatalf("err = %v, requests = %d; want ErrTooManyCalls after 1 call + 2 pauses = 3 requests", err, len(m2.snaps))
	}
}

func TestLoop_ContextWindowExceededStopKeepsTextAndEndsTurn(t *testing.T) {
	se := &StopError{Reason: StopContextWindowExceeded, Response: &Response{
		Blocks: []Block{{Kind: BlockText, Text: "cut at the window"}}, StopReason: StopContextWindowExceeded, Usage: Usage{Input: 7, Output: 3},
	}}
	m := &fakeModel{steps: []fakeStep{{err: se}}}
	l, b := newLoop(t, m, &fakeHost{})
	res, err := l.RunTurn(context.Background(), "q")
	if !errors.Is(err, ErrContextFull) || !strings.Contains(err.Error(), "no longer fits the model's context window") {
		t.Fatalf("err = %v, want the context-full sentence wrapping ErrContextFull", err)
	}
	got := history(b)
	if len(got) != 2 || got[1].Blocks[0].Text != "cut at the window" || res.Usage.Input != 7 {
		t.Fatalf("history %+v, usage %+v; want the partial text kept and the usage counted", got, res.Usage)
	}
}

func TestLoop_DuplicateEntryIDIsATurnErrorNotASilentDrop(t *testing.T) {
	// The results entry repeats the assistant entry's id: it cannot be added.
	ids := []string{"e_user", "e_dup", "e_dup", "e_next"}
	n := 0
	m := &fakeModel{steps: []fakeStep{{resp: toolResp("tu_1", "knomit_repos", `{}`, Usage{})}, {resp: textResp("never", Usage{})}}}
	h := &fakeHost{out: map[string]ToolOutcome{"knomit_repos": {Content: "r"}}}
	l, b := newLoop(t, m, h)
	l.NewID = func() string { id := ids[n%len(ids)]; n++; return id }
	_, err := l.RunTurn(context.Background(), "q")
	if !errors.Is(err, ErrDuplicateItem) {
		t.Fatalf("err = %v, want ErrDuplicateItem", err)
	}
	if len(m.snaps) != 1 {
		t.Fatalf("requests = %d, want 1 (the turn ends at the failed record)", len(m.snaps))
	}
	// The tool_use whose result could not be recorded must not reach hook 6.
	if got := history(b); len(got) != 0 || len(b.Snapshot().Hooks[HookTurn]) != 0 {
		t.Fatalf("history = %+v, hook 7 = %d; want both empty (no unpaired tool_use)", got, len(b.Snapshot().Hooks[HookTurn]))
	}
}
