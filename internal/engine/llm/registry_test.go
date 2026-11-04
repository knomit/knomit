package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/foundation/config"
)

// fakeAnthropicModels serves GET /v1/models listing ids, and counts requests.
func fakeAnthropicModels(t *testing.T, ids ...string) (*httptest.Server, *int) {
	t.Helper()
	n := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*n++
		w.Header().Set("Content-Type", "application/json")
		data := []map[string]any{}
		for _, id := range ids {
			data = append(data, map[string]any{"id": id, "type": "model", "display_name": id, "created_at": "2026-01-01T00:00:00Z"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": false})
	}))
	t.Cleanup(srv.Close)
	return srv, n
}

// fakeOllama serves /api/tags for pulled and /api/show with caps per model;
// trained is the context_length reported for every model.
func fakeOllama(t *testing.T, caps map[string][]string, trained int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			ms := []map[string]string{}
			for m := range caps {
				ms = append(ms, map[string]string{"name": m, "model": m})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": ms})
		case "/api/show":
			var b struct{ Model string }
			_ = json.NewDecoder(r.Body).Decode(&b)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"capabilities": caps[b.Model],
				"model_info":   map[string]any{"general.architecture": "x", "x.context_length": trained},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRegistry_ListMarksUnavailableAndToolless(t *testing.T) {
	an, _ := fakeAnthropicModels(t, "a1")
	ol := fakeOllama(t, map[string][]string{"o1": {"completion"}}, 8192)
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"local":     {Kind: "ollama", Host: ol.URL, Models: []string{"o1"}},
		"anthropic": {Kind: "anthropic", BaseURL: an.URL, APIKey: "sk-test-1", Models: []string{"a1", "a2"}},
	})
	got := r.List(context.Background())
	require.Equal(t, []ProviderStatus{
		{Name: "anthropic", Kind: "anthropic", Models: []ModelStatus{
			{ID: "a1", Available: true, Tools: true},
			{ID: "a2", Available: false, Reason: "not offered by the provider"},
		}},
		{Name: "local", Kind: "ollama", Models: []ModelStatus{
			{ID: "o1", Available: true, Tools: false, Reason: "model does not support tool calling (capabilities: completion)"},
		}},
	}, got)
}

func TestRegistry_ListIsCachedFor60Seconds(t *testing.T) {
	an, hits := fakeAnthropicModels(t, "a1")
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"anthropic": {Kind: "anthropic", BaseURL: an.URL, APIKey: "sk-test-1", Models: []string{"a1"}},
	})
	now := time.Unix(1000, 0)
	r.now = func() time.Time { return now }
	r.List(context.Background())
	now = now.Add(59 * time.Second)
	r.List(context.Background())
	require.Equal(t, 1, *hits, "second call inside the TTL is served from the cache")
	now = now.Add(2 * time.Second)
	r.List(context.Background())
	require.Equal(t, 2, *hits, "past 60s it asks again")
}

func TestRegistry_OpenRefusesOutsideAllowlist(t *testing.T) {
	ol := fakeOllama(t, map[string][]string{"o1": {"completion"}, "o2": {"tools"}}, 8192)
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"anthropic": {Kind: "anthropic", APIKey: "sk-test-2", Models: []string{"a1", "a2"}},
		"local":     {Kind: "ollama", Host: ol.URL, Models: []string{"o1", "o2"}, NumCtx: 4096},
	})
	_, err := r.Open(context.Background(), "anthropic", "zzz")
	require.ErrorIs(t, err, ErrUnknownModel)
	require.ErrorContains(t, err, "allowlist (a1, a2)")

	_, err = r.Open(context.Background(), "local", "o1")
	require.ErrorContains(t, err, "does not support tool calling")

	m, err := r.Open(context.Background(), "local", "o2")
	require.NoError(t, err)
	require.Equal(t, "o2", m.Model())
	require.Equal(t, 4096, m.Caps().ContextWindow)
}

func TestRegistry_OpenAnthropicUsesConfiguredProfileAndWindow(t *testing.T) {
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"anthropic": {Kind: "anthropic", APIKey: "sk-test-3", ContextWindow: 123000, Models: []string{"a1", "a2"},
			ModelDefaults: map[string]config.LLMModelConfig{
				"a1": {Effort: "low", MaxOutputTokens: 31000, ContextWindow: 777000},
			}},
	})
	m, err := r.Open(context.Background(), "anthropic", "a1")
	require.NoError(t, err)
	require.Equal(t, Caps{Tools: true, MaxBreakpoints: anthropicMaxBreakpoints, ContextWindow: 777000, MaxOutput: 31000}, m.Caps())
	m2, err := r.Open(context.Background(), "anthropic", "a2")
	require.NoError(t, err)
	require.Equal(t, 123000, m2.Caps().ContextWindow, "a model without its own window takes the provider's")
	require.Equal(t, DefaultModelProfile.MaxOutput, m2.Caps().MaxOutput)
}

