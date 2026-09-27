package fact

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"gopkg.in/yaml.v3"
)

// Triggers (F07): an ontology node may declare `triggers:`, rules of the form
// "when a fact whose path matches `match` is added, changed or removed under
// this node, and `if` holds, do `do`". This file is the PARSE side only: the
// lenient YAML shape, validation, placeholders, compilation and a cache keyed
// by the ontology blob. Nothing here runs a trigger; the dispatcher (1b) does.
//
// THE RULE THIS FILE EXISTS TO KEEP: a trigger problem is NEVER fatal to the
// ontology. A fatal diagnostic fails ParseOntology, and the repo then opens
// with no ontology and refuses every write (repos/builder.go loadOntology →
// ontologyErr). A missing trigger loses nothing — its watermark freezes in 1b
// and catches up once fixed — while a fatal one would stop writes on every
// machine that pulls the typo. So every problem is a warning, and the trigger
// is recorded as `invalid` with its error text (user ruling D-a, 2026-09-27;
// and "invalid / malformed triggers and recipes should be skipped and at least
// logged as errors" — the logging is 1b's, which knows the repo).

// Trigger actions and episodes. A value this binary does not know drops the
// trigger (it may come from a newer knomit); a known value that this version
// does not act on yet is kept but inactive.
const (
	TriggerDoEmit   = "emit"
	TriggerDoScript = "script"
	TriggerDoPush   = "push"
	TriggerDoRun    = "run"

	TriggerOnLearn   = "learn"
	TriggerOnUpdate  = "update"
	TriggerOnRetract = "retract"
	TriggerOnDue     = "due"
)

// Trigger states, per declared trigger.
const (
	TriggerActive  = "active"
	TriggerInvalid = "invalid"
)

var (
	knownTriggerOn = map[string]bool{TriggerOnLearn: true, TriggerOnUpdate: true, TriggerOnRetract: true, TriggerOnDue: true}
	knownTriggerDo = map[string]bool{TriggerDoEmit: true, TriggerDoScript: true, TriggerDoPush: true, TriggerDoRun: true}
	// activeTriggerDo is what this version acts on. The others parse, so an
	// ontology written for a later PR is not "unknown", but their triggers are
	// invalid here: they must not run, and their watermark freezes until a
	// knomit that implements them arrives.
	activeTriggerDo = map[string]bool{TriggerDoEmit: true}
)

// triggerKeys is every key a trigger entry may carry, with its editor doc.
// OntologySchema serves it; a key missing from here is reported as unknown.
var triggerKeys = []SchemaField{
	{"Trigger", "name", "Unique name of the trigger in this ontology (kebab-case). Its bookmark is keyed by it: renaming starts it fresh."},
	{"Trigger", "match", "Path pattern relative to the ontology root, starting with this topic's path: * one segment, ** any depth, ? one character; {agent}, {host} and {fp8} are this instance's. Absent means everything under the topic. Never a dot path."},
	{"Trigger", "on", "learn, update, retract (and due) — a single value or a list"},
	{"Trigger", "if", "Optional JavaScript condition over fact, agent and change; empty means always"},
	{"Trigger", "do", "The action: emit (log line and SSE event). script, push and run are reserved for later versions."},
	{"Trigger", "script", "For do: script — the name of .knomit/triggers/<name>.js"},
	{"Trigger", "recipe", "For do: run — the name of the recipe to invoke"},
}

// TriggerSpec is one declared trigger as written. Raw holds the entry's keys
// verbatim so Serialize writes back what a newer knomit wrote; the typed
// fields are read from it leniently, and anything wrong is kept in problem.
type TriggerSpec struct {
	Name   string
	Match  string
	On     []string
	If     string
	Do     string
	Script string
	Recipe string

	Raw     map[string]any
	problem string
	line    int
}

