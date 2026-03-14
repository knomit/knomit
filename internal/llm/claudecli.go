package llm

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ClaudeCLIAdapter shells out to the `claude` CLI binary (Claude Code) in
// print mode (-p). This is a non-streaming, single-turn adapter: all user
// messages are concatenated and piped to stdin; assistant-role entries are
// discarded because the CLI has no multi-turn support.
//
// Useful as a zero-config fallback when no API key is available but the
// user has Claude Code installed.
type ClaudeCLIAdapter struct {
	model string
}

// NewClaudeCLIAdapter creates an adapter. If model starts with "claude-",
// it is passed as --model; otherwise the CLI default is used.
func NewClaudeCLIAdapter(model string) *ClaudeCLIAdapter {
	return &ClaudeCLIAdapter{model: model}
}

// Complete implements LLMAdapter by running `claude -p` as a subprocess.
// The full response is returned at once (no streaming).
func (a *ClaudeCLIAdapter) Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error) {

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

// Model returns the model name used by this adapter.
func (a *ClaudeCLIAdapter) Model() string { return a.model }
