package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// BedrockAdapter calls Anthropic models hosted on AWS Bedrock using the
// InvokeModelWithResponseStream API. It manually constructs the Anthropic
// JSON request body (anthropic_version "bedrock-2023-05-31") and parses
// the NDJSON stream of content_block_delta events.
//
// AWS credentials are loaded from the default credential chain
// (env vars, ~/.aws/credentials, IAM role, etc.).
type BedrockAdapter struct {
	client *bedrockruntime.Client
	model  string
}

// NewBedrockAdapter creates an adapter using the default AWS config.
// Model should be a Bedrock model ID (e.g. "anthropic.claude-3-5-sonnet-20241022-v2:0"
// or a cross-region ID like "us.anthropic.claude-3-5-sonnet-20241022-v2:0").
func NewBedrockAdapter(ctx context.Context, model string) (*BedrockAdapter, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	client := bedrockruntime.NewFromConfig(cfg)
	return &BedrockAdapter{client: client, model: model}, nil
}

// Complete implements LLMAdapter by invoking the Bedrock streaming API.
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
