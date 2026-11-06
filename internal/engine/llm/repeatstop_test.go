package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// streamStep is one streamed response: the tool calls it emits one by one (the
// model keeps emitting until the context is cancelled or the list is done), or
// a text answer when calls is empty.
type streamStep struct {
	calls     []ToolUse
	text      string
	reasoning string // the reasoning block's native JSON, emitted before the calls
	cancelAt  int    // with cancelParent: cancel the caller's context while emitting this call (1-based; 0 never)
}

// streamModel streams its steps the way Ollama does: each call is an event, a
// cancelled context stops the stream with the context's error and no usage.
type streamModel struct {
	stoppable    bool
	steps        []streamStep
	emitted      int // tool_use events produced, across all requests
	cancelParent func()
	snaps        []Snapshot
}

func (m *streamModel) Provider() string { return "fake" }
func (m *streamModel) Model() string    { return "m" }
func (m *streamModel) Caps() Caps       { return Caps{StreamStoppable: m.stoppable} }

func (m *streamModel) Stream(ctx context.Context, s Snapshot, _ StreamOptions, sink func(Event)) (*Response, error) {
	m.snaps = append(m.snaps, s)
	st := m.steps[0]
	m.steps = m.steps[1:]
	if len(st.calls) == 0 {
		sink(Event{Kind: EventTextDelta, Text: st.text})
		return &Response{Blocks: []Block{{Kind: BlockText, Text: st.text}}, StopReason: StopEndTurn, Usage: Usage{Input: 10, Output: 5}}, nil
	}
	var blocks []Block
	if st.reasoning != "" {
		r := &Reasoning{Provider: "fake", Model: "m", Native: json.RawMessage(st.reasoning)}
		blocks = append(blocks, Block{Kind: BlockReasoning, Reasoning: r})
		sink(Event{Kind: EventReasoning, Reasoning: r})
	}
	for i := range st.calls {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c := st.calls[i]
		c.ID = fmt.Sprintf("tu_%d_%d", len(m.snaps), i)
		m.emitted++
		blocks = append(blocks, Block{Kind: BlockToolUse, ToolUse: &c})
		sink(Event{Kind: EventToolUse, ToolUse: &c})
		if st.cancelAt == i+1 && m.cancelParent != nil {
			m.cancelParent()
		}
	}
	return &Response{Blocks: blocks, StopReason: StopToolUse, Usage: Usage{Input: 100, Output: 900}}, nil
}

func callOf(name, args string) ToolUse { return ToolUse{Name: name, Args: json.RawMessage(args)} }

func repeatRun(t *testing.T, m *streamModel) (*TurnResult, error, *fakeHost, *Loop) {
	t.Helper()
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "system", Hook: HookSystem, Text: "sys"}))
	h := &fakeHost{out: map[string]ToolOutcome{"knomit_a": {Content: "a"}, "knomit_b": {Content: "b"}}}
	l := &Loop{Model: m, Builder: b, Tools: h, Log: &recLog{}}
	res, err := l.RunTurn(context.Background(), "go")
	return res, err, h, l
}

// The live runaway: one response streams the same call over and over. The
// generation is cancelled at the second A; A and B run once each, the repeats
// have no tool_use, and the turn goes on to its next step. The cancelled
// request's usage is counted (estimated: the provider never sent a final one).
//
// SABOTAGE: agent.go repeatStop.repeat, `if r.seen[key] {` -> `if false {` ->
// all 200 events are consumed and four calls run; this fails.
func TestLoop_RepeatedCallInOneResponseCancelsTheGeneration(t *testing.T) {
	calls := []ToolUse{callOf("knomit_a", `{}`), callOf("knomit_b", `{"x":1}`)}
	for range 200 {
		calls = append(calls, callOf("knomit_a", `{}`))
	}
	m := &streamModel{stoppable: true, steps: []streamStep{{calls: calls}, {text: "done"}}}
	res, err, h, l := repeatRun(t, m)
	require.NoError(t, err)
	require.Equal(t, 3, m.emitted, "A, B, then the second A, which cancels; nothing after it was generated")
	require.Equal(t, []hostCall{{"knomit_a", `{}`}, {"knomit_b", `{"x":1}`}}, h.calls, "A and B ran once each")
	require.Equal(t, 2, res.ToolCalls)
	require.Equal(t, "done", res.Text, "the turn continued with its next step")
	for _, id := range []string{"tu_1_0", "tu_1_1"} {
		require.NotNil(t, resultIn(m.snaps[1], id), "%s is answered", id)
	}
	require.Len(t, toolUses(history(l.Builder)[1].Blocks), 2, "the repeats have no tool_use")

	require.Greater(t, res.Usage.Input, int64(0))
	require.Greater(t, res.Usage.Output, int64(0), "the cancelled request is counted")
	require.True(t, res.Usage.Estimated)

	reqs := l.Log.(*recLog).req
	require.Len(t, reqs, 2)
	require.True(t, reqs[0].RepeatStopped, "the session log says the first request was cut at a repeat")
	require.True(t, reqs[0].Usage.Estimated)
	require.Equal(t, []string{"input", "output"}, reqs[0].Usage.EstimatedFields, "both are the loop's own numbers")
	require.False(t, reqs[1].RepeatStopped)
}

