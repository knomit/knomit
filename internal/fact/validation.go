package fact

import (
	"fmt"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// ruleEvalTimeout bounds a single rule's execution. Rules are pure boolean
// expressions over a small fact map and finish in microseconds; this ceiling
// exists only to stop a pathological rule (e.g. an infinite loop in a
// hand-edited ontology) from hanging the request goroutine, since goja does
// not observe context cancellation on its own.
const ruleEvalTimeout = 100 * time.Millisecond

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
//
// Every field of Fact is exposed, keyed by its json tag, with one deliberate
// exception: RefWarnings. That field is derived on read and never stored, and
// a rule over it could not be trusted in either direction — it is structurally
// always empty on the knomit_learn path (the fact is built in memory, and
// SerializeFact refuses to write a malformed ref anyway), and stale on the
// knomit_update path (ParseFact computes it from the on-disk refs, which the
// handler replaces wholesale before ValidateFact runs).
//
// TestFactToJS_ExposesEveryFactField enforces both halves of that: reflection
// over the struct fails if a newly added field is not exposed here, and the
// omission list it checks against carries the reason above. Add a field to
// Fact, and either expose it here or record why it must stay hidden.
func factToJS(f Fact) map[string]any {
	return map[string]any{
		"kind":            string(f.Kind),
		"type":            string(f.Type),
		"domain":          append([]string{}, f.Domain...),
		"entities":        append([]string{}, f.Entities...),
		"refs":            append([]string{}, f.Refs...),
		"title":           f.Title,
		"body":            f.Body,
		"path":            f.Path(),
		"confidence":      f.Confidence,
		"sources":         f.Sources,
		"origin":          string(resolvedOrigin(f)),
		"evidence_weight": f.EvidenceWeight,
	}
}

// resolvedOrigin is the origin a rule sees: the value that will actually land
// on disk, never the raw field.
//
// The two write paths disagree about whether Origin is set by the time
// ValidateFact runs. knomit_learn deliberately leaves it empty when the caller
// omitted it, so the serialize/parse round trip can apply the default — which
// happens after validation. knomit_update, by contrast, hands ValidateFact a
// fact ParseFact already resolved. Passing the raw field through would
// therefore make one rule disagree between the two: `fact.origin ===
// 'authored'` would reject nearly every learn write while passing the
// equivalent update, and `fact.origin !== 'discovered'` would quietly pass
// facts whose origin is genuinely unset.
//
// Resolving through defaultOriginForType keeps this in lockstep with
// ParseFact and SerializeFact rather than restating their rule — the drift
// between two independent expressions of that default is what once
// round-tripped authored synthesis facts into distilled.
func resolvedOrigin(f Fact) Origin {
	if f.Origin != "" {
		return f.Origin
	}
	return defaultOriginForType(f.Type)
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
	// Guard against a runaway rule: interrupt the VM if it outlives the
	// timeout. Stop() cancels the timer on the normal (fast) path; a late
	// interrupt is harmless because each evaluation uses a fresh VM.
	timer := time.AfterFunc(ruleEvalTimeout, func() {
		vm.Interrupt(fmt.Sprintf("rule %s: exceeded %s", r.Name, ruleEvalTimeout))
	})
	defer timer.Stop()
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
	// Ontology keys are lowercase kebab-case (validateKeys), and the rules
	// cache is keyed by those same lowercase keys. Lowercase each segment so
	// the node lookup AND the cache lookup share one canonical key — otherwise
	// a mixed-case topic path would resolve the node but miss the cache,
	// silently skipping every rule. Mirrors ValidatePath's case-insensitive walk.
	top := strings.ToLower(parts[0])
	node, ok := o.Topics[top]
	if !ok {
		return nil // unknown topic — let ValidatePath handle it
	}
	if err := runRulesCached(o, top, f); err != nil {
		return err
	}
	prefix := top
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
