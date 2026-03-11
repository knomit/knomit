package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

type BedrockAdapter struct {
	client *bedrockruntime.Client
	model  string
}

func NewBedrockAdapter(ctx context.Context, model string) (*BedrockAdapter, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	client := bedrockruntime.NewFromConfig(cfg)
	return &BedrockAdapter{client: client, model: model}, nil
}

func (a *BedrockAdapter) Complete(ctx context.Context, system string, msgs []Message, onChunk func(string)) (string, error) {
	type bedrockMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type bedrockRequest struct {
		AnthropicVersion string           `json:"anthropic_version"`
		MaxTokens        int              `json:"max_tokens"`
		System           string           `json:"system"`
		Messages         []bedrockMessage `json:"messages"`
	}

	bedrockMsgs := make([]bedrockMessage, len(msgs))
	for i, m := range msgs {
		bedrockMsgs[i] = bedrockMessage{Role: m.Role, Content: m.Content}
	}

	payload, err := json.Marshal(bedrockRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        defaultMaxTokens,
		System:           system,
		Messages:         bedrockMsgs,
	})
	if err != nil {
		return "", fmt.Errorf("marshaling Bedrock request: %w", err)
	}

	output, err := a.client.InvokeModelWithResponseStream(ctx, &bedrockruntime.InvokeModelWithResponseStreamInput{
		ModelId:     &a.model,
		Body:        payload,
		ContentType: strPtr("application/json"),
		Accept:      strPtr("application/json"),
	})
	if err != nil {
		return "", fmt.Errorf("Bedrock InvokeModelWithResponseStream: %w", err)
	}
	defer output.GetStream().Close()

	type deltaEvent struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}

	var accumulated string
	for event := range output.GetStream().Events() {
		switch v := event.(type) {
		case *types.ResponseStreamMemberChunk:
			var delta deltaEvent
			if err := json.Unmarshal(v.Value.Bytes, &delta); err != nil {
				continue
			}
			if delta.Type == "content_block_delta" && delta.Delta.Type == "text_delta" && delta.Delta.Text != "" {
				accumulated += delta.Delta.Text
				if onChunk != nil {
					onChunk(delta.Delta.Text)
				}
			}
		}
	}
	if err := output.GetStream().Err(); err != nil {
		return "", fmt.Errorf("Bedrock stream error: %w", err)
	}

	return accumulated, nil
}

func strPtr(s string) *string { return &s }
