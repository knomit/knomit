package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
			ms := []map[string]any{}
			for m := range caps {
				ms = append(ms, map[string]any{"name": m, "model": m, "details": map[string]string{"parameter_size": "7B", "quantization_level": "Q4_K_M"}})
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

// The display name comes from the config at read time: a named model shows its
// name, an unnamed one its id, and a provider or model no longer configured its id.
func TestRegistry_ModelNameFromConfig(t *testing.T) {
	reg := NewRegistry(map[string]config.LLMProviderConfig{
		"prov": {Kind: "ollama", Host: "http://127.0.0.1:1", Models: []string{"o1", "o2"},
			Entries: []config.LLMModelEntry{{ID: "o1", Name: "Ollama One"}, {ID: "o2"}}},
	})
	require.Equal(t, "Ollama One", reg.ModelName("prov", "o1"))
	require.Equal(t, "o2", reg.ModelName("prov", "o2"))
	require.Equal(t, "gone", reg.ModelName("prov", "gone"))
	require.Equal(t, "o1", reg.ModelName("nope", "o1"))
	st := reg.List(context.Background())
	require.Equal(t, "Ollama One", st[0].Models[0].Name)
	require.Equal(t, "o1", st[0].Models[0].ID, "the id stays the identity")
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
		{Name: "anthropic", Kind: "anthropic", Reachable: true, Models: []ModelStatus{
			{ID: "a1", Name: "a1", Available: true, Tools: true},
			{ID: "a2", Name: "a2", Available: false, Reason: "not offered by the provider"},
		}},
		{Name: "local", Kind: "ollama", Host: ol.URL, Reachable: true, Models: []ModelStatus{
			{ID: "o1", Name: "o1", Available: true, Tools: false, Reason: "model does not support tool calling (capabilities: completion)", ParameterSize: "7B", Quantization: "Q4_K_M"},
		}},
	}, got)
}

// A key placed in api_key_env marks every model of THAT provider unavailable
// with the message as the reason (what GET /chat-providers shows); the other
// provider stays available, and the reason never holds the value.
//
// SABOTAGE: drop the APIKeyEnvBlocked check in List → red.
func TestRegistry_APIKeyEnvHoldingAKeyMakesOnlyThatProviderUnavailable(t *testing.T) {
	const key = "sk-ant-api03-Zq9xT7mK2vLpR4wB"
	an, _ := fakeAnthropicModels(t, "a1", "a2", "a3")
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"prov-x": {Kind: "anthropic", BaseURL: an.URL, APIKeyEnv: key, Models: []string{"a1", "a2"}},
		"prov-y": {Kind: "anthropic", BaseURL: an.URL, APIKey: "sk-test-1", Models: []string{"a3"}},
	})
	got := r.List(context.Background())
	want := fmt.Sprintf(config.APIKeyEnvNotANameMessage, "prov-x")
	require.Equal(t, []ProviderStatus{
		{Name: "prov-x", Kind: "anthropic", Models: []ModelStatus{{ID: "a1", Name: "a1", Reason: want}, {ID: "a2", Name: "a2", Reason: want}}},
		{Name: "prov-y", Kind: "anthropic", Reachable: true, Models: []ModelStatus{{ID: "a3", Name: "a3", Available: true, Tools: true}}},
	}, got)
	for i := 0; i+8 <= len(key); i++ {
		require.NotContains(t, want, key[i:i+8])
	}
	_, err := r.Open(context.Background(), "prov-x", "a1")
	require.ErrorIs(t, err, ErrNoCredentials)
	require.NotContains(t, err.Error(), key[:8])
}

