package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// Tool names the stub tests use: only knomit_query is replayable in the fake
// Replayable below, so the tests choose along that axis and no other.
const (
	stubReadTool  = "knomit_query"
	stubWriteTool = "knomit_learn"
	stubPageTool  = "knomit_review"
)

func fakeReplayable(tool string, _ json.RawMessage) bool { return tool == stubReadTool }

// histPair is one assistant tool_use entry and the user entry answering it.
func histPair(id, tool, args, content string, isErr bool) []Entry {
	return []Entry{
		{ID: "a-" + id, Role: RoleAssistant, Blocks: []Block{{Kind: BlockToolUse, ToolUse: &ToolUse{ID: id, Name: tool, Args: json.RawMessage(args)}}}},
		{ID: "r-" + id, Role: RoleUser, Blocks: []Block{{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: id, Content: content, IsError: isErr}}}},
	}
}

// stubWin is a window over fakeReplayable, and a builder holding one tool.
func stubWin(t *testing.T) (*ToolWindow, *Builder) {
	t.Helper()
	w, err := NewToolWindow(ToolWindowSpec{
		Loader: LoaderDef("- " + stubReadTool), Names: []string{stubReadTool},
		Replayable: fakeReplayable,
	})
	require.NoError(t, err)
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "tool." + stubReadTool, Hook: HookTools, Tool: winTool(stubReadTool, 10)}))
	require.NoError(t, b.Add(Item{ID: "system", Hook: HookSystem, Text: "sys"}))
	return w, b
}

func addHist(t *testing.T, b *Builder, hook Hook, es ...Entry) {
	t.Helper()
	for i := range es {
		e := es[i]
		require.NoError(t, b.Add(Item{ID: e.ID, Hook: hook, Entry: &e}))
	}
}

func resultsOf(s Snapshot, hook Hook) []ToolResult {
	var out []ToolResult
	for _, it := range s.Hooks[hook] {
		for _, bl := range it.Entry.Blocks {
			if bl.Kind == BlockToolResult {
				out = append(out, *bl.ToolResult)
			}
		}
	}
	return out
}

func jsonBody(n int) string {
	var sb strings.Builder
	sb.WriteString(`{"facts":[`)
	for sb.Len() < n {
		fmt.Fprintf(&sb, `{"title":"fact %d","body":"some words about retries"},`, sb.Len())
	}
	return sb.String()[:n]
}

func TestStub_OnlyEndedTurnsAreStubbed(t *testing.T) {
	w, b := stubWin(t)
	old, running := strings.Repeat("o", 4000), strings.Repeat("r", 4000)
	addHist(t, b, HookConversation, histPair("old", stubReadTool, `{}`, old, false)...)
	addHist(t, b, HookTurn, histPair("now", stubReadTool, `{}`, running, false)...)
	snap := b.Snapshot()

	p := w.StubEnded(w.Project(snap), 6400)
	ended, current := resultsOf(p, HookConversation), resultsOf(p, HookTurn)
	require.Len(t, ended, 1)
	require.True(t, ended[0].Elided)
	require.Less(t, len(ended[0].Content), len(old))
	require.Len(t, current, 1)
	require.Equal(t, running, current[0].Content, "the running turn is sent whole")
	require.False(t, current[0].Elided)
	require.Equal(t, snap.Hooks[HookTurn], p.Hooks[HookTurn])
}

