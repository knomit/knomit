package llm

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// GeminiCLIAdapter shells out to the `gemini` CLI binary in headless mode
// (-p ""). Like ClaudeCLIAdapter, this is non-streaming and single-turn:
// the system prompt and all user messages are concatenated and piped to stdin.
type GeminiCLIAdapter struct {
	model string
}

// NewGeminiCLIAdapter creates an adapter. If model starts with "gemini-",
// it is passed as --model; otherwise the CLI default is used.
func NewGeminiCLIAdapter(model string) *GeminiCLIAdapter {
	return &GeminiCLIAdapter{model: model}
}

// Complete implements LLMAdapter by running `gemini -p ""` as a subprocess.
// The full response is returned at once (no streaming).
func (a *GeminiCLIAdapter) Complete(ctx context.Context, system string, msgs []Message, onChunk func(string)) (string, error) {

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