// The bad api_key_env blocks the provider only when it is the source that would
// have been used. An api_key_file or a plaintext api_key (both rank ahead of or
// stand in for it) keeps the provider available, and nothing echoes the value.
//
// SABOTAGE: make APIKeyEnvBlocked ignore ResolveKey → the available cases go red.
func TestRegistry_APIKeyEnvBadButAnotherKeySourceSetStaysAvailable(t *testing.T) {
	const key = "sk-ant-api03-Zq9xT7mK2vLpR4wB"
	an, _ := fakeAnthropicModels(t, "a1", "a2")
	keyFile := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(keyFile, []byte("sk-test-file\n"), 0o600))
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"prov-f": {Kind: "anthropic", BaseURL: an.URL, APIKeyEnv: key, APIKeyFile: keyFile, Models: []string{"a1"}},
		"prov-k": {Kind: "anthropic", BaseURL: an.URL, APIKeyEnv: key, APIKey: "sk-test-plain", Models: []string{"a2"}},
	})
	got := r.List(context.Background())
	require.Equal(t, []ProviderStatus{
		{Name: "prov-f", Kind: "anthropic", Reachable: true, Models: []ModelStatus{{ID: "a1", Name: "a1", Available: true, Tools: true}}},
		{Name: "prov-k", Kind: "anthropic", Reachable: true, Models: []ModelStatus{{ID: "a2", Name: "a2", Available: true, Tools: true}}},
	}, got)
	for _, p := range []string{"prov-f", "prov-k"} {
		m, err := r.Open(context.Background(), p, map[string]string{"prov-f": "a1", "prov-k": "a2"}[p])
		require.NoError(t, err)
		require.NotNil(t, m)
	}
	out, err := json.Marshal(got)
	require.NoError(t, err)
	for i := 0; i+8 <= len(key); i++ {
		require.NotContains(t, string(out), key[i:i+8])
	}
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
	require.Equal(t, Caps{Tools: true, MaxBreakpoints: anthropicMaxBreakpoints, ContextWindow: 777000, MaxOutput: 31000, StablePrefix: true}, m.Caps())
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

// The start screen's details come from the config and the provider: best_for,
// effort, the price card and the context window from config; num_ctx, size and
// quantization from Ollama; the key source is the env var's NAME.
func TestRegistry_ListCarriesStartScreenDetails(t *testing.T) {
	an, _ := fakeAnthropicModels(t, "a1")
	ol := fakeOllama(t, map[string][]string{"o1": {"completion", "tools"}}, 8192)
	t.Setenv("KNOMIT_TEST_ANTHROPIC_KEY", "sk-ant-should-never-show")
	card := &config.LLMPriceCard{
		Tiers:         []config.LLMPriceTier{{UpToPromptTokens: 100_000, InputPerMTok: 0.7, OutputPerMTok: 3.5}, {InputPerMTok: 1.4, OutputPerMTok: 7}},
		CacheReadMult: 0.1, CacheWrite5mMult: 1.25, WarnPromptTokens: 95_000,
	}
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"cloud": {Kind: "anthropic", BaseURL: an.URL, APIKeyEnv: "KNOMIT_TEST_ANTHROPIC_KEY", Models: []string{"a1"},
			ModelDefaults: map[string]config.LLMModelConfig{"a1": {BestFor: "Everything", Effort: "high", ContextWindow: 500_000, Price: card}}},
		"local": {Kind: "ollama", Host: ol.URL, NumCtx: 4096, KeepAlive: "5m", Models: []string{"o1"}},
	})
	got := r.List(context.Background())
	require.Len(t, got, 2)
	cloud, local := got[0], got[1]
	require.Equal(t, "KNOMIT_TEST_ANTHROPIC_KEY", cloud.KeySource, "the NAME of the variable")
	require.True(t, cloud.Reachable)
	require.Equal(t, "", cloud.Host)
	a1 := cloud.Models[0]
	require.Equal(t, "Everything", a1.BestFor)
	require.Equal(t, "high", a1.Effort)
	require.Equal(t, 500_000, a1.ContextWindow)
	require.Equal(t, card, a1.Price)
	require.Equal(t, ol.URL, local.Host)
	require.True(t, local.Reachable)
	o1 := local.Models[0]
	require.Equal(t, 4096, o1.NumCtx)
	require.Equal(t, 4096, o1.ContextWindow, "the trained length (8192) is above num_ctx")
	require.Equal(t, "5m", o1.KeepAlive)
	require.Equal(t, "7B", o1.ParameterSize)
	require.Equal(t, "Q4_K_M", o1.Quantization)
	require.Nil(t, o1.Price, "an Ollama model without a price card has none")
	require.NotContains(t, fmt.Sprintf("%+v", got), "sk-ant-should-never-show")
}

