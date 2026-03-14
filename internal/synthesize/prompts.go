package synthesize

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"
)

//go:embed prompts/large/*.txt prompts/small/*.txt
var promptFS embed.FS

// PromptData is the data passed to prompt templates.
type PromptData struct {
	Facts        string
	RecipePrompt string
	StepPrompt   string
}

// RenderTemplate loads and renders a prompt template.
// profile: "large" or "small"
// operation: "prune" or "distill"
// promptType: "system", "user", or "retry"
func RenderTemplate(profile, operation, promptType string, data PromptData) (string, error) {
	path := fmt.Sprintf("prompts/%s/%s_%s.txt", profile, operation, promptType)
	raw, err := promptFS.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("load template %s: %w", path, err)
	}

	tmpl, err := template.New(path).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse template %s: %w", path, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render template %s: %w", path, err)
	}
	return buf.String(), nil
}