// TriggerList is a node's `triggers:`. Its UnmarshalYAML never fails: go-yaml
// decoding straight into typed fields would turn `on: 5` into a FATAL parse
// error for the whole ontology, which is exactly what must not happen. The
// original node is kept for Serialize and for comparison by IsSubsetOf.
type TriggerList struct {
	Specs []TriggerSpec
	node  *yaml.Node
}

// UnmarshalYAML implements yaml.Unmarshaler. It records problems and never
// returns an error.
func (l *TriggerList) UnmarshalYAML(n *yaml.Node) error {
	l.node = n
	l.Specs = nil
	if n.Kind != yaml.SequenceNode {
		l.Specs = append(l.Specs, TriggerSpec{problem: "triggers must be a list of entries", line: n.Line})
		return nil
	}
	for _, item := range n.Content {
		l.Specs = append(l.Specs, decodeTriggerSpec(item))
	}
	return nil
}

// IsZero lets `omitempty` drop an empty list.
func (l TriggerList) IsZero() bool { return l.node == nil && len(l.Specs) == 0 }

func decodeTriggerSpec(item *yaml.Node) TriggerSpec {
	s := TriggerSpec{line: item.Line}
	if item.Kind != yaml.MappingNode {
		s.problem = "a trigger entry must be a mapping with name, on and do"
		return s
	}
	var raw map[string]any
	if err := item.Decode(&raw); err != nil {
		s.problem = "malformed trigger entry: " + err.Error()
		return s
	}
	s.Raw = raw
	// The name first, whatever else is wrong: a trigger that is present but
	// invalid must still be DECLARED, so its bookmark freezes instead of being
	// deleted (and its fires lost) until it is fixed.
	if n, ok := raw["name"].(string); ok {
		s.Name = n
	}
	for _, k := range slices.Sorted(maps.Keys(raw)) {
		if !slices.ContainsFunc(triggerKeys, func(f SchemaField) bool { return f.Field == k }) {
			s.problem = fmt.Sprintf("unknown key %q", k)
			return s
		}
	}
	str := func(key string, dst *string) bool {
		v, ok := raw[key]
		if !ok || v == nil {
			return true
		}
		sv, ok := v.(string)
		if !ok {
			s.problem = fmt.Sprintf("%s must be a string, got %v (%T)", key, v, v)
			return false
		}
		*dst = sv
		return true
	}
	if !str("name", &s.Name) || !str("match", &s.Match) || !str("if", &s.If) ||
		!str("do", &s.Do) || !str("script", &s.Script) || !str("recipe", &s.Recipe) {
		return s
	}
	switch v := raw["on"].(type) {
	case nil:
	case string:
		s.On = []string{v}
	case []any:
		for _, e := range v {
			es, ok := e.(string)
			if !ok {
				s.problem = fmt.Sprintf("on must be a string or a list of strings, got %v (%T)", e, e)
				return s
			}
			s.On = append(s.On, es)
		}
	default:
		s.problem = fmt.Sprintf("on must be a string or a list of strings, got %v (%T)", v, v)
	}
	return s
}

// TriggerIdentity is THIS instance, for the {agent}, {host} and {fp8}
// placeholders. The dispatcher passes the process identity: agent is the
// agent branch minus "agent/", which is also the commit author's name.
type TriggerIdentity struct {
	Agent, Host, FP8 string
}

// placeholderRe finds every {name} in a pattern.
var placeholderRe = regexp.MustCompile(`\{([^{}]*)\}`)

// substitutePlaceholders replaces {agent}, {host} and {fp8}. An unknown name,
// a stray brace, or a value that would read as glob syntax or cross a segment
// is an error.
func substitutePlaceholders(pattern string, id TriggerIdentity) (string, error) {
	var err error
	out := placeholderRe.ReplaceAllStringFunc(pattern, func(m string) string {
		name := m[1 : len(m)-1]
		var v string
		switch name {
		case "agent":
			v = id.Agent
		case "host":
			v = id.Host
		case "fp8":
			v = id.FP8
		default:
			if err == nil {
				err = fmt.Errorf("unknown placeholder %s (known: {agent}, {host}, {fp8})", m)
			}
			return m
		}
		if v == "" || strings.ContainsAny(v, "/*?{}") || strings.HasPrefix(v, ".") {
			if err == nil {
				err = fmt.Errorf("placeholder %s has no usable value on this instance (%q)", m, v)
			}
			return m
		}
		return strings.ToLower(v)
	})
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(out, "{}") {
		return "", fmt.Errorf("unbalanced brace in %q", pattern)
	}
	return out, nil
}

