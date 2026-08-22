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
	ApplicableMethodology string // for distill_user.txt
	// MotifVocabulary is the §3.3 health picture, for reflect_user.txt. Empty
	// on a corpus with no motif vocabulary, and the template omits the section
	// entirely in that case rather than printing zeroes — a reflection prompt
	// carrying "0 clusters, recurrence 0%" invites the model to reason about a
	// mechanism the corpus is not using.
	MotifVocabulary string
	// SharedMotifs is the motifs already carried by two or more facts in a
	// distill cluster (§6 "distill enrichment"). Free context for an LLM that
	// is running anyway: if several members instantiate one regularity, the
	// synthesized claim probably does too. Empty when the cluster shares none,
	// and the template omits the line entirely then.
	SharedMotifs string
}

// RenderTemplate loads and renders a prompt template.
// operation: "prune", "distill", or "reflect"
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
