package config

import (
	_ "embed"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
)

// Provider kinds with an adapter. "bedrock" and "gemini" are refused until
// their v2 adapters exist.
const (
	LLMKindAnthropic = "anthropic"
	LLMKindOllama    = "ollama"

	// DefaultAnthropicKeyEnv is the env var an anthropic provider reads when it
	// names none.
	DefaultAnthropicKeyEnv = "ANTHROPIC_API_KEY"

	DefaultOllamaKeepAlive = "30m"
	DefaultOllamaNumCtx    = 32768
)

// LLMProviderConfig is one [llm.providers.<name>] table: where a chat model
// runs, how to authenticate, which models the chat may use, and per-model
// defaults and price cards. Prices and model ids live in config, never in code.
type LLMProviderConfig struct {
	Kind string `toml:"kind"` // "anthropic" | "ollama"

	// Secret sources, in precedence order: api_key_file, then api_key_env,
	// then api_key. A plaintext api_key is allowed (it is not refused); it is
	// never logged.
	APIKeyEnv  string `toml:"api_key_env"`  // default ANTHROPIC_API_KEY for anthropic
	APIKeyFile string `toml:"api_key_file"` // owner-only file
	APIKey     string `toml:"api_key"`

	BaseURL string `toml:"base_url"`
	Host    string `toml:"host"`

	Models []string `toml:"models"` // allowlist; required and non-empty

	KeepAlive     string `toml:"keep_alive"`     // ollama
	NumCtx        int    `toml:"num_ctx"`        // ollama
	ContextWindow int    `toml:"context_window"` // anthropic default for models that name none

	// ModelDefaults are keyed by model id from Models. A model without an
	// entry gets llm.DefaultModelProfile, no price card (cost unknown) and
	// the provider's context window.
	ModelDefaults map[string]LLMModelConfig `toml:"model"`
}

// LLMModelConfig is [llm.providers.<name>.model.<id>].
type LLMModelConfig struct {
	Effort          string `toml:"effort"` // low|medium|high|xhigh|max; always sent
	MaxOutputTokens int    `toml:"max_output_tokens"`
	ThinkingDisplay string `toml:"thinking_display"` // summarized|omitted
	// ContextWindow is this model's window in tokens (anthropic). It comes
	// from config, not code: Haiku, Sonnet and Opus 5.5 are 1M.
	ContextWindow int           `toml:"context_window"`
	Price         *LLMPriceCard `toml:"price"`
}

// LLMPriceCard is a model's USD-per-million-token prices. The tier is chosen
// per request on ALL prompt tokens (input, cache read and cache write).
type LLMPriceCard struct {
	// Tiers ascend by UpToPromptTokens; the last has 0 (unbounded). One or
	// two tiers are supported.
	Tiers            []LLMPriceTier `toml:"tiers"`
	CacheReadMult    float64        `toml:"cache_read_mult"`
	CacheWrite5mMult float64        `toml:"cache_write_5m_mult"`
	CacheWrite1hMult float64        `toml:"cache_write_1h_mult"`
	// WarnPromptTokens is the prompt size that triggers the price-tier
	// warning; 0 disables it.
	WarnPromptTokens int `toml:"warn_prompt_tokens"`
}

// LLMPriceTier prices requests whose prompt has at most UpToPromptTokens
// tokens (0: no upper bound).
type LLMPriceTier struct {
	UpToPromptTokens int     `toml:"up_to_prompt_tokens"`
	InputPerMTok     float64 `toml:"input_per_mtok"`
	OutputPerMTok    float64 `toml:"output_per_mtok"`
}

var (
	llmEfforts  = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}
	llmDisplays = map[string]bool{"summarized": true, "omitted": true}
)

// applyProviderDefaults fills the provider defaults that depend on its kind.
func (c *LLMConfig) applyProviderDefaults() {
	for name, p := range c.Providers {
		switch p.Kind {
		case LLMKindOllama:
			if p.KeepAlive == "" {
				p.KeepAlive = DefaultOllamaKeepAlive
			}
			if p.NumCtx <= 0 {
				p.NumCtx = DefaultOllamaNumCtx
			}
		case LLMKindAnthropic:
			if p.APIKeyEnv == "" {
				p.APIKeyEnv = DefaultAnthropicKeyEnv
			}
		}
		c.Providers[name] = p
	}
}

// validateProviders checks every [llm.providers.*] table.
func (c LLMConfig) validateProviders() error {
	names := make([]string, 0, len(c.Providers))
	for n := range c.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := c.Providers[name].validate(name); err != nil {
			return err
		}
	}
	return nil
}

