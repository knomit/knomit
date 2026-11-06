package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func stubByName(s Snapshot, name string) *ToolDef {
	for _, it := range s.Hooks[HookTools] {
		if it.Tool.Name == name {
			return it.Tool
		}
	}
	return nil
}

// Every catalogue tool that is not loaded goes out as a stub: its name, the
// approved purpose line as its description, the permissive schema. A loaded
// tool goes out with its full definition. Nothing else is in hook 1.
//
// SABOTAGE: toolwindow.go Project, drop the `default:` stub case -> fails
// (the unloaded tools are absent); send the full definition in the default
// case -> fails (the stubs are not stubs).
func TestWindow_ProjectStubsEveryUnloadedToolAndSendsLoadedInFull(t *testing.T) {
	w, _, snap := newWin(t, winTool("b", 200), winTool("a", 200), winTool("c", 200))
	w.purposes = map[string]string{"a": "purpose of a", "b": "purpose of b", "c": "purpose of c"}
	p := w.Project(snap)
	require.Equal(t, []string{LoadToolsName}, hook1Names(p))
	require.Equal(t, []string{"a", "b", "c"}, hook1Stubs(p), "a stub for each unloaded tool")
	a := stubByName(p, "a")
	require.Equal(t, "purpose of a", a.Description)
	require.JSONEq(t, `{"type":"object","properties":{},"additionalProperties":true}`, string(a.Schema))

	w.Run(loadCall("b"), snap, plenty, nil)
	p = w.Project(snap)
	require.Equal(t, []string{LoadToolsName, "b"}, hook1Names(p), "the loaded tool has its full definition")
	require.Equal(t, []string{"a", "c"}, hook1Stubs(p))
	require.True(t, snap.Hooks[HookTools][1].Tool.Name == "b" && snap.Hooks[HookTools][1].Tool == stubByName(p, "b"), "b is the builder's own definition")
	require.Len(t, p.Hooks[HookTools], 4, "loader + b + two stubs")
}

// A stub is a fraction of the definition it stands for.
//
// SABOTAGE: toolwindow.go stubSchema, add a long description property -> fails.
func TestWindow_StubIsMuchCheaperThanTheDefinition(t *testing.T) {
	w, _, snap := newWin(t, winTool("a", 400))
	w.Project(snap)
	require.Less(t, w.stubBytes["a"], 400, "stub is under a quarter of a 400-token (1600-byte) definition")
	require.Equal(t, w.stubBytes["a"]/4, w.StubTokens())
}

// Evicting a tool does not drop it: it is a stub again.
//
// SABOTAGE: toolwindow.go Project, skip the stub for unloaded tools -> fails.
func TestWindow_EvictedToolBecomesAStubAgain(t *testing.T) {
	w, _, snap := newWin(t, winTool("a", 200), winTool("b", 200))
	w.Run(loadCall("a"), snap, plenty, nil)
	w.BeginTurn()
	_, ok := w.EvictOne(nil)
	require.True(t, ok)
	p := w.Project(snap)
	require.Equal(t, []string{LoadToolsName}, hook1Names(p))
	require.Equal(t, []string{"a", "b"}, hook1Stubs(p))
}

