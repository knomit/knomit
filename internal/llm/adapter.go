package llm

import "context"

type Message struct {
	Role    string // "user" | "assistant"
	Content string
}

type LLMAdapter interface {
	Complete(ctx context.Context, system string, msgs []Message, onChunk func(string)) (string, error)
}
