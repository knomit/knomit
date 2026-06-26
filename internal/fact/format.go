package fact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Fact represents a single knomit fact file (YAML frontmatter + Markdown body).
// path is private and always lowercase — use NewFact to construct, Path() to read.
type Fact struct {
	path           string   // private — always lowercase
	Title          string   `json:"title"`
	Body           string   `json:"body"`
	Kind           Kind     `json:"kind,omitempty"`
	Type           Type     `json:"type"`
	Domain         []string `json:"domain"`
	Confidence     float64  `json:"confidence"`
	Sources        int      `json:"sources"`
	Entities       []string `json:"entities"`
	Refs           []string `json:"refs"`
	EvidenceWeight float64  `json:"evidence_weight,omitempty"`
	Origin         Origin   `json:"origin,omitempty"`
}

// NewFact is the sole constructor. path is always lowercased.
func NewFact(path string) Fact { return Fact{path: strings.ToLower(path)} }

// Path returns the fact's canonical (lowercase) path.
func (f Fact) Path() string { return f.path }

// MarshalJSON exposes the private path field as "path" in JSON output.
// Kind is omitted when epistemic (the default) so existing consumers see
// no change in shape for epistemic facts.
func (f Fact) MarshalJSON() ([]byte, error) {
	type plain struct {
		Path           string   `json:"path"`
		Title          string   `json:"title"`
		Body           string   `json:"body"`
		Kind           Kind     `json:"kind,omitempty"`
		Type           Type     `json:"type"`
		Domain         []string `json:"domain"`
		Confidence     float64  `json:"confidence"`
		Sources        int      `json:"sources"`
		Entities       []string `json:"entities"`
		Refs           []string `json:"refs"`
		EvidenceWeight float64  `json:"evidence_weight,omitempty"`
		Origin         Origin   `json:"origin,omitempty"`
	}
	kind := f.Kind
	if kind == DefaultKind {
		kind = ""
	}
	origin := f.Origin
	if origin == DefaultOrigin {
		origin = ""
	}
	return json.Marshal(plain{
		Path:           f.path,
		Title:          f.Title,
		Body:           f.Body,
		Kind:           kind,
		Type:           f.Type,
		Domain:         f.Domain,
		Confidence:     f.Confidence,
		Sources:        f.Sources,
		Entities:       f.Entities,
		Refs:           f.Refs,
		EvidenceWeight: f.EvidenceWeight,
		Origin:         origin,
	})
}

// UnmarshalJSON reads "path" into the private field, enforcing lowercase.
// Missing "kind" defaults to epistemic.
func (f *Fact) UnmarshalJSON(data []byte) error {
	type plain struct {
		Path           string   `json:"path"`
		Title          string   `json:"title"`
		Body           string   `json:"body"`
		Kind           Kind     `json:"kind,omitempty"`
		Type           Type     `json:"type"`
		Domain         []string `json:"domain"`
		Confidence     float64  `json:"confidence"`
		Sources        int      `json:"sources"`
		Entities       []string `json:"entities"`
		Refs           []string `json:"refs"`
		EvidenceWeight float64  `json:"evidence_weight,omitempty"`
		Origin         Origin   `json:"origin,omitempty"`
	}
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	f.path = strings.ToLower(p.Path)
	f.Title = p.Title
	f.Body = p.Body
	f.Kind = p.Kind
	if f.Kind == "" {
		f.Kind = DefaultKind
	}
	f.Type = p.Type
	f.Domain = p.Domain
	f.Confidence = p.Confidence
	f.Sources = p.Sources
	f.Entities = p.Entities
	f.Refs = p.Refs
	f.EvidenceWeight = p.EvidenceWeight
	f.Origin = p.Origin
	if f.Origin == "" {
		f.Origin = DefaultOrigin
	}
	return nil
}

// frontmatter is the YAML structure parsed from the --- block.
type frontmatter struct {
	Kind           string   `yaml:"kind"`
	Type           string   `yaml:"type"`
	Domain         []string `yaml:"domain"`
	Confidence     float64  `yaml:"confidence"`
	Sources        int      `yaml:"sources"`
	Entities       []string `yaml:"entities"`
	Refs           []string `yaml:"refs"`
	EvidenceWeight float64  `yaml:"evidence_weight,omitempty"`
	Origin         string   `yaml:"origin"`
}

