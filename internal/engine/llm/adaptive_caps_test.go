package llm

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/foundation/config"
)

func TestAnthropicCaps_StablePrefixTrue(t *testing.T) {
	require.True(t, NewAnthropicChat("m", AnthropicOptions{}).Caps().StablePrefix)
}

func TestOllamaCaps_StablePrefixFalse(t *testing.T) {
	srv := newOllamaServer(t, []string{"tools"})
	require.False(t, newTestOllama(t, srv, OllamaOptions{}).Caps().StablePrefix)
}

func TestWrapChat_KeepsStablePrefix(t *testing.T) {
	for _, stable := range []bool{true, false} {
		f := &fakeChat{caps: Caps{Tools: true, ContextWindow: 10, StablePrefix: stable}}
		w := WrapChat(f, policyFor("anthropic"))
		require.Equal(t, stable, w.Caps().StablePrefix)
		tr, c, err := NewTracingChat(w, filepath.Join(t.TempDir(), "t.jsonl"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		require.Equal(t, stable, tr.Caps().StablePrefix)
	}
}

// adaptive_tools = auto follows the adapter's capability: adaptive exactly when
// the provider's requests need no stable prefix. off forces the static path.
// Each provider has its own fixture, so a mix-up between them fails.
func TestRegistry_AdaptiveResolution(t *testing.T) {
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"local-auto": {Kind: "ollama", Models: []string{"o1"}},
		"local-off":  {Kind: "ollama", Models: []string{"o1"}, AdaptiveTools: "off"},
		"local-set":  {Kind: "ollama", Models: []string{"o1"}, AdaptiveTools: "auto"},
		"cloud":      {Kind: "anthropic", Models: []string{"a1"}, AdaptiveTools: "auto"},
		"cloud-def":  {Kind: "anthropic", Models: []string{"a1"}},
	})
	noPrefix := Caps{Tools: true, ContextWindow: 1000}
	stable := Caps{Tools: true, ContextWindow: 1000, StablePrefix: true}
	require.True(t, r.Adaptive("local-auto", "o1", noPrefix))
	require.True(t, r.Adaptive("local-set", "o1", noPrefix))
	require.False(t, r.Adaptive("local-off", "o1", noPrefix))
	require.False(t, r.Adaptive("cloud", "a1", stable))
	require.False(t, r.Adaptive("cloud-def", "a1", stable))
	require.False(t, r.Adaptive("nope", "o1", noPrefix), "an unknown provider is never adaptive")
	require.False(t, r.Adaptive("local-auto", "other", noPrefix), "a model outside the allowlist is never adaptive")
}
