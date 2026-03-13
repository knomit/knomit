package llm

import (
	"context"
	"fmt"
	"strings"
)

func ResolveProvider(model, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if model == "claude" {
		return "claudecli", nil
	}
	if model == "gemini" {
		return "geminicli", nil
	}
	if strings.HasPrefix(model, "claude-") {
		return "anthropic", nil
	}
	if strings.HasPrefix(model, "gemini-") {
		return "gemini", nil
	}
	if strings.HasPrefix(model, "anthropic.") ||
		strings.HasPrefix(model, "us.") ||
		strings.HasPrefix(model, "eu.") {
		return "bedrock", nil
	}
	return "", fmt.Errorf("cannot infer provider from model %q", model)
}

func NewAdapter(ctx context.Context, provider, model string) (LLMAdapter, error) {
	switch provider {
	case "anthropic":
		return NewAnthropicAdapter(model), nil
	case "gemini":
		return NewGeminiAdapter(ctx, model)
	case "bedrock":
		return NewBedrockAdapter(ctx, model)
	case "claudecli":
		return NewClaudeCLIAdapter(model), nil
	case "geminicli":
		return NewGeminiCLIAdapter(model), nil
	case "ollama":
		return NewOllamaAdapter(ctx, model)
	default:
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
}