// ExtractBody strips the YAML frontmatter and the leading "# Title" heading
// from a raw fact file, returning just the prose body. It is a lightweight
// splitter (no YAML parse) used on hot indexing/embedding paths where the full
// ParseFact is unnecessary; it is the single source of this logic, shared by
// the store indexer and tools/calibrate. Returns the input unchanged when it
// has no frontmatter block.
func ExtractBody(raw []byte) string {
	content := string(raw)
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return content
	}
	afterFrontmatter := strings.TrimSpace(parts[2])
	// Skip the title line (first # heading).
	if idx := strings.Index(afterFrontmatter, "\n"); idx >= 0 {
		return strings.TrimSpace(afterFrontmatter[idx+1:])
	}
	return ""
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

	// Resolve kind: missing → epistemic (backward compat with every
	// existing fact file authored before pragmatic facts existed).
	kind := Kind(fm.Kind)
	if kind == "" {
		kind = DefaultKind
	}
	// Resolve leaf type. Epistemic facts retain the historical "missing
	// type → observation" default; pragmatic facts require an explicit
	// type because policy and heuristic are not interchangeable.
	leaf := Type(fm.Type)
	if leaf == "" && kind == Epistemic {
		leaf = DefaultEpistemicType
	}
	kind, err := validateKindAndType(kind, leaf)
	if err != nil {
		return Fact{}, fmt.Errorf("ParseFact %q: %w", path, err)
	}
	if err := validateBounds(fm.Confidence, fm.Sources); err != nil {
		return Fact{}, fmt.Errorf("ParseFact %q: %w", path, err)
	}

	// Resolve origin: explicit value wins; missing → distilled for synthesis
	// facts (all pre-origin synthesis facts were pipeline-distilled),
	// authored otherwise. New facts always set origin explicitly; this
	// default only covers legacy files, so no file rewrite is needed.
	origin := Origin(fm.Origin)
	if origin == "" {
		if leaf == Synthesis {
			origin = Distilled
		} else {
			origin = DefaultOrigin
		}
	}
	if err := origin.Validate(); err != nil {
		return Fact{}, fmt.Errorf("ParseFact %q: %w", path, err)
	}

	// Extract title from the first # heading in bodyRaw.
	title, body, err := extractTitle(path, bodyRaw)
	if err != nil {
		return Fact{}, err
	}

	f := NewFact(path)
	f.Title = title
	f.Body = body
	f.Kind = kind
	f.Type = leaf
	f.Domain = fm.Domain
	f.Confidence = fm.Confidence
	f.Sources = fm.Sources
	f.Entities = fm.Entities
	f.Refs = fm.Refs
	f.EvidenceWeight = fm.EvidenceWeight
	f.Origin = origin
	return f, nil
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
//	type: observation
//	domain: [a, b]
//	confidence: 0.9
//	sources: 1
//	entities: [foo]
//	refs: []
//	---
//	# Title
//
//	Body content.
//
// The frontmatter is rendered via gopkg.in/yaml.v3 (the same library
// used by ParseFact), so all YAML escaping rules — flow-context
// indicators (?, :, [, ], {, }, ,), reserved words, leading whitespace,
// control characters — are handled correctly by construction. Inline
// lists use FlowStyle to preserve the compact one-line per-key layout.
func SerializeFact(f Fact) (string, error) {
	// Validate (kind, type) before writing anything. Symmetric with
	// ParseFact: a Fact that survives a round-trip is guaranteed to
	// carry a valid pair.
	kind, err := validateKindAndType(f.Kind, f.Type)
	if err != nil {
		return "", fmt.Errorf("SerializeFact %q: %w", f.path, err)
	}
	if err := validateBounds(f.Confidence, f.Sources); err != nil {
		return "", fmt.Errorf("SerializeFact %q: %w", f.path, err)
	}

	// scalar leaves Tag empty so yaml.v3's resolver picks the type from
	// Value. Used for keys ("type", "domain", ...) and numeric values
	// ("0.85", "1") where auto-resolution to !!float / !!int is correct.
	scalar := func(v string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Value: v}
	}
	// strScalar pins Tag to !!str so list items like "null", "yes",
	// "No", "true" — values that would otherwise auto-resolve to YAML
	// null/bool — are emitted as quoted strings and read back as
	// strings. Without this, `entities: [No, yes, null, true]` parses
	// back as ["No", "yes", "true"] (null becomes Go nil and is
	// dropped from the slice). Items that don't collide with YAML
	// keywords stay unquoted because yaml.v3 only quotes when needed
	// to preserve the string tag.
	strScalar := func(v string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
	}
	flowSeq := func(items []string) *yaml.Node {
		n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, s := range items {
			n.Content = append(n.Content, strScalar(s))
		}
		return n
	}

	root := &yaml.Node{Kind: yaml.MappingNode}
	add := func(key string, val *yaml.Node) {
		root.Content = append(root.Content, scalar(key), val)
	}
	// Emit kind only when pragmatic. Epistemic is the default and historical
	// fact files have no kind field — omitting preserves byte-identical
	// round-trip for the existing corpus.
	if kind == Pragmatic {
		add("kind", strScalar(string(kind)))
	}
	add("type", strScalar(string(f.Type)))
	add("domain", flowSeq(f.Domain))
	add("confidence", scalar(fmt.Sprintf("%g", f.Confidence)))
	add("sources", scalar(fmt.Sprintf("%d", f.Sources)))
	if f.EvidenceWeight > 0 {
		add("evidence_weight", scalar(fmt.Sprintf("%g", f.EvidenceWeight)))
	}
	if f.Origin != "" && f.Origin != DefaultOrigin {
		add("origin", strScalar(string(f.Origin)))
	}
	add("entities", flowSeq(f.Entities))
	add("refs", flowSeq(f.Refs))

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return "", fmt.Errorf("SerializeFact: encode frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("SerializeFact: close encoder: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("---\n")
	sb.Write(buf.Bytes())
	sb.WriteString("---\n# ")
	sb.WriteString(f.Title)
	sb.WriteString("\n")
	if f.Body != "" {
		sb.WriteString("\n")
		sb.WriteString(f.Body)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}
