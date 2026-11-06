package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// streamStep is one streamed response: the tool calls it emits one by one (the
// model keeps emitting until the context is cancelled or the list is done), or
// a text answer when calls is empty.
type streamStep struct {
	calls []ToolUse
	text  string
}

// streamModel streams its steps the way Ollama does: each call is an event, a
// cancelled context stops the stream with the context's error and no usage.
type streamModel struct {
	stoppable bool
	steps     []streamStep
	emitted   int // tool_use events produced, across all requests
	snaps     []Snapshot
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
	for i := range st.calls {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c := st.calls[i]
		c.ID = fmt.Sprintf("tu_%d_%d", len(m.snaps), i)
		m.emitted++
		blocks = append(blocks, Block{Kind: BlockToolUse, ToolUse: &c})
		sink(Event{Kind: EventToolUse, ToolUse: &c})
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
