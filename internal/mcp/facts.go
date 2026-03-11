package mcp

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Fact represents a single knomit fact file (YAML frontmatter + Markdown body).
type Fact struct {
	Path       string
	Title      string
	Body       string
	Domain     []string
	Confidence float64
	Sources    int
	Entities   []string
	Refs       []string
}

// frontmatter is the YAML structure parsed from the --- block.
type frontmatter struct {
	Domain     []string `yaml:"domain"`
	Confidence float64  `yaml:"confidence"`
	Sources    int      `yaml:"sources"`
	Entities   []string `yaml:"entities"`
	Refs       []string `yaml:"refs"`
}

// ParseFact parses a fact file. path is the git path (stored in Fact.Path, not
// used for content parsing). Handles both \n and \r\n line endings.
func ParseFact(path, content string) (Fact, error) {
	// Normalise CRLF → LF.
	content = strings.ReplaceAll(content, "\r\n", "\n")

	// Split on the --- delimiters. The file must start with ---.
	if !strings.HasPrefix(content, "---\n") {
		return Fact{}, fmt.Errorf("ParseFact %q: missing opening frontmatter delimiter", path)
	}
	// Find the closing ---.
	rest := content[4:] // skip opening "---\n"
	closeIdx := strings.Index(rest, "\n---\n")
	if closeIdx < 0 {
		return Fact{}, fmt.Errorf("ParseFact %q: missing closing frontmatter delimiter", path)
	}

	yamlBlock := rest[:closeIdx]
	bodyRaw := strings.TrimSpace(rest[closeIdx+5:]) // skip "\n---\n"

	// Parse YAML frontmatter.
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(yamlBlock), &fm); err != nil {
		return Fact{}, fmt.Errorf("ParseFact %q: yaml: %w", path, err)
	}

	// Ensure slices are non-nil (important for round-trip fidelity).
	if fm.Domain == nil {
		fm.Domain = []string{}
	}
	if fm.Entities == nil {
		fm.Entities = []string{}
	}
	if fm.Refs == nil {
		fm.Refs = []string{}
	}

	// Extract title from the first # heading in bodyRaw.
	title, body, err := extractTitle(path, bodyRaw)
	if err != nil {
		return Fact{}, err
	}

	return Fact{
		Path:       path,
		Title:      title,
		Body:       body,
		Domain:     fm.Domain,
		Confidence: fm.Confidence,
		Sources:    fm.Sources,
		Entities:   fm.Entities,
		Refs:       fm.Refs,
	}, nil
}

// extractTitle finds the first # heading in body, strips it, and returns
// (title, remainingBody, err). The remaining body is trimmed of leading whitespace.
func extractTitle(path, body string) (string, string, error) {
	lines := strings.SplitN(body, "\n", 2)
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "#") {
		return "", "", fmt.Errorf("ParseFact %q: no title heading found", path)
	}
	// Strip one or more leading '#' characters and trim spaces.
	heading := strings.TrimLeft(lines[0], "#")
	title := strings.TrimSpace(heading)
	if title == "" {
		return "", "", fmt.Errorf("ParseFact %q: empty title heading", path)
	}

	rest := ""
	if len(lines) == 2 {
		rest = strings.TrimSpace(lines[1])
	}
	return title, rest, nil
}

// SerializeFact produces a fact file in the standard format:
//
//	---
//	domain: [a, b]
//	confidence: 0.9
//	sources: 1
//	entities: [foo]
//	refs: []
//	---
//	# Title
//
//	Body content.
func SerializeFact(f Fact) string {
	var sb strings.Builder

	sb.WriteString("---\n")
	sb.WriteString("domain: ")
	sb.WriteString(serializeInlineList(f.Domain))
	sb.WriteString("\n")

	// confidence: format without trailing zeros but always show at least one decimal.
	sb.WriteString(fmt.Sprintf("confidence: %g\n", f.Confidence))
	sb.WriteString(fmt.Sprintf("sources: %d\n", f.Sources))

	sb.WriteString("entities: ")
	sb.WriteString(serializeInlineList(f.Entities))
	sb.WriteString("\n")

	sb.WriteString("refs: ")
	sb.WriteString(serializeInlineList(f.Refs))
	sb.WriteString("\n")

	sb.WriteString("---\n")
	sb.WriteString("# ")
	sb.WriteString(f.Title)
	sb.WriteString("\n")

	if f.Body != "" {
		sb.WriteString("\n")
		sb.WriteString(f.Body)
		sb.WriteString("\n")
	}

	return sb.String()
}

// serializeInlineList renders a []string as a YAML inline list: [a, b, c] or [].
// Items containing commas, closing brackets, or double quotes are double-quoted.
func serializeInlineList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	quoted := make([]string, len(items))
	for i, item := range items {
		if strings.ContainsAny(item, ",]\"") {
			// Use double-quoted YAML string
			escaped := strings.ReplaceAll(item, `\`, `\\`)
			escaped = strings.ReplaceAll(escaped, `"`, `\"`)
			quoted[i] = `"` + escaped + `"`
		} else {
			quoted[i] = item
		}
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
