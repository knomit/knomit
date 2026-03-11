package llm

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	// Concatenate user messages
	var userParts []string
	for _, m := range msgs {
		if m.Role == "user" {
			userParts = append(userParts, m.Content)
		}
	}
	userContent := strings.Join(userParts, "\n\n")

	// Use stdin for prompt content; -p "" triggers headless mode
	args := []string{"-p", ""}
	if a.model != "" && a.model != "auto" {
		args = append(args, "--model", a.model)
	}

	stdinContent := system + "\n\n" + userContent

	cmd := exec.CommandContext(ctx, "gemini", args...)
	cmd.Stdin = bytes.NewBufferString(stdinContent)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("gemini CLI stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting gemini CLI: %w", err)
	}

	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	for {
		n, readErr := stdout.Read(tmp)
		if n > 0 {
			chunk := string(tmp[:n])
			buf.WriteString(chunk)
			if onChunk != nil {
				onChunk(chunk)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = cmd.Wait()
			return "", fmt.Errorf("reading gemini CLI output: %w", readErr)
		}
	}

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("gemini CLI exited with error: %w", err)
	}

	return buf.String(), nil
}
