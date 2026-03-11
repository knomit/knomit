package llm

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	// Concatenate user messages as the prompt content
	var userParts []string
	for _, m := range msgs {
		if m.Role == "user" {
			userParts = append(userParts, m.Content)
		}
	}
	userContent := strings.Join(userParts, "\n\n")

	args := []string{"-p", "--system", system, "--output-format", "text"}
	if a.model != "" && a.model != "auto" {
		args = append(args, "--model", a.model)
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = bytes.NewBufferString(userContent)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("claude CLI stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting claude CLI: %w", err)
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
			return "", fmt.Errorf("reading claude CLI output: %w", readErr)
		}
	}

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("claude CLI exited with error: %w", err)
	}

	return buf.String(), nil
}
