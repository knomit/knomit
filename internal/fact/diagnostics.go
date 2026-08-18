package fact

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Diagnostic is one problem found while validating ontology YAML. Line and
// Column are 1-based; Line 0 means "no position available" (a document-level
// problem such as a missing top-level key).
type Diagnostic struct {
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Message  string `json:"message"`
	Severity `json:"-"`
}

// Severity separates "this document cannot be used" from "this document is
// usable, but something in it is off".
//
// The distinction exists because the two READERS of an ontology want opposite
// things. Someone about to create a repository is choosing a taxonomy they can
// never change afterwards, so every oddity — an invented key, a misspelt
// `descriptionn:` — must be put in front of them while they can still fix it.
// Someone OPENING a repository that already exists has no such choice: the
// ontology is committed, it is what every fact in the repo was written against,
// and the only correct thing to do with a key this binary does not recognise is
// to carry on without it. Refusing there does not protect anyone — it hands the
// repo a DIFFERENT taxonomy, which is the one outcome nothing can undo.
//
// It is deliberately not on the wire (`json:"-"`). The validate endpoint
// reports ok:false for any diagnostic at all, which is the behaviour the editor
// wants; exposing severity would be an API change that this distinction does
// not require.
type Severity string

const (
	// SeverityError is the ZERO VALUE, so every Diagnostic written without a
	// thought about severity fails closed — an unclassified problem is treated
	// as fatal, never quietly tolerated.
	SeverityError Severity = ""
	// SeverityWarning marks a document that PARSES and is safe to use.
	SeverityWarning Severity = "warning"
)

// IsError reports whether this diagnostic makes the document unusable.
func (d Diagnostic) IsError() bool { return d.Severity != SeverityWarning }

// hasError reports whether any diagnostic in the set is fatal.
func hasError(diags []Diagnostic) bool {
	for _, d := range diags {
		if d.IsError() {
			return true
		}
	}
	return false
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
		return nil, parseErrDiags(err)
	}
	var o Ontology
	if doc.Kind != 0 {
		if err := doc.Decode(&o); err != nil {
			return nil, parseErrDiags(err)
		}
	}

	var diags []Diagnostic
	// Unknown keys are REPORTED, not ignored.
	//
	// doc.Decode above is go-yaml's default, which silently drops any key it
	// does not recognise. That made the editor's validation actively
	// misleading: a whole invented block (`fuusbar:` with children) came back
	// ok:true, and — worse, because it looks like it worked — a typo'd
	// `descriptionn:` dropped the description without a word. A user's only
	// signal that an ontology is well-formed is this response, and the ontology
	// is immutable once the repo is created.
	//
	// Runs BEFORE the required-key checks so a document that is both misspelt
	// and incomplete reports the misspelling first — that is the one the reader
	// can act on, and it is usually the cause of the other.
	diags = append(diags, unknownFieldDiags(data)...)
	if o.ID == "" {
		diags = append(diags, Diagnostic{Message: "parse ontology: id is required"})
	}
	if o.Name == "" {
		diags = append(diags, Diagnostic{Message: "parse ontology: name is required"})
	}
	if len(o.Topics) == 0 {
		diags = append(diags, Diagnostic{Message: "parse ontology: at least one topic is required"})
	}

	// Two different nodes per topic, and mixing them up costs the position:
	// topicsNode is the MAPPING under `topics:`, whose VALUE for a topic is that
	// topic's body (where `children:` lives), while topicNodes holds each topic's
	// KEY node (which is what carries the position to report). Descending into
	// `children` from the key node silently yields nothing — valueForKey returns
	// nil for a non-mapping — and every child diagnostic then reports line 0.
	topicsNode := valueForKey(documentRoot(&doc), "topics")
	topicNodes := mappingChildren(topicsNode)
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
		childNodes := mappingChildren(valueForKey(valueForKey(topicsNode, key), "children"))
		for _, child := range sortedKeys(node.Children) {
			if !validKeyRe.MatchString(child) {
				diags = append(diags, diagAt(childNodes[child], fmt.Sprintf(
					"parse ontology: invalid key %q in topic %q child: must be lowercase kebab-case", child, key)))
			}
		}
	}
	// Only a FATAL diagnostic withholds the ontology. Warnings travel back
	// ALONGSIDE a usable document, which is what lets the open path read a repo
	// whose ontology carries a key this binary does not declare.
	if hasError(diags) {
		return nil, diags
	}
	if err := o.buildRulesCache(); err != nil {
		// Appended, not substituted: dropping the warnings here would hide the
		// unknown key that is usually the reason the rules did not compile.
		return nil, append(diags, Diagnostic{Message: err.Error()})
	}
	return &o, diags
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

