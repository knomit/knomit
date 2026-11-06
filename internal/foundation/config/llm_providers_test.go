package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLLMProviders_DecodeTwoKinds(t *testing.T) {
	writeConfig(t, `
[llm.providers.anthropic]
kind = "anthropic"
models = ["a1", "a2"]
[llm.providers.local]
kind = "ollama"
models = ["o1"]
`)
	cfg, err := Load()
	require.NoError(t, err)
	require.Len(t, cfg.LLM.Providers, 2)
	a, l := cfg.LLM.Providers["anthropic"], cfg.LLM.Providers["local"]
	require.Equal(t, []string{"a1", "a2"}, a.Models)
	require.Equal(t, "ANTHROPIC_API_KEY", a.APIKeyEnv)
	require.Equal(t, []string{"o1"}, l.Models)
	require.Equal(t, "30m", l.KeepAlive)
	require.Equal(t, 0, l.NumCtx, "num_ctx has no default: unset means derive")
}

func TestLLMProviders_ModelDefaultsAndTieredPrice(t *testing.T) {
	writeConfig(t, `
[llm.providers.anthropic]
kind = "anthropic"
models = ["a1", "a2"]
[llm.providers.anthropic.model.a1]
effort = "low"
max_output_tokens = 32000
thinking_display = "omitted"
context_window = 777000
[llm.providers.anthropic.model.a1.price]
cache_read_mult = 0.11
cache_write_5m_mult = 1.3
cache_write_1h_mult = 2.1
warn_prompt_tokens = 95000
tiers = [
  { up_to_prompt_tokens = 100000, input_per_mtok = 0.1, output_per_mtok = 0.5 },
  { up_to_prompt_tokens = 0, input_per_mtok = 0.7, output_per_mtok = 2.9 },
]
`)
	cfg, err := Load()
	require.NoError(t, err)
	m := cfg.LLM.Providers["anthropic"].ModelDefaults["a1"]
	require.Equal(t, LLMModelConfig{
		Effort: "low", MaxOutputTokens: 32000, ThinkingDisplay: "omitted", ContextWindow: 777000,
		Price: &LLMPriceCard{
			Tiers: []LLMPriceTier{
				{UpToPromptTokens: 100000, InputPerMTok: 0.1, OutputPerMTok: 0.5},
				{UpToPromptTokens: 0, InputPerMTok: 0.7, OutputPerMTok: 2.9},
			},
			CacheReadMult: 0.11, CacheWrite5mMult: 1.3, CacheWrite1hMult: 2.1, WarnPromptTokens: 95000,
		},
	}, m)
	require.NotContains(t, cfg.LLM.Providers["anthropic"].ModelDefaults, "a2", "a model without an entry has none")

	for name, tc := range map[string]struct{ body, want string }{
		"unknown effort":  {"effort = \"extreme\"", "effort"},
		"unknown display": {"thinking_display = \"loud\"", "thinking_display"},
		"unlisted model":  {"", "not in models"},
	} {
		t.Run(name, func(t *testing.T) {
			key := "a1"
			if name == "unlisted model" {
				key = "zz"
			}
			writeConfig(t, "[llm.providers.anthropic]\nkind = \"anthropic\"\nmodels = [\"a1\"]\n[llm.providers.anthropic.model."+key+"]\n"+tc.body+"\n")
			_, err := Load()
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// The shipped example carries the Haiku 5.5 price card and the 1M windows, and
// the default chat model is claude-haiku-5-5 at medium effort.
func TestLLMProviders_ShippedExample(t *testing.T) {
	writeConfig(t, ExampleLLMProvidersTOML)
	cfg, err := Load()
	require.NoError(t, err)
	p := cfg.LLM.Providers["anthropic"]
	require.Equal(t, "claude-haiku-5-5", p.Models[0])
	h := p.ModelDefaults["claude-haiku-5-5"]
	require.Equal(t, "medium", h.Effort)
	require.Equal(t, 1000000, h.ContextWindow)
	require.Equal(t, &LLMPriceCard{
		Tiers: []LLMPriceTier{
			{UpToPromptTokens: 100000, InputPerMTok: 0.10, OutputPerMTok: 0.50},
			{InputPerMTok: 0.50, OutputPerMTok: 2.50},
		},
		CacheReadMult: 0.1, CacheWrite5mMult: 1.25, CacheWrite1hMult: 2, WarnPromptTokens: 95000,
	}, h.Price)
	s := p.ModelDefaults["claude-sonnet-5-5"]
	require.Equal(t, "high", s.Effort, "Sonnet 5.5's documented default effort")
	require.Equal(t, 2.0, s.Price.Tiers[0].InputPerMTok)
	require.Equal(t, 10.0, s.Price.Tiers[0].OutputPerMTok)
	o := p.ModelDefaults["claude-opus-5-5"]
	require.Equal(t, 4.0, o.Price.Tiers[0].InputPerMTok)
	require.Equal(t, 20.0, o.Price.Tiers[0].OutputPerMTok)
	require.Equal(t, 0.05, o.Price.CacheReadMult)
	for _, m := range p.Models {
		require.Equal(t, 1000000, p.ModelDefaults[m].ContextWindow, m)
	}
}

func TestLLMProviders_PlaintextKeyAccepted(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	writeConfig(t, "[llm.providers.anthropic]\nkind = \"anthropic\"\nmodels = [\"a1\"]\napi_key = \"sk-plain\"\n")
	cfg, err := Load()
	require.NoError(t, err)
	k, err := cfg.LLM.Providers["anthropic"].ResolveKey()
	require.NoError(t, err)
	require.Equal(t, "sk-plain", k)
}

func TestLLMProviders_RefuseEmptyAllowlist(t *testing.T) {
	writeConfig(t, "[llm.providers.anthropic]\nkind = \"anthropic\"\n")
	_, err := Load()
	require.ErrorContains(t, err, "models is empty")
}

// A key placed in api_key_env does NOT fail the load; the problem text names
// the provider and never holds the value or any 8-character piece of it.
func TestLLMProviders_APIKeyEnvHoldingAKeyLoadsAndReportsWithoutEchoingIt(t *testing.T) {
	const key = "sk-ant-api03-Zq9xT7mK2vLpR4wB"
	writeConfig(t, "[llm.providers.prov-x]\nkind = \"anthropic\"\nmodels = [\"a1\"]\napi_key_env = \""+key+"\"\n[llm.providers.prov-y]\nkind = \"anthropic\"\nmodels = [\"a2\"]\napi_key_env = \"MY_SECRET_KEY\"\n")
	cfg, err := Load()
	require.NoError(t, err)
	problems := cfg.LLM.APIKeyEnvProblems()
	require.Equal(t, []string{fmt.Sprintf(APIKeyEnvNotANameMessage, "prov-x")}, problems)
	for i := 0; i+8 <= len(key); i++ {
		require.NotContains(t, problems[0], key[i:i+8], "the message leaks part of the value")
	}
}

func TestLLMProviders_APIKeyEnvNameShapes(t *testing.T) {
	for _, ok := range []string{"", "ANTHROPIC_API_KEY", "_x9", "my_Key_2"} {
		require.True(t, validEnvVarName(ok), ok)
	}
	for _, bad := range []string{"sk-ant-x", "9KEY", "A B", "A=B", "Kéy"} {
		require.False(t, validEnvVarName(bad), bad)
	}
}

func TestLLMProviders_APIKeyEnvValidNameAndEmptyLoad(t *testing.T) {
	writeConfig(t, "[llm.providers.prov-x]\nkind = \"anthropic\"\nmodels = [\"a1\"]\napi_key_env = \"MY_SECRET_KEY\"\n[llm.providers.prov-y]\nkind = \"ollama\"\nmodels = [\"o1\"]\napi_key_env = \"\"\n")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "MY_SECRET_KEY", cfg.LLM.Providers["prov-x"].APIKeyEnv)
	require.Equal(t, "", cfg.LLM.Providers["prov-y"].APIKeyEnv)
}

// A models entry is a plain id or an inline { id, name } table, mixed freely.
// The name is display-only: the id is the identity, so the per-model table
// is still keyed by id.
func TestLLMProviders_ModelEntriesMixedWithNames(t *testing.T) {
	writeConfig(t, `
[llm.providers.anthropic]
kind = "anthropic"
models = [
  { id = "hf.co/unsloth/Qwen3.5-4B-GGUF:Q4_K_M", name = "Qwen 3.5 4B" },
  "lfm3:1.7b",
]
[llm.providers.anthropic.model."hf.co/unsloth/Qwen3.5-4B-GGUF:Q4_K_M"]
effort = "low"
`)
	cfg, err := Load()
	require.NoError(t, err)
	p := cfg.LLM.Providers["anthropic"]
	require.Equal(t, []string{"hf.co/unsloth/Qwen3.5-4B-GGUF:Q4_K_M", "lfm3:1.7b"}, p.Models)
	require.Equal(t, "Qwen 3.5 4B", p.ModelName("hf.co/unsloth/Qwen3.5-4B-GGUF:Q4_K_M"))
	require.Equal(t, "lfm3:1.7b", p.ModelName("lfm3:1.7b"), "no name: the id")
	require.Equal(t, "gone:1", p.ModelName("gone:1"), "not listed: the id")
	require.Equal(t, "low", p.ModelDefaults["hf.co/unsloth/Qwen3.5-4B-GGUF:Q4_K_M"].Effort,
		"the name does not change the per-model table lookup")
}

// Each shape or uniqueness rejection names the provider, the index and the rule.
func TestLLMProviders_ModelEntryRejections(t *testing.T) {
	const at = "config: [llm.providers.anthropic]"
	for name, tc := range map[string]struct{ models, want string }{
		"number entry":       {`[1]`, at + ".models[0]: must be a model id or { id, name }"},
		"table without id":   {`[{ name = "X" }]`, at + ".models[0]: id is required"},
		"non-string id":      {`[{ id = 3 }]`, at + ".models[0]: must be a model id or { id, name }"},
		"empty id":           {`[{ id = "", name = "X" }]`, at + ".models[0]: id is required"},
		"empty plain id":     {`["a1", ""]`, at + ".models[1]: id is required"},
		"empty name":         {`[{ id = "a1", name = "  " }]`, at + ".models[0]: name must not be empty"},
		"non-string name":    {`[{ id = "a1", name = 7 }]`, at + ".models[0]: must be a model id or { id, name }"},
		"duplicate id":       {`["a1", { id = "a1", name = "Again" }]`, at + ".models: id \"a1\" is listed more than once"},
		"duplicate name":     {`[{ id = "a1", name = "Same" }, { id = "a2", name = "Same" }]`, at + ".models: name \"Same\" is used by more than one model"},
		"name is another id": {`[{ id = "a1", name = "a2" }, "a2"]`, at + ".models: name \"a2\" is the id of another model"},
		"plain id is a name": {`["a2", { id = "a1", name = "a2" }]`, at + ".models: name \"a2\" is the id of another model"},
	} {
		t.Run(name, func(t *testing.T) {
			writeConfig(t, "[llm.providers.anthropic]\nkind = \"anthropic\"\nmodels = "+tc.models+"\n")
			_, err := Load()
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// A name equal to the model's own id is fine, and two providers may share a name.
func TestLLMProviders_NamesUniqueOnlyWithinProvider(t *testing.T) {
	writeConfig(t, `
[llm.providers.one]
kind = "anthropic"
models = [{ id = "a1", name = "a1" }, { id = "a2", name = "Shared" }]
[llm.providers.two]
kind = "anthropic"
models = [{ id = "b1", name = "Shared" }]
`)
	_, err := Load()
	require.NoError(t, err)
}

func TestLLMProviders_RefuseUnbuiltKind(t *testing.T) {
	writeConfig(t, "[llm.providers.g]\nkind = \"gemini\"\nmodels = [\"g1\"]\n")
	_, err := Load()
	require.ErrorContains(t, err, "no chat adapter yet")
}

func TestLLMProviders_OllamaMaxOutputBelowNumCtx(t *testing.T) {
	body := func(out string) string {
		return "[llm.providers.local]\nkind = \"ollama\"\nmodels = [\"o1\"]\nnum_ctx = 4096\n[llm.providers.local.model.o1]\nmax_output_tokens = " + out + "\n"
	}
	writeConfig(t, body("4096"))
	_, err := Load()
	require.ErrorContains(t, err, "must be below num_ctx 4096")
	writeConfig(t, body("4095"))
	_, err = Load()
	require.NoError(t, err)
}

// An unset num_ctx stays 0 (the llm package derives it), and the
// max_output_tokens < num_ctx rule applies only when num_ctx is set: with it
// unset there is nothing to compare against until the model is probed.
func TestConfig_NumCtxUnsetStaysZero(t *testing.T) {
	writeConfig(t, "[llm.providers.local]\nkind = \"ollama\"\nmodels = [\"o1\"]\n[llm.providers.local.model.o1]\nmax_output_tokens = 100000\n")
	cfg, err := Load()
	require.NoError(t, err, "no num_ctx to compare max_output_tokens with")
	require.Equal(t, 0, cfg.LLM.Providers["local"].NumCtx)

	writeConfig(t, "[llm.providers.local]\nkind = \"ollama\"\nmodels = [\"o1\"]\nnum_ctx = 4096\n[llm.providers.local.model.o1]\nmax_output_tokens = 100000\n")
	_, err = Load()
	require.ErrorContains(t, err, "must be below num_ctx 4096", "the rule still applies once num_ctx is set")
}

func TestLLMProviderConfig_APIKeyFileModeChecked(t *testing.T) {
	f := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(f, []byte("  sk-file\n"), 0o644))
	require.NoError(t, os.Chmod(f, 0o644))
	p := LLMProviderConfig{APIKeyFile: f}
	_, err := p.ResolveKey()
	require.ErrorContains(t, err, "owner only")
	require.NoError(t, os.Chmod(f, 0o600))
	k, err := p.ResolveKey()
	require.NoError(t, err)
	require.Equal(t, "sk-file", k)
}

func TestLLMProviderConfig_KeyPrecedence(t *testing.T) {
	f := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(f, []byte("sk-file\n"), 0o600))
	t.Setenv("LLM_TEST_KEY", "sk-env")
	p := LLMProviderConfig{APIKeyFile: f, APIKeyEnv: "LLM_TEST_KEY", APIKey: "sk-plain"}
	k, err := p.ResolveKey()
	require.NoError(t, err)
	require.Equal(t, "sk-file", k)
	p.APIKeyFile = ""
	k, _ = p.ResolveKey()
	require.Equal(t, "sk-env", k)
	t.Setenv("LLM_TEST_KEY", "")
	k, _ = p.ResolveKey()
	require.Equal(t, "sk-plain", k)
}
