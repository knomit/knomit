package llm

import (
	"context"
	"fmt"
	"knomit/internal/config"
	"strings"
)

// ResolveProvider determines the provider name from a model string.
// If explicit is non-empty it is returned as-is. Otherwise the model name
// is matched by prefix:
//
//   - "claude"             → "claudecli"  (bare name = CLI shorthand)
//   - "gemini"             → "geminicli"
//   - "claude-*"           → "anthropic"
//   - "gemini-*"           → "gemini"
//   - "anthropic.*|us.*|eu.*" → "bedrock"
//
// Returns an error if no rule matches.
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

// NewAdapter is the top-level factory: given a provider name (from
// ResolveProvider) and a model string, it returns the corresponding
// LLMAdapter. Each provider performs its own initialization (API client
// creation, health checks, credential loading).
//
// Every provider is returned wrapped in the resilience decorator, so retry,
// per-attempt timeout and request metrics are a property of *having* an
// adapter rather than something each call site must remember to ask for. The
// per-provider budget comes from policyFor.
func NewAdapter(ctx context.Context, provider, model string, cfg ...config.LLMConfig) (LLMAdapter, error) {
	var c config.LLMConfig
	if len(cfg) > 0 {
		c = cfg[0]
	}
	var (
		inner LLMAdapter
		err   error
	)
	switch provider {
	case "anthropic":
		inner = NewAnthropicAdapter(model)
	case "gemini":
		inner, err = NewGeminiAdapter(ctx, model, c)
	case "bedrock":
		inner, err = NewBedrockAdapter(ctx, model)
	case "claudecli":
		inner = NewClaudeCLIAdapter(model)
	case "geminicli":
		inner = NewGeminiCLIAdapter(model)
	case "ollama":
		inner, err = NewOllamaAdapter(ctx, model)
	default:
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
	if err != nil {
		return nil, err
	}
	return wrapResilient(inner, policyFor(provider)), nil
}
