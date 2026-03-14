package fact

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Ontology defines a hierarchical taxonomy for organizing knowledge.
type Ontology struct {
	ID          string                   `yaml:"id"`
	Name        string                   `yaml:"name"`
	Description string                   `yaml:"description"`
	Topics      map[string]*OntologyNode `yaml:"topics"`
}

// OntologyNode is a single node in the ontology tree.
type OntologyNode struct {
	Description string                   `yaml:"description"`
	Children    map[string]*OntologyNode `yaml:"children,omitempty"`
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
	node, ok := o.Topics[parts[0]]
	if !ok {
		return fmt.Errorf("validate path: unknown topic %q", parts[0])
	}
	for _, seg := range parts[1:] {
		if node == nil || node.Children == nil {
			break // freeform from here
		}
		child, ok := node.Children[seg]
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

	if len(node.Children) > 0 {
		childKey := &yaml.Node{Kind: yaml.ScalarNode, Value: "children"}
		childVal := &yaml.Node{Kind: yaml.MappingNode}
		valNode.Content = append(valNode.Content, childKey, childVal)
		for _, ck := range sortedKeys(node.Children) {
			serializeNode(childVal, ck, node.Children[ck])
		}
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