func TestStub_KeepsPairingAndFlags(t *testing.T) {
	w, b := stubWin(t)
	addHist(t, b, HookConversation, histPair("e1", stubReadTool, `{"q":"x"}`, jsonBody(4000), true)...)
	addHist(t, b, HookConversation, histPair("e2", stubWriteTool, `{"f":1}`, jsonBody(3000), false)...)
	snap := b.Snapshot()

	p := w.StubEnded(w.Project(snap), 6400)
	rs := resultsOf(p, HookConversation)
	require.Len(t, rs, 2)
	require.Equal(t, "e1", rs[0].ToolUseID)
	require.True(t, rs[0].IsError, "the error flag survives")
	require.True(t, rs[0].Elided)
	require.Equal(t, "e2", rs[1].ToolUseID)
	require.False(t, rs[1].IsError)
	require.True(t, rs[1].Elided)
	// The assistant entries holding the tool_use blocks are byte-identical.
	for i, it := range p.Hooks[HookConversation] {
		if it.Entry.Role != RoleAssistant {
			continue
		}
		want, _ := json.Marshal(snap.Hooks[HookConversation][i].Entry)
		got, _ := json.Marshal(it.Entry)
		require.Equal(t, string(want), string(got))
	}
	_, err := renderOllamaChecked(p, "m", OllamaOptions{}, false)
	require.NoError(t, err, "every tool message still has its call")
}

func TestStub_ReplayableTextAndExcerpt(t *testing.T) {
	w, b := stubWin(t)
	body := jsonBody(4000)
	addHist(t, b, HookConversation, histPair("t1", stubReadTool, `{}`, body, false)...)

	usable := 1024 // E = 16 tokens: a 64-byte excerpt
	p := w.StubEnded(w.Project(b.Snapshot()), usable)
	got := resultsOf(p, HookConversation)[0].Content

	header := fmt.Sprintf(StubReplayable, stubReadTool, 4000/4, stubReadTool)
	require.True(t, strings.HasPrefix(got, header), got)
	rest := strings.TrimPrefix(got, header+" "+StubExcerptIntro+" ")
	require.NotEqual(t, got, rest, "the excerpt follows the intro: %q", got)
	require.True(t, strings.HasSuffix(rest, StubExcerptCut), "a cut excerpt ends with the cut mark")
	excerpt := strings.TrimSuffix(rest, StubExcerptCut)
	require.True(t, strings.HasPrefix(body, excerpt), "the excerpt is a prefix of the result")
	require.NotEmpty(t, excerpt)
	require.LessOrEqual(t, len(excerpt), 64)
	require.True(t, utf8.ValidString(excerpt))
}

func TestStub_NonReplayableNeverOffersARecall(t *testing.T) {
	w, b := stubWin(t)
	body := jsonBody(4000)
	tools := []string{stubWriteTool, stubPageTool, "mystery", LoadToolsName}
	for i, tool := range tools {
		addHist(t, b, HookConversation, histPair(fmt.Sprintf("n%d", i), tool, `{}`, body, false)...)
	}
	addHist(t, b, HookConversation, histPair("r", stubReadTool, `{}`, body, false)...)

	p := w.StubEnded(w.Project(b.Snapshot()), 6400)
	rs := resultsOf(p, HookConversation)
	require.Len(t, rs, len(tools)+1)
	for i, tool := range tools {
		want := fmt.Sprintf(StubNotReplayable, tool, 4000/4)
		require.True(t, strings.HasPrefix(rs[i].Content, want), "%s: %q", tool, rs[i].Content)
		require.NotContains(t, rs[i].Content, "again with the same arguments", tool)
		require.Contains(t, rs[i].Content, "Do not repeat this call unless the person asks.", tool)
	}
	last := rs[len(tools)].Content
	require.Contains(t, last, "Call "+stubReadTool+" again with the same arguments to get it back.")
	require.NotContains(t, last, "Do not repeat")
}

// A nil Replayable means nothing is replayable, whatever the tool.
func TestStub_NilReplayableIsNeverReplayable(t *testing.T) {
	w, err := NewToolWindow(ToolWindowSpec{Loader: LoaderDef(""), Names: []string{stubReadTool}})
	require.NoError(t, err)
	b := NewBuilder()
	addHist(t, b, HookConversation, histPair("x", stubReadTool, `{}`, jsonBody(4000), false)...)
	got := resultsOf(w.StubEnded(b.Snapshot(), 6400), HookConversation)[0].Content
	require.Contains(t, got, "Do not repeat this call unless the person asks.")
}

