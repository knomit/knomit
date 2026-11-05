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

// The adapter refusing an opening the estimate says fits (about 100 tokens in a
// 32768 window) must not claim the opening needs more than the window leaves:
// it says the server refused it despite the estimate, and for Ollama points at
// the server's num_ctx.
//
// SABOTAGE: agent.go, call openingTooBig instead of openingRefused on the
// adapter path -> the false "leaves" sentence appears and this fails.
func TestLoop_AdapterRefusalOfTheOpeningSaysTheServerRefusedDespiteTheEstimate(t *testing.T) {
	fm := &fakeModel{caps: Caps{ContextWindow: 32768, MaxOutput: 8192}, steps: []fakeStep{{err: fmt.Errorf("wrap: %w", ErrContextFull)}}}
	l := openingLoop(t, ollamaModel{fm}, 100)
	_, err := l.RunTurn(context.Background(), "hi")
	if !errors.Is(err, ErrContextFull) {
		t.Fatalf("err = %v, want ErrContextFull", err)
	}
	msg := err.Error()
	for _, want := range []string{"The server refused the opening of this chat", "fits the context window reported", "(32768)", "num_ctx"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	for _, bad := range []string{"needs about", "leaves", "reserved for the reply"} {
		if strings.Contains(msg, bad) {
			t.Errorf("message %q contains the false claim %q", msg, bad)
		}
	}
	// Another provider has no num_ctx to name.
	fm2 := &fakeModel{caps: Caps{ContextWindow: 32768, MaxOutput: 8192}, steps: []fakeStep{{err: ErrContextFull}}}
	_, err = openingLoop(t, fm2, 100).RunTurn(context.Background(), "hi")
	if err == nil || strings.Contains(err.Error(), "num_ctx") || !strings.Contains(err.Error(), "The server refused the opening") {
		t.Fatalf("err = %v, want the server-refused message without num_ctx", err)
	}
}