// Identical arguments to different tools, and one tool with different
// arguments, are different calls: nothing is cancelled.
func TestLoop_DifferentCallsInOneResponseAreNotStopped(t *testing.T) {
	m := &streamModel{stoppable: true, steps: []streamStep{
		{calls: []ToolUse{
			callOf("knomit_a", `{"n":1}`), callOf("knomit_a", `{"n":2}`), callOf("knomit_b", `{"n":1}`), callOf("knomit_a", `{"n":3}`),
		}},
		{text: "done"},
	}}
	res, err, h, _ := repeatRun(t, m)
	require.NoError(t, err)
	require.Equal(t, 4, m.emitted)
	require.Len(t, h.calls, 4)
	require.Equal(t, 4, res.ToolCalls)
	require.False(t, res.Usage.Estimated, "the provider's own usage")
}

// The same arguments spelled with the keys in another order are the same call.
func TestLoop_RepeatIsJudgedOnCanonicalArguments(t *testing.T) {
	m := &streamModel{stoppable: true, steps: []streamStep{
		{calls: []ToolUse{callOf("knomit_a", `{"a":1,"b":2}`), callOf("knomit_a", `{ "b": 2, "a": 1 }`), callOf("knomit_a", `{"a":9}`)}},
		{text: "done"},
	}}
	_, err, h, _ := repeatRun(t, m)
	require.NoError(t, err)
	require.Equal(t, 2, m.emitted)
	require.Len(t, h.calls, 1)
}

// A provider whose cancelled stream is not safe to assemble (Anthropic) is
// left alone: the whole response is kept.
func TestLoop_RepeatStopIsOffForAProviderThatIsNotStoppable(t *testing.T) {
	m := &streamModel{stoppable: false, steps: []streamStep{
		{calls: []ToolUse{callOf("knomit_a", `{}`), callOf("knomit_a", `{}`)}},
		{text: "done"},
	}}
	_, _, _, _ = repeatRun(t, m)
	require.Equal(t, 2, m.emitted)
}

// The output estimate counts what streamed: the reasoning block's bytes, the
// kept calls' names and arguments and the text, divided by 4. The repeat is
// not counted.
//
// SABOTAGE: agent.go repeatStop.response, delete the `case BlockReasoning:`
// arm -> the exact value is off by len(reasoning)/4 and this fails.
func TestLoop_RepeatStopOutputEstimateCountsReasoningAndKeptCalls(t *testing.T) {
	reasoning := `{"thinking":"0123456789012345678901234567890123456789"}`
	m := &streamModel{stoppable: true, steps: []streamStep{
		{reasoning: reasoning, calls: []ToolUse{callOf("knomit_a", `{}`), callOf("knomit_b", `{"x":1}`), callOf("knomit_a", `{}`), callOf("knomit_a", `{}`)}},
		{text: "done"},
	}}
	res, err, _, _ := repeatRun(t, m)
	require.NoError(t, err)
	want := (len(reasoning) + len("knomit_a") + len("{}") + len("knomit_b") + len(`{"x":1}`)) / 4
	// The second request (the text answer) reports Output 5.
	require.Equal(t, int64(want)+5, res.Usage.Output)
}

// Calls that differ only in an integer above 2^53 are different calls: neither
// is dropped.
//
// SABOTAGE: agent.go canonicalArgs, remove dec.UseNumber() -> both ids become
// the same float64, the second call is judged a repeat and this fails.
func TestLoop_RepeatIsNotJudgedThroughFloat64(t *testing.T) {
	m := &streamModel{stoppable: true, steps: []streamStep{
		{calls: []ToolUse{callOf("knomit_a", `{"id":9007199254740992}`), callOf("knomit_a", `{"id":9007199254740993}`)}},
		{text: "done"},
	}}
	_, err, h, _ := repeatRun(t, m)
	require.NoError(t, err)
	require.Equal(t, 2, m.emitted)
	require.Len(t, h.calls, 2, "both calls ran")
}

// The assembled response keeps the order the provider sent: reasoning first,
// then the calls.
func TestLoop_RepeatStopKeepsTheReasoningFirst(t *testing.T) {
	m := &streamModel{stoppable: true, steps: []streamStep{
		{reasoning: `{"thinking":"hm"}`, calls: []ToolUse{callOf("knomit_a", `{}`), callOf("knomit_b", `{}`), callOf("knomit_a", `{}`)}},
		{text: "done"},
	}}
	_, err, _, l := repeatRun(t, m)
	require.NoError(t, err)
	var kinds []BlockKind
	for _, b := range history(l.Builder)[1].Blocks {
		kinds = append(kinds, b.Kind)
	}
	require.Equal(t, []BlockKind{BlockReasoning, BlockToolUse, BlockToolUse}, kinds)
}