// The args the model wrote reach Replayable (a cursor makes a read not replayable).
func TestStub_ReplayableSeesTheCallsArguments(t *testing.T) {
	var seen []string
	w, err := NewToolWindow(ToolWindowSpec{
		Loader: LoaderDef(""), Names: []string{stubReadTool},
		Replayable: func(tool string, args json.RawMessage) bool { seen = append(seen, tool+string(args)); return false },
	})
	require.NoError(t, err)
	b := NewBuilder()
	addHist(t, b, HookConversation, histPair("x", stubReadTool, `{"cursor":"c9"}`, jsonBody(4000), false)...)
	w.StubEnded(b.Snapshot(), 6400)
	require.Equal(t, []string{stubReadTool + `{"cursor":"c9"}`}, seen)
}

func TestStub_ShortResultIsLeftWhole(t *testing.T) {
	w, b := stubWin(t)
	addHist(t, b, HookConversation, histPair("s", stubReadTool, `{}`, "ok", false)...)
	addHist(t, b, HookConversation, histPair("l", stubReadTool, `{}`, jsonBody(4000), false)...)
	rs := resultsOf(w.StubEnded(w.Project(b.Snapshot()), 6400), HookConversation)
	require.Equal(t, ToolResult{ToolUseID: "s", Content: "ok"}, rs[0])
	require.True(t, rs[1].Elided)
}

func TestStub_NeverLongerThanTheOriginal(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 20; i++ {
		n := rng.Intn(1200)
		content := strings.Repeat("é", n/2) + strings.Repeat("z", n%2)
		for _, replay := range []bool{true, false} {
			res := ToolResult{ToolUseID: "t", Content: content}
			got := stubResult(res, "knomit_query", replay, 4*stubExcerptTokens(2048))
			require.LessOrEqual(t, len(got.Content), len(content), "n=%d", n)
			require.Equal(t, got.Elided, got.Content != content, "n=%d: Elided says whether it was replaced", n)
		}
	}
}

func TestStub_MultibyteCutIsValidUTF8(t *testing.T) {
	// 3-byte runes cut at 64 bytes (64 % 3 == 1) must not leave half a rune.
	content := strings.Repeat("世", 2000)
	got := stubResult(ToolResult{ToolUseID: "t", Content: content}, "knomit_query", true, 64)
	require.True(t, got.Elided)
	require.True(t, utf8.ValidString(got.Content))
	require.Contains(t, got.Content, strings.Repeat("世", 21)+StubExcerptCut, "63 bytes: 21 whole runes")
}

func TestStub_CutPrefersALineBoundary(t *testing.T) {
	lines := strings.Repeat("0123456789\n", 30)
	got := stubResult(ToolResult{ToolUseID: "t", Content: lines}, "knomit_query", true, 64)
	require.True(t, strings.HasSuffix(got.Content, "0123456789"+StubExcerptCut), got.Content)
}

func TestStub_ExcerptSizeIsDerivedFromTheWindow(t *testing.T) {
	require.Equal(t, 100, stubExcerptTokens(6400))
	require.Equal(t, 400, stubExcerptTokens(25600))
	require.Equal(t, 0, stubExcerptTokens(0))

	w, b := stubWin(t)
	addHist(t, b, HookConversation, histPair("t", stubReadTool, `{}`, jsonBody(8000), false)...)
	small := resultsOf(w.StubEnded(b.Snapshot(), 6400), HookConversation)[0].Content
	large := resultsOf(w.StubEnded(b.Snapshot(), 25600), HookConversation)[0].Content
	require.Greater(t, len(large), len(small)+1000, "four times the room, about four times the excerpt")
	require.LessOrEqual(t, len(small), 400+300)
	require.LessOrEqual(t, len(large), 1600+300)
}

