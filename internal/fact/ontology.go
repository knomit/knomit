package fact

import (
	_ "embed"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed ontology_default.yaml
var defaultOntologyYAML []byte

var (
	defaultOntology     *Ontology
	defaultOntologyOnce sync.Once
)

// DefaultOntology returns the embedded general-purpose ontology.
// It panics if the embedded YAML is invalid.
func DefaultOntology() *Ontology {
	defaultOntologyOnce.Do(func() {
		o, err := ParseOntology(defaultOntologyYAML)
		if err != nil {
			panic(fmt.Sprintf("embedded default ontology is invalid: %v", err))
		}
		defaultOntology = o
	})
	return defaultOntology
}

//go:embed ontology_code.yaml
var codeOntologyYAML []byte

var (
	codeOntology     *Ontology
	codeOntologyOnce sync.Once
)

// CodeOntology returns the embedded source-code ontology preset.
// It panics if the embedded YAML is invalid.
func CodeOntology() *Ontology {
	codeOntologyOnce.Do(func() {
		o, err := ParseOntology(codeOntologyYAML)
		if err != nil {
			panic(fmt.Sprintf("embedded code ontology is invalid: %v", err))
		}
		codeOntology = o
	})
	return codeOntology
}

// OntologyByPreset returns one of the embedded ontology presets by name.
// Known presets: "default", "code".
func OntologyByPreset(name string) (*Ontology, error) {
	switch name {
	case "default":
		return DefaultOntology(), nil
	case "code":
		return CodeOntology(), nil
	default:
		return nil, fmt.Errorf("unknown ontology preset: %q", name)
	}
}

// EmbeddedPresetByID returns the embedded preset whose ontology id matches
// the given id, or nil if no preset matches. Used by boot-time refresh to
// determine whether a stored ontology is derived from a known preset and
// therefore a candidate for auto-upgrade.
func EmbeddedPresetByID(id string) *Ontology {
	switch id {
	case "general":
		return DefaultOntology()
	case "source-code":
		return CodeOntology()
	default:
		return nil
	}
}

// IsSubsetOf returns true if every topic, child, and Validation in o also
// appears in other (matched by key/name). Used by boot-time refresh to
// decide whether upgrading to a newer embedded preset is safe — if the
// stored ontology is a strict subset, the preset can only add, never break.
//
// Validations are matched by Name only — rule body and message differences
// don't block an upgrade (this is how a preset would deliver bug fixes to
// existing rules).
func (o *Ontology) IsSubsetOf(other *Ontology) bool {
	if o == nil || other == nil {
		return false
	}
	// Root-level validations: every name in o must exist in other.
	if !validationsSubset(o.Validations, other.Validations) {
		return false
	}
	// Every topic in o must exist in other, recursively.
	for key, node := range o.Topics {
		otherNode, ok := other.Topics[key]
		if !ok {
			return false
		}
		if !nodeIsSubsetOf(node, otherNode) {
			return false
		}
	}
	return true
}

// nodeIsSubsetOf returns true if every Validation and child in n also
// appears in other.
func nodeIsSubsetOf(n, other *OntologyNode) bool {
	if n == nil {
		return true
	}
	if other == nil {
		return false
	}
	if !validationsSubset(n.Validations, other.Validations) {
		return false
	}
	for key, child := range n.Children {
		otherChild, ok := other.Children[key]
		if !ok {
			return false
		}
		if !nodeIsSubsetOf(child, otherChild) {
			return false
		}
	}
	return true
}

// validationsSubset returns true if every Validation Name in a appears as a
// Validation Name in b. Rule body and Message are not compared.
func validationsSubset(a, b []Validation) bool {
	if len(a) == 0 {
		return true
	}
	names := make(map[string]struct{}, len(b))
	for _, v := range b {
		names[v.Name] = struct{}{}
	}
	for _, v := range a {
		if _, ok := names[v.Name]; !ok {
			return false
		}
	}
	return true
}

// Ontology defines a hierarchical taxonomy for organizing knowledge.
type Ontology struct {
	ID          string                   `yaml:"id"`
	Name        string                   `yaml:"name"`
	Description string                   `yaml:"description"`
	Topics      map[string]*OntologyNode `yaml:"topics"`
	Validations []Validation             `yaml:"validations,omitempty"`

	cache compiledRulesCache
}

// compiledRulesCache holds compiled rules keyed by topic path. Built once
// at ParseOntology time; safe for concurrent reads thereafter.
type compiledRulesCache struct {
	byTopic      map[string][]compiledRule
	compileCalls int // test hook — incremented once at build time
}

// buildRulesCache compiles every Validation rule in the ontology (root +
// every node) and stores the result in o.cache. Called once at parse time.
// Returns an error if any rule fails to compile.
func (o *Ontology) buildRulesCache() error {
	o.cache = compiledRulesCache{
		byTopic: map[string][]compiledRule{},
	}
	o.cache.compileCalls++

	if rs, err := compileRules("<root>", o.Validations); err != nil {
		return err
	} else if len(rs) > 0 {
		o.cache.byTopic["<root>"] = rs
	}

	var walk func(prefix string, n *OntologyNode) error
	walk = func(prefix string, n *OntologyNode) error {
		if n == nil {
			return nil
		}
		if rs, err := compileRules(prefix, n.Validations); err != nil {
			return err
		} else if len(rs) > 0 {
			o.cache.byTopic[prefix] = rs
		}
		for k, c := range n.Children {
			if err := walk(prefix+"/"+k, c); err != nil {
				return err
			}
		}
		return nil
	}
	for k, n := range o.Topics {
		if err := walk(k, n); err != nil {
			return err
		}
	}
	return nil
}

// OntologyNode is a single node in the ontology tree.
type OntologyNode struct {
	Description string                   `yaml:"description"`
	Children    map[string]*OntologyNode `yaml:"children,omitempty"`
	Validations []Validation             `yaml:"validations,omitempty"`
}

// Validation is one ontology-declared rule evaluated against a fact on write.
type Validation struct {
	Name    string `yaml:"name"`
	Message string `yaml:"message"`
	Rule    string `yaml:"rule"`
}

// validKeyRe matches lowercase kebab-case identifiers.
var validKeyRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ParseOntology parses and validates an ontology from YAML bytes.
func ParseOntology(data []byte) (*Ontology, error) {
	var o Ontology
	if err := yaml.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("parse ontology: %w", err)
	}
	if o.ID == "" {
		return nil, fmt.Errorf("parse ontology: id is required")
	}
	if o.Name == "" {
		return nil, fmt.Errorf("parse ontology: name is required")
	}
	if len(o.Topics) == 0 {
		return nil, fmt.Errorf("parse ontology: at least one topic is required")
	}
	if err := validateKeys("topic", o.Topics); err != nil {
		return nil, err
	}
	for key, node := range o.Topics {
		if node.Children != nil {
			if err := validateKeys(fmt.Sprintf("topic %q child", key), node.Children); err != nil {
				return nil, err
			}
		}
	}
	if err := o.buildRulesCache(); err != nil {
		return nil, err
	}
	return &o, nil
}

