package llm

import "context"

type Message struct {
	Role    string // "user" | "assistant"
	Content string
}

type LLMAdapter interface {
	// Complete sends msgs to the LLM with a system prompt and streams the response.
	// onChunk is called for each text delta as it arrives; it may be nil.
	// Returns the complete accumulated response text.
	Complete(ctx context.Context, system string, msgs []Message, onChunk func(string)) (string, error)
}
