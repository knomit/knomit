package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// runsAll is a ShortCircuit that allows every call to be answered from the
// turn's earlier result.
func runsAll(string, json.RawMessage) bool { return true }

func okHost(content string) *scriptedHost {
	return &scriptedHost{outs: []ToolOutcome{{Content: content}}}
}

func sameCallSteps(n int, tool, args string) []fakeStep {
	var steps []fakeStep
	for i := 1; i <= n; i++ {
		steps = append(steps, fakeStep{resp: toolResp(fmt.Sprintf("tu_%d", i), tool, args, Usage{})})
	}
	return steps
}

// 23 identical successful calls: the first runs, the second is answered with
// the earlier result and the note, the third ends the turn. Every tool_use is
// paired and the turn is flushed.
//
// SABOTAGE: agent.go runCalls, delete the `guard.repeatOf` block -> fails.
func TestLoop_IdenticalSuccessfulCallIsShortCircuitedThenEndsTheTurn(t *testing.T) {
	m := &fakeModel{steps: sameCallSteps(23, "knomit_bind", `{}`)}
	h := okHost("bound: h1")
	l, b := newLoop(t, m, h)
	l.ShortCircuit = runsAll
	res, err := l.RunTurn(context.Background(), "go")
	if !errors.Is(err, ErrNoProgress) {
		t.Fatalf("err = %v, want ErrNoProgress", err)
	}
	if h.calls != 1 {
		t.Fatalf("host calls = %d, want 1", h.calls)
	}
	if len(m.snaps) != 3 || res.ToolCalls != 3 {
		t.Fatalf("requests %d, tool calls %d; want 3 each", len(m.snaps), res.ToolCalls)
	}
	want := fmt.Sprintf(NoProgressMessage_DRAFT, "knomit_bind", 3)
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	got := history(b)
	var results []*ToolResult
	for _, e := range got {
		for _, bl := range e.Blocks {
			if bl.Kind == BlockToolResult {
				results = append(results, bl.ToolResult)
			}
		}
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 (every tool_use paired)", len(results))
	}
	if results[0].Content != "bound: h1" || results[0].IsError {
		t.Fatalf("first result = %+v, want the real one", results[0])
	}
	if results[1].Content != "bound: h1"+NoProgressNote_DRAFT || results[1].IsError {
		t.Fatalf("second result = %q, want the earlier result plus the note", results[1].Content)
	}
	if results[2].ToolUseID != "tu_3" {
		t.Fatalf("last result answers %s, want tu_3", results[2].ToolUseID)
	}
}

// A different argument, a different tool, a failure or a decline between two
// identical calls starts the count over: nothing is short-circuited.
//
// SABOTAGE: agent.go runCalls, replace the `guard.forget()` in the else branch
// after a call with `guard.remember(c, out.Content)` -> "different arguments"
// fails.
func TestLoop_NoProgressGuardResets(t *testing.T) {
	type step struct{ tool, args string }
	cases := map[string][]step{
		"different arguments":    {{"knomit_bind", `{"a":1}`}, {"knomit_bind", `{"a":2}`}, {"knomit_bind", `{"a":1}`}, {"knomit_bind", `{"a":2}`}},
		"different tool between": {{"knomit_query", `{}`}, {"knomit_repos", `{}`}, {"knomit_query", `{}`}, {"knomit_repos", `{}`}},
	}
	for name, seq := range cases {
		t.Run(name, func(t *testing.T) {
			var steps []fakeStep
			for i, s := range seq {
				steps = append(steps, fakeStep{resp: toolResp(fmt.Sprintf("tu_%d", i), s.tool, s.args, Usage{})})
			}
			steps = append(steps, fakeStep{resp: textResp("done", Usage{})})
			m := &fakeModel{steps: steps}
			h := okHost("ok")
			l, _ := newLoop(t, m, h)
			l.ShortCircuit = runsAll
			if _, err := l.RunTurn(context.Background(), "go"); err != nil {
				t.Fatalf("err = %v, want the turn to run on", err)
			}
			if h.calls != len(seq) {
				t.Fatalf("host calls = %d, want %d (nothing short-circuited)", h.calls, len(seq))
			}
		})
	}
}

// A failed call is never cached as an answer: the same call made again after a
// failure runs for real.
//
// SABOTAGE: agent.go runCalls, drop the `!out.IsError` in the remember
// condition -> fails.
func TestLoop_NoProgressGuardNeverCachesAFailure(t *testing.T) {
	steps := sameCallSteps(2, "knomit_query", `{"q":1}`)
	steps = append(steps, fakeStep{resp: textResp("done", Usage{})})
	m := &fakeModel{steps: steps}
	h := &scriptedHost{outs: []ToolOutcome{failing("boom"), {Content: "ok"}}}
	l, _ := newLoop(t, m, h)
	l.ShortCircuit = runsAll
	if _, err := l.RunTurn(context.Background(), "go"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if h.calls != 2 {
		t.Fatalf("host calls = %d, want 2", h.calls)
	}
}

// A call the predicate refuses (its result may legitimately change when
// repeated) is run every time, and so is every call when the predicate is nil.
//
// SABOTAGE: agent.go, call ShortCircuit without the nil/false check -> fails.
func TestLoop_NoProgressGuardSparesCallsThatMayAdvance(t *testing.T) {
	for name, sc := range map[string]func(string, json.RawMessage) bool{
		"predicate says no": func(string, json.RawMessage) bool { return false },
		"nil predicate":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			steps := sameCallSteps(6, "knomit_review", `{"page":1}`)
			steps = append(steps, fakeStep{resp: textResp("done", Usage{})})
			m := &fakeModel{steps: steps}
			h := okHost("page")
			l, _ := newLoop(t, m, h)
			l.ShortCircuit = sc
			if _, err := l.RunTurn(context.Background(), "go"); err != nil {
				t.Fatalf("err = %v", err)
			}
			if h.calls != 6 {
				t.Fatalf("host calls = %d, want 6", h.calls)
			}
		})
	}
}

// Three identical calls in ONE reply: the second is answered from the first,
// the third ends the turn, all three paired.
func TestLoop_NoProgressGuardWithinOneReply(t *testing.T) {
	call := func(id string) Block {
		return Block{Kind: BlockToolUse, ToolUse: &ToolUse{ID: id, Name: "knomit_bind", Args: json.RawMessage(`{}`)}}
	}
	m := &fakeModel{steps: []fakeStep{{resp: &Response{
		Blocks: []Block{call("a"), call("b"), call("c"), call("d")}, StopReason: StopToolUse,
	}}}}
	h := okHost("bound")
	l, b := newLoop(t, m, h)
	l.ShortCircuit = runsAll
	_, err := l.RunTurn(context.Background(), "go")
	if !errors.Is(err, ErrNoProgress) {
		t.Fatalf("err = %v, want ErrNoProgress", err)
	}
	if h.calls != 1 {
		t.Fatalf("host calls = %d, want 1", h.calls)
	}
	n := 0
	for _, e := range history(b) {
		for _, bl := range e.Blocks {
			if bl.Kind == BlockToolResult {
				n++
			}
		}
	}
	if n != 4 {
		t.Fatalf("results = %d, want 4 (all paired, the last skipped)", n)
	}
	if !strings.Contains(history(b)[len(history(b))-1].Blocks[3].ToolResult.Content, SkippedResult) {
		t.Fatalf("the call after the ending one should be skipped")
	}
}
