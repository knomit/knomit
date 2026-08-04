package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// OllamaAdapter calls a local or remote Ollama instance via its /api/chat
// REST endpoint. Responses are streamed as newline-delimited JSON (NDJSON),
// with each line containing a message fragment and a done flag.
//
// The server address is read from OLLAMA_HOST (default http://localhost:11434).
// Unlike the API-based adapters, Ollama supports any model the server has
// pulled — including quantized open-weight models.
type OllamaAdapter struct {
	host   string
	model  string
	client *http.Client
	// thinks records whether the server reports this model as capable of
	// reasoning. It decides how ForceJSON is honoured, so it is resolved once at
	// construction rather than per request: the answer is a property of the
	// model, and asking on every completion would put an extra round trip in
	// front of work that is already the slowest thing in the pipeline.
	thinks bool
}

// ollamaUncapped disables the generation limit. Reasoning tokens are generated
// tokens — ollama counts them against num_predict before any of them reach us —
// so a cap sized to bound spend on a metered API (defaultMaxTokens) is instead
// spent on reasoning here, and the answer never gets written. A local model
// costs nothing per token, so the cap buys nothing and truncates real work; the
// context window and the caller's attempt timeout are the meaningful bounds.
const ollamaUncapped = -1

// jsonOnlyInstruction carries the ForceJSON requirement in the prompt, for the
// case where the grammar constraint cannot. It asks for bare JSON, but the
// callers that need it also tolerate fences (internal/synthesize's extractJSON),
// so a model that answers with one anyway still parses.
const jsonOnlyInstruction = "Respond with a single valid JSON object and nothing else: no prose before or after it, and no markdown code fences."

// NewOllamaAdapter creates an adapter after verifying the Ollama server is
// reachable (5-second health check against /api/tags). Returns an error if
// the server is down or unreachable.
//
// It then probes /api/show for the model's capabilities, which decides how
// ForceJSON is honoured (see jsonStrategy). Only the health check is fatal: an
// unreachable server has nothing to offer, whereas an unanswered capability
// probe just picks the more conservative of two working strategies.
func NewOllamaAdapter(ctx context.Context, model string) (*OllamaAdapter, error) {
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://localhost:11434"
	}

	client := &http.Client{}

	// Health check with a short timeout.
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, host+"/api/tags", nil)
	if err != nil {
		return nil, fmt.Errorf("ollama: build health check request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: server unreachable at %s: %w", host, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama: health check returned %d", resp.StatusCode)
	}

	a := &OllamaAdapter{host: host, model: model, client: client}
	a.thinks = a.reportsThinking(ctx)
	return a, nil
}

// reportsThinking asks /api/show whether the model advertises the "thinking"
// capability.
//
// A failure here is deliberately not fatal. The probe only selects between two
// working ways of asking for JSON, so a server that cannot answer it should
// still yield a usable adapter — and the false it returns lands on the
// grammar-constrained path, which is what this adapter did before the probe
// existed. Refusing to construct would turn a cosmetic unknown into an outage.
func (a *OllamaAdapter) reportsThinking(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	body, err := json.Marshal(map[string]string{"model": a.model})
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, a.host+"/api/show", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		log.Debug().Err(err).Str("model", a.model).Msg("ollama: capability probe failed; assuming no thinking")
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Debug().Int("status", resp.StatusCode).Str("model", a.model).Msg("ollama: capability probe rejected; assuming no thinking")
		return false
	}

	var show struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&show); err != nil {
		return false
	}
	return slices.Contains(show.Capabilities, "thinking")
}

// ollamaChatRequest is the /api/chat body. Format and Think are both omitempty
// and for the same reason: each is a mode switch, and sending one inertly — an
// empty format, or think on a model that cannot — is either meaningless or an
// error, never a no-op worth risking.
type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Format   string          `json:"format,omitempty"`
	Think    *bool           `json:"think,omitempty"`
	Stream   bool            `json:"stream"`
	Options  ollamaOptions   `json:"options"`
}

// ollamaMessage carries both channels of a chat turn. Thinking is populated
// only on responses, and only by models that reason: ollama demultiplexes the
// reasoning tokens out of the raw stream into their own field, leaving Content
// as the answer alone. It is omitempty because the same struct is used for the
// request, where a thinking field has no meaning.
type ollamaMessage struct {
	Role     string `json:"role"`
	Content  string `json:"content"`
	Thinking string `json:"thinking,omitempty"`
}

