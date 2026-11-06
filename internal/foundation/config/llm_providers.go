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

	// Entries is the models allowlist as written: each element a model id or
	// { id, name }. Load derives Models from it.
	Entries []LLMModelEntry `toml:"models"`

	// Models is the allowlist of model ids, in order; required and non-empty.
	// The id is the identity everywhere; a name is display-only (ModelName).
	Models []string `toml:"-"`

	KeepAlive     string `toml:"keep_alive"`     // ollama
	NumCtx        int    `toml:"num_ctx"`        // ollama
	ContextWindow int    `toml:"context_window"` // anthropic default for models that name none

	// ModelDefaults are keyed by model id from Models. A model without an
	// entry gets llm.DefaultModelProfile, no price card (cost unknown) and
	// the provider's context window.
	ModelDefaults map[string]LLMModelConfig `toml:"model"`

	// AdaptiveTools is "" or "auto" (adaptive when the provider's adapter needs
	// no stable prefix) or "off" (always send every tool). "on" does not exist:
	// forcing it on a provider that needs a stable prefix would break its
	// requests.
	AdaptiveTools string `toml:"adaptive_tools"`
}

// adaptive_tools values.
const (
	AdaptiveToolsAuto = "auto"
	AdaptiveToolsOff  = "off"
)

// AdaptiveToolsOnRefused is the refusal for adaptive_tools = "on" on a provider
// whose adapter needs a stable prefix. It takes the provider name.
const AdaptiveToolsOnRefused = `adaptive_tools = "on" is not allowed for provider %s: its requests must keep a stable prefix.` // approved copy (design C10)

// AdaptiveToolsInvalid is the refusal for any other adaptive_tools value. It
// takes the provider's config path and the value.
const AdaptiveToolsInvalid = `%s.adaptive_tools %q: must be "auto" or "off"`

// validateAdaptiveTools checks adaptive_tools, the part that needs no model.
func (p LLMProviderConfig) validateAdaptiveTools(name, at string) error {
	switch p.AdaptiveTools {
	case "", AdaptiveToolsAuto, AdaptiveToolsOff:
		return nil
	case "on":
		if p.Kind == LLMKindAnthropic {
			return fmt.Errorf("%s: "+AdaptiveToolsOnRefused, at, name)
		}
	}
	return fmt.Errorf(AdaptiveToolsInvalid, at, p.AdaptiveTools)
}

// LLMModelEntry is one element of [llm.providers.<name>] models: a model id, or
// an inline table { id, name } whose name is what a person sees. Shape problems
// are recorded here and refused by validate, which knows the provider and index.
type LLMModelEntry struct {
	ID   string
	Name string // display name; "" when the entry names none

	bad     bool // neither a string nor a table with only string id and name
	hasName bool
}

// UnmarshalTOML decodes a string or an inline table; anything else is marked bad.
func (e *LLMModelEntry) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		e.ID = x
	case map[string]any:
		for k, val := range x {
			s, ok := val.(string)
			switch {
			case !ok || (k != "id" && k != "name"):
				e.bad = true
			case k == "id":
				e.ID = s
			default:
				e.Name, e.hasName = s, true
			}
		}
	default:
		e.bad = true
	}
	return nil
}

// displayName is the name a person sees: Name, or the id when none is given.
func (e LLMModelEntry) displayName() string {
	if e.Name != "" {
		return e.Name
	}
	return e.ID
}

// ModelName is the display name of model id in this provider: the name its
// entry gives, or the id itself when it gives none or the model is not listed.
func (p LLMProviderConfig) ModelName(id string) string {
	for _, e := range p.Entries {
		if e.ID == id && e.Name != "" {
			return e.Name
		}
	}
	return id
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
		p.Models = make([]string, 0, len(p.Entries))
		for _, e := range p.Entries {
			p.Models = append(p.Models, e.ID)
		}
		switch p.Kind {
		case LLMKindOllama:
			if p.KeepAlive == "" {
				p.KeepAlive = DefaultOllamaKeepAlive
			}
			// NumCtx has no default: 0 means "derive it from the model" (llm/numctx.go).
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

// APIKeyEnvNotANameMessage is the refusal for an api_key_env that cannot be an
// environment variable name. It takes the provider name and must never be given
// the value: that is the secret the user mistyped here.
const APIKeyEnvNotANameMessage = "llm.providers.%s.api_key_env must name an environment variable (like ANTHROPIC_API_KEY), not hold the key itself; put the key in api_key_file or api_key"

// APIKeyEnvProblem is the refusal text for a provider whose api_key_env is not
// an environment variable name, or "" when it is fine. It does not fail the
// config load: the llm registry marks the provider's models unavailable with
// this text, and the app logs it once. The text never contains the value.
func (p LLMProviderConfig) APIKeyEnvProblem(name string) string {
	if validEnvVarName(p.APIKeyEnv) {
		return ""
	}
	return fmt.Sprintf(APIKeyEnvNotANameMessage, name)
}

// APIKeyEnvBlocked is the APIKeyEnvProblem text when the bad api_key_env is the
// source the provider would have used: an anthropic provider that is left with
// no key once api_key_file and api_key are tried (ResolveKey never reads a bad
// api_key_env). A higher-precedence api_key_file or a plaintext api_key keeps
// the provider available; the config is still wrong and is still logged.
// An unreadable api_key_file is reported by ResolveKey's own error instead.
func (p LLMProviderConfig) APIKeyEnvBlocked(name string) string {
	why := p.APIKeyEnvProblem(name)
	if why == "" || p.Kind != LLMKindAnthropic {
		return ""
	}
	if k, err := p.ResolveKey(); err != nil || k != "" {
		return ""
	}
	return why
}

// APIKeyEnvProblems lists every provider's APIKeyEnvProblem, sorted by
// provider name.
func (c LLMConfig) APIKeyEnvProblems() []string {
	names := make([]string, 0, len(c.Providers))
	for n := range c.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		if m := c.Providers[n].APIKeyEnvProblem(n); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// validEnvVarName reports whether s is empty (unset: the default applies) or
// matches [A-Za-z_][A-Za-z0-9_]*.
func validEnvVarName(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
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
	if err := p.validateAdaptiveTools(name, at); err != nil {
		return err
	}
	allowed := map[string]bool{}
	for i, e := range p.Entries {
		ep := fmt.Sprintf("%s.models[%d]", at, i)
		if e.bad {
			return fmt.Errorf("%s: must be a model id or { id, name }", ep)
		}
		if strings.TrimSpace(e.ID) == "" {
			return fmt.Errorf("%s: id is required", ep)
		}
		if e.hasName && strings.TrimSpace(e.Name) == "" {
			return fmt.Errorf("%s: name must not be empty", ep)
		}
		if allowed[e.ID] {
			return fmt.Errorf("%s.models: id %q is listed more than once", at, e.ID)
		}
		allowed[e.ID] = true
	}
	// A name is unique within the provider and is never another model's id.
	// Two providers may share a name.
	names := map[string]bool{}
	for _, e := range p.Entries {
		n := e.displayName()
		for _, o := range p.Entries {
			if o.ID == n && o.ID != e.ID {
				return fmt.Errorf("%s.models: name %q is the id of another model", at, n)
			}
		}
		if names[n] {
			return fmt.Errorf("%s.models: name %q is used by more than one model", at, n)
		}
		names[n] = true
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
		if p.Kind == LLMKindOllama && md.MaxOutputTokens > 0 && p.NumCtx > 0 && md.MaxOutputTokens >= p.NumCtx {
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
	if p.APIKeyEnv != "" && validEnvVarName(p.APIKeyEnv) {
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
