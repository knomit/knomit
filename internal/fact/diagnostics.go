package fact

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Diagnostic is one problem found while validating ontology YAML. Line and
// Column are 1-based; Line 0 means "no position available" (a document-level
// problem such as a missing top-level key).
type Diagnostic struct {
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}

// ValidateOntologyYAML parses and validates ontology YAML, collecting EVERY
// problem rather than stopping at the first. It returns the parsed ontology
// when there are no diagnostics, and nil otherwise.
//
// The diagnostic ORDER is load-bearing: ParseOntology returns diags[0] as its
// error, so this function must apply checks in the same order ParseOntology
// historically did — id, name, topics-empty, topic keys, child keys, rules
// cache — or ParseOntology's first error changes for some inputs.
func ValidateOntologyYAML(data []byte) (*Ontology, []Diagnostic) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, []Diagnostic{{Message: fmt.Sprintf("parse ontology: %v", err)}}
	}
	var o Ontology
	if doc.Kind != 0 {
		if err := doc.Decode(&o); err != nil {
			return nil, []Diagnostic{{Message: fmt.Sprintf("parse ontology: %v", err)}}
		}
	}

	var diags []Diagnostic
	if o.ID == "" {
		diags = append(diags, Diagnostic{Message: "parse ontology: id is required"})
	}
	if o.Name == "" {
		diags = append(diags, Diagnostic{Message: "parse ontology: name is required"})
	}
	if len(o.Topics) == 0 {
		diags = append(diags, Diagnostic{Message: "parse ontology: at least one topic is required"})
	}

	topicNodes := mappingChildren(valueForKey(documentRoot(&doc), "topics"))
	for _, key := range sortedKeys(o.Topics) {
		if !validKeyRe.MatchString(key) {
			diags = append(diags, diagAt(topicNodes[key], fmt.Sprintf(
				"parse ontology: invalid key %q in topic: must be lowercase kebab-case", key)))
		}
	}
	for _, key := range sortedKeys(o.Topics) {
		node := o.Topics[key]
		if node == nil || node.Children == nil {
			continue
		}
		childNodes := mappingChildren(valueForKey(topicNodes[key], "children"))
		for _, child := range sortedKeys(node.Children) {
			if !validKeyRe.MatchString(child) {
				diags = append(diags, diagAt(childNodes[child], fmt.Sprintf(
					"parse ontology: invalid key %q in topic %q child: must be lowercase kebab-case", child, key)))
			}
		}
	}
	if len(diags) > 0 {
		return nil, diags
	}
	if err := o.buildRulesCache(); err != nil {
		return nil, []Diagnostic{{Message: err.Error()}}
	}
	return &o, nil
}

// documentRoot returns the mapping node inside a document node.
func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	return doc.Content[0]
}

// valueForKey returns the value node for key in a mapping node, or nil.
func valueForKey(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mappingChildren maps each key in a mapping node to its KEY node, which is
// what carries the position we want to report.
func mappingChildren(m *yaml.Node) map[string]*yaml.Node {
	out := map[string]*yaml.Node{}
	if m == nil || m.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		out[m.Content[i].Value] = m.Content[i]
	}
	return out
}

func diagAt(n *yaml.Node, msg string) Diagnostic {
	d := Diagnostic{Message: msg}
	if n != nil {
		d.Line, d.Column = n.Line, n.Column
	}
	return d
}
