package fact

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Context (F22) is an optional per-fact map of short, typed labels: the
// properties a mission needs to find a fact by that are not its subject — the
// task a verdict belongs to, the verdict itself, a score. It is NOT entities
// (what a fact is about) and NOT #349's trace (what a commit was made for).
//
// Two gates, as for motifs and expires:
//
//  1. SHAPE, here, run by SerializeFact (the one write gate) and by ParseFact
//     (which DROPS a malformed map into ContextWarnings rather than failing):
//     key names, scalar values only, one line, no control characters, a hard
//     cap of MaxContextValueBytes, at most MaxContextKeys keys.
//  2. TYPES, against the ontology's `context:` declarations
//     (ValidateContext, run by ValidateFact and by REST PUT): declared keys,
//     types, required, pattern, ranges, the per-key length bound.
//
// Values in memory are string, float64 or bool, nothing else. A value is data,
// never an instruction: knomit never renders one into prompt instruction text.

// Bounds (user ruling 2026-10-03: "Make sure the length is bound as well, no
// newlines, etc. We cannot have a key storing like multiline paragraphs.").
const (
	MaxContextKeys = 16
	// MaxContextKeyLen is the longest key name, in bytes (keys are ASCII).
	MaxContextKeyLen = 32
	// DefaultContextValueBytes is the string length bound when a declaration
	// sets no max_len.
	DefaultContextValueBytes = 128
	// MaxContextValueBytes is the hard cap: no declaration may raise a string
	// value's bound above it, and the shape gate refuses anything longer
	// whatever the ontology says.
	MaxContextValueBytes = 256
)

// ErrInvalidContext is wrapped by every shape-gate refusal.
var ErrInvalidContext = errors.New("invalid context")

// contextKeyRe is the key grammar: snake case, so `fact.context.task_id` works
// in JavaScript without brackets.
var contextKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ValidContextKey reports whether k is a legal context key name.
func ValidContextKey(k string) bool {
	return len(k) <= MaxContextKeyLen && contextKeyRe.MatchString(k)
}

// ValidateContextShape is the shape gate. It names the offending key, and
// checks keys in sorted order so the error is deterministic.
func ValidateContextShape(ctx map[string]any) error {
	if len(ctx) > MaxContextKeys {
		return fmt.Errorf("%w: %d keys, at most %d", ErrInvalidContext, len(ctx), MaxContextKeys)
	}
	for _, k := range sortedContextKeys(ctx) {
		if !ValidContextKey(k) {
			return fmt.Errorf("%w: key %q: must match [a-z][a-z0-9_]* and be at most %d characters", ErrInvalidContext, k, MaxContextKeyLen)
		}
		if err := contextValueShape(ctx[k]); err != nil {
			return fmt.Errorf("%w: key %q: %v", ErrInvalidContext, k, err)
		}
	}
	return nil
}

// contextValueShape checks one value: a scalar of an allowed Go type and, for
// a string, one line of valid UTF-8 within the hard cap.
func contextValueShape(v any) error {
	switch x := v.(type) {
	case string:
		return contextStringShape(x)
	case bool:
		return nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return errors.New("a number must be finite")
		}
		return nil
	case nil:
		return errors.New("a value is required: null is not a context value")
	case []any, map[string]any:
		return errors.New("a value must be a string, a number or a boolean: lists and objects are not allowed")
	default:
		return fmt.Errorf("a value must be a string, a number or a boolean, got %T", v)
	}
}

// contextStringShape is the "one line, a label not a paragraph" rule.
//
// unicode.IsControl covers C0 (newline, carriage return, tab, …), DEL and C1
// (NEL among them). U+2028 LINE SEPARATOR and U+2029 PARAGRAPH SEPARATOR are
// not control characters to Go, but they are line breaks, so they are refused
// too: the ruling is one line.
func contextStringShape(s string) error {
	if !utf8.ValidString(s) {
		return errors.New("a string must be valid UTF-8")
	}
	if len(s) > MaxContextValueBytes {
		return fmt.Errorf("a string is %d bytes, at most %d", len(s), MaxContextValueBytes)
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return fmt.Errorf("a string must be one line with no control characters (found %U)", r)
		}
	}
	return nil
}

// NormalizeContextValues returns a copy of ctx with Go integer types widened
// to float64, so callers building a map in Go (tests, scripts' exported
// values) hand the gate the one numeric type it knows. nil and empty both
// return nil: absent and empty are one thing, as for motifs.
func NormalizeContextValues(ctx map[string]any) map[string]any {
	if len(ctx) == 0 {
		return nil
	}
	out := make(map[string]any, len(ctx))
	for k, v := range ctx {
		switch x := v.(type) {
		case int:
			out[k] = float64(x)
		case int32:
			out[k] = float64(x)
		case int64:
			out[k] = float64(x)
		case float32:
			out[k] = float64(x)
		default:
			out[k] = v
		}
	}
	return out
}

