package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfig_AdaptiveToolsValues(t *testing.T) {
	body := func(kind, v string) string {
		s := "[llm.providers.p]\nkind = \"" + kind + "\"\nmodels = [\"m1\"]\n"
		if v != "" {
			s += "adaptive_tools = \"" + v + "\"\n"
		}
		return s
	}
	for _, v := range []string{"", "auto", "off"} {
		writeConfig(t, body("ollama", v))
		cfg, err := Load()
		require.NoError(t, err, "ollama adaptive_tools=%q", v)
		require.Equal(t, v, cfg.LLM.Providers["p"].AdaptiveTools)
	}
	for _, v := range []string{"on", "yes", "ON", "true"} {
		writeConfig(t, body("ollama", v))
		_, err := Load()
		require.ErrorContains(t, err, "adaptive_tools", "ollama %q", v)
		require.ErrorContains(t, err, `"auto" or "off"`, "the error names the accepted values")
	}
	// An anthropic provider asking for "on" is told why, in the approved words.
	writeConfig(t, body("anthropic", "on"))
	_, err := Load()
	require.ErrorContains(t, err, `adaptive_tools = "on" is not allowed for provider p: its requests must keep a stable prefix.`)
	writeConfig(t, body("anthropic", "auto"))
	_, err = Load()
	require.NoError(t, err)
}
