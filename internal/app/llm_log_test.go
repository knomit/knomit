package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"knomit/internal/llm"
)

// logLines runs logLLMSetup into a buffer and returns each line's level and
// message.
func logLines(t *testing.T, adapter llm.LLMAdapter, err error) [][2]string {
	t.Helper()
	var buf bytes.Buffer
	logLLMSetup(zerolog.New(&buf), adapter, err)
	var out [][2]string
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var e struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		require.NoError(t, json.Unmarshal([]byte(l), &e))
		out = append(out, [2]string{e.Level, e.Message})
	}
	return out
}

// A missing key is ONE info line, and no warning at all.
//
// SABOTAGE S3a (log it at warn) → red. SABOTAGE S3b (keep the second
// "synthesis disabled" line) → two lines → red.
func TestLogLLMSetup_MissingKeyIsOneInfoLine(t *testing.T) {
	err := fmt.Errorf("%w: GEMINI_API_KEY (or GOOGLE_AI_API_KEY) is required for Gemini provider", llm.ErrNoCredentials)
	got := logLines(t, nil, err)
	require.Len(t, got, 1, "one line: %v", got)
	require.Equal(t, "info", got[0][0])
	require.Contains(t, got[0][1], "no LLM API key")
}

// Any other init failure is a configured key or provider that does not work:
// both warnings stay.
func TestLogLLMSetup_OtherFailureStaysWarn(t *testing.T) {
	got := logLines(t, nil, errors.New("creating Gemini client: boom"))
	require.Equal(t, [][2]string{
		{"warn", "LLM adapter init failed"},
		{"warn", "synthesis disabled (no LLM adapter)"},
	}, got)
}

func TestLogLLMSetup_AdapterIsEnabled(t *testing.T) {
	got := logLines(t, llm.NewClaudeCLIAdapter("claude"), nil)
	require.Equal(t, [][2]string{{"info", "synthesis enabled"}}, got)
}