// validationIdentity stands in for the instance while validating at parse
// time, where no identity is known: it proves the placeholders are known and
// the substituted pattern compiles.
var validationIdentity = TriggerIdentity{Agent: "agent-00000000", Host: "host", FP8: "00000000"}

// CompiledTrigger is an active trigger, ready to match and evaluate.
type CompiledTrigger struct {
	Name  string
	Node  string // the declaring node's path, lowercase ("tasks/research")
	Match string // the pattern after substitution
	On    []string
	Do    string
	If    string

	glob   *glob
	ifProg *goja.Program
}

// Matches reports whether a fact path (relative to the ontology root) is under
// this trigger. It allocates nothing.
func (t *CompiledTrigger) Matches(path string) bool { return t.glob.Match(path) }

// OnEpisode reports whether the trigger listens for episode.
func (t *CompiledTrigger) OnEpisode(episode string) bool { return slices.Contains(t.On, episode) }

// TriggerState is the outcome for one declared trigger: active, or invalid
// with the error verbatim. Key is stable per (name, ontology blob) so the
// dispatcher can log an invalid trigger ONCE per blob, not on every advance.
type TriggerState struct {
	Name  string
	Node  string
	State string
	Error string
	Key   string
	Line  int
}

// TriggerSet is every trigger of one ontology, compiled for one instance.
type TriggerSet struct {
	Blob   string
	Active []*CompiledTrigger
	States []TriggerState
	// Declared is every trigger NAME present, valid or not. The dispatcher
	// deletes a bookmark only when its name is absent from here; an invalid
	// trigger keeps (freezes) its bookmark and catches up once fixed.
	Declared map[string]bool
}

// declaredTrigger is one entry found by the walk, with the node that holds it.
type declaredTrigger struct {
	node string
	spec TriggerSpec
}

// collectTriggers walks every node (topics then children, in sorted order).
func collectTriggers(o *Ontology) []declaredTrigger {
	var out []declaredTrigger
	var walk func(prefix string, n *OntologyNode)
	walk = func(prefix string, n *OntologyNode) {
		if n == nil {
			return
		}
		for _, s := range n.Triggers.Specs {
			out = append(out, declaredTrigger{node: prefix, spec: s})
		}
		for _, k := range sortedKeys(n.Children) {
			walk(prefix+"/"+k, n.Children[k])
		}
	}
	if o != nil {
		for _, k := range sortedKeys(o.Topics) {
			walk(k, o.Topics[k])
		}
	}
	return out
}

// CompileTriggers validates and compiles every trigger of o for instance id.
// blob identifies the ontology file (its git blob hash) and goes into each
// state's Key. It never fails: a bad trigger is an `invalid` state.
func CompileTriggers(o *Ontology, id TriggerIdentity, blob string) *TriggerSet {
	set := &TriggerSet{Blob: blob, Declared: map[string]bool{}}
	all := collectTriggers(o)
	// Duplicate names: the SHALLOWER declaration wins, a tie goes to the path
	// that sorts first, so the result does not depend on map order.
	order := make([]int, len(all))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		da, db := strings.Count(all[order[a]].node, "/"), strings.Count(all[order[b]].node, "/")
		if da != db {
			return da < db
		}
		return all[order[a]].node < all[order[b]].node
	})
	winner := map[string]int{}
	for _, i := range order {
		if n := all[i].spec.Name; n != "" {
			if _, seen := winner[n]; !seen {
				winner[n] = i
			}
		}
	}
	for i, d := range all {
		st := TriggerState{Name: d.spec.Name, Node: d.node, Line: d.spec.line}
		if d.spec.Name != "" {
			set.Declared[d.spec.Name] = true
			st.Key = d.spec.Name + "@" + blob
		}
		var ct *CompiledTrigger
		var err error
		if w, ok := winner[d.spec.Name]; ok && w != i {
			err = fmt.Errorf("duplicate trigger name %q: already declared at %s", d.spec.Name, all[w].node)
		} else {
			ct, err = compileTrigger(d.node, d.spec, id)
		}
		if err != nil {
			st.State, st.Error = TriggerInvalid, err.Error()
		} else {
			st.State = TriggerActive
			set.Active = append(set.Active, ct)
		}
		set.States = append(set.States, st)
	}
	return set
}