func TestStub_WithoutRoomThereIsNoExcerptButStillAStub(t *testing.T) {
	w, b := stubWin(t)
	addHist(t, b, HookConversation, histPair("t", stubReadTool, `{}`, jsonBody(4000), false)...)
	got := resultsOf(w.StubEnded(b.Snapshot(), 32), HookConversation)[0]
	require.True(t, got.Elided)
	require.Equal(t, fmt.Sprintf(StubReplayable, stubReadTool, 1000, stubReadTool), got.Content, "E rounds to 0: the header alone")
}

func TestStub_UnknownCallLeavesTheResultWhole(t *testing.T) {
	w, b := stubWin(t)
	e := Entry{ID: "orphan", Role: RoleUser, Blocks: []Block{{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "nope", Content: jsonBody(4000)}}}}
	addHist(t, b, HookConversation, e)
	rs := resultsOf(w.StubEnded(b.Snapshot(), 6400), HookConversation)
	require.False(t, rs[0].Elided, "no tool_use to name: not stubbed")
}

func TestStub_StoredHistoryIsWhole(t *testing.T) {
	w, b := stubWin(t)
	addHist(t, b, HookConversation, histPair("t", stubReadTool, `{}`, jsonBody(4000), false)...)
	before, _ := json.Marshal(b.Snapshot().Hooks[HookConversation])
	p := w.StubEnded(w.Project(b.Snapshot()), 6400)
	require.True(t, resultsOf(p, HookConversation)[0].Elided)
	after, _ := json.Marshal(b.Snapshot().Hooks[HookConversation])
	require.Equal(t, string(before), string(after), "the builder's entries were not edited")
	require.False(t, resultsOf(b.Snapshot(), HookConversation)[0].Elided)
}

// ---- the loop ----

// stubRig is an adaptive rig with a window of `window` tokens and a replayable
// query tool, so the second turn's view carries the first turn's stub.
func stubRig(t *testing.T, m *fakeModel, window int) *rig {
	t.Helper()
	m.caps = Caps{ContextWindow: window}
	r := newRig(t, m, winTool(stubReadTool, 10))
	r.w.replayable = fakeReplayable
	r.loadAhead(t, stubReadTool)
	return r
}

func TestLoop_StoredHistoryIsWholeAndTurnLogIsWhole(t *testing.T) {
	big := jsonBody(9000)
	m := &fakeModel{steps: []fakeStep{
		{resp: toolResp("tu1", stubReadTool, `{}`, Usage{Input: 100, Output: 5})},
		{resp: textResp("one", Usage{Input: 3000, Output: 5})},
		{resp: textResp("two", Usage{Input: 400, Output: 5})},
	}}
	r := stubRig(t, m, 16384)
	r.out = big
	_, err := r.l.RunTurn(context.Background(), "first")
	require.NoError(t, err)
	_, err = r.l.RunTurn(context.Background(), "second")
	require.NoError(t, err)

	// The third request is the second turn's: it carries the stub.
	sent := resultText(m.snaps[2], "tu1")
	require.NotNil(t, sent)
	require.True(t, sent.Elided)
	require.True(t, strings.HasPrefix(sent.Content, fmt.Sprintf(StubReplayable, stubReadTool, len(big)/4, stubReadTool)), sent.Content)
	// The first turn's own second request (the running turn) carried it whole.
	require.Equal(t, big, resultText(m.snaps[1], "tu1").Content)
	// The Builder and the turn log hold the whole result.
	require.Equal(t, big, resultIn(r.b.Snapshot(), "tu1").Content)
	whole := false
	for _, e := range r.log.turns {
		for _, bl := range e.Blocks {
			if bl.Kind == BlockToolResult && bl.ToolResult.Content == big && !bl.ToolResult.Elided {
				whole = true
			}
		}
	}
	require.True(t, whole, "Log.Turn received the whole entry at flush")
}