// CopyContext returns an independent copy (values are immutable scalars).
func CopyContext(ctx map[string]any) map[string]any {
	if len(ctx) == 0 {
		return nil
	}
	out := make(map[string]any, len(ctx))
	for k, v := range ctx {
		out[k] = v
	}
	return out
}

// EqualContext compares two maps by canonical text and type; nil and empty
// are equal.
func EqualContext(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !equalContextValue(av, bv) {
			return false
		}
	}
	return true
}

func equalContextValue(a, b any) bool {
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	}
	return false
}

// ContextText is a value's canonical text: the string itself, "true"/"false",
// or a number in its shortest form. It is what the index stores and what a
// query compares against, and how a number is written to the file.
func ContextText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	return fmt.Sprint(v)
}

// ContextNum returns the value as a number for the index's num column; ok is
// false for anything that is not a number.
func ContextNum(v any) (float64, bool) {
	x, ok := v.(float64)
	return x, ok
}

// sortedContextKeys returns ctx's keys in order.
func sortedContextKeys(ctx map[string]any) []string {
	keys := make([]string, 0, len(ctx))
	for k := range ctx {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// contextNode renders ctx as a one-line flow map with sorted keys. Strings
// carry the !!str tag, so the encoder quotes any string that would read back
// as another type ("true", "17", a timestamp); numbers and booleans are plain
// scalars in canonical text.
func contextNode(ctx map[string]any) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
	for _, k := range sortedContextKeys(ctx) {
		v := ctx[k]
		val := &yaml.Node{Kind: yaml.ScalarNode, Value: ContextText(v)}
		if _, ok := v.(string); ok {
			val.Tag = "!!str"
		}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, val)
	}
	return m
}

// decodeContext reads the frontmatter's `context:` node. Lenient: absent, null
// and an empty map are "no context" with no warning; anything malformed drops
// the WHOLE map and returns one warning, so the fact still loads (a version
// that was legal when committed must stay readable, and a half-kept map would
// describe something nobody wrote).
//
// The node is read by TAG, never decoded into Go types by yaml.v3: an unquoted
// `2026-10-01` would otherwise become a time.Time. Such a timestamp is kept as
// its text. A duplicate key is detected here — yaml.v3 checks duplicates when
// it decodes into a Go map, not inside a yaml.Node, so the last value would
// otherwise silently win.
func decodeContext(n *yaml.Node) (map[string]any, []string) {
	if n == nil || n.Kind == 0 {
		return nil, nil
	}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return nil, nil
	}
	bad := func(format string, a ...any) (map[string]any, []string) {
		return nil, []string{fmt.Sprintf("context dropped: "+format, a...)}
	}
	if n.Kind != yaml.MappingNode {
		return bad("must be a map of key: value")
	}
	out := make(map[string]any, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		kn, vn := n.Content[i], n.Content[i+1]
		if kn.Kind != yaml.ScalarNode || (kn.Tag != "!!str" && kn.Tag != "") {
			return bad("a key must be a plain name")
		}
		k := kn.Value
		if _, dup := out[k]; dup {
			return bad("key %q appears twice", k)
		}
		v, err := contextScalar(vn)
		if err != nil {
			return bad("key %q: %v", k, err)
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, nil
	}
	if err := ValidateContextShape(out); err != nil {
		return bad("%v", err)
	}
	return out, nil
}

// contextScalar reads one value node by its resolved tag.
func contextScalar(n *yaml.Node) (any, error) {
	if n.Kind == yaml.AliasNode {
		return nil, errors.New("aliases are not allowed")
	}
	if n.Kind != yaml.ScalarNode {
		return nil, errors.New("a value must be a string, a number or a boolean: lists and objects are not allowed")
	}
	switch n.Tag {
	case "!!null":
		return nil, errors.New("null is not a context value")
	case "!!bool":
		b, err := strconv.ParseBool(strings.ToLower(n.Value))
		if err != nil {
			return nil, fmt.Errorf("bad boolean %q", n.Value)
		}
		return b, nil
	case "!!int", "!!float":
		var f float64
		if err := n.Decode(&f); err != nil {
			return nil, fmt.Errorf("bad number %q", n.Value)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, errors.New("a number must be finite")
		}
		return f, nil
	default:
		// !!str, !!timestamp (kept as its TEXT — see decodeContext), and any
		// other tag: the text as written.
		return n.Value, nil
	}
}

// ExtractContext reads only the context map from a raw fact file, as ParseFact
// would (lenient: a malformed map yields nil). Read paths that already hold the
// blob use it to put a fact's context on a result without a full parse. Cheap
// for the common case: a blob without the token "context" is not parsed.
func ExtractContext(raw []byte) map[string]any {
	if !strings.Contains(string(raw), "context") {
		return nil
	}
	content := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return nil
	}
	rest := content[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return nil
	}
	var fm struct {
		Context yaml.Node `yaml:"context"`
	}
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return nil
	}
	ctx, _ := decodeContext(&fm.Context)
	return ctx
}