// The model calls the knomit_learn stub: the call is not run and not asked
// about, the tool is loaded and the call answered with NotLoadedCallResult. The
// next request has the full schema; a second call reaches the gate and runs.
//
// SABOTAGE: agent.go runCalls, skip the Window.Handles branch -> the stub call
// reaches the gate and the host and the test fails.
func TestLoop_CallToAStubLoadsItThenTheNextCallIsGated(t *testing.T) {
	learn := winTool("knomit_learn", 300)
	m := &fakeModel{steps: []fakeStep{
		{resp: toolResp("tu_1", "knomit_learn", `{"title":"x"}`, Usage{})},
		{resp: toolResp("tu_2", "knomit_learn", `{"facts":[1]}`, Usage{})},
		{resp: textResp("done", Usage{})},
	}}
	r := newRig(t, m, winTool("knomit_query", 100), learn)
	r.w.purposes = map[string]string{"knomit_learn": "write a new fact (the person approves first)"}
	_, err := r.l.RunTurn(context.Background(), "record this")
	require.NoError(t, err)
	require.Len(t, m.snaps, 3)

	first := stubByName(m.snaps[0], "knomit_learn")
	require.Equal(t, "write a new fact (the person approves first)", first.Description)
	require.True(t, isStub(first))

	got := resultText(m.snaps[1], "tu_1")
	require.NotNil(t, got, "the stub call is answered, paired")
	require.Equal(t, fmt.Sprintf(NotLoadedCallResult, "knomit_learn"), got.Content)
	require.True(t, got.IsError)
	require.Equal(t, learn, stubByName(m.snaps[1], "knomit_learn"), "the next request carries the full definition")

	require.Equal(t, []string{"knomit_learn"}, r.decide, "only the second call reached the gate")
	require.Equal(t, []hostCall{{"knomit_learn", `{"facts":[1]}`}}, r.calls, "and the host")
	require.Equal(t, []string{"gate:knomit_learn", "host:knomit_learn"}, r.order)
	require.Equal(t, "result of knomit_learn", resultText(m.snaps[2], "tu_2").Content)
}

// Two stub calls in one batch are both answered and neither runs.
//
// SABOTAGE: as above.
func TestLoop_StubCallsInOneBatchAreAllAnsweredAndNoneRun(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{
		{resp: multiResp(Usage{},
			ToolUse{ID: "a", Name: "knomit_learn", Args: json.RawMessage(`{}`)},
			ToolUse{ID: "b", Name: "knomit_update", Args: json.RawMessage(`{}`)})},
		{resp: textResp("done", Usage{})},
	}}
	r := newRig(t, m, winTool("knomit_learn", 100), winTool("knomit_update", 100))
	_, err := r.l.RunTurn(context.Background(), "go")
	require.NoError(t, err)
	require.Empty(t, r.calls)
	require.Empty(t, r.decide)
	for _, id := range []string{"a", "b"} {
		require.NotNil(t, resultText(m.snaps[1], id), id)
	}
	require.Empty(t, hook1Stubs(m.snaps[1]), "both are loaded now")
}

func stubBatch(ids ...string) *Response {
	var calls []ToolUse
	for _, id := range ids {
		calls = append(calls, ToolUse{ID: id, Name: "knomit_learn", Args: json.RawMessage(`{}`)})
	}
	return multiResp(Usage{}, calls...)
}

// The live repro (acceptance run 3): one response carries three identical
// knomit_learn({}) stub calls. They are one episode, not three failures: the
// tool loads once, the model reads "call it again" once, every tool_use is
// still paired, the turn stays alive and the real call that follows runs.
//
// SABOTAGE: agent.go runCalls, window branch: observe the stub outcome with
// guard.observe again -> the turn ends with ErrRepeatedFailure and this fails.
func TestLoop_ThreeIdenticalStubCallsAreOneEpisodeNotAFailureStreak(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{
		{resp: stubBatch("a", "b", "c")},
		{resp: toolResp("real", "knomit_learn", `{"facts":[1]}`, Usage{})},
		{resp: textResp("done", Usage{})},
	}}
	r := newRig(t, m, winTool("knomit_query", 100), winTool("knomit_learn", 300))
	_, err := r.l.RunTurn(context.Background(), "record this")
	require.NoError(t, err, "the turn is not ended by the repeated-failure guard")
	require.Len(t, m.snaps, 3)

	for _, id := range []string{"a", "b", "c"} {
		require.NotNil(t, resultText(m.snaps[1], id), "%s is paired", id)
	}
	require.Equal(t, fmt.Sprintf(NotLoadedCallResult, "knomit_learn"), resultText(m.snaps[1], "a").Content)
	for _, id := range []string{"b", "c"} {
		require.Equal(t, fmt.Sprintf(AlreadyLoadedResult, "knomit_learn"), resultText(m.snaps[1], id).Content,
			"the duplicates do not repeat the call-again instruction")
	}
	loads := 0
	for _, e := range r.events {
		if e.Kind == LoopToolsChanged {
			loads++
		}
	}
	require.Equal(t, 1, loads, "the tool loads once")
	require.Equal(t, []hostCall{{"knomit_learn", `{"facts":[1]}`}}, r.calls, "the real call ran, the stubs did not")
	require.Equal(t, "result of knomit_learn", resultText(m.snaps[2], "real").Content)
}

