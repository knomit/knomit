package llm

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/genai"
)

type GeminiAdapter struct {
	client *genai.Client
	model  string
}

func NewGeminiAdapter(ctx context.Context, model string) (*GeminiAdapter, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("GOOGLE_AI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY (or GOOGLE_AI_API_KEY) is required for Gemini provider")
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("creating Gemini client: %w", err)
	}
	return &GeminiAdapter{client: client, model: model}, nil
}

func (a *GeminiAdapter) Complete(ctx context.Context, system string, msgs []Message, onChunk func(string)) (string, error) {
	var contents []*genai.Content
	for _, m := range msgs {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, genai.NewContentFromText(m.Content, genai.Role(role)))
	}

	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(system, genai.Role("")),
		MaxOutputTokens:   defaultMaxTokens,
	}

	var accumulated string
	for resp, err := range a.client.Models.GenerateContentStream(ctx, a.model, contents, cfg) {
		if err != nil {
			return "", fmt.Errorf("Gemini stream error: %w", err)
		}
		text := resp.Text()
		if text != "" {
			accumulated += text
			if onChunk != nil {
				onChunk(text)
			}
		}
	}

	return accumulated, nil
}
