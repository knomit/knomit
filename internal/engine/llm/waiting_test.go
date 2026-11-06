package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// thinkingStream is an Anthropic stream with two thinking deltas, then text.
func thinkingStream() []string {
	return []string{
		sseEvent("message_start", msgStartFull),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Comparing "}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"blast radius"}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIG"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"done"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	}
}

func streamThinking(t *testing.T, display string) []Event {
	srv, _ := anthropicSSEServer(t, thinkingStream()...)
	chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL, Profile: ModelProfile{ThinkingDisplay: display}})
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "turn", Hook: HookTurn, Text: "q"}))
	var got []Event
	_, err := chat.Stream(context.Background(), b.Snapshot(), StreamOptions{}, func(e Event) { got = append(got, e) })
	require.NoError(t, err)
	return got
}

func TestAnthropicChat_SummarizedThinkingStreamsAsThinkingDeltas(t *testing.T) {
	var deltas []string
	var order []EventKind
	for _, e := range streamThinking(t, DisplaySummarized) {
		order = append(order, e.Kind)
		if e.Kind == EventThinkingDelta {
			deltas = append(deltas, e.Text)
		}
	}
	require.Equal(t, []string{"Comparing ", "blast radius"}, deltas)
	require.Equal(t, []EventKind{EventThinkingDelta, EventThinkingDelta, EventReasoning, EventTextDelta}, order[:4],
		"deltas precede the complete reasoning block")
}

func TestAnthropicChat_OmittedThinkingStreamsNoDeltas(t *testing.T) {
	for _, e := range streamThinking(t, DisplayOmitted) {
		require.NotEqual(t, EventThinkingDelta, e.Kind)
	}
}

// thinkingModel streams two thinking deltas before every reply.
type thinkingModel struct{ *fakeModel }

func (m *thinkingModel) Stream(ctx context.Context, s Snapshot, o StreamOptions, sink func(Event)) (*Response, error) {
	sink(Event{Kind: EventThinkingDelta, Text: "hmm "})
	sink(Event{Kind: EventThinkingDelta, Text: "ok"})
	return m.fakeModel.Stream(ctx, s, o, sink)
}

type slowHost struct{ delay time.Duration }

func (h *slowHost) CallTool(context.Context, string, json.RawMessage) (ToolOutcome, error) {
	time.Sleep(h.delay)
	return ToolOutcome{Content: "ok"}, nil
}

func TestLoop_ForwardsThinkingDeltasAndTimesTools(t *testing.T) {
	fm := &fakeModel{steps: []fakeStep{
		{resp: toolResp("tu_1", "knomit_repos", `{}`, Usage{})},
		{resp: textResp("done", Usage{})},
	}}
	l, _ := newLoop(t, fm, &slowHost{delay: 30 * time.Millisecond})
	l.Model = &thinkingModel{fakeModel: fm}
	var thinking []string
	var took []time.Duration
	l.Sink = func(ev LoopEvent) {
		switch ev.Kind {
		case LoopThinking:
			thinking = append(thinking, ev.Text)
		case LoopToolResult:
			took = append(took, ev.Took)
		}
	}
	_, err := l.RunTurn(context.Background(), "q")
	require.NoError(t, err)
	require.Equal(t, []string{"hmm ", "ok", "hmm ", "ok"}, thinking, "one pair per request")
	require.Len(t, took, 1)
	require.GreaterOrEqual(t, took[0], 30*time.Millisecond, "Took is the tool's own run time")
	require.Less(t, took[0], 2*time.Second)
}

func TestLoop_DeclinedCallHasNoDuration(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{
		{resp: toolResp("tu_1", "knomit_repos", `{}`, Usage{})},
		{resp: textResp("done", Usage{})},
	}}
	l, _ := newLoop(t, m, &fakeHost{out: map[string]ToolOutcome{"knomit_repos": {Content: "x"}}})
	l.Gate = gateFn(func(context.Context, ToolUse) (Decision, error) { return Decision{Allow: false}, nil })
	var took []time.Duration
	l.Sink = func(ev LoopEvent) {
		if ev.Kind == LoopToolResult {
			took = append(took, ev.Took)
		}
	}
	_, err := l.RunTurn(context.Background(), "q")
	require.NoError(t, err)
	require.Equal(t, []time.Duration{0}, took)
}

func TestContextFullInfo_CarriesNeededAndWindow(t *testing.T) {
	m := &fakeModel{
		caps:  Caps{ContextWindow: 2000, MaxOutput: 1000},
		steps: []fakeStep{{resp: textResp("fine", Usage{Input: 30})}},
	}
	l, _ := newLoop(t, m, &fakeHost{})
	_, err := l.RunTurn(context.Background(), "short")
	require.NoError(t, err)
	_, err = l.RunTurn(context.Background(), strings.Repeat("x", 2000*4))
	require.ErrorIs(t, err, ErrContextFull)
	needed, window, ok := ContextFullInfo(err)
	require.True(t, ok)
	require.Equal(t, 2000, window)
	require.Greater(t, needed, window, "needed is the prompt plus the reply reserve, more than the window")

	_, _, ok = ContextFullInfo(errors.New("other"))
	require.False(t, ok)
}
