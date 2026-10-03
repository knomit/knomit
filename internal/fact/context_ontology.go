package fact

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Context declarations (F22) live on topic nodes of the repository's own
// ontology, as a `context:` block:
//
//	topics:
//	  verdicts:
//	    context:
//	      task:    {type: string, required: true, pattern: "^t-[a-z0-9-]{1,40}$"}
//	      verdict: {type: enum, values: [agree, disagree, unsure], required: true}
//	      score:   {type: number, min: 0, max: 1}
//
// A key is DECLARED for a fact when the `context:` block of the fact's topic
// or of any ancestor on its walk names it; anything else is refused on write.
// Walking root → topic → child, a node's keys are added to its parent's and a
// child may redeclare a key (the nearest declaration wins). A topic with no
// `context:` block anywhere on its walk accepts no context at all. There is no
// root-level block.

// The declared types (v1, decided: primitives only, no lists).
const (
	ContextTypeString = "string"
	ContextTypeNumber = "number"
	ContextTypeBool   = "bool"
	ContextTypeEnum   = "enum"
	ContextTypeTime   = "time"
)

// ContextDecl is one key's declaration.
type ContextDecl struct {
	Type     string   `yaml:"type"`
	Values   []string `yaml:"values,omitempty"`   // enum only
	Required bool     `yaml:"required,omitempty"` // a fact under the topic must carry the key
	Pattern  string   `yaml:"pattern,omitempty"`  // string only; a Go regexp, matched unanchored
	Min      *float64 `yaml:"min,omitempty"`      // number only, inclusive
	Max      *float64 `yaml:"max,omitempty"`      // number only, inclusive
	MaxLen   int      `yaml:"max_len,omitempty"`  // string only, bytes; 1..MaxContextValueBytes
}

// compiledContextDecl is a resolved declaration. A declaration that failed
// its check is kept POISONED (problem set): every value for the key is then
// refused naming the ontology problem. Dropping it instead would silently fall
// back to a parent's declaration, or make the key read as undeclared with a
// misleading message — failing closed with the real reason is the useful
// answer. A poisoned key is never required.
type compiledContextDecl struct {
	ContextDecl
	re      *regexp.Regexp
	problem string
}

// contextDeclProblem describes what is wrong with a declaration, "" when it is
// well formed.
func contextDeclProblem(key string, d ContextDecl) string {
	if !ValidContextKey(key) {
		return fmt.Sprintf("context key %q must match [a-z][a-z0-9_]* and be at most %d characters", key, MaxContextKeyLen)
	}
	switch d.Type {
	case ContextTypeString, ContextTypeNumber, ContextTypeBool, ContextTypeEnum, ContextTypeTime:
	case "":
		return fmt.Sprintf("context key %q has no type (string, number, bool, enum or time)", key)
	default:
		return fmt.Sprintf("context key %q has unknown type %q (string, number, bool, enum or time)", key, d.Type)
	}
	if d.Type == ContextTypeEnum && len(d.Values) == 0 {
		return fmt.Sprintf("context key %q is an enum with no values", key)
	}
	if d.Type != ContextTypeEnum && len(d.Values) > 0 {
		return fmt.Sprintf("context key %q: values belong to an enum, not a %s", key, d.Type)
	}
	if d.Type == ContextTypeEnum {
		for _, v := range d.Values {
			if err := contextStringShape(v); err != nil {
				return fmt.Sprintf("context key %q: enum value %q: %v", key, v, err)
			}
		}
	}
	if d.Pattern != "" {
		if d.Type != ContextTypeString {
			return fmt.Sprintf("context key %q: pattern applies to a string, not a %s", key, d.Type)
		}
		if _, err := regexp.Compile(d.Pattern); err != nil {
			return fmt.Sprintf("context key %q: pattern does not compile: %v", key, err)
		}
	}
	if (d.Min != nil || d.Max != nil) && d.Type != ContextTypeNumber {
		return fmt.Sprintf("context key %q: min and max apply to a number, not a %s", key, d.Type)
	}
	if d.Min != nil && d.Max != nil && *d.Min > *d.Max {
		return fmt.Sprintf("context key %q: min %v is above max %v", key, *d.Min, *d.Max)
	}
	if d.MaxLen != 0 {
		if d.Type != ContextTypeString {
			return fmt.Sprintf("context key %q: max_len applies to a string, not a %s", key, d.Type)
		}
		if d.MaxLen < 1 || d.MaxLen > MaxContextValueBytes {
			return fmt.Sprintf("context key %q: max_len %d is outside 1..%d", key, d.MaxLen, MaxContextValueBytes)
		}
	}
	return ""
}

