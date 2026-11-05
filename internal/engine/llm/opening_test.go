package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ollamaModel is a fakeModel that reports itself as an Ollama provider.
type ollamaModel struct{ *fakeModel }

func (ollamaModel) Provider() string { return "ollama" }

// openingLoop is a Loop whose system block alone is about `toolTokens` tokens.
func openingLoop(t *testing.T, m ChatModel, toolTokens int) *Loop {
	t.Helper()
	l, b := newLoop(t, nil, &fakeHost{})
	l.Model = m
	if err := b.Add(Item{ID: "bulk", Hook: HookSystem, Text: strings.Repeat("t", toolTokens*4)}); err != nil {
		t.Fatalf("add bulk: %v", err)
	}
	return l
}

// When the very first request of a chat does not fit, the refusal names the
// opening's size, the window, the reserve and the room left, and for Ollama
// says to raise num_ctx. Nothing is sent and the turn is dropped.
//
// SABOTAGE: agent.go checkBudget, drop the `isOpening(snap)` branch -> the
// message is the generic one and this fails.
func TestLoop_OpeningTooBigNamesTheSizesAndNumCtxForOllama(t *testing.T) {
	fm := &fakeModel{caps: Caps{ContextWindow: 32768, MaxOutput: 8192}, steps: []fakeStep{{resp: textResp("never", Usage{})}}}
	l := openingLoop(t, ollamaModel{fm}, 25000)
	_, err := l.RunTurn(context.Background(), "hi")
	if !errors.Is(err, ErrContextFull) {
		t.Fatalf("err = %v, want ErrContextFull", err)
	}
	msg := err.Error()
	for _, want := range []string{"The opening of this chat", "about 25", "32768", "8192 of it is reserved", "leaves 24576", "raise num_ctx"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if len(fm.snaps) != 0 {
		t.Fatalf("the model got %d requests, want none", len(fm.snaps))
	}
}

// A provider that is not Ollama has no num_ctx to raise.
func TestLoop_OpeningTooBigOmitsNumCtxForOtherProviders(t *testing.T) {
	fm := &fakeModel{caps: Caps{ContextWindow: 32768, MaxOutput: 8192}}
	l := openingLoop(t, fm, 25000)
	_, err := l.RunTurn(context.Background(), "hi")
	if !errors.Is(err, ErrContextFull) || strings.Contains(err.Error(), "num_ctx") || !strings.Contains(err.Error(), "The opening of this chat") {
		t.Fatalf("err = %v, want the opening message without num_ctx", err)
	}
}

// A later turn that no longer fits keeps the generic message: the fix for a
// long chat is a new chat, not a larger window.
func TestLoop_LaterTurnTooBigKeepsTheGenericMessage(t *testing.T) {
	fm := &fakeModel{caps: Caps{ContextWindow: 2000, MaxOutput: 1000}, steps: []fakeStep{{resp: textResp("fine", Usage{Input: 30})}}}
	l := openingLoop(t, ollamaModel{fm}, 10)
	if _, err := l.RunTurn(context.Background(), "short"); err != nil {
		t.Fatal(err)
	}
	_, err := l.RunTurn(context.Background(), strings.Repeat("x", 2000*4))
	if !errors.Is(err, ErrContextFull) || strings.Contains(err.Error(), "The opening") || !strings.Contains(err.Error(), "no longer fits") {
		t.Fatalf("err = %v, want the generic context-full message", err)
	}
}

// The adapter refusing the opening (the estimate was under the window but the
// server disagrees) gets the same message.
func TestLoop_AdapterRefusalOfTheOpeningNamesTheSizes(t *testing.T) {
	fm := &fakeModel{caps: Caps{ContextWindow: 32768, MaxOutput: 8192}, steps: []fakeStep{{err: fmt.Errorf("wrap: %w", ErrContextFull)}}}
	l := openingLoop(t, ollamaModel{fm}, 100)
	_, err := l.RunTurn(context.Background(), "hi")
	if !errors.Is(err, ErrContextFull) || !strings.Contains(err.Error(), "The opening of this chat") || !strings.Contains(err.Error(), "raise num_ctx") {
		t.Fatalf("err = %v, want the opening message", err)
	}
}
