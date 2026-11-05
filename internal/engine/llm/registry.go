package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"knomit/internal/foundation/config"
)

const registryCacheTTL = 60 * time.Second

// ModelStatus is one allowlisted model's availability. Tools is whether the
// model can run the chat's tool calls (Ollama: the /api/show "tools"
// capability). Reason says why a model is unavailable or toolless.
type ModelStatus struct {
	ID        string
	Available bool
	Tools     bool
	Reason    string
}

// ProviderStatus is one configured provider and its allowlisted models.
type ProviderStatus struct {
	Name, Kind string
	Models     []ModelStatus
}

// Registry opens chat models from the [llm.providers] config and reports
// which of the allowlisted models are usable. It holds no model ids or
// prices: everything comes from the config it was built with.
type Registry struct {
	cfg map[string]config.LLMProviderConfig
	now func() time.Time

	mu     sync.Mutex
	cached []ProviderStatus
	at     time.Time

	tracePath string
	tracers   []io.Closer
}

// NewRegistry builds a registry over cfg.
func NewRegistry(cfg map[string]config.LLMProviderConfig) *Registry {
	return &Registry{cfg: cfg, now: time.Now}
}

// SetTrace makes every model Open returns append a per-request debugging line
// to path (KNOMIT_LLM_TRACE). Close releases the files.
func (r *Registry) SetTrace(path string) {
	r.mu.Lock()
	r.tracePath = path
	r.mu.Unlock()
}

// Close releases the trace files Open created.
func (r *Registry) Close() error {
	r.mu.Lock()
	ts := r.tracers
	r.tracers = nil
	r.mu.Unlock()
	var first error
	for _, t := range ts {
		if err := t.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// traced wraps m with the tracer when one is set.
func (r *Registry) traced(m ChatModel) (ChatModel, error) {
	r.mu.Lock()
	path := r.tracePath
	r.mu.Unlock()
	if path == "" {
		return m, nil
	}
	t, c, err := NewTracingChat(m, path)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.tracers = append(r.tracers, c)
	r.mu.Unlock()
	return t, nil
}

// List reports every provider, sorted by name, with each allowlisted model's
// availability. The answer is cached for 60 seconds.
func (r *Registry) List(ctx context.Context) []ProviderStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cached != nil && r.now().Sub(r.at) < registryCacheTTL {
		return r.cached
	}
	names := make([]string, 0, len(r.cfg))
	for n := range r.cfg {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ProviderStatus, 0, len(names))
	for _, n := range names {
		p := r.cfg[n]
		st := ProviderStatus{Name: n, Kind: p.Kind}
		if why := p.APIKeyEnvBlocked(n); why != "" {
			st.Models = allUnavailable(p, why)
			out = append(out, st)
			continue
		}
		switch p.Kind {
		case config.LLMKindAnthropic:
			st.Models = r.anthropicStatus(ctx, p)
		case config.LLMKindOllama:
			st.Models = r.ollamaStatus(ctx, p)
		}
		out = append(out, st)
	}
	r.cached, r.at = out, r.now()
	return out
}

func allUnavailable(p config.LLMProviderConfig, reason string) []ModelStatus {
	out := make([]ModelStatus, 0, len(p.Models))
	for _, m := range p.Models {
		out = append(out, ModelStatus{ID: m, Reason: reason})
	}
	return out
}

func (r *Registry) anthropicStatus(ctx context.Context, p config.LLMProviderConfig) []ModelStatus {
	key, err := p.ResolveKey()
	if err != nil {
		return allUnavailable(p, "credentials unreadable: "+err.Error())
	}
	if key == "" {
		return allUnavailable(p, "no API key configured")
	}
	opts := []option.RequestOption{option.WithAPIKey(key)}
	if p.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(p.BaseURL))
	}
	client := anthropic.NewClient(opts...)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	offered := map[string]bool{}
	pager := client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	for pager.Next() {
		offered[pager.Current().ID] = true
	}
	if err := pager.Err(); err != nil {
		// The key travels only as a header, but the SDK's error text includes
		// the response body, and a proxy or misconfigured endpoint can echo
		// request headers into it. Blank the key before the text is shown.
		return allUnavailable(p, "could not list models: "+redactKey(err, key).Error())
	}
	out := make([]ModelStatus, 0, len(p.Models))
	for _, m := range p.Models {
		ms := ModelStatus{ID: m, Available: offered[m], Tools: true}
		if !ms.Available {
			ms.Tools = false
			ms.Reason = "not offered by the provider"
		}
		out = append(out, ms)
	}
	return out
}