func (p LLMProviderConfig) validate(name string) error {
	at := "config: [llm.providers." + name + "]"
	switch p.Kind {
	case LLMKindAnthropic, LLMKindOllama:
	case "bedrock", "gemini":
		return fmt.Errorf("%s.kind %q has no chat adapter yet; use %q or %q", at, p.Kind, LLMKindAnthropic, LLMKindOllama)
	default:
		return fmt.Errorf("%s.kind %q: must be %q or %q", at, p.Kind, LLMKindAnthropic, LLMKindOllama)
	}
	if len(p.Models) == 0 {
		return fmt.Errorf("%s.models is empty: list the model ids the chat may use", at)
	}
	allowed := map[string]bool{}
	for _, m := range p.Models {
		if strings.TrimSpace(m) == "" {
			return fmt.Errorf("%s.models has an empty model id", at)
		}
		allowed[m] = true
	}
	ids := make([]string, 0, len(p.ModelDefaults))
	for id := range p.ModelDefaults {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		md := p.ModelDefaults[id]
		mat := at + ".model." + id
		if !allowed[id] {
			return fmt.Errorf("%s: %q is not in models %v", mat, id, p.Models)
		}
		if md.Effort != "" && !llmEfforts[md.Effort] {
			return fmt.Errorf("%s.effort %q: must be low, medium, high, xhigh or max", mat, md.Effort)
		}
		if md.ThinkingDisplay != "" && !llmDisplays[md.ThinkingDisplay] {
			return fmt.Errorf("%s.thinking_display %q: must be summarized or omitted", mat, md.ThinkingDisplay)
		}
		if md.MaxOutputTokens < 0 || md.ContextWindow < 0 {
			return fmt.Errorf("%s: max_output_tokens and context_window must not be negative", mat)
		}
		if p.Kind == LLMKindOllama && md.MaxOutputTokens > 0 && md.MaxOutputTokens >= p.NumCtx {
			// The trained-length half of this rule can only be checked at probe time
			// (llm.Registry), because /api/show reports it.
			return fmt.Errorf("%s.max_output_tokens %d must be below num_ctx %d: num_ctx covers the prompt and the reply together", mat, md.MaxOutputTokens, p.NumCtx)
		}
		if md.Price != nil {
			if err := md.Price.validate(mat + ".price"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *LLMPriceCard) validate(at string) error {
	if n := len(c.Tiers); n == 0 || n > 2 {
		return fmt.Errorf("%s.tiers: want 1 or 2 tiers, got %d", at, n)
	}
	prev := 0
	for i, t := range c.Tiers {
		last := i == len(c.Tiers)-1
		if last && t.UpToPromptTokens != 0 {
			return fmt.Errorf("%s.tiers[%d]: the last tier must have up_to_prompt_tokens = 0 (unbounded)", at, i)
		}
		if !last && t.UpToPromptTokens <= prev {
			return fmt.Errorf("%s.tiers[%d]: up_to_prompt_tokens must ascend and be positive", at, i)
		}
		if t.InputPerMTok < 0 || t.OutputPerMTok < 0 {
			return fmt.Errorf("%s.tiers[%d]: prices must not be negative", at, i)
		}
		prev = t.UpToPromptTokens
	}
	if c.CacheReadMult < 0 || c.CacheWrite5mMult < 0 || c.CacheWrite1hMult < 0 || c.WarnPromptTokens < 0 {
		return fmt.Errorf("%s: multipliers and warn_prompt_tokens must not be negative", at)
	}
	if len(c.Tiers) == 2 && c.WarnPromptTokens > c.Tiers[0].UpToPromptTokens {
		return fmt.Errorf("%s.warn_prompt_tokens %d is above the tier boundary %d: the warning would come after the price rise", at, c.WarnPromptTokens, c.Tiers[0].UpToPromptTokens)
	}
	return nil
}

// ResolveKey returns the provider's API key: the owner-only api_key_file, then
// the env var named by api_key_env, then a plaintext api_key. It returns ""
// and no error when none is set (the SDK may still find its own credentials).
// The key must never be logged.
func (p LLMProviderConfig) ResolveKey() (string, error) {
	if p.APIKeyFile != "" {
		st, err := os.Stat(p.APIKeyFile)
		if err != nil {
			return "", fmt.Errorf("config: [llm.providers].api_key_file: %w", err)
		}
		if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("config: [llm.providers].api_key_file %q has mode %o; it must be readable by its owner only (0600 or stricter)", p.APIKeyFile, st.Mode().Perm())
		}
		b, err := os.ReadFile(p.APIKeyFile)
		if err != nil {
			return "", fmt.Errorf("config: [llm.providers].api_key_file: %w", err)
		}
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, nil
		}
	}
	if p.APIKeyEnv != "" {
		if v := os.Getenv(p.APIKeyEnv); v != "" {
			return v, nil
		}
	}
	return p.APIKey, nil
}

// ExampleLLMProvidersTOML is the shipped example [llm.providers] config: the
// Anthropic models with their context windows and price cards, and a
// commented Ollama table. A test parses and validates it.
//
//go:embed llm_example.toml
var ExampleLLMProvidersTOML string