// compileTrigger validates one entry declared at node and compiles it.
func compileTrigger(node string, s TriggerSpec, id TriggerIdentity) (*CompiledTrigger, error) {
	if s.problem != "" {
		return nil, fmt.Errorf("%s", s.problem)
	}
	if s.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if !validKeyRe.MatchString(s.Name) {
		return nil, fmt.Errorf("name %q must be lowercase kebab-case", s.Name)
	}
	if len(s.On) == 0 {
		return nil, fmt.Errorf("on is required (learn, update, retract or due)")
	}
	for _, e := range s.On {
		if !knownTriggerOn[e] {
			return nil, fmt.Errorf("unknown on value %q (known: learn, update, retract, due)", e)
		}
	}
	if s.Do == "" {
		return nil, fmt.Errorf("do is required")
	}
	if !knownTriggerDo[s.Do] {
		return nil, fmt.Errorf("unknown do value %q (known: emit, script, push, run)", s.Do)
	}
	if s.Do == TriggerDoScript && s.Script == "" {
		return nil, fmt.Errorf("do: script needs a script name")
	}
	if s.Do == TriggerDoRun && s.Recipe == "" {
		return nil, fmt.Errorf("do: run needs a recipe name")
	}
	if !activeTriggerDo[s.Do] {
		return nil, fmt.Errorf("do: %s is not active in this knomit version; the trigger is kept and waits for a version that implements it", s.Do)
	}
	pattern := s.Match
	if pattern == "" {
		pattern = node + "/**"
	}
	sub, err := substitutePlaceholders(pattern, id)
	if err != nil {
		return nil, fmt.Errorf("match: %w", err)
	}
	g, err := compileGlob(sub)
	if err != nil {
		return nil, fmt.Errorf("match: %w", err)
	}
	if !g.literalPrefix(strings.Split(node, "/")) {
		return nil, fmt.Errorf("match %q leaves its topic: it must start with %q", s.Match, node+"/")
	}
	ct := &CompiledTrigger{Name: s.Name, Node: node, Match: sub, On: s.On, Do: s.Do, If: s.If, glob: g}
	if strings.TrimSpace(s.If) != "" {
		prog, err := goja.Compile("trigger "+s.Name, s.If, true)
		if err != nil {
			return nil, fmt.Errorf("if does not compile: %w", err)
		}
		ct.ifProg = prog
	}
	return ct, nil
}

// triggerDiags reports every trigger problem of o as a WARNING (never an
// error: see the rule at the top of this file) so the editor shows it. Parse
// time has no instance identity, so placeholders are checked against a
// stand-in.
func triggerDiags(o *Ontology) []Diagnostic {
	var diags []Diagnostic
	for _, st := range CompileTriggers(o, validationIdentity, "").States {
		if st.State != TriggerInvalid {
			continue
		}
		name := st.Name
		if name == "" {
			name = "(unnamed)"
		}
		diags = append(diags, Diagnostic{
			Line: st.Line, Column: 1, Severity: SeverityWarning,
			Message: fmt.Sprintf("parse ontology: trigger %s in topic %q is skipped: %s", name, st.Node, st.Error),
		})
	}
	return diags
}

