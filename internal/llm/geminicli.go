package llm

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type GeminiCLIAdapter struct {
	model string
}

func NewGeminiCLIAdapter(model string) *GeminiCLIAdapter {
	return &GeminiCLIAdapter{model: model}
}

func (a *GeminiCLIAdapter) Complete(ctx context.Context, system string, msgs []Message, onChunk func(string)) (string, error) {
	// Note: multi-turn conversation (msgs with assistant-role entries) is not supported
	// by the CLI interface; only user messages are sent to the process.

	var userParts []string
	for _, m := range msgs {
		if m.Role == "user" {
			userParts = append(userParts, m.Content)
		}
	}
	userContent := strings.Join(userParts, "\n\n")

	// Use stdin for prompt content; -p "" triggers headless mode
	args := []string{"-p", ""}
	if strings.HasPrefix(a.model, "gemini-") {
		args = append(args, "--model", a.model)
	}

	stdinContent := system + "\n\n" + userContent

	cmd := exec.CommandContext(ctx, "gemini", args...)
	cmd.Stdin = strings.NewReader(stdinContent)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("geminicli: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	result := strings.TrimSpace(string(out))
	if onChunk != nil {
		onChunk(result)
	}
	return result, nil
}
