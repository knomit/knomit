package synthesize

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"
)

//go:embed prompts/large/*.txt
var promptFS embed.FS

// PromptData is the data passed to prompt templates. OntologyRoot is the
// configured root (e.g. "kb") under which all generated fact paths must
// live; templates substitute it into example paths so the LLM receives
// concrete, validated path conventions instead of hardcoded placeholders.
type PromptData struct {
	Facts                 string
	OntologyRoot          string
	ExistingMethodology   string // for reflect_user.txt
	ApplicableMethodology string // for distill_user.txt (Task 6)
}

// RenderTemplate loads and renders a prompt template.
// operation: "prune" or "distill"
// promptType: "system", "user", or "retry"
func RenderTemplate(operation, promptType string, data PromptData) (string, error) {
	path := fmt.Sprintf("prompts/large/%s_%s.txt", operation, promptType)
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
