package fact

import (
	"fmt"
	"regexp"
	"sort"

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

// validateKeys checks that all map keys match validKeyRe.
func validateKeys(ctx string, m map[string]*OntologyNode) error {
	for k := range m {
		if !validKeyRe.MatchString(k) {
			return fmt.Errorf("parse ontology: invalid key %q in %s: must be lowercase kebab-case", k, ctx)
		}
	}
	return nil
}