func TestRegistry_OpenRefusesOllamaMaxOutputAboveTrainedLength(t *testing.T) {
	ol := fakeOllama(t, map[string][]string{"o1": {"tools"}}, 2048)
	mk := func(out int) *Registry {
		return NewRegistry(map[string]config.LLMProviderConfig{
			"local": {Kind: "ollama", Host: ol.URL, Models: []string{"o1"}, NumCtx: 8192,
				ModelDefaults: map[string]config.LLMModelConfig{"o1": {MaxOutputTokens: out}}},
		})
	}
	_, err := mk(3000).Open(context.Background(), "local", "o1")
	require.ErrorContains(t, err, "effective context window 2048")
	_, err = mk(2048).Open(context.Background(), "local", "o1")
	require.ErrorContains(t, err, "must be below")
	_, err = mk(2047).Open(context.Background(), "local", "o1")
	require.NoError(t, err)
}

func TestRegistry_MissingKeyIsErrNoCredentials(t *testing.T) {
	t.Setenv("LLM_REG_TEST_KEY", "")
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"anthropic": {Kind: "anthropic", APIKeyEnv: "LLM_REG_TEST_KEY", Models: []string{"a1"}},
	})
	_, err := r.Open(context.Background(), "anthropic", "a1")
	require.ErrorIs(t, err, ErrNoCredentials)
}

func TestRegistry_PriceCardFromConfig(t *testing.T) {
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"anthropic": {Kind: "anthropic", Models: []string{"a1", "a2"}, ModelDefaults: map[string]config.LLMModelConfig{
			"a1": {Price: &config.LLMPriceCard{
				Tiers: []config.LLMPriceTier{
					{UpToPromptTokens: 51000, InputPerMTok: 0.3, OutputPerMTok: 0.7},
					{InputPerMTok: 0.9, OutputPerMTok: 2.3},
				},
				CacheReadMult: 0.12, CacheWrite5mMult: 1.3, CacheWrite1hMult: 2.2, WarnPromptTokens: 47000,
			}},
		}},
	})
	require.Equal(t, &PriceCard{
		InputPerMTok: 0.3, OutputPerMTok: 0.7, TierTokens: 51000, HighInputPerMTok: 0.9, HighOutputPerMTok: 2.3,
		CacheReadMult: 0.12, CacheWrite5mMult: 1.3, CacheWrite1hMult: 2.2, WarnTokens: 47000,
	}, r.Price("anthropic", "a1"))
	require.Nil(t, r.Price("anthropic", "a2"), "no card means unknown cost")
}

// An endpoint that answers 401 and echoes the request headers puts the API key
// into the SDK's error text. Neither the provider list's reason nor a stream
// error shows it, and the stream error is still the typed original.
//
// SABOTAGE: redactKey returns err unchanged -> both halves fail.
func TestRegistry_EchoedAPIKeyIsRedacted(t *testing.T) {
	const key = "sk-echo-5a4b3c2d1e0f9a8b"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key ` + r.Header.Get("X-Api-Key") + `"}}`))
	}))
	t.Cleanup(srv.Close)
	reg := NewRegistry(map[string]config.LLMProviderConfig{
		"p": {Kind: "anthropic", BaseURL: srv.URL, APIKey: key, Models: []string{"m"}},
	})

	list := reg.List(context.Background())
	require.Len(t, list, 1)
	require.Len(t, list[0].Models, 1)
	require.Contains(t, list[0].Models[0].Reason, "bad key [redacted]")
	require.NotContains(t, list[0].Models[0].Reason, key)

	chat := NewAnthropicChat("m", AnthropicOptions{APIKey: key, BaseURL: srv.URL})
	_, err := chat.Stream(context.Background(), prefixSnapshot(t, false), StreamOptions{}, func(Event) {})
	require.Error(t, err)
	require.NotContains(t, err.Error(), key)
	require.Contains(t, err.Error(), "bad key [redacted]")
	var re *redactedError
	require.ErrorAs(t, err, &re, "the original error is still reachable through Unwrap")
}