type ollamaOptions struct {
	NumPredict int `json:"num_predict"`
}

// ollamaStreamLine is one NDJSON frame. DoneReason accompanies the final frame
// and is the only signal distinguishing a completed answer ("stop") from one
// the server cut short ("length") — both of which arrive as a successful HTTP
// response with done: true.
type ollamaStreamLine struct {
	Message    ollamaMessage `json:"message"`
	Done       bool          `json:"done"`
	DoneReason string        `json:"done_reason"`
}

// jsonStrategy returns the system prompt and the format value to send for a
// request. It is the single place the choice between "constrain the grammar"
// and "ask in the prompt" is made, so the two can never both apply — belt and
// braces here would reintroduce exactly the constraint being avoided.
func (a *OllamaAdapter) jsonStrategy(system string, forceJSON bool) (string, string) {
	if !forceJSON {
		return system, ""
	}
	if !a.thinks {
		return system, "json"
	}
	if system == "" {
		return jsonOnlyInstruction, ""
	}
	return system + "\n\n" + jsonOnlyInstruction, ""
}

// Complete implements LLMAdapter by POSTing to /api/chat with stream: true
// and reading NDJSON lines until done: true.
func (a *OllamaAdapter) Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error) {
	// How ForceJSON is honoured depends on whether the model reasons.
	//
	// format:"json" is a grammar constraint the runner must suspend for the
	// reasoning phase and resume for the answer, and on a reasoning model that
	// seam is where the output breaks: measured against gemma4:26b-mlx, the
	// constrained request produced parseable JSON 1 time in 4, while the same
	// request without it produced it 4 times in 4. The constraint meant to
	// guarantee JSON is what destroys it, so for a thinking model the
	// requirement moves into the prompt instead.
	//
	// On a model that does not reason there is no seam, the constraint holds,
	// and it stays: a grammar is a guarantee where a prompt is only a strong
	// convention, and those models never had the problem.
	format, think := "", (*bool)(nil)
	system, format = a.jsonStrategy(system, opts.ForceJSON)
	if a.thinks {
		think = new(bool)
		*think = true
	}

	chatMsgs := make([]ollamaMessage, 0, len(msgs)+1)
	chatMsgs = append(chatMsgs, ollamaMessage{Role: "system", Content: system})
	for _, m := range msgs {
		chatMsgs = append(chatMsgs, ollamaMessage{Role: m.Role, Content: m.Content})
	}

	reqBody := ollamaChatRequest{
		Model:    a.model,
		Messages: chatMsgs,
		Format:   format,
		Think:    think,
		Stream:   true,
		Options:  ollamaOptions{NumPredict: ollamaUncapped},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("ollama: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ollama: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(errBody))
	}

	// Only the content channel is the completion. Reasoning arrives in its own
	// field and is dropped here — deliberately, and it is what makes running a
	// reasoning model free of consequence for callers: they get the answer, not
	// the model's private deliberation, and never have to strip it themselves.
	var accumulated strings.Builder
	var doneReason string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var sl ollamaStreamLine
		if err := json.Unmarshal(line, &sl); err != nil {
			continue
		}
		if sl.Message.Content != "" {
			accumulated.WriteString(sl.Message.Content)
			if onChunk != nil {
				onChunk(sl.Message.Content)
			}
		}
		if sl.Done {
			doneReason = sl.DoneReason
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("ollama: read stream: %w", err)
	}

	// A cut-off stream is a *successful* HTTP response, so without this check it
	// reaches the caller as a completion — and on a reasoning model the text it
	// carries is routinely empty, the whole limit having gone to thinking before
	// the answer began. An empty string that parses as "the model said nothing"
	// is worse than an error, because nothing downstream can tell it apart from
	// a real answer.
	if doneReason == "length" {
		return accumulated.String(), fmt.Errorf(
			"ollama: response truncated (done_reason=length) after %d bytes: the model hit its generation limit before finishing",
			accumulated.Len())
	}

	return accumulated.String(), nil
}

// Model returns the model name used by this adapter.
func (a *OllamaAdapter) Model() string { return a.model }
