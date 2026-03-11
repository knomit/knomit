package llm

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type AnthropicAdapter struct {
	client anthropic.Client
	model  string
}

func NewAnthropicAdapter(model string, opts ...option.RequestOption) *AnthropicAdapter {
	client := anthropic.NewClient(opts...)
	return &AnthropicAdapter{client: client, model: model}
}

func (a *AnthropicAdapter) Complete(ctx context.Context, system string, msgs []Message, onChunk func(string)) (string, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 8192,
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
