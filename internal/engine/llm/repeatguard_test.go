package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// scriptedHost answers each call with the next outcome (the last one repeats).
type scriptedHost struct {
	calls int
	outs  []ToolOutcome
}

func (h *scriptedHost) CallTool(_ context.Context, _ string, _ json.RawMessage) (ToolOutcome, error) {
	o := h.outs[min(h.calls, len(h.outs)-1)]
	h.calls++
	return o, nil
}

func failing(msg string) ToolOutcome { return ToolOutcome{Content: msg, IsError: true} }

func repeatSteps(n int, args func(i int) string) []fakeStep {
	var steps []fakeStep
	for i := 1; i <= n; i++ {
		steps = append(steps, fakeStep{resp: toolResp(fmt.Sprintf("tu_%d", i), "knomit_learn", args(i), Usage{})})
	}
	return steps
}

// The same failing call RepeatFailureLimit times in a row ends the turn with
// the named sentence; the call is not run again and every tool_use is paired.
//
// SABOTAGE: agent.go runCalls, delete the `guard.observe` block -> fails.
func TestLoop_RepeatedIdenticalFailureEndsTheTurn(t *testing.T) {
	m := &fakeModel{steps: repeatSteps(10, func(int) string { return `{"a":1}` })}
	h := &scriptedHost{outs: []ToolOutcome{failing("empty path")}}
	l, b := newLoop(t, m, h)
	_, err := l.RunTurn(context.Background(), "write")
	if !errors.Is(err, ErrRepeatedFailure) {
		t.Fatalf("err = %v, want ErrRepeatedFailure", err)
	}
	if h.calls != RepeatFailureLimit || len(m.snaps) != RepeatFailureLimit {
		t.Fatalf("host calls %d, requests %d; want %d each", h.calls, len(m.snaps), RepeatFailureLimit)
	}
	want := fmt.Sprintf(RepeatedFailureMessage_DRAFT, "knomit_learn", RepeatFailureLimit, "empty path")
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	// Flushed and paired: the last entry answers the last tool_use.
	got := history(b)
	last := got[len(got)-1]
	if r := last.Blocks[0].ToolResult; r == nil || r.ToolUseID != fmt.Sprintf("tu_%d", RepeatFailureLimit) {
		t.Fatalf("last entry = %+v, want the last call answered", last)
	}
}

func TestLoop_RepeatGuardResetsOnDifferentArgsErrorOrSuccess(t *testing.T) {
	cases := map[string]struct {
		args func(i int) string
		outs []ToolOutcome
	}{
		"different arguments": {
			args: func(i int) string { return fmt.Sprintf(`{"a":%d}`, i) },
			outs: []ToolOutcome{failing("empty path")},
		},
		"different error": {
			args: func(int) string { return `{"a":1}` },
			outs: []ToolOutcome{failing("e1"), failing("e2"), failing("e3"), failing("e4"), failing("e5"), failing("e6")},
		},
		"success between": {
			args: func(int) string { return `{"a":1}` },
			outs: []ToolOutcome{failing("x"), failing("x"), {Content: "ok"}, failing("x"), failing("x"), {Content: "ok"}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			steps := repeatSteps(6, tc.args)
			steps = append(steps, fakeStep{resp: textResp("done", Usage{})})
			m := &fakeModel{steps: steps}
			h := &scriptedHost{outs: tc.outs}
			l, _ := newLoop(t, m, h)
			if _, err := l.RunTurn(context.Background(), "go"); err != nil {
				t.Fatalf("err = %v; want the turn to run on", err)
			}
			if h.calls != 6 {
				t.Fatalf("host calls = %d, want 6", h.calls)
			}
		})
	}
}

func TestLoop_RepeatGuardBoundsALongError(t *testing.T) {
	m := &fakeModel{steps: repeatSteps(5, func(int) string { return `{}` })}
	h := &scriptedHost{outs: []ToolOutcome{failing(strings.Repeat("e", 5000))}}
	l, _ := newLoop(t, m, h)
	_, err := l.RunTurn(context.Background(), "go")
	if !errors.Is(err, ErrRepeatedFailure) || len(err.Error()) > 700 {
		t.Fatalf("err len %d (%v), want the guard's sentence with a bounded error", len(err.Error()), err)
	}
}

func badArgsResp(id, raw string) *Response {
	return &Response{
		Blocks:     []Block{{Kind: BlockToolUse, ToolUse: &ToolUse{ID: id, Name: "knomit_learn", Args: json.RawMessage(`{}`), RawArgs: raw}}},
		StopReason: StopToolUse,
	}
}

// Malformed JSON repeated byte for byte ends the turn like a failing call.
//
// SABOTAGE: agent.go runCalls, delete the guard.observeKey call in the
// ArgsErr branch -> fails.
func TestLoop_RepeatedInvalidJSONArgumentsEndTheTurn(t *testing.T) {
	var steps []fakeStep
	for i := 1; i <= 10; i++ {
		steps = append(steps, fakeStep{resp: badArgsResp(fmt.Sprintf("tu_%d", i), `{"facts": [`)})
	}
	m := &fakeModel{steps: steps}
	h := &scriptedHost{outs: []ToolOutcome{{Content: "ok"}}}
	l, _ := newLoop(t, m, h)
	_, err := l.RunTurn(context.Background(), "go")
	if !errors.Is(err, ErrRepeatedFailure) || len(m.snaps) != RepeatFailureLimit || h.calls != 0 {
		t.Fatalf("err = %v, requests = %d, host calls = %d; want ErrRepeatedFailure after %d requests and no host call", err, len(m.snaps), h.calls, RepeatFailureLimit)
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("err = %q, want it to say the arguments are not valid JSON", err)
	}

	// Different malformed bytes each time do not count as a repeat.
	steps = nil
	for i := 1; i <= 5; i++ {
		steps = append(steps, fakeStep{resp: badArgsResp(fmt.Sprintf("tu_%d", i), fmt.Sprintf(`{"a": %d`, i))})
	}
	steps = append(steps, fakeStep{resp: textResp("done", Usage{})})
	l2, _ := newLoop(t, &fakeModel{steps: steps}, h)
	if _, err := l2.RunTurn(context.Background(), "go"); err != nil {
		t.Fatalf("err = %v; want the turn to run on", err)
	}
}

// A decline is new information, so it resets the streak: two failures, a
// decline, two more failures of the same call do not end the turn.
//
// SABOTAGE: agent.go runCalls, delete guard.reset() in the decline branch ->
// the fifth call is not reached and this fails.
func TestLoop_DeclineResetsTheRepeatStreak(t *testing.T) {
	steps := repeatSteps(5, func(int) string { return `{"a":1}` })
	steps = append(steps, fakeStep{resp: textResp("done", Usage{})})
	m := &fakeModel{steps: steps}
	h := &scriptedHost{outs: []ToolOutcome{failing("x")}}
	l, _ := newLoop(t, m, h)
	n := 0
	l.Gate = gateFn(func(context.Context, ToolUse) (Decision, error) {
		n++
		return Decision{Allow: n != 3}, nil // the third call is declined
	})
	if _, err := l.RunTurn(context.Background(), "go"); err != nil {
		t.Fatalf("err = %v; want the turn to run on past the decline", err)
	}
	if h.calls != 4 {
		t.Fatalf("host calls = %d, want 4 (five asked, one declined)", h.calls)
	}
}