func (r *Registry) ollamaStatus(ctx context.Context, p config.LLMProviderConfig) []ModelStatus {
	host := ollamaHost(p)
	pulled, err := ollamaTags(ctx, host)
	if err != nil {
		return allUnavailable(p, "ollama unreachable: "+err.Error())
	}
	out := make([]ModelStatus, 0, len(p.Models))
	for _, m := range p.Models {
		if !pulled[m] {
			out = append(out, ModelStatus{ID: m, Reason: "not pulled on the Ollama host"})
			continue
		}
		c, err := NewOllamaChat(ctx, m, OllamaOptions{Host: host, NumCtx: p.NumCtx, KeepAlive: p.KeepAlive})
		if err != nil {
			out = append(out, ModelStatus{ID: m, Reason: err.Error()})
			continue
		}
		out = append(out, ModelStatus{ID: m, Available: true, Tools: c.Caps().Tools, Reason: c.UnusableReason()})
	}
	return out
}

func ollamaHost(p config.LLMProviderConfig) string {
	h := p.Host
	if h == "" {
		h = p.BaseURL
	}
	return h
}

func ollamaTags(ctx context.Context, host string) (map[string]bool, error) {
	o := OllamaOptions{Host: host}.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.Host+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("unreadable response: %w", err)
	}
	out := map[string]bool{}
	for _, m := range body.Models {
		out[m.Name] = true
		out[m.Model] = true
	}
	return out, nil
}

// redactedError is err with every occurrence of a secret blanked out of its
// text. Unwrap keeps errors.Is and errors.As working on the original.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactKey returns err with key replaced by "[redacted]" in its text, or err
// itself when the text does not contain it. The SDK's error text carries the
// HTTP response body, which an endpoint that echoes request headers fills with
// the API key.
func redactKey(err error, key string) error {
	if err == nil || key == "" || !strings.Contains(err.Error(), key) {
		return err
	}
	return &redactedError{msg: strings.ReplaceAll(err.Error(), key, "[redacted]"), err: err}
}

// lookup finds the provider and checks the model against its allowlist.
func (r *Registry) lookup(provider, model string) (config.LLMProviderConfig, error) {
	p, ok := r.cfg[provider]
	if !ok {
		names := make([]string, 0, len(r.cfg))
		for n := range r.cfg {
			names = append(names, n)
		}
		sort.Strings(names)
		return p, fmt.Errorf("%w: provider %q (configured: %s)", ErrUnknownModel, provider, strings.Join(names, ", "))
	}
	for _, m := range p.Models {
		if m == model {
			return p, nil
		}
	}
	return p, fmt.Errorf("%w: model %q is not in provider %q's allowlist (%s)", ErrUnknownModel, model, provider, strings.Join(p.Models, ", "))
}

