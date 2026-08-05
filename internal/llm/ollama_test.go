package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// ollamaTestServerWithCaps is ollamaTestServer with the model's advertised
// capabilities made explicit. /api/show is answered like /api/tags — outside
// the counter — because it is construction-time probing, not a completion: a
// test asserting "exactly one retry" must not see the probe as an attempt.
func ollamaTestServerWithCaps(t *testing.T, caps []string, chat http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"models":[]}`))
			return
		case "/api/show":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": caps})
			return
		}
		calls.Add(1)
		chat(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OLLAMA_HOST", srv.URL)
	return srv, &calls
}

// captureOllamaRequest runs one completion against a server advertising caps
// and returns the decoded /api/chat request body. The stream it replies with is
// a single terminal chunk: these tests are about what we *send*.
func captureOllamaRequest(t *testing.T, caps []string, opts CompletionOptions) map[string]any {
	t.Helper()
	var body map[string]any
	ollamaTestServerWithCaps(t, caps, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		writeOllamaChunk(t, w, "ok", true)
	})

	adapter, err := NewOllamaAdapter(context.Background(), "test-model")
	require.NoError(t, err)
	_, err = adapter.Complete(context.Background(), "sys", []Message{{Role: "user", Content: "hi"}}, opts, nil)
	require.NoError(t, err)
	require.NotNil(t, body, "the adapter must have POSTed to /api/chat")
	return body
}

// TestOllamaForceJSON_ThinkingModelOmitsFormat is the whole reason this change
// exists. format:"json" is a grammar constraint the runner has to suspend across
// the reasoning phase and resume afterwards, and on a reasoning model it is the
// constraint itself that corrupts the answer: measured against gemma4:26b-mlx,
// format:"json" with thinking on parsed 1/4, and dropping it parsed 4/4. So for
// a model advertising the thinking capability, ForceJSON must be honoured by
// asking for JSON in the prompt rather than by constraining the grammar.
func TestOllamaForceJSON_ThinkingModelOmitsFormat(t *testing.T) {
	body := captureOllamaRequest(t, []string{"completion", "thinking"}, CompletionOptions{ForceJSON: true})

	require.NotContains(t, body, "format",
		"a thinking model must not be sent the JSON grammar constraint — it is what breaks the output")
	require.Equal(t, true, body["think"], "reasoning is the point of running this model; it stays on")
}

// TestOllamaForceJSON_ThinkingModelAsksForJSONInPrompt — dropping the grammar
// constraint must not drop the requirement with it. ForceJSON means the caller
// cannot use anything but JSON; when the constraint is unavailable the demand
// has to move into the prompt, which is the only channel left to carry it.
func TestOllamaForceJSON_ThinkingModelAsksForJSONInPrompt(t *testing.T) {
	body := captureOllamaRequest(t, []string{"completion", "thinking"}, CompletionOptions{ForceJSON: true})

	msgs, ok := body["messages"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, msgs)
	sys, ok := msgs[0].(map[string]any)["content"].(string)
	require.True(t, ok)
	require.Contains(t, strings.ToLower(sys), "json",
		"with no grammar to enforce it, the system prompt is the only place the JSON requirement can live")
	require.Contains(t, sys, "sys", "the caller's own system prompt must survive")
}

// TestOllamaForceJSON_NonThinkingModelSendsFormat is the control. Without a
// reasoning phase there is nothing for the grammar to be suspended across, and
// format:"json" is a hard guarantee worth keeping — it measured 3/3 where the
// prompt-only convention is merely very likely. Dropping it everywhere would
// trade a guarantee for a convention on the models that never had the problem.
func TestOllamaForceJSON_NonThinkingModelSendsFormat(t *testing.T) {
	body := captureOllamaRequest(t, []string{"completion"}, CompletionOptions{ForceJSON: true})

	require.Equal(t, "json", body["format"])
	require.NotContains(t, body, "think",
		"asking a model that cannot think to think is an error from ollama, not a no-op")
}

// TestOllamaComplete_OmitsFormatWhenJSONNotForced guards the field that used to
// be sent unconditionally: `format` had no omitempty, so every ordinary
// completion carried format:"" — accepted by ollama today, but a constraint
// field is not something to send by accident.
func TestOllamaComplete_OmitsFormatWhenJSONNotForced(t *testing.T) {
	body := captureOllamaRequest(t, []string{"completion"}, CompletionOptions{})

	require.NotContains(t, body, "format")
}

// TestOllamaComplete_DoesNotCapGeneration — reasoning tokens are generated
// tokens: ollama counts them against num_predict before the caller sees them,
// so a cap shared with the metered providers (defaultMaxTokens, sized to bound
// spend) is spent on reasoning and the answer never gets written. Measured on
// gemma4:26b-mlx, num_predict=2000 returned done_reason "length" with empty
// content, while the same prompt uncapped answered correctly in 7565 tokens.
// Locally there is no spend to bound, so there is nothing for the cap to buy.
func TestOllamaComplete_DoesNotCapGeneration(t *testing.T) {
	body := captureOllamaRequest(t, []string{"completion", "thinking"}, CompletionOptions{})

	opts, ok := body["options"].(map[string]any)
	require.True(t, ok, "options must still be sent")
	require.Equal(t, float64(-1), opts["num_predict"],
		"generation must run to a natural stop; the context window and the attempt timeout are the real bounds")
}

// TestOllamaComplete_TruncationIsAnError — the silent failure this change
// closes. A truncated stream is a successful HTTP response: ollama sets
// done_reason "length", the scanner sees done and stops, and Complete used to
// return whatever it had accumulated with a nil error. With a reasoning model
// that accumulation is routinely the empty string, because the budget went to
// thinking — so the caller (internal/synthesize, which checks only err) parses
// an empty response as though the model had answered.
func TestOllamaComplete_TruncationIsAnError(t *testing.T) {
	ollamaTestServerWithCaps(t, []string{"completion", "thinking"}, func(w http.ResponseWriter, r *http.Request) {
		line, err := json.Marshal(ollamaStreamLine{
			Message:    ollamaMessage{Role: "assistant", Content: ""},
			Done:       true,
			DoneReason: "length",
		})
		require.NoError(t, err)
		_, _ = w.Write(append(line, '\n'))
	})

	adapter, err := NewOllamaAdapter(context.Background(), "test-model")
	require.NoError(t, err)

	_, err = adapter.Complete(context.Background(), "sys", []Message{{Role: "user", Content: "hi"}}, CompletionOptions{}, nil)
	require.Error(t, err, "truncation must not reach the caller as a successful empty completion")
	require.Contains(t, err.Error(), "truncated")
}

// TestOllamaComplete_SkipsThinkingTokens locks the demultiplexing half of the
// contract. Ollama routes reasoning into message.thinking and the answer into
// message.content; only content is the completion. This is what makes discarding
// the reasoning chain safe, and it must hold for the streamed callback too — a
// caller rendering deltas would otherwise splice the model's private reasoning
// into the answer it displays.
func TestOllamaComplete_SkipsThinkingTokens(t *testing.T) {
	ollamaTestServerWithCaps(t, []string{"completion", "thinking"}, func(w http.ResponseWriter, r *http.Request) {
		for _, l := range []ollamaStreamLine{
			{Message: ollamaMessage{Role: "assistant", Thinking: "let me "}},
			{Message: ollamaMessage{Role: "assistant", Thinking: "consider"}},
			{Message: ollamaMessage{Role: "assistant", Content: "the "}},
			{Message: ollamaMessage{Role: "assistant", Content: "answer"}, Done: true, DoneReason: "stop"},
		} {
			line, err := json.Marshal(l)
			require.NoError(t, err)
			_, _ = w.Write(append(line, '\n'))
			w.(http.Flusher).Flush()
		}
	})

	adapter, err := NewOllamaAdapter(context.Background(), "test-model")
	require.NoError(t, err)

	var chunks []string
	out, err := adapter.Complete(context.Background(), "sys", []Message{{Role: "user", Content: "hi"}},
		CompletionOptions{}, func(s string) { chunks = append(chunks, s) })
	require.NoError(t, err)
	require.Equal(t, "the answer", out, "the completion is the content channel, never the reasoning channel")
	require.Equal(t, []string{"the ", "answer"}, chunks, "no reasoning delta may reach the caller's callback")
}
