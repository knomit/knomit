package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"google.golang.org/genai"
)

// defaultModel: this is compression of text a human already wrote, not a
// reasoning task, so the cheapest current Flash-Lite is the right tier. One
// release costs a fraction of a cent; upgrading to gemini-3.6-flash is a
// one-line change if the summaries read poorly.
const defaultModel = "gemini-3.5-flash-lite"

const prompt = `You are writing the "What's new" section of a release note for
knomit, a knowledge-base tool with a desktop app, a server, and an MCP
integration.

Below is the changelog for this release, grouped by type, with pull request
titles and descriptions. Rewrite it as at most six short bullets aimed at
someone deciding whether to upgrade.

Rules:
- Describe what changed for a user of the software, not how it was implemented.
- Omit internal refactors, test changes, CI changes, and review-fix churn
  unless they change observable behaviour.
- One sentence per bullet. No marketing language. No trailing period-free
  fragments — write complete sentences.
- Do not invent anything that is not in the changelog.

Changelog:

`

type summarizer interface {
	Summarize(ctx context.Context, changelog string) ([]string, error)
}

// summarySchema constrains the response so the result needs validating, not
// parsing — the model cannot hand back prose we then have to guess at.
var summarySchema = &genai.Schema{
	Type: genai.TypeObject,
	Properties: map[string]*genai.Schema{
		"summary": {
			Type:  genai.TypeArray,
			Items: &genai.Schema{Type: genai.TypeString},
		},
	},
	Required: []string{"summary"},
}

type geminiSummarizer struct {
	client *genai.Client
	model  string
}

func (g geminiSummarizer) Summarize(ctx context.Context, changelog string) ([]string, error) {
	resp, err := g.client.Models.GenerateContent(ctx, g.model,
		genai.Text(prompt+changelog),
		&genai.GenerateContentConfig{
			ResponseMIMEType: "application/json",
			ResponseSchema:   summarySchema,
		})
	if err != nil {
		return nil, err
	}
	var out struct {
		Summary []string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(resp.Text()), &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return out.Summary, nil
}

// newSummarizer returns nil when no key is configured. A nil summarizer is a
// supported state, not an error: it is what every release looks like before
// GEMINI_API_KEY is set.
func newSummarizer(ctx context.Context) summarizer {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "relnotes: $GEMINI_API_KEY unset — skipping distillation")
		return nil
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  key,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "relnotes: gemini client:", err)
		return nil
	}
	return geminiSummarizer{client: client, model: defaultModel}
}

// Distill returns the rendered section, or "" if it could not produce one.
// It never returns an error: the caller composes whatever it got, and a
// release is never blocked by a summarization failure.
func Distill(ctx context.Context, s summarizer, changelog string) string {
	if s == nil {
		return ""
	}
	bullets, err := s.Summarize(ctx, changelog)
	if err != nil {
		fmt.Fprintln(os.Stderr, "relnotes: distillation failed:", err)
		return ""
	}
	var b strings.Builder
	b.WriteString("## What's new\n\n")
	n := 0
	for _, line := range bullets {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s\n", strings.TrimPrefix(line, "- "))
		n++
	}
	if n == 0 {
		fmt.Fprintln(os.Stderr, "relnotes: distillation returned no usable bullets")
		return ""
	}
	return b.String()
}

func runDistill(args []string) error {
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		// Even this is non-fatal: exit 0 having written nothing.
		fmt.Fprintln(os.Stderr, "relnotes: read stdin:", err)
		return nil
	}
	ctx := context.Background()
	fmt.Print(Distill(ctx, newSummarizer(ctx), string(in)))
	return nil
}
