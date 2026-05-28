package fact

import (
	"fmt"
	"strings"

	"github.com/dop251/goja"
)

// compiledRule is a Validation with its rule precompiled to a goja.Program.
type compiledRule struct {
	Name    string
	Message string
	Program *goja.Program
}

// compileRules compiles every rule under a topic path. The topic argument is
// used only in error messages so an ontology load failure points at the
// right place.
func compileRules(topic string, rules []Validation) ([]compiledRule, error) {
	out := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		prog, err := goja.Compile(fmt.Sprintf("%s/%s", topic, r.Name), r.Rule, true)
		if err != nil {
			return nil, fmt.Errorf("compile rule %q at %s: %w", r.Name, topic, err)
		}
		out = append(out, compiledRule{Name: r.Name, Message: r.Message, Program: prog})
	}
	return out, nil
}

// factToJS converts a Fact into a plain map suitable for read-only JS access.
// Only fields a rule might branch on are exposed.
func factToJS(f Fact) map[string]any {
	return map[string]any{
		"kind":       string(f.Kind),
		"type":       string(f.Type),
		"domain":     append([]string{}, f.Domain...),
		"entities":   append([]string{}, f.Entities...),
		"refs":       append([]string{}, f.Refs...),
		"title":      f.Title,
		"body":       f.Body,
		"path":       f.Path(),
		"confidence": f.Confidence,
	}
}

// evaluateRule runs one compiled rule against a fact. Returns (pass, err).
// A fresh goja Runtime is used per evaluation: the cost is small (μs per
// rule) and it guarantees no state leaks between evaluations.
func evaluateRule(r compiledRule, f Fact) (bool, error) {
	vm := goja.New()
	// Sandbox: remove Node-flavored hooks if any are accidentally present.
	// Goja exposes a JS standard library (Array, String, JSON, etc.) which
	// is fine — those are pure.
	_ = vm.GlobalObject().Delete("process")
	_ = vm.GlobalObject().Delete("require")

	// Bind `fact` as a non-enumerable, non-writable, non-configurable global
	// so the rule sees it but Object.keys(globalThis) stays empty. This keeps
	// the sandbox observably minimal: rules can't enumerate what's available.
	factVal := vm.ToValue(factToJS(f))
	if err := vm.GlobalObject().DefineDataProperty("fact", factVal, goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_FALSE); err != nil {
		return false, fmt.Errorf("rule %s: bind fact: %w", r.Name, err)
	}
	v, err := vm.RunProgram(r.Program)
	if err != nil {
		return false, fmt.Errorf("rule %s: eval: %w", r.Name, err)
	}
	return v.ToBoolean(), nil
}

// ValidationError is returned when a fact fails an ontology validation rule.
// It carries the rule's name and human-readable message.
type ValidationError struct {
	RuleName string
	Message  string
	Topic    string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation %q at %s: %s", e.RuleName, e.Topic, e.Message)
}

// ValidateFact walks the ontology from root → leaf for the given topic path
// and evaluates every Validation rule encountered. Returns the first failing
// rule as a *ValidationError. Compilation errors return a plain error.
func ValidateFact(o *Ontology, topicPath string, f Fact) error {
	if o == nil {
		return nil
	}
	// Root-level rules first.
	if err := runRulesCached(o, "<root>", f); err != nil {
		return err
	}
	parts := strings.Split(topicPath, "/")
	if len(parts) == 0 {
		return nil
	}
	node, ok := o.Topics[strings.ToLower(parts[0])]
	if !ok {
		return nil // unknown topic — let ValidatePath handle it
	}
	if err := runRulesCached(o, parts[0], f); err != nil {
		return err
	}
	prefix := parts[0]
	for _, seg := range parts[1:] {
		if node == nil || node.Children == nil {
			break
		}
		child, ok := node.Children[strings.ToLower(seg)]
		if !ok {
			break
		}
		prefix = prefix + "/" + seg
		if err := runRulesCached(o, prefix, f); err != nil {
			return err
		}
		node = child
	}
	return nil
}

// runRulesCached looks up precompiled rules for `topic` in the ontology
// cache and evaluates them. Misses are no-ops.
func runRulesCached(o *Ontology, topic string, f Fact) error {
	for _, r := range o.cache.byTopic[topic] {
		ok, err := evaluateRule(r, f)
		if err != nil {
			return err
		}
		if !ok {
			return &ValidationError{RuleName: r.Name, Message: r.Message, Topic: topic}
		}
	}
	return nil
}