// TopicNames returns the sorted top-level topic keys.
func (o *Ontology) TopicNames() []string {
	names := make([]string, 0, len(o.Topics))
	for k := range o.Topics {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// ValidatePath checks that path is valid against this ontology.
// The first segment must match a top-level topic. Subsequent segments are
// walked against defined children; once no matching child is found the
// remaining segments are accepted as freeform.
func (o *Ontology) ValidatePath(path string) error {
	if path == "" {
		return fmt.Errorf("validate path: empty path")
	}
	parts := strings.Split(path, "/")
	// Lowercase the topic for lookup — ontology keys are always lowercase.
	node, ok := o.Topics[strings.ToLower(parts[0])]
	if !ok {
		return fmt.Errorf("validate path: unknown topic %q", parts[0])
	}
	for _, seg := range parts[1:] {
		if node == nil || node.Children == nil {
			break // freeform from here
		}
		child, ok := node.Children[strings.ToLower(seg)]
		if !ok {
			break // freeform from here
		}
		node = child
	}
	return nil
}

// Serialize renders the ontology as YAML with deterministic key ordering.
func (o *Ontology) Serialize() ([]byte, error) {
	doc := &yaml.Node{Kind: yaml.DocumentNode}
	root := &yaml.Node{Kind: yaml.MappingNode}
	doc.Content = append(doc.Content, root)

	addScalar(root, "id", o.ID)
	addScalar(root, "name", o.Name)
	if o.Description != "" {
		addScalar(root, "description", o.Description)
	}
	if len(o.Validations) > 0 {
		serializeValidations(root, o.Validations)
	}

	topicsKey := &yaml.Node{Kind: yaml.ScalarNode, Value: "topics"}
	topicsVal := &yaml.Node{Kind: yaml.MappingNode}
	root.Content = append(root.Content, topicsKey, topicsVal)

	for _, k := range sortedKeys(o.Topics) {
		serializeNode(topicsVal, k, o.Topics[k])
	}

	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("serialize ontology: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("serialize ontology: %w", err)
	}
	return []byte(buf.String()), nil
}

func serializeNode(parent *yaml.Node, key string, node *OntologyNode) {
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	valNode := &yaml.Node{Kind: yaml.MappingNode}
	parent.Content = append(parent.Content, keyNode, valNode)

	addScalar(valNode, "description", node.Description)

	if len(node.Validations) > 0 {
		serializeValidations(valNode, node.Validations)
	}

	if len(node.Children) > 0 {
		childKey := &yaml.Node{Kind: yaml.ScalarNode, Value: "children"}
		childVal := &yaml.Node{Kind: yaml.MappingNode}
		valNode.Content = append(valNode.Content, childKey, childVal)
		for _, ck := range sortedKeys(node.Children) {
			serializeNode(childVal, ck, node.Children[ck])
		}
	}
}

func serializeValidations(parent *yaml.Node, vs []Validation) {
	if len(vs) == 0 {
		return
	}
	key := &yaml.Node{Kind: yaml.ScalarNode, Value: "validations"}
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	parent.Content = append(parent.Content, key, seq)
	for _, v := range vs {
		item := &yaml.Node{Kind: yaml.MappingNode}
		addScalar(item, "name", v.Name)
		addScalar(item, "message", v.Message)
		addScalar(item, "rule", v.Rule)
		seq.Content = append(seq.Content, item)
	}
}

func addScalar(parent *yaml.Node, key, value string) {
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value},
	)
}

func sortedKeys(m map[string]*OntologyNode) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateKeys checks that all map keys match validKeyRe.
func validateKeys(ctx string, m map[string]*OntologyNode) error {
	for k := range m {
		if !validKeyRe.MatchString(k) {
			return fmt.Errorf("parse ontology: invalid key %q in %s: must be lowercase kebab-case", k, ctx)
		}
	}
	return nil
}
