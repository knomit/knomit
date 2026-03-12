package llm

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// defaultMaxTokens is the max_tokens value sent to all providers that require it.
const defaultMaxTokens = 8192

// AnthropicAdapter calls the Anthropic Messages API using the official Go SDK.
// It streams responses via server-sent events, invoking onChunk for each
// content_block_delta of type text_delta.
type AnthropicAdapter struct {
	client anthropic.Client
	model  string
}

// NewAnthropicAdapter creates an adapter for the given model (e.g.
// "claude-sonnet-4-6"). Additional request options (custom base URL,
// API key override, etc.) are forwarded to the SDK client.
func NewAnthropicAdapter(model string, opts ...option.RequestOption) *AnthropicAdapter {
	client := anthropic.NewClient(opts...)
	return &AnthropicAdapter{client: client, model: model}
}

// Complete implements LLMAdapter by opening a streaming Messages request.
func (a *AnthropicAdapter) Complete(ctx context.Context, system string, msgs []Message, onChunk func(string)) (string, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: defaultMaxTokens,
		System: []anthropic.TextBlockParam{
			{Text: system},
		},
	}

	for _, m := range msgs {
		block := anthropic.NewTextBlock(m.Content)
		switch m.Role {
		case "assistant":
			params.Messages = append(params.Messages, anthropic.NewAssistantMessage(block))
		default:
			params.Messages = append(params.Messages, anthropic.NewUserMessage(block))
		}
	}

	stream := a.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	var accumulated string
	for stream.Next() {
		event := stream.Current()
		if event.Type == "content_block_delta" {
			delta := event.AsContentBlockDelta()
			if delta.Delta.Type == "text_delta" {
				text := delta.Delta.AsTextDelta().Text
				if text != "" {
					accumulated += text
					if onChunk != nil {
						onChunk(text)
					}
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return "", err
	}

	return accumulated, nil
}
