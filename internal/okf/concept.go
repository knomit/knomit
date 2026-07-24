// internal/okf/concept.go
package okf

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"knomit/internal/fact"
)

// ErrNoType is returned by Concept when a fact has no type; the caller skips
// and counts it rather than emitting a non-conformant document.
var ErrNoType = errors.New("okf: fact has empty type")

// conceptFrontmatter is marshaled to YAML in a fixed key order. omitempty
// keeps absent optional data out of the output for stable bytes. The knomit_*
// block preserves every fact field so the deferred importer can reconstruct
// the fact from frontmatter alone (Task 13), independent of the derived tags.
type conceptFrontmatter struct {
	Type             string   `yaml:"type"`
	Title            string   `yaml:"title,omitempty"`
	Resource         string   `yaml:"resource,omitempty"`
	Tags             []string `yaml:"tags,omitempty"`
	Timestamp        string   `yaml:"timestamp,omitempty"`
	KnomitKind       string   `yaml:"knomit_kind,omitempty"`
	KnomitConfidence float64  `yaml:"knomit_confidence"`
	KnomitEvidenceWt float64  `yaml:"knomit_evidence_weight,omitempty"`
	KnomitOrigin     string   `yaml:"knomit_origin,omitempty"`
	KnomitSources    int      `yaml:"knomit_sources,omitempty"`
	KnomitDomain     []string `yaml:"knomit_domain,omitempty"`
	KnomitEntities   []string `yaml:"knomit_entities,omitempty"`
	KnomitRefs       []string `yaml:"knomit_refs,omitempty"`
	KnomitPath       string   `yaml:"knomit_path"`
}

// Concept renders one fact as a conformant OKF concept document.
func Concept(fi FactInput, repo RepoIdentity) ([]byte, error) {
	f := fi.Fact
	if strings.TrimSpace(string(f.Type)) == "" {
		return nil, ErrNoType
	}

	fm := conceptFrontmatter{
		Type:             string(f.Type),
		Title:            f.Title,
		Resource:         fmt.Sprintf("knomit://%s/%s", repo.ID, f.Path()),
		Tags:             buildTags(f),
		Timestamp:        fi.Timestamp.UTC().Format(time.RFC3339),
		KnomitKind:       string(f.Kind),
		KnomitConfidence: f.Confidence,
		KnomitEvidenceWt: f.EvidenceWeight,
		KnomitOrigin:     string(f.Origin),
		KnomitSources:    f.Sources,
		KnomitDomain:     f.Domain,
		KnomitEntities:   f.Entities,
		KnomitRefs:       f.Refs,
		KnomitPath:       f.Path(),
	}

	var yb bytes.Buffer
	enc := yaml.NewEncoder(&yb)
	enc.SetIndent(2)
	if err := enc.Encode(fm); err != nil {
		return nil, fmt.Errorf("okf: encode frontmatter: %w", err)
	}
	_ = enc.Close()

	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(yb.Bytes())
	out.WriteString("---\n\n")
	title := f.Title
	if title == "" {
		title = string(f.Type)
	}
	out.WriteString("# " + title + "\n\n")
	out.WriteString(strings.TrimRight(f.Body, "\n"))
	out.WriteString("\n")
	if len(f.Refs) > 0 {
		out.WriteString("\n# Citations\n\n")
		for _, r := range f.Refs {
			out.WriteString("- `" + r + "`\n")
		}
	}
	return out.Bytes(), nil
}

// buildTags concatenates domain, entities, and kind in that order, dropping
// empties and de-duplicating while preserving first-seen order.
func buildTags(f fact.Fact) []string {
	var tags []string
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		tags = append(tags, v)
	}
	for _, d := range f.Domain {
		add(d)
	}
	for _, e := range f.Entities {
		add(e)
	}
	add(string(f.Kind))
	return tags
}
