package llm

import (
	"fmt"
	"strings"
)

func ResolveProvider(model, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
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

func NewAdapter(provider, model string) (LLMAdapter, error) {
	switch provider {
	case "anthropic":
		return NewAnthropicAdapter(model), nil
	default:
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
}
