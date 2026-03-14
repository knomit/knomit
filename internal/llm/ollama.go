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
	"time"
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
}

// NewOllamaAdapter creates an adapter after verifying the Ollama server is
// reachable (5-second health check against /api/tags). Returns an error if
// the server is down or unreachable.
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

	return &OllamaAdapter{host: host, model: model, client: client}, nil
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Format   string          `json:"format"`
	Stream   bool            `json:"stream"`
	Options  ollamaOptions   `json:"options"`
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaOptions struct {
	NumPredict int `json:"num_predict"`
}

type ollamaStreamLine struct {
	Message ollamaMessage `json:"message"`
	Done    bool          `json:"done"`
}

// Complete implements LLMAdapter by POSTing to /api/chat with stream: true
// and reading NDJSON lines until done: true.
func (a *OllamaAdapter) Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error) {
	chatMsgs := make([]ollamaMessage, 0, len(msgs)+1)
	chatMsgs = append(chatMsgs, ollamaMessage{Role: "system", Content: system})
	for _, m := range msgs {
		chatMsgs = append(chatMsgs, ollamaMessage{Role: m.Role, Content: m.Content})
	}

	format := ""
	if opts.ForceJSON {
		format = "json"
	}
	reqBody := ollamaChatRequest{
		Model:    a.model,
		Messages: chatMsgs,
		Format:   format,
		Stream:   true,
		Options:  ollamaOptions{NumPredict: defaultMaxTokens},
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

	var accumulated string
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
			accumulated += sl.Message.Content
			if onChunk != nil {
				onChunk(sl.Message.Content)
			}
		}
		if sl.Done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("ollama: read stream: %w", err)
	}

	return accumulated, nil
}

// Model returns the model name used by this adapter.
func (a *OllamaAdapter) Model() string { return a.model }