// deepFreezeSrc freezes an object graph in place. Globals are handed to `if`
// as plain JavaScript values (JSON) and frozen, so a condition can neither
// mutate what the caller passed nor leak state into the next evaluation.
var deepFreezeProg = goja.MustCompile("deepFreeze",
	`(function f(o) { if (o !== null && typeof o === 'object') { Object.freeze(o); for (const k of Object.keys(o)) f(o[k]); } return o; })`, true)

// EvalIf runs the trigger's condition with the given globals (fact, agent,
// change in the dispatcher). An empty condition is true. It uses a FRESH VM
// per call, the validations' time budget, no process/require, and frozen
// copies of the globals. A throw or timeout is returned as an error, which the
// dispatcher counts as false and records.
func (t *CompiledTrigger) EvalIf(globals map[string]any) (bool, error) {
	if t.ifProg == nil {
		return true, nil
	}
	vm := goja.New()
	_ = vm.GlobalObject().Delete("process")
	_ = vm.GlobalObject().Delete("require")
	timer := time.AfterFunc(ruleEvalTimeout, func() {
		vm.Interrupt(fmt.Sprintf("trigger %s: if exceeded %s", t.Name, ruleEvalTimeout))
	})
	defer timer.Stop()

	freezeV, err := vm.RunProgram(deepFreezeProg)
	if err != nil {
		return false, fmt.Errorf("trigger %s: sandbox: %w", t.Name, err)
	}
	freeze, _ := goja.AssertFunction(freezeV)
	parse, _ := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("parse"))
	for _, k := range slices.Sorted(maps.Keys(globals)) {
		b, err := json.Marshal(globals[k])
		if err != nil {
			return false, fmt.Errorf("trigger %s: global %s: %w", t.Name, k, err)
		}
		v, err := parse(goja.Undefined(), vm.ToValue(string(b)))
		if err != nil {
			return false, fmt.Errorf("trigger %s: global %s: %w", t.Name, k, err)
		}
		if v, err = freeze(goja.Undefined(), v); err != nil {
			return false, fmt.Errorf("trigger %s: global %s: %w", t.Name, k, err)
		}
		if err := vm.GlobalObject().DefineDataProperty(k, v, goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_FALSE); err != nil {
			return false, fmt.Errorf("trigger %s: bind %s: %w", t.Name, k, err)
		}
	}
	v, err := vm.RunProgram(t.ifProg)
	if err != nil {
		return false, fmt.Errorf("trigger %s: if: %w", t.Name, err)
	}
	return v.ToBoolean(), nil
}

// TriggerCache holds the compiled triggers of ONE ontology blob — the blob at
// the observed branch's head. A different blob replaces the entry, so a
// changed trigger (or its `if`) is recompiled before its next evaluation and
// the old program becomes unreachable (user: "if a loaded trigger or recipe is
// changed in the repo, then knomit must reload that trigger / recipe goja
// VM"). The same blob returns the cached set without compiling.
type TriggerCache struct {
	mu       sync.Mutex
	id       TriggerIdentity
	set      *TriggerSet
	compiles int // test hook
}

// Get returns the trigger set for the ontology file with git blob hash blob and
// content data. When data does not parse at all, it returns the LAST GOOD set
// (possibly nil) together with the error, so one broken commit does not switch
// every trigger off.
func (c *TriggerCache) Get(blob string, data []byte, id TriggerIdentity) (*TriggerSet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set != nil && c.set.Blob == blob && c.id == id {
		return c.set, nil
	}
	o, err := ParseOntology(data)
	if err != nil {
		return c.set, fmt.Errorf("triggers: ontology blob %s does not parse; keeping the last good triggers: %w", blob, err)
	}
	c.compiles++
	c.set, c.id = CompileTriggers(o, id, blob), id
	return c.set, nil
}