// parseErrDiags turns a go-yaml failure into diagnostics that carry their
// POSITION, instead of one blob reporting line 0.
//
// go-yaml puts the line in the message text — "line 4: mapping key \"topics\"
// already defined at line 2" — and a duplicate key arrives as a TypeError
// holding one such string per problem. Passing that straight through as a
// message left the editor with nothing to anchor a marker to, so a document
// with two `topics:` keys reported "Line 0" while naming line 4 in its own
// text. Splitting the prefix off puts the number where Diagnostic.Line can
// use it and takes it out of the sentence, where it read as noise.
func parseErrDiags(err error) []Diagnostic {
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) && len(typeErr.Errors) > 0 {
		diags := make([]Diagnostic, 0, len(typeErr.Errors))
		for _, msg := range typeErr.Errors {
			line, text := splitYAMLLinePrefix(msg)
			diags = append(diags, Diagnostic{Line: line, Column: 1, Message: "parse ontology: " + text})
		}
		return diags
	}
	// A syntax error is a single string, prefixed "yaml: " and often carrying
	// its own "line N:" after that.
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	line, text := splitYAMLLinePrefix(msg)
	return []Diagnostic{{Line: line, Column: 1, Message: "parse ontology: " + text}}
}

// unknownFieldDiags reports every YAML key that no struct field accepts.
//
// go-yaml exposes this only through a Decoder (KnownFields), not through
// Node.Decode, so this is a second pass over the same bytes. The document is
// capped at 256 KiB by both callers, so the duplicate parse is not worth
// avoiding at the cost of hand-walking the tree against a field list that
// would then need maintaining alongside OntologySchema.
//
// Topic and child names are NOT affected: they are map keys
// (map[string]*OntologyNode), and KnownFields only constrains struct fields —
// so an ontology may still name its topics whatever it likes, while
// `descriptionn:` inside one is caught.
func unknownFieldDiags(data []byte) []Diagnostic {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var strict Ontology
	err := dec.Decode(&strict)
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		// Anything else here (a syntax error) is already reported by the
		// Unmarshal above; reporting it twice would double every message.
		return nil
	}
	diags := make([]Diagnostic, 0, len(typeErr.Errors))
	for _, msg := range typeErr.Errors {
		// go-yaml formats these as "line N: field X not found in type T".
		// Only the field errors belong to this check — a type mismatch
		// ("cannot unmarshal !!str into int") is a different problem and is
		// reported by the decode above.
		if !strings.Contains(msg, "not found in type") {
			continue
		}
		line, text := splitYAMLLinePrefix(msg)
		// WARNING, not error. An unrecognised key does not stop this binary
		// reading the rest of the document, and the document may simply have
		// been written by a newer knomit — go-yaml's own default is to ignore
		// these entirely. Reported so the editor can show them; not fatal, so
		// opening an existing repository never trades its taxonomy for the
		// default one over a field name.
		diags = append(diags, Diagnostic{
			Line: line, Column: 1, Message: "parse ontology: " + text, Severity: SeverityWarning,
		})
	}
	return diags
}

// splitYAMLLinePrefix pulls the "line N: " prefix off a go-yaml error, so the
// number can drive an inline marker instead of sitting in the message text.
// Returns line 0 when there is no prefix — Diagnostic documents that as "no
// position available", which the editor renders as a document-level problem.
func splitYAMLLinePrefix(msg string) (int, string) {
	rest, ok := strings.CutPrefix(msg, "line ")
	if !ok {
		return 0, msg
	}
	num, text, ok := strings.Cut(rest, ": ")
	if !ok {
		return 0, msg
	}
	line, err := strconv.Atoi(num)
	if err != nil {
		return 0, msg
	}
	return line, text
}
