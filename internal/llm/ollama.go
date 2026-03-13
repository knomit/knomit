package llm

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

// OllamaAdapter calls a local or remote Ollama instance via its REST API.
type OllamaAdapter struct {
	host   string
	model  string
	client *http.Client
}

// NewOllamaAdapter creates an adapter after verifying the Ollama server is reachable.
// Reads OLLAMA_HOST from the environment (default: http://localhost:11434).
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

func (a *OllamaAdapter) Complete(ctx context.Context, system string, msgs []Message, onChunk func(string)) (string, error) {
	return "", fmt.Errorf("not implemented")
}