// compileContextDecls compiles one node's block. Problems are returned for the
// diagnostics, and the bad declarations are kept poisoned.
func compileContextDecls(decls map[string]ContextDecl) map[string]*compiledContextDecl {
	out := make(map[string]*compiledContextDecl, len(decls))
	for k, d := range decls {
		c := &compiledContextDecl{ContextDecl: d, problem: contextDeclProblem(k, d)}
		if c.problem == "" && d.Pattern != "" {
			c.re = regexp.MustCompile(d.Pattern) // compiled above without error
		}
		out[k] = c
	}
	return out
}

// contextDiags reports every bad declaration under node (topic path `path`)
// and below it. Each is a WARNING that ParseNewOntology refuses (the F02 rule):
// an existing repository must keep opening whatever its committed file says,
// and its bad declaration is poisoned, so writes of that key are refused with
// the reason; a new ontology is told now.
func contextDiags(path string, node *OntologyNode, body *yaml.Node) []Diagnostic {
	if node == nil {
		return nil
	}
	var diags []Diagnostic
	keys := mappingChildren(valueForKey(body, "context"))
	for _, k := range slices.Sorted(maps.Keys(node.Context)) {
		if p := contextDeclProblem(k, node.Context[k]); p != "" {
			d := diagAt(keys[k], fmt.Sprintf("parse ontology: topic %q: %s; every value of it is refused", path, p))
			d.Severity, d.newOnly = SeverityWarning, true
			diags = append(diags, d)
		}
	}
	children := valueForKey(body, "children")
	for _, ck := range sortedKeys(node.Children) {
		diags = append(diags, contextDiags(path+"/"+ck, node.Children[ck], valueForKey(children, ck))...)
	}
	return diags
}

// ContextSpec resolves the declarations in force for topicPath, by the walk
// Attr uses. nil means nothing is declared there: any context is refused.
func (o *Ontology) ContextSpec(topicPath string) map[string]*compiledContextDecl {
	if o == nil || topicPath == "" {
		return nil
	}
	return o.cache.contextByTopic[o.resolveDeclaredPrefix(topicPath)]
}

// resolveDeclaredPrefix is Attr's walk: lowercase each segment, follow
// declared children, stop at the first undeclared one. "" when the topic
// itself is not declared.
func (o *Ontology) resolveDeclaredPrefix(topicPath string) string {
	parts := strings.Split(topicPath, "/")
	prefix := strings.ToLower(parts[0])
	node, ok := o.Topics[prefix]
	if !ok {
		return ""
	}
	for _, seg := range parts[1:] {
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
	return prefix
}

// ContextError is a typed-gate refusal. It names the topic and the key, and
// says how to fix the call.
type ContextError struct {
	Topic string
	Key   string
	Msg   string
}

func (e *ContextError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("context at %s: %s", e.Topic, e.Msg)
	}
	return fmt.Sprintf("context key %q at %s: %s; send a corrected context, or {} to clear it", e.Key, e.Topic, e.Msg)
}

// ErrContextWithoutOntology is the refusal for a non-empty context where no
// ontology applies: a private-state path, or no ontology at all. Nothing
// declares a key there, so every key is undeclared.
var ErrContextWithoutOntology = errors.New("context is not allowed here: no ontology topic declares any context key for this path; send no context, or {} to clear it")

