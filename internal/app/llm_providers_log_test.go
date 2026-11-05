package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"knomit/internal/foundation/config"
)

// The warning still fires when another key source keeps the provider available:
// the config is still wrong.
func TestLogLLMProviderProblems_StillWarnsWhenAnotherKeySourceIsSet(t *testing.T) {
	const key = "sk-ant-api03-Zq9xT7mK2vLpR4wB"
	var buf bytes.Buffer
	logLLMProviderProblems(zerolog.New(&buf), config.LLMConfig{Providers: map[string]config.LLMProviderConfig{
		"prov-k": {Kind: "anthropic", APIKeyEnv: key, APIKey: "sk-test-plain"},
	}})
	out := buf.String()
	require.Equal(t, 1, strings.Count(out, fmt.Sprintf(config.APIKeyEnvNotANameMessage, "prov-k")), out)
	for i := 0; i+8 <= len(key); i++ {
		require.NotContains(t, out, key[i:i+8])
	}
}

// A key pasted into api_key_env is ONE warn line with the message, and neither
// the line nor any 8-character piece of the value appears in it. The valid
// provider logs nothing.
//
// SABOTAGE: make the loop body a no-op, or log the value → red.
func TestLogLLMProviderProblems_WarnsOnceWithoutTheValue(t *testing.T) {
	const key = "sk-ant-api03-Zq9xT7mK2vLpR4wB"
	var buf bytes.Buffer
	logLLMProviderProblems(zerolog.New(&buf), config.LLMConfig{Providers: map[string]config.LLMProviderConfig{
		"prov-x": {Kind: "anthropic", APIKeyEnv: key},
		"prov-y": {Kind: "anthropic", APIKeyEnv: "MY_SECRET_KEY"},
	}})
	out := buf.String()
	require.Equal(t, 1, strings.Count(out, fmt.Sprintf(config.APIKeyEnvNotANameMessage, "prov-x")), out)
	require.Equal(t, 1, strings.Count(out, "\n"), out)
	var line struct {
		Level   string `json:"level"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &line))
	require.Equal(t, fmt.Sprintf(config.APIKeyEnvNotANameMessage, "prov-x"), line.Message, "the line is exactly the message")
	require.Contains(t, out, `"level":"warn"`)
	require.NotContains(t, out, "prov-y")
	for i := 0; i+8 <= len(key); i++ {
		require.NotContains(t, out, key[i:i+8])
	}
}
