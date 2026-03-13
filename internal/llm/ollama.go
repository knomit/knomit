package llm

import (
	"context"
	"fmt"
)

type OllamaAdapter struct{}

func NewOllamaAdapter(ctx context.Context, model string) (*OllamaAdapter, error) {
	return nil, fmt.Errorf("not implemented")
}

func (a *OllamaAdapter) Complete(ctx context.Context, system string, msgs []Message, onChunk func(string)) (string, error) {
	return "", fmt.Errorf("not implemented")
}