// ValidateContext is the typed gate: every key must be declared on the walk of
// topicPath, each value must satisfy its declaration, and every required key
// must be present. Values are checked as given; time values must already be
// normalised or at least RFC 3339 (NormalizeContext does that first on every
// write path). A nil ontology refuses any non-empty context.
func ValidateContext(o *Ontology, topicPath string, ctx map[string]any) error {
	if o == nil {
		if len(ctx) > 0 {
			return ErrContextWithoutOntology
		}
		return nil
	}
	spec := o.ContextSpec(topicPath)
	topic := strings.ToLower(topicPath)
	for _, k := range sortedContextKeys(ctx) {
		d, ok := spec[k]
		if !ok {
			return &ContextError{Topic: topic, Key: k, Msg: "not declared: add it to the `context:` block of this topic or a parent in .knomit/ontology.yaml"}
		}
		if d.problem != "" {
			return &ContextError{Topic: topic, Key: k, Msg: "its ontology declaration is invalid (" + d.problem + ")"}
		}
		if msg := checkContextValue(d, ctx[k]); msg != "" {
			return &ContextError{Topic: topic, Key: k, Msg: msg}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(spec)) {
		d := spec[k]
		if d.Required && d.problem == "" {
			if _, ok := ctx[k]; !ok {
				return &ContextError{Topic: topic, Key: k, Msg: "required by the ontology and missing"}
			}
		}
	}
	return nil
}

// ContextKeyAllowed reports why key=v may NOT be written under topicPath (nil
// when it may): undeclared, a poisoned declaration, or a value its declaration
// refuses. It does not check `required` — it judges one key, for a writer
// that carries keys over rather than authoring a map (review's prune merge).
// A nil ontology allows nothing.
func (o *Ontology) ContextKeyAllowed(topicPath, key string, v any) error {
	if o == nil {
		return ErrContextWithoutOntology
	}
	topic := strings.ToLower(topicPath)
	d, ok := o.ContextSpec(topicPath)[key]
	switch {
	case !ok:
		return &ContextError{Topic: topic, Key: key, Msg: "not declared"}
	case d.problem != "":
		return &ContextError{Topic: topic, Key: key, Msg: "its ontology declaration is invalid (" + d.problem + ")"}
	}
	if msg := checkContextValue(d, v); msg != "" {
		return &ContextError{Topic: topic, Key: key, Msg: msg}
	}
	return nil
}

// checkContextValue checks one value against its declaration ("" when fine).
func checkContextValue(d *compiledContextDecl, v any) string {
	switch d.Type {
	case ContextTypeString, ContextTypeEnum, ContextTypeTime:
		s, ok := v.(string)
		if !ok {
			return fmt.Sprintf("must be a %s, got %s", d.Type, contextTypeName(v))
		}
		switch d.Type {
		case ContextTypeString:
			limit := DefaultContextValueBytes
			if d.MaxLen > 0 {
				limit = d.MaxLen
			}
			if len(s) > limit {
				return fmt.Sprintf("is %d bytes, at most %d", len(s), limit)
			}
			if d.re != nil && !d.re.MatchString(s) {
				return fmt.Sprintf("%q does not match the pattern %q", s, d.Pattern)
			}
		case ContextTypeEnum:
			if !slices.Contains(d.Values, s) {
				return fmt.Sprintf("%q is not one of %s", s, strings.Join(d.Values, ", "))
			}
		case ContextTypeTime:
			if _, err := time.Parse(time.RFC3339, s); err != nil {
				return fmt.Sprintf("%q is not an RFC 3339 timestamp with an offset, e.g. 2026-10-01T00:00:00Z", s)
			}
		}
	case ContextTypeNumber:
		x, ok := v.(float64)
		if !ok {
			return fmt.Sprintf("must be a number, got %s", contextTypeName(v))
		}
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "must be finite"
		}
		if d.Min != nil && x < *d.Min {
			return fmt.Sprintf("%v is below the minimum %v", x, *d.Min)
		}
		if d.Max != nil && x > *d.Max {
			return fmt.Sprintf("%v is above the maximum %v", x, *d.Max)
		}
	case ContextTypeBool:
		if _, ok := v.(bool); !ok {
			return fmt.Sprintf("must be a bool, got %s", contextTypeName(v))
		}
	}
	return ""
}

func contextTypeName(v any) string {
	switch v.(type) {
	case string:
		return "a string"
	case float64:
		return "a number"
	case bool:
		return "a bool"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

// NormalizeContext returns ctx with every time-typed value rewritten as the
// same instant in UTC with an explicit Z at whole seconds (the expires rule:
// all times are UTC). It runs in the typed layer, not in SerializeFact,
// because only the ontology knows a key is a time. A value that does not parse
// is left as it is for ValidateContext to refuse.
func NormalizeContext(o *Ontology, topicPath string, ctx map[string]any) map[string]any {
	ctx = NormalizeContextValues(ctx)
	if o == nil || len(ctx) == 0 {
		return ctx
	}
	spec := o.ContextSpec(topicPath)
	for k, v := range ctx {
		d, ok := spec[k]
		if !ok || d.Type != ContextTypeTime || d.problem != "" {
			continue
		}
		if s, ok := v.(string); ok {
			if n, err := NormalizeExpires(s); err == nil {
				ctx[k] = n
			}
		}
	}
	return ctx
}

// serializeContextDecls writes a node's `context:` block back with sorted keys
// and the fields in a fixed order, so Serialize does not drop declarations
// (initSeed and the custom-create path serialize before committing).
func serializeContextDecls(parent *yaml.Node, decls map[string]ContextDecl) error {
	m := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range slices.Sorted(maps.Keys(decls)) {
		var v yaml.Node
		if err := v.Encode(decls[k]); err != nil {
			return fmt.Errorf("context %q: %w", k, err)
		}
		v.Style = yaml.FlowStyle
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, &v)
	}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "context"}, m)
	return nil
}

// contextDeclsSubset reports whether a's declarations are safe to overwrite
// with b's: a declares none, or exactly what b declares.
func contextDeclsSubset(a, b map[string]ContextDecl) bool {
	if len(a) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for k, da := range a {
		db, ok := b[k]
		if !ok || !equalContextDecl(da, db) {
			return false
		}
	}
	return true
}

func equalContextDecl(a, b ContextDecl) bool {
	eqf := func(x, y *float64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.Type == b.Type && slices.Equal(a.Values, b.Values) && a.Required == b.Required &&
		a.Pattern == b.Pattern && eqf(a.Min, b.Min) && eqf(a.Max, b.Max) && a.MaxLen == b.MaxLen
}