// A request the static loop refuses is sent once the ended turn's result is a
// stub: the estimate falls by the difference (the delta rule).
func TestLoop_StubbingShrinksTheNextEstimate(t *testing.T) {
	big := strings.Repeat("a", 24000)   // about 6000 tokens
	second := strings.Repeat("m", 6000) // about 1500 tokens
	steps := func() []fakeStep {
		return []fakeStep{
			{resp: toolResp("tu1", stubReadTool, `{}`, Usage{Input: 100, Output: 5})},
			{resp: textResp("one", Usage{Input: 6400, Output: 5})},
			{resp: textResp("two", Usage{Input: 400, Output: 5})},
		}
	}
	// Window 10000 tokens, reserve 2500: room 7500. The first turn fits (about
	// 6400). With the result whole, the second turn's request is about 7900 and
	// does not; with a stub it is about 2000 and does.
	run := func(r *rig) error {
		r.out = big
		_, err := r.l.RunTurn(context.Background(), "first")
		require.NoError(t, err)
		_, err = r.l.RunTurn(context.Background(), second)
		return err
	}

	control := &fakeModel{steps: steps()}
	rc := stubRig(t, control, 10000)
	rc.l.Window = nil // the static view: every tool, the whole history
	require.ErrorIs(t, run(rc), ErrContextFull, "fixture: without stubs the second turn is refused")
	require.Len(t, control.snaps, 2, "the refused request was never sent")

	m := &fakeModel{steps: steps()}
	r := stubRig(t, m, 10000)
	require.NoError(t, run(r), "stubbed, the request fits")
	require.Len(t, m.snaps, 3)
	require.True(t, resultText(m.snaps[2], "tu1").Elided)
}

func TestLoop_SessionLogRequestRecordsProjection(t *testing.T) {
	big := jsonBody(9000)
	m := &fakeModel{steps: []fakeStep{
		{resp: toolResp("tu1", stubReadTool, `{}`, Usage{Input: 100, Output: 5})},
		{resp: textResp("one", Usage{Input: 3000, Output: 5})},
		{resp: textResp("two", Usage{Input: 400, Output: 5})},
	}}
	r := stubRig(t, m, 16384)
	r.out = big
	_, err := r.l.RunTurn(context.Background(), "first")
	require.NoError(t, err)
	_, err = r.l.RunTurn(context.Background(), "second")
	require.NoError(t, err)
	require.Len(t, r.log.req, 3)

	// Request 2 (still the first turn) holds the result in hook 7; request 3 has
	// it in hook 6, stubbed: hook 6 tokens are the projected sizes.
	third := r.log.req[2].RequestRecord
	want := m.snaps[2].Record()
	require.Equal(t, want.Tokens[HookConversation], third.Tokens[HookConversation])
	stored := r.b.Snapshot().Record()
	require.Less(t, third.Tokens[HookConversation], stored.Tokens[HookConversation]-1500,
		"the log records the projected hook 6, not the stored one")
}

// A model with a stable prefix never gets a window, so nothing is stubbed: the
// second turn's request carries the first turn's result whole.
func TestLoop_StablePrefixNeverStubs(t *testing.T) {
	big := jsonBody(9000)
	m := &fakeModel{
		caps: Caps{ContextWindow: 100000, StablePrefix: true},
		steps: []fakeStep{
			{resp: toolResp("tu1", stubReadTool, `{}`, Usage{Input: 100, Output: 5})},
			{resp: textResp("one", Usage{Input: 3000, Output: 5})},
			{resp: textResp("two", Usage{Input: 400, Output: 5})},
		}}
	h := &fakeHost{out: map[string]ToolOutcome{stubReadTool: {Content: big}}}
	l, b := newLoop(t, m, h)
	_, err := l.RunTurn(context.Background(), "first")
	require.NoError(t, err)
	_, err = l.RunTurn(context.Background(), "second")
	require.NoError(t, err)
	got := resultText(m.snaps[2], "tu1")
	require.Equal(t, big, got.Content)
	require.False(t, got.Elided)
	require.Equal(t, b.Snapshot().Hooks[HookConversation][:len(m.snaps[2].Hooks[HookConversation])], m.snaps[2].Hooks[HookConversation])
}