// key_source is shown only when the env var is the key source in effect and is
// a variable name: a key pasted into api_key_env, a key file and a plaintext
// key all give none, and the pasted value is never repeated.
//
// SABOTAGE: keySource returns p.APIKeyEnv unconditionally -> the pasted-key
// case shows the key.
func TestRegistry_KeySourceIsNeverTheKey(t *testing.T) {
	const pasted = "sk-ant-api03-Zq9xT7mK2vLpR4wB"
	t.Setenv("SOME_NAME", "a-key-value")
	an, _ := fakeAnthropicModels(t, "a1")
	keyFile := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(keyFile, []byte("sk-test-file\n"), 0o600))
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"a-env":    {Kind: "anthropic", BaseURL: an.URL, APIKeyEnv: "SOME_NAME", Models: []string{"a1"}},
		"b-pasted": {Kind: "anthropic", BaseURL: an.URL, APIKeyEnv: pasted, Models: []string{"a1"}},
		"c-file":   {Kind: "anthropic", BaseURL: an.URL, APIKeyEnv: "SOME_NAME", APIKeyFile: keyFile, Models: []string{"a1"}},
		"d-plain":  {Kind: "anthropic", BaseURL: an.URL, APIKeyEnv: "SOME_NAME", APIKey: "sk-test-plain", Models: []string{"a1"}},
	})
	got := r.List(context.Background())
	var sources []string
	for _, p := range got {
		sources = append(sources, p.KeySource)
	}
	require.Equal(t, []string{"SOME_NAME", "", "", ""}, sources)
	require.NotContains(t, sources, "a-key-value")
	all, err := json.Marshal(got)
	require.NoError(t, err)
	for _, secret := range []string{pasted, "sk-test-file", "sk-test-plain", keyFile} {
		require.NotContains(t, string(all), secret)
	}
}

// A key pasted into api_key_env that happens to be made only of letters, digits
// and underscores looks like a variable name; it is reported only if such a
// variable is set, and an unset name is not the key in effect.
//
// SABOTAGE: keySource drops the os.Getenv check -> the pasted key and the unset
// name are reported.
func TestRegistry_KeySourceNeedsTheVariableToBeSet(t *testing.T) {
	const pasted = "skantapi03abcDEF123"
	an, _ := fakeAnthropicModels(t, "a1")
	t.Setenv("KNOMIT_TEST_SET_VAR", "value")
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"a-set":    {Kind: "anthropic", BaseURL: an.URL, APIKeyEnv: "KNOMIT_TEST_SET_VAR", Models: []string{"a1"}},
		"b-unset":  {Kind: "anthropic", BaseURL: an.URL, APIKeyEnv: "KNOMIT_TEST_NOT_SET_VAR", Models: []string{"a1"}},
		"c-pasted": {Kind: "anthropic", BaseURL: an.URL, APIKeyEnv: pasted, Models: []string{"a1"}},
	})
	var sources []string
	for _, p := range r.List(context.Background()) {
		sources = append(sources, p.KeySource)
	}
	require.Equal(t, []string{"KNOMIT_TEST_SET_VAR", "", ""}, sources)
}

// An Ollama host is shown as scheme, host and port: no credentials, no query
// and no fragment.
//
// SABOTAGE: displayHost keeps RawQuery and Fragment -> the token cases fail.
func TestRegistry_HostDropsCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"http://user:secret@box:11434/":        "http://box:11434",
		"box:11434":                            "http://box:11434",
		"http://box:11434?token=SECRET":        "http://box:11434",
		"http://box:11434/#x":                  "http://box:11434",
		"http://box:11434#SECRET":              "http://box:11434",
		"http://box:11434/v1?api_key=SECRET":   "http://box:11434/v1",
		"http://u:p@box:11434/v1?a=b&c=d#frag": "http://box:11434/v1",
		"http://box:11434?":                    "http://box:11434",
	} {
		got := displayHost(in)
		require.Equal(t, want, got, in)
		require.NotContains(t, got, "SECRET")
	}
}

// A provider that cannot be reached is not reachable, and says why per model.
func TestRegistry_UnreachableProvider(t *testing.T) {
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"gone":  {Kind: "ollama", Host: "http://127.0.0.1:1", Models: []string{"o1"}},
		"nokey": {Kind: "anthropic", APIKeyEnv: "KNOMIT_TEST_UNSET_KEY_VAR", Models: []string{"a1"}},
	})
	got := r.List(context.Background())
	require.False(t, got[0].Reachable)
	require.Contains(t, got[0].Models[0].Reason, "ollama unreachable")
	require.False(t, got[1].Reachable)
	require.Equal(t, "", got[1].KeySource, "an unset variable is not the key in effect")
	require.Equal(t, "no API key configured", got[1].Models[0].Reason)
}
