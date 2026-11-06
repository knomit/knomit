package llm

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// adaptiveOpeningLoop is an adaptive Loop whose first request (no conversation
// yet) carries a system block of about bulkTokens tokens.
func adaptiveOpeningLoop(t *testing.T, m ChatModel, bulkTokens int) *Loop {
	t.Helper()
	w, err := NewToolWindow(ToolWindowSpec{Loader: LoaderDef("- a: first"), Names: []string{"a"}})
	require.NoError(t, err)
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "tool.a", Hook: HookTools, Tool: winTool("a", 50)}))
	require.NoError(t, b.Add(Item{ID: "bulk", Hook: HookSystem, Text: strings.Repeat("t", bulkTokens*4)}))
	return &Loop{Model: m, Builder: b, Tools: &fakeHost{}, Window: w}
}

// An adaptive chat whose opening does not fit says what the opening is made of
// (the catalogue, not the tool definitions), with the same sizes and the
// Ollama hint. A static chat keeps the original sentence.
//
// SABOTAGE: agent.go OpeningTooBigError, ignore `adaptive` -> the first
// assertion fails (the static sentence is used).
func TestLoop_AdaptiveOpeningTooBigUsesTheAdaptiveVariant(t *testing.T) {
	fm := &fakeModel{caps: Caps{ContextWindow: 32768, MaxOutput: 8192}, steps: []fakeStep{{resp: textResp("never", Usage{})}}}
	l := adaptiveOpeningLoop(t, ollamaModel{fm}, 25000)
	_, err := l.RunTurn(context.Background(), "hi")
	require.True(t, errors.Is(err, ErrContextFull))
	// The estimate is the opening as the loop saw it (the dropped turn's user
	// message included): read it off the sentence, then require the whole
	// sentence to be the approved one filled with it.
	m := regexp.MustCompile(`needs about (\d+) tokens`).FindStringSubmatch(err.Error())
	require.Len(t, m, 2)
	est, convErr := strconv.Atoi(m[1])
	require.NoError(t, convErr)
	require.InDelta(t, 25000, est, 300, "the system block alone is 25000 tokens")
	require.Equal(t, fmt.Sprintf(OpeningTooBigAdaptiveMessage, est, fm.Model(), 32768, 8192, 24576, OllamaRaiseNumCtxHint), err.Error())
	require.Contains(t, err.Error(), "the tool catalogue")
	require.NotContains(t, err.Error(), "the tool definitions")
	require.Empty(t, fm.snaps, "nothing was sent")

	// The same opening on a static loop (Window nil) keeps the original text.
	fs := &fakeModel{caps: Caps{ContextWindow: 32768, MaxOutput: 8192, StablePrefix: true}}
	ls := openingLoop(t, ollamaModel{fs}, 25000)
	_, err = ls.RunTurn(context.Background(), "hi")
	require.Contains(t, err.Error(), "the tool definitions")
	require.NotContains(t, err.Error(), "the tool catalogue")
}

// The request record of an adaptive chat says how much the projection
// shrank the request; a static chat's record has no projection.
//
// SABOTAGE: agent.go account, drop the Projection field -> this fails.
func TestLoop_RequestLogCarriesTheProjection(t *testing.T) {
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

	for i := 0; i < 2; i++ {
		require.NotNil(t, r.log.req[i].Projection, "request %d", i)
		require.Zero(t, r.log.req[i].Projection.StubbedResults, "no ended turn yet in request %d", i)
	}
	p := r.log.req[2].Projection
	require.NotNil(t, p)
	require.Equal(t, 1, p.StubbedResults, "the first turn's 9000-byte result is stubbed in the third request")
	// Hook 6 was complete when the third request was built, so the saving is
	// the stored hook 6 less the sent hook 6; every tool the builder holds was
	// either loaded or nothing else is sized here.
	storedByID := map[string]Item{}
	for _, it := range r.b.Snapshot().Hooks[HookConversation] {
		storedByID[it.ID] = it
	}
	stored, sent := 0, 0
	for _, it := range m.snaps[2].Hooks[HookConversation] {
		sent += itemBytes(it)
		stored += itemBytes(storedByID[it.ID])
	}
	// Hook 1 differs too: the sent one is the loader and the loaded tools, the
	// stored one every tool, so the saving is net of the loader's definition.
	for _, it := range r.b.Snapshot().Hooks[HookTools] {
		stored += itemBytes(it)
	}
	for _, it := range m.snaps[2].Hooks[HookTools] {
		sent += itemBytes(it)
	}
	require.Equal(t, (stored-sent)/4, p.SavedTokensEst, "stored less sent, in bytes/4")
	require.Greater(t, p.SavedTokensEst, 1500)
}

func TestLoop_StaticRequestLogHasNoProjection(t *testing.T) {
	m := &fakeModel{caps: Caps{StablePrefix: true}, steps: []fakeStep{{resp: textResp("hi", Usage{})}}}
	l, _ := newLoop(t, m, &fakeHost{})
	rl := &recLog{}
	l.Log = rl
	_, err := l.RunTurn(context.Background(), "x")
	require.NoError(t, err)
	require.Len(t, rl.req, 1)
	require.Nil(t, rl.req[0].Projection)
}

// A catalogue tool named like the loader is refused with the approved wording
// (design C10), with no package prefix.
func TestWindow_ReservedNameErrorIsTheApprovedSentence(t *testing.T) {
	_, err := NewToolWindow(ToolWindowSpec{Loader: LoaderDef(""), Names: []string{LoadToolsName}})
	require.EqualError(t, err, fmt.Sprintf(ReservedToolNameMessage, LoadToolsName))
}

func TestReplyReserve_IsTheLoopsOwn(t *testing.T) {
	require.Equal(t, 8192, ReplyReserve(Caps{ContextWindow: 32768, MaxOutput: 8192}))
	require.Equal(t, 100, ReplyReserve(Caps{ContextWindow: 400}))
}
