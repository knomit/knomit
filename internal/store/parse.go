// Fact markdown parsing: converts a knomit fact file (YAML frontmatter +
// markdown body) into a FactRecord. Used during git sync to index new or
// modified files.
package store

import (
	"fmt"
	"strings"
)

// parseFact parses a knomit fact markdown file into a FactRecord.
// Expected format:
//
//	---
//	domain: [databases, sql]
//	confidence: 0.9
//	sources: 2
//	entities: [postgres, mysql]
//	refs: []
//	---
//	# Title of the fact
//
//	Body content.
func parseFact(path, content, commitHash string) (FactRecord, error) {
	// Split on "---" delimiters to separate frontmatter from body.
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return FactRecord{}, fmt.Errorf("parseFact: no frontmatter in %q", path)
	}

	frontmatter := parts[1]
	body := strings.TrimSpace(parts[2])

	// Parse each key: value line in the frontmatter.
	var domain []string
	var entities []string
	var refs []string
	var confidence float64
	var sources int

	for _, line := range strings.Split(frontmatter, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)

		switch k {
		case "domain":
			domain = parseYAMLList(v)
		case "entities":
			entities = parseYAMLList(v)
		case "refs":
			refs = parseYAMLList(v)
		case "confidence":
			fmt.Sscanf(v, "%f", &confidence)
		case "sources":
			fmt.Sscanf(v, "%d", &sources)
		}
	}

	// Extract title from the first markdown heading line (e.g. "# My Title").
	title := ""
	if strings.HasPrefix(body, "#") {
		nl := strings.IndexByte(body, '\n')
		if nl < 0 {
			title = strings.TrimSpace(strings.TrimLeft(body, "#"))
		} else {
			title = strings.TrimSpace(body[:nl])
			title = strings.TrimSpace(strings.TrimLeft(title, "#"))
		}
	}

	if title == "" {
		return FactRecord{}, fmt.Errorf("parseFact: no title heading in %q", path)
	}

	return FactRecord{
		Path:       path,
		Title:      title,
		Domain:     domain,
		Entities:   entities,
		Confidence: confidence,
		Sources:    sources,
		Refs:       refs,
		CommitHash: commitHash,
	}, nil
}

// parseYAMLList parses a simple YAML inline list like "[a, b, c]" or "[]".
func parseYAMLList(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	v = strings.TrimSpace(v)
	if v == "" {
		return []string{}
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
