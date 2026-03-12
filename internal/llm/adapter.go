// Package llm provides a uniform interface for calling large language models.
//
// All providers implement LLMAdapter, whose single method (Complete) accepts
// a system prompt and a conversation, streams text deltas via an optional
// callback, and returns the full response. The provider is selected at
// runtime by ResolveProvider (model-name heuristics) or explicitly via
// NewAdapter.
//
// Supported providers:
//
//   - anthropic    — Anthropic Messages API via the official Go SDK (streaming).
//   - bedrock      — AWS Bedrock InvokeModelWithResponseStream (Anthropic format).
//   - gemini       — Google Gemini API via the genai SDK (streaming).
//   - ollama       — Local/remote Ollama REST API (/api/chat, NDJSON streaming).
//   - claudecli    — Shells out to the `claude` CLI (non-streaming, single-turn).
//   - geminicli    — Shells out to the `gemini` CLI (non-streaming, single-turn).
//
// Files in this package:
//
//   - adapter.go   — Core types: Message, LLMAdapter interface.
//   - config.go    — Config struct and DefaultConfig.
//   - resolver.go  — ResolveProvider (model→provider mapping) and NewAdapter factory.
//   - anthropic.go — AnthropicAdapter (streaming via anthropic-sdk-go).
//   - bedrock.go   — BedrockAdapter (streaming via AWS SDK).
//   - gemini.go    — GeminiAdapter (streaming via genai SDK).
//   - ollama.go    — OllamaAdapter (NDJSON streaming via HTTP).
//   - claudecli.go — ClaudeCLIAdapter (subprocess, single-turn).
//   - geminicli.go — GeminiCLIAdapter (subprocess, single-turn).
package llm

import "context"

// Message represents a single turn in a conversation.
// Role is "user" or "assistant".
type Message struct {
	Role    string // "user" | "assistant"
	Content string
}

// LLMAdapter is the common interface implemented by every provider.
// Complete sends a system prompt and conversation to the model, streaming
// text deltas through onChunk (which may be nil). It returns the full
// accumulated response or an error.
type LLMAdapter interface {
	Complete(ctx context.Context, system string, msgs []Message, onChunk func(string)) (string, error)
}