// Open builds the chat model for an allowlisted provider/model, wrapped in the
// provider's resilience policy. The model's configured effort, output cap,
// thinking display and context window become its ModelProfile. A missing key
// is ErrNoCredentials. An Ollama model without tool support, or whose
// max_output_tokens does not fit its effective window, is refused with the
// reason.
func (r *Registry) Open(ctx context.Context, provider, model string) (ChatModel, error) {
	p, err := r.lookup(provider, model)
	if err != nil {
		return nil, err
	}
	if why := p.APIKeyEnvBlocked(provider); why != "" {
		return nil, fmt.Errorf("%w: %s", ErrNoCredentials, why)
	}
	md := p.ModelDefaults[model]
	switch p.Kind {
	case config.LLMKindAnthropic:
		key, err := p.ResolveKey()
		if err != nil {
			return nil, err
		}
		if key == "" {
			return nil, fmt.Errorf("%w: provider %q has no api_key_file, api_key_env value or api_key", ErrNoCredentials, provider)
		}
		window := md.ContextWindow
		if window <= 0 {
			window = p.ContextWindow
		}
		m := NewAnthropicChat(model, AnthropicOptions{
			APIKey: key, BaseURL: p.BaseURL, ContextWindow: window,
			Profile: ModelProfile{Effort: md.Effort, MaxOutput: md.MaxOutputTokens, ThinkingDisplay: md.ThinkingDisplay},
		})
		if err := m.o.Profile.validate(); err != nil {
			return nil, err
		}
		return r.traced(WrapChat(m, policyFor("anthropic")))
	case config.LLMKindOllama:
		c, err := NewOllamaChat(ctx, model, OllamaOptions{
			Host: ollamaHost(p), KeepAlive: p.KeepAlive, NumCtx: p.NumCtx, MaxOutput: md.MaxOutputTokens,
		})
		if err != nil {
			return nil, err
		}
		if why := c.UnusableReason(); why != "" {
			return nil, fmt.Errorf("llm: ollama model %q cannot run a chat: %s", model, why)
		}
		// The trained length is only known from /api/show, so the half of the
		// max_output < min(num_ctx, trained) rule that config cannot check is
		// refused here.
		if w := c.Caps().ContextWindow; md.MaxOutputTokens > 0 && md.MaxOutputTokens >= w {
			return nil, fmt.Errorf("llm: ollama model %q: max_output_tokens %d must be below its effective context window %d (the smaller of num_ctx and the model's trained length)", model, md.MaxOutputTokens, w)
		}
		return r.traced(WrapChat(c, policyFor("ollama")))
	}
	return nil, fmt.Errorf("llm: provider %q has unsupported kind %q", provider, p.Kind)
}

// Price returns the model's price card built from config, or nil when the
// model has none (cost is then unknown, not zero).
func (r *Registry) Price(provider, model string) *PriceCard {
	p, err := r.lookup(provider, model)
	if err != nil {
		return nil
	}
	c := p.ModelDefaults[model].Price
	if c == nil || len(c.Tiers) == 0 {
		return nil
	}
	pc := &PriceCard{
		InputPerMTok: c.Tiers[0].InputPerMTok, OutputPerMTok: c.Tiers[0].OutputPerMTok,
		CacheReadMult: c.CacheReadMult, CacheWrite5mMult: c.CacheWrite5mMult, CacheWrite1hMult: c.CacheWrite1hMult,
		WarnTokens: c.WarnPromptTokens,
	}
	if len(c.Tiers) == 2 {
		pc.TierTokens = c.Tiers[0].UpToPromptTokens
		pc.HighInputPerMTok, pc.HighOutputPerMTok = c.Tiers[1].InputPerMTok, c.Tiers[1].OutputPerMTok
	}
	return pc
}

// Adaptive reports whether a chat on this provider and model runs the adaptive
// tool path (ToolWindow): the provider's adapter must not require a stable
// prefix (caps.StablePrefix false) and the provider's adaptive_tools must not
// be "off" (empty means "auto"). An unknown provider or model is never
// adaptive. caps are the opened model's own Caps, so the capability stays the
// adapter's, not a list of provider kinds kept here.
func (r *Registry) Adaptive(provider, model string, caps Caps) bool {
	p, err := r.lookup(provider, model)
	if err != nil {
		return false
	}
	return p.AdaptiveTools != config.AdaptiveToolsOff && !caps.StablePrefix
}

// ErrUnknownModel is what lookup-style callers can test for; Open wraps it.
var ErrUnknownModel = errors.New("llm: unknown provider or model")
