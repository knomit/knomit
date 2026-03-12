package llm

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type ClaudeCLIAdapter struct {
	model string
}

func NewClaudeCLIAdapter(model string) *ClaudeCLIAdapter {
	return &ClaudeCLIAdapter{model: model}
}

func (a *ClaudeCLIAdapter) Complete(ctx context.Context, system string, msgs []Message, onChunk func(string)) (string, error) {
	// Note: multi-turn conversation (msgs with assistant-role entries) is not supported
	// by the CLI interface; only user messages are sent to the process.

	var userParts []string
	for _, m := range msgs {
		if m.Role == "user" {
			userParts = append(userParts, m.Content)
		}
	}
	userContent := strings.Join(userParts, "\n\n")

	args := []string{"-p", "--system", system, "--output-format", "text"}
	if strings.HasPrefix(a.model, "claude-") {
		args = append(args, "--model", a.model)
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = strings.NewReader(userContent)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("claudecli: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	result := strings.TrimSpace(string(out))
	if onChunk != nil {
		onChunk(result)
	}
	return result, nil
}