// The person's Stop at the moment of the repeat is a cancelled turn, not a
// repeat-stop: no assembled response, no request logged as cut.
//
// SABOTAGE: agent.go RunTurn, drop the `&& ctx.Err() == nil` after
// `rs.stopped` -> the cancel is swallowed into a repeat-stop and this fails.
func TestLoop_UserStopAtTheRepeatEndsTheTurnCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &streamModel{stoppable: true, cancelParent: cancel, steps: []streamStep{
		{calls: []ToolUse{callOf("knomit_a", `{}`), callOf("knomit_a", `{}`)}, cancelAt: 2},
		{text: "never"},
	}}
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "system", Hook: HookSystem, Text: "sys"}))
	log := &recLog{}
	l := &Loop{Model: m, Builder: b, Tools: &fakeHost{}, Log: log}
	_, err := l.RunTurn(ctx, "go")
	require.ErrorIs(t, err, ErrCancelled)
	for _, r := range log.req {
		require.False(t, r.RepeatStopped)
	}
}

// Stopping a response leaves no goroutine behind.
func TestLoop_RepeatStopLeavesNoGoroutine(t *testing.T) {
	run := func() {
		calls := []ToolUse{callOf("knomit_a", `{}`), callOf("knomit_a", `{}`)}
		m := &streamModel{stoppable: true, steps: []streamStep{{calls: calls}, {text: "done"}}}
		_, err, _, _ := repeatRun(t, m)
		require.NoError(t, err)
	}
	run() // warm anything lazily started
	before := runtime.NumGoroutine()
	for range 20 {
		run()
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	require.LessOrEqual(t, runtime.NumGoroutine(), before)
}

// orderedModel streams the way Ollama does since blocks are built in stream
// order: a content run, a thinking run closed into one reasoning event, more
// content, then calls, the second of them a repeat.
type orderedModel struct{ streamModel }

func (m *orderedModel) Stream(ctx context.Context, s Snapshot, _ StreamOptions, sink func(Event)) (*Response, error) {
	m.snaps = append(m.snaps, s)
	if len(m.snaps) > 1 {
		sink(Event{Kind: EventTextDelta, Text: "done"})
		return &Response{Blocks: []Block{{Kind: BlockText, Text: "done"}}, StopReason: StopEndTurn, Usage: Usage{Input: 10, Output: 5}}, nil
	}
	sink(Event{Kind: EventTextDelta, Text: "aaaa"})
	sink(Event{Kind: EventTextDelta, Text: "bbbb"})
	sink(Event{Kind: EventReasoning, Reasoning: &Reasoning{Provider: "fake", Model: "m", Native: json.RawMessage(`{"thinking":"first"}`)}})
	sink(Event{Kind: EventTextDelta, Text: "cccc"})
	sink(Event{Kind: EventReasoning, Reasoning: &Reasoning{Provider: "fake", Model: "m", Native: json.RawMessage(`{"thinking":"second"}`)}})
	for i := 0; i < 3; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c := callOf("knomit_a", `{}`)
		c.ID = fmt.Sprintf("tu_%d", i)
		sink(Event{Kind: EventToolUse, ToolUse: &c})
	}
	return nil, ctx.Err()
}

// A response cut at a repeat keeps the order the stream delivered: text,
// reasoning, text, reasoning, call. The estimate counts all of their bytes.
//
// SABOTAGE: agent.go repeatStop.addReasoning, delete `r.closeText()` -> the
// first text run lands after the reasoning and this fails; swap the two
// reasoning payloads in orderedModel -> the "first", "second" check fails.
func TestLoop_RepeatStopKeepsStreamOrderAndCountsEveryRun(t *testing.T) {
	m := &orderedModel{streamModel{stoppable: true}}
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "system", Hook: HookSystem, Text: "sys"}))
	h := &fakeHost{out: map[string]ToolOutcome{"knomit_a": {Content: "a"}}}
	l := &Loop{Model: m, Builder: b, Tools: h, Log: &recLog{}}
	res, err := l.RunTurn(context.Background(), "go")
	require.NoError(t, err)
	var kinds []BlockKind
	var text string
	var thinking []string
	entry := history(l.Builder)[1]
	for _, bl := range entry.Blocks {
		kinds = append(kinds, bl.Kind)
		text += bl.Text
		if bl.Kind == BlockReasoning {
			var n struct {
				Thinking string `json:"thinking"`
			}
			require.NoError(t, json.Unmarshal(bl.Reasoning.Native, &n))
			thinking = append(thinking, n.Thinking)
		}
	}
	require.Equal(t, []BlockKind{BlockText, BlockReasoning, BlockText, BlockReasoning, BlockToolUse}, kinds)
	require.Equal(t, []string{"first", "second"}, thinking, "the reasoning runs keep their order")
	// Replay to Ollama joins the runs with a newline, in that order.
	require.Equal(t, "first\nsecond", ollamaAssistant(entry, map[string]ollamaCallRef{}).Thinking)
	require.Equal(t, "aaaabbbbcccc", text)
	want := (12 + len(`{"thinking":"first"}`) + len(`{"thinking":"second"}`) + len("knomit_a") + len("{}")) / 4
	require.Equal(t, int64(want)+5, res.Usage.Output)
}
