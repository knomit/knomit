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
	path       string   // private — always lowercase
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Kind       Kind     `json:"kind,omitempty"`
	Type       Type     `json:"type"`
	Domain     []string `json:"domain"`
	Confidence float64  `json:"confidence"`
	Sources    int      `json:"sources"`
	Entities   []string `json:"entities"`
	// Motifs names the general regularities this fact is an instance of —
	// the ASPECT axis, orthogonal to Entities (subject) and Domain (area).
	// Optional and capped at MaxMotifs; see motif.go for the rules and
	// blueprint §1 for the contract. omitempty is load-bearing: the entire
	// pre-motif corpus must round-trip byte-identically, so an absent list
	// and an empty list are the same thing at every boundary.
	Motifs         []string `json:"motifs,omitempty"`
	Refs           []string `json:"refs"`
	EvidenceWeight float64  `json:"evidence_weight,omitempty"`
	Origin         Origin   `json:"origin,omitempty"`

	// RefWarnings describes refs whose SHAPE is malformed, as ParseFact found
	// them. Derived on read, never stored: it is absent from the frontmatter
	// struct, so a round trip neither reads nor writes it.
	//
	// It exists because ParseFact is deliberately lenient about ref shape (see
	// there) — a bad ref must not make a fact unreadable, but it must not be
	// invisible either, or the corpus quietly accumulates citations nobody can
	// follow. SerializeFact still refuses to write one.
	RefWarnings []string `json:"ref_warnings,omitempty"`
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
		Motifs         []string `json:"motifs,omitempty"`
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
		Motifs:         f.Motifs,
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
		Motifs         []string `json:"motifs,omitempty"`
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
	f.Motifs = p.Motifs
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
	Motifs         []string `yaml:"motifs,omitempty"`
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
	// Ref shape is the fourth validation axis, and — like origin below, and for
	// the same reason — it is ASYMMETRIC. Writing a malformed ref is an error;
	// reading one is a warning.
	//
	// Failing the parse made a fact carrying one bad ref permanently
	// unloadable: every consumer goes through ParseFact, so the fact vanished
	// from the search index and the provenance graph (silently — a rebuild does
	// not fail), the API returned 500 for that path, and it could be neither
	// viewed nor repaired. Worse, it applies TODAY's rules to a version written
	// under yesterday's: this is a historical graph, and a version that was
	// legal when committed must stay readable forever.
	//
	// Nothing a reader does depends on ref SHAPE: ClassifyRef already reports a
	// malformed ref as RefMalformed, which forms no edge and renders inert. So
	// the fact loses nothing by being read. SerializeFact still rejects, which
	// is what stops the corpus from accumulating more of them, and the write gate
	// runs on every write path besides.
	refWarnings := refShapeWarnings(fm.Refs)

	// Resolve origin: explicit value wins; missing → defaultOriginForType.
	// That helper is shared with SerializeFact's elision rule, so the two
	// directions cannot drift apart on what an absent field means.
	//
	// Origin validation is deliberately ASYMMETRIC with SerializeFact. Writing
	// an illegal origin is an error; reading one is not. An unrecognized origin
	// string, or a legal origin on the wrong type, degrades to the type-aware
	// default instead of failing the parse.
	//
	// The reason is that origin enforcement arrived after facts carrying bad
	// pairings had already been committed (observation+distilled, from the
	// window when knomit_learn accepted any origin). Rejecting those on read
	// made them permanently unloadable: every consumer that goes through
	// ParseFact — the web fact endpoint, MCP query/explain, index rebuild —
	// returned 500 for that path, so the fact could be neither viewed nor
	// repaired. Origin is provenance metadata; no reader's correctness depends
	// on it the way it depends on type. Losing the field beats losing the fact.
	//
	// Type is authoritative and origin yields to it, so the degraded value is
	// always legal for leaf and the fact stays serializable — the next write
	// self-heals the frontmatter. SerializeFact still rejects both conditions,
	// which is what stops the corpus from accumulating more of them.
	origin := Origin(fm.Origin)
	if origin == "" {
		origin = defaultOriginForType(leaf)
	}
	if origin.Validate() != nil || origin.ValidateForType(leaf) != nil {
		origin = defaultOriginForType(leaf)
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
	// Deliberately NOT normalized to []string{} when absent: motifs is the
	// only elided list field, so nil and empty must remain indistinguishable
	// all the way through, or a round trip changes the bytes.
	//
	// Lenient on read for the same reason refs and origin above are lenient:
	// a fact must never become unloadable because of a field nothing's
	// correctness depends on. Same helper as the write gate, so there is one
	// definition of a well-formed motif, not two.
	f.Motifs = DropInvalidMotifs(fm.Motifs)
	f.Refs = fm.Refs
	f.EvidenceWeight = fm.EvidenceWeight
	f.Origin = origin
	f.RefWarnings = refWarnings
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
	if err := ValidateRefs(f.Refs); err != nil {
		return "", fmt.Errorf("SerializeFact %q: %w", f.path, err)
	}
	if err := ValidateMotifs(f.Motifs); err != nil {
		return "", fmt.Errorf("SerializeFact %q: %w", f.path, err)
	}
	// Origin is held to the same standard as (kind, type): both
	// well-formedness and the origin×type pairing. Without this a fact with
	// `origin: distilled` on `type: observation` serialized cleanly and then
	// failed ParseFact on read-back — a file the writer could create but
	// nothing could load. Empty means "unset", not "invalid": the write
	// paths deliberately leave Origin zero so serialize elides the field and
	// ParseFact applies the type-aware default on read.
	if f.Origin != "" {
		if err := f.Origin.Validate(); err != nil {
			return "", fmt.Errorf("SerializeFact %q: %w", f.path, err)
		}
		if err := f.Origin.ValidateForType(f.Type); err != nil {
			return "", fmt.Errorf("SerializeFact %q: %w", f.path, err)
		}
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
	// Emit origin unless a reader would reconstruct this exact value from its
	// absence. The condition is deliberately narrower than the tempting
	// `f.Origin != defaultOriginForType(f.Type)`:
	//
	//   - empty            → elide; Origin unset means "let parse decide".
	//   - authored + non-synthesis → elide. defaultOriginForType agrees, and
	//     this is what keeps the entire pre-origin corpus byte-identical.
	//   - authored + synthesis → WRITE. This is the case the old
	//     `f.Origin != DefaultOrigin` test got wrong: it elided, and parse
	//     then resolved the missing line to distilled, silently converting a
	//     human-authored synthesis fact into pipeline output.
	//   - distilled + synthesis → WRITE, explicitly, even though parse would
	//     default to exactly that. Eliding here would be sound on read-back
	//     but would rewrite the frontmatter of the single most common
	//     synthesis fact in the corpus, churning every file for no gain.
	if f.Origin != "" && !(f.Origin == DefaultOrigin && defaultOriginForType(f.Type) == DefaultOrigin) {
		add("origin", strScalar(string(f.Origin)))
	}
	add("entities", flowSeq(f.Entities))
	// Emitted only when non-empty. `motifs: []` on the thousands of facts
	// that predate the field would churn every file in the corpus for no
	// information.
	if len(f.Motifs) > 0 {
		add("motifs", flowSeq(f.Motifs))
	}
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
