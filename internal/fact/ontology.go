package fact

import (
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
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

// IsSubsetOf returns true if every topic, child, Validation, and attribute in
// o also appears in other (matched by key/name). Used by boot-time refresh to
// decide whether upgrading to a newer embedded preset is safe — if the
// stored ontology is a strict subset, the preset can only add, never break.
//
// Validations are matched by Name only — rule body and message differences
// don't block an upgrade (this is how a preset would deliver bug fixes to
// existing rules).
//
// Attributes ARE compared, by value: a subset here means "safe to OVERWRITE
// with other", and the refresh in repos/builder.go writes the preset over the
// stored file. A repo that flags a preset topic `learn_dedup: off` has the
// same taxonomy as the preset, but overwriting it would erase the flag on
// every boot. So a stored attribute the preset does not carry, with the same
// value, is divergence (a value that behaves as absent, like learn_dedup: on,
// is compared as absent) — the repo keeps its file and forgoes auto-upgrade,
// exactly as it would for a custom topic or rule. See SubsetDivergence for
// telling the two apart in a log.
func (o *Ontology) IsSubsetOf(other *Ontology) bool {
	return o.SubsetDivergence(other) == ""
}

// Divergence reasons returned by SubsetDivergence.
const (
	DivergenceShape      = "shape"      // a topic, child, or validation other lacks
	DivergenceAttributes = "attributes" // taxonomy is a subset; only attributes differ
)

// SubsetDivergence reports why o is NOT a subset of other: "" when it is,
// DivergenceAttributes when the taxonomy and validations would be a subset and
// only attributes stand in the way, DivergenceShape otherwise. It exists so the
// refresh can tell an operator that flagging a topic is what stopped preset
// auto-upgrades, rather than leaving them to guess.
func (o *Ontology) SubsetDivergence(other *Ontology) string {
	if o == nil || other == nil {
		return DivergenceShape
	}
	// Root-level validations: every name in o must exist in other.
	if !validationsSubset(o.Validations, other.Validations) {
		return DivergenceShape
	}
	// Every topic in o must exist in other, recursively. One walk: shape
	// divergence stops it; attribute divergence is only noted, since a shape
	// difference further on must still win.
	attrsDiverge := false
	for key, node := range o.Topics {
		otherNode, ok := other.Topics[key]
		if !ok || !nodeIsSubsetOf(node, otherNode, &attrsDiverge) {
			return DivergenceShape
		}
	}
	if attrsDiverge {
		return DivergenceAttributes
	}
	return ""
}

// nodeIsSubsetOf returns true if every Validation and child in n also appears
// in other. It sets *attrsDiverge when an attribute of n is missing from other
// or differs from it; a value that behaves as absent (learn_dedup: on) is
// compared as absent, so spelling out the default never stops an upgrade.
func nodeIsSubsetOf(n, other *OntologyNode, attrsDiverge *bool) bool {
	if n == nil {
		return true
	}
	if other == nil {
		return false
	}
	if !validationsSubset(n.Validations, other.Validations) {
		return false
	}
	for k, v := range n.Attributes {
		if attrIsAbsent(k, v) {
			continue
		}
		if ov, ok := other.Attributes[k]; !ok || !reflect.DeepEqual(v, ov) {
			*attrsDiverge = true
		}
	}
	for key, child := range n.Children {
		otherChild, ok := other.Children[key]
		if !ok {
			return false
		}
		if !nodeIsSubsetOf(child, otherChild, attrsDiverge) {
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

	// attrsByTopic maps every DECLARED topic path (lowercase, "topic" or
	// "topic/child/…") to its fully RESOLVED attribute map: its own
	// attributes laid over everything inherited from its ancestors. Paths
	// with nothing resolved are absent. Built in the same pass as byTopic, so
	// parse stays the single build point; read-only afterwards. The maps are
	// shared between a parent and any child that adds nothing — never write
	// to one.
	attrsByTopic map[string]map[string]any
}

// buildRulesCache compiles every Validation rule in the ontology (root +
// every node) and stores the result in o.cache. Called once at parse time.
// Returns an error if any rule fails to compile.
func (o *Ontology) buildRulesCache() error {
	o.cache = compiledRulesCache{
		byTopic:      map[string][]compiledRule{},
		attrsByTopic: map[string]map[string]any{},
	}
	o.cache.compileCalls++

	if rs, err := compileRules("<root>", o.Validations); err != nil {
		return err
	} else if len(rs) > 0 {
		o.cache.byTopic["<root>"] = rs
	}

	var walk func(prefix string, n *OntologyNode, inherited map[string]any) error
	walk = func(prefix string, n *OntologyNode, inherited map[string]any) error {
		// A bare key (`research:` with nothing under it) is a nil node but is
		// still DECLARED — Attr's walk, like ValidateFact's, stops one step
		// past it — so it records what it inherits before the nil return.
		resolved := inherited
		if n != nil && len(n.Attributes) > 0 {
			resolved = make(map[string]any, len(inherited)+len(n.Attributes))
			for k, v := range inherited {
				resolved[k] = v
			}
			for k, v := range n.Attributes {
				resolved[k] = v
			}
		}
		if len(resolved) > 0 {
			o.cache.attrsByTopic[prefix] = resolved
		}
		if n == nil {
			return nil
		}
		if rs, err := compileRules(prefix, n.Validations); err != nil {
			return err
		} else if len(rs) > 0 {
			o.cache.byTopic[prefix] = rs
		}
		for k, c := range n.Children {
			if err := walk(prefix+"/"+k, c, resolved); err != nil {
				return err
			}
		}
		return nil
	}
	for k, n := range o.Topics {
		if err := walk(k, n, nil); err != nil {
			return err
		}
	}
	return nil
}

// AttrLearnDedup is the attribute that, set to "off", makes knomit_learn skip
// both its auto-merge search and its same-subject refusal for incoming facts
// under the topic. It governs LEARN ONLY — review never reads it.
const AttrLearnDedup = "learn_dedup"

// attributeSpec declares one attribute key: the values it accepts, and which
// of them means the same as leaving the key out.
type attributeSpec struct {
	accepts string         // human description of the accepted values, for diagnostics
	valid   func(any) bool // reports whether a decoded value is accepted
	absent  any            // the value that behaves as absent; compared as absent by IsSubsetOf
}

// attributeRegistry is the ONLY place an attribute key is declared.
//
// A key missing from this map is NOT an error: it may have been written by a
// newer knomit, and the open path must still read that ontology (see
// ParseOntology). ValidateOntologyYAML reports it as a warning. A bad value
// for a key that IS here is fatal, on the open path too: warning and treating
// it as absent would make an older binary silently run dedup on a topic a
// newer one switched off, which is the silent loss the flag exists to stop. A
// repo that refuses writes with a named error is loud, and upgrading fixes it.
//
// CONSEQUENCE: each key's value set is CLOSED. A new behaviour ships as a NEW
// key, never as a new value of an existing key — an older binary would reject
// the new value and stop accepting writes to the repo.
var attributeRegistry = map[string]attributeSpec{
	// Exactly the strings "off" and "on"; "on" behaves as absent (and exists
	// so a child can undo a parent's "off"). A yaml bool is rejected on
	// purpose: `learn_dedup: false` reads as "dedup is off" to one person and
	// "the off-switch is false" to another, and go-yaml v3 decodes the bare
	// scalar `off` as the STRING "off" (YAML 1.2), so the unambiguous spelling
	// is also the one that parses.
	AttrLearnDedup: {
		accepts: `"off" or "on"`,
		valid: func(v any) bool {
			s, ok := v.(string)
			return ok && (s == "off" || s == "on")
		},
		absent: "on",
	},
}

// attrIsAbsent reports whether v is the value that means the same as leaving
// key out. Unknown keys have no such value.
func attrIsAbsent(key string, v any) bool {
	spec, ok := attributeRegistry[key]
	return ok && spec.absent != nil && reflect.DeepEqual(v, spec.absent)
}

// Attr resolves an attribute for a topic path by the same walk ValidateFact
// uses: lowercase each segment, root → topic → each DECLARED child, stopping
// at the first undeclared segment. The nearest declared value wins, so an
// undeclared deeper category inherits from its deepest declared ancestor.
// topicPath is "topic" or "topic/category/…" — the string learn passes to
// ValidateFact. Returns (nil, false) when nothing on the path sets key.
// Read-only after parse; safe for concurrent use.
func (o *Ontology) Attr(topicPath, key string) (any, bool) {
	if o == nil || topicPath == "" {
		return nil, false
	}
	parts := strings.Split(topicPath, "/")
	prefix := strings.ToLower(parts[0])
	node, ok := o.Topics[prefix]
	if !ok {
		return nil, false
	}
	for _, seg := range parts[1:] {
		// node can be nil at ANY depth — a bare topic key or a bare child
		// key — so this check comes before touching Children, exactly as in
		// ValidateFact.
		if node == nil || node.Children == nil {
			break
		}
		segLower := strings.ToLower(seg)
		child, ok := node.Children[segLower]
		if !ok {
			break
		}
		prefix = prefix + "/" + segLower
		node = child
	}
	v, ok := o.cache.attrsByTopic[prefix][key]
	return v, ok
}

// LearnDedupOff reports whether learn's dedup stages are switched off for
// facts written under topicPath. Callers use this rather than comparing the
// raw Attr value. A nil ontology returns false.
func (o *Ontology) LearnDedupOff(topicPath string) bool {
	v, _ := o.Attr(topicPath, AttrLearnDedup)
	return v == "off"
}

// OntologyNode is a single node in the ontology tree.
type OntologyNode struct {
	Description string                   `yaml:"description"`
	Children    map[string]*OntologyNode `yaml:"children,omitempty"`
	Validations []Validation             `yaml:"validations,omitempty"`
	// Attributes switch store behaviour for facts under this node and every
	// node below it that does not override them. Keys are declared in
	// attributeRegistry; resolve with Ontology.Attr, never by reading this
	// map directly (it holds only this node's own values, not inherited ones).
	Attributes map[string]any `yaml:"attributes,omitempty"`
}

// Validation is one ontology-declared rule evaluated against a fact on write.
type Validation struct {
	Name    string `yaml:"name"`
	Message string `yaml:"message"`
	Rule    string `yaml:"rule"`
}

// validKeyRe matches lowercase kebab-case identifiers.
var validKeyRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ParseOntology parses and validates an ontology from YAML bytes. It reports
// only the FIRST FATAL problem; use ValidateOntologyYAML when you want them all,
// warnings included.
//
// Warnings do not fail the parse. The callers here are the ones that READ an
// ontology which already exists — the repo open path (repos/builder.go) and the
// okf source reader — and for them a key this binary does not declare is not a
// reason to reject a document. Rejecting it there meant returning the DEFAULT
// ontology instead of the repo's own, which is unrecoverable: the ontology is
// fixed at create time and every fact in the repo was written against it.
// Callers that accept NEW input (Manager.Create, the validate endpoint) surface
// warnings themselves rather than relying on this.
func ParseOntology(data []byte) (*Ontology, error) {
	o, diags := ValidateOntologyYAML(data)
	for _, d := range diags {
		if d.IsError() {
			return nil, errors.New(d.Message)
		}
	}
	if o == nil {
		return nil, errors.New("parse ontology: no ontology in document")
	}
	return o, nil
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
		if err := serializeNode(topicsVal, k, o.Topics[k]); err != nil {
			return nil, fmt.Errorf("serialize ontology: topic %q: %w", k, err)
		}
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

// serializeNode tolerates a nil node: "topics:\n  alpha:\n" — a topic key with
// nothing under it — decodes to a nil *OntologyNode, and that YAML arrives
// from outside through POST /ontologies:validate and POST /repos (modes custom
// and seed). Dereferencing it here would be a remotely triggerable panic. The
// key is still emitted, as an empty mapping, so a round trip does not silently
// drop the topic the author declared.
func serializeNode(parent *yaml.Node, key string, node *OntologyNode) error {
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	valNode := &yaml.Node{Kind: yaml.MappingNode}
	parent.Content = append(parent.Content, keyNode, valNode)

	if node == nil {
		return nil
	}

	addScalar(valNode, "description", node.Description)

	if len(node.Attributes) > 0 {
		if err := serializeAttributes(valNode, node.Attributes); err != nil {
			return err
		}
	}

	if len(node.Validations) > 0 {
		serializeValidations(valNode, node.Validations)
	}

	if len(node.Children) > 0 {
		childKey := &yaml.Node{Kind: yaml.ScalarNode, Value: "children"}
		childVal := &yaml.Node{Kind: yaml.MappingNode}
		valNode.Content = append(valNode.Content, childKey, childVal)
		for _, ck := range sortedKeys(node.Children) {
			if err := serializeNode(childVal, ck, node.Children[ck]); err != nil {
				return fmt.Errorf("child %q: %w", ck, err)
			}
		}
	}
	return nil
}

// serializeAttributes emits the block with sorted keys. Without it Serialize
// silently dropped attributes — and initSeed and the custom-create path both
// serialize before committing, so a new repo would lose them. Values go
// through the yaml encoder, which quotes "off"/"on" (YAML 1.1 bools) — they
// still re-parse as the same strings.
func serializeAttributes(parent *yaml.Node, attrs map[string]any) error {
	m := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range slices.Sorted(maps.Keys(attrs)) {
		var v yaml.Node
		if err := v.Encode(attrs[k]); err != nil {
			return fmt.Errorf("attribute %q: %w", k, err)
		}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, &v)
	}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "attributes"}, m)
	return nil
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