// A real call that fails is still counted: three identical failures in an
// adaptive chat end the turn, exactly as in a static one.
//
// SABOTAGE: agent.go runCalls, delete the closing `guard.observe` block ->
// fails (the turn runs on).
func TestLoop_RealFailingCallsStillTripTheGuardInAnAdaptiveChat(t *testing.T) {
	var steps []fakeStep
	steps = append(steps, fakeStep{resp: stubBatch("s1")})
	for i := 1; i <= 5; i++ {
		steps = append(steps, fakeStep{resp: toolResp(fmt.Sprintf("r%d", i), "knomit_learn", `{"a":1}`, Usage{})})
	}
	m := &fakeModel{steps: steps}
	r := newRig(t, m, winTool("knomit_query", 100), winTool("knomit_learn", 300))
	r.out, r.fail = "category is required", true
	_, err := r.l.RunTurn(context.Background(), "record this")
	require.ErrorIs(t, err, ErrRepeatedFailure)
	require.Len(t, r.calls, RepeatFailureLimit, "the stub call did not count, the real ones did")
}

// A stub call between two identical successful calls changes what the second
// would see (the tool set), so the second is not answered from the first: it
// runs.
//
// SABOTAGE: agent.go runCalls, window branch, delete `guard.forget()` -> the
// second query is answered with NoProgressNote and the host runs once.
func TestLoop_StubCallBetweenTwoIdenticalSuccessesForgetsTheFirst(t *testing.T) {
	q := func(id string) ToolUse {
		return ToolUse{ID: id, Name: "knomit_query", Args: json.RawMessage(`{"text":"x"}`)}
	}
	m := &fakeModel{steps: []fakeStep{
		{resp: multiResp(Usage{}, q("a1"), ToolUse{ID: "b", Name: "knomit_learn", Args: json.RawMessage(`{}`)}, q("a2"))},
		{resp: textResp("done", Usage{})},
	}}
	r := newRig(t, m, winTool("knomit_query", 100), winTool("knomit_learn", 100))
	r.l.ShortCircuit = runsAll
	r.loadAhead(t, "knomit_query")
	_, err := r.l.RunTurn(context.Background(), "go")
	require.NoError(t, err)
	require.Len(t, r.calls, 2, "both queries ran")
	require.Equal(t, "result of knomit_query", resultText(m.snaps[1], "a2").Content, "no no-progress note")
	require.Equal(t, fmt.Sprintf(NotLoadedCallResult, "knomit_learn"), resultText(m.snaps[1], "b").Content)
}

// A stub call that carries arguments valid for the real schema is still not
// run: the tool loads, the arguments are dropped with the call, and the call
// the model makes next, with the schema in front of it, is the one that runs.
//
// SABOTAGE: agent.go runCalls, skip the Window.Handles branch -> the stub call
// reaches the host with its arguments and this fails.
func TestLoop_StubCallWithValidArgumentsIsNotRun(t *testing.T) {
	learn := winTool("knomit_learn", 300)
	learn.Schema = json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`)
	m := &fakeModel{steps: []fakeStep{
		{resp: toolResp("s", "knomit_learn", `{"title":"valid"}`, Usage{})},
		{resp: toolResp("r", "knomit_learn", `{"title":"valid"}`, Usage{})},
		{resp: textResp("done", Usage{})},
	}}
	r := newRig(t, m, winTool("knomit_query", 100), learn)
	_, err := r.l.RunTurn(context.Background(), "record")
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf(NotLoadedCallResult, "knomit_learn"), resultText(m.snaps[1], "s").Content)
	require.Equal(t, []hostCall{{"knomit_learn", `{"title":"valid"}`}}, r.calls, "only the call made after loading ran, once")
	require.Equal(t, []string{"gate:knomit_learn", "host:knomit_learn"}, r.order)
}
