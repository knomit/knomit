package fact

import (
	"strings"
	"testing"
	"time"
)

var testIdentity = TriggerIdentity{Agent: "mindev.local-8ef0cd32", Host: "mindev.local", FP8: "8ef0cd32"}

// ontologyWithTriggers wraps topic YAML (indented under `topics:`) into a
// minimal ontology document.
func ontologyWithTriggers(topics string) []byte {
	return []byte("id: t\nname: T\ntopics:\n" + topics)
}

func mustParseTriggerDoc(t *testing.T, data []byte) *Ontology {
	t.Helper()
	o, err := ParseOntology(data)
	if err != nil {
		t.Fatalf("ParseOntology: %v", err)
	}
	return o
}

func stateOf(t *testing.T, set *TriggerSet, name string) TriggerState {
	t.Helper()
	for _, s := range set.States {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no state for trigger %q in %+v", name, set.States)
	return TriggerState{}
}

func activeNamed(set *TriggerSet, name string) *CompiledTrigger {
	for _, a := range set.Active {
		if a.Name == name {
			return a
		}
	}
	return nil
}

func TestTriggers_InboxPlaceholder(t *testing.T) {
	o := mustParseTriggerDoc(t, ontologyWithTriggers(`
  tasks:
    description: t
    triggers:
      - name: my-inbox
        match: "tasks/*/inbox/{agent}/*.md"
        on: [learn, update]
        do: emit
`))
	set := CompileTriggers(o, testIdentity, "blob1")
	tr := activeNamed(set, "my-inbox")
	if tr == nil {
		t.Fatalf("my-inbox not active: %+v", set.States)
	}
	if !tr.Matches("tasks/research/inbox/mindev.local-8ef0cd32/t1.md") {
		t.Error("own inbox did not match")
	}
	if tr.Matches("tasks/research/inbox/other-host-12345678/t1.md") {
		t.Error("another agent's inbox matched")
	}
	if !tr.OnEpisode(TriggerOnUpdate) || tr.OnEpisode(TriggerOnRetract) {
		t.Errorf("episodes wrong: %v", tr.On)
	}
}

func TestTriggers_OneBadTriggerSiblingsStillActive(t *testing.T) {
	data := ontologyWithTriggers(`
  tasks:
    description: t
    triggers:
      - name: good-one
        on: learn
        do: emit
      - name: bad-one
        match: "tasks/{user}/*.md"
        on: learn
        do: emit
  decisions:
    description: d
    triggers:
      - name: good-two
        on: retract
        do: emit
`)
	o, diags := ValidateOntologyYAML(data)
	if o == nil {
		t.Fatalf("ontology with one bad trigger did not load: %+v", diags)
	}
	for _, d := range diags {
		if d.IsError() {
			t.Fatalf("a trigger problem became an ERROR diagnostic (would refuse every write): %+v", d)
		}
	}
	if _, err := ParseOntology(data); err != nil {
		t.Fatalf("ParseOntology refused an ontology with one bad trigger: %v", err)
	}
	set := CompileTriggers(o, testIdentity, "abc123")
	for _, n := range []string{"good-one", "good-two"} {
		if st := stateOf(t, set, n); st.State != TriggerActive {
			t.Errorf("%s: state %q (%s), want active", n, st.State, st.Error)
		}
	}
	bad := stateOf(t, set, "bad-one")
	if bad.State != TriggerInvalid || !strings.Contains(bad.Error, "{user}") {
		t.Errorf("bad-one: %+v, want invalid naming {user}", bad)
	}
	if bad.Key != "bad-one@abc123" {
		t.Errorf("dedup key %q, want bad-one@abc123", bad.Key)
	}
	if !set.Declared["bad-one"] {
		t.Error("an invalid trigger must stay DECLARED so its bookmark freezes instead of being deleted")
	}
	if activeNamed(set, "bad-one") != nil {
		t.Error("an invalid trigger is active")
	}
}

func TestTriggers_MalformedNeverFatal(t *testing.T) {
	cases := map[string]string{
		"not a list":         "    triggers: foo\n",
		"scalar entry":       "    triggers:\n      - 5\n",
		"on is a number":     "    triggers:\n      - {name: a, on: 5, do: emit}\n",
		"unknown on":         "    triggers:\n      - {name: a, on: [learn, bogus], do: emit}\n",
		"unknown do":         "    triggers:\n      - {name: a, on: learn, do: nuke}\n",
		"missing name":       "    triggers:\n      - {on: learn, do: emit}\n",
		"bad name":           "    triggers:\n      - {name: Bad_Name, on: learn, do: emit}\n",
		"if does not parse":  "    triggers:\n      - {name: a, on: learn, do: emit, if: \"((\"}\n",
		"unknown key":        "    triggers:\n      - {name: a, on: learn, do: emit, rate: 5}\n",
		"missing on":         "    triggers:\n      - {name: a, do: emit}\n",
		"name is a list":     "    triggers:\n      - {name: [a], on: learn, do: emit}\n",
		"script needs name":  "    triggers:\n      - {name: a, on: learn, do: script}\n",
		"dot path":           "    triggers:\n      - {name: a, on: learn, do: emit, match: \"tasks/.knomit/**\"}\n",
		"private under node": "    triggers:\n      - {name: a, on: learn, do: emit, match: \"tasks/x/.private/*.md\"}\n",
		"leaves node":        "    triggers:\n      - {name: a, on: learn, do: emit, match: \"other/**\"}\n",
		"wild over node":     "    triggers:\n      - {name: a, on: learn, do: emit, match: \"*/inbox/*.md\"}\n",
		"unbalanced brace":   "    triggers:\n      - {name: a, on: learn, do: emit, match: \"tasks/{agent/*.md\"}\n",
	}
	for label, trig := range cases {
		t.Run(label, func(t *testing.T) {
			data := ontologyWithTriggers("  tasks:\n    description: t\n" + trig)
			o, diags := ValidateOntologyYAML(data)
			if o == nil {
				t.Fatalf("ontology did not load: %+v", diags)
			}
			warned := false
			for _, d := range diags {
				if d.IsError() {
					t.Fatalf("ERROR diagnostic for a trigger problem: %+v", d)
				}
				if strings.Contains(d.Message, "trigger") {
					warned = true
				}
			}
			if !warned {
				t.Errorf("no warning for %s: %+v", label, diags)
			}
			set := CompileTriggers(o, testIdentity, "b")
			if len(set.Active) != 0 {
				t.Errorf("malformed trigger is active: %+v", set.Active)
			}
		})
	}
}

func TestTriggers_AbsentMatchCoversSubtree(t *testing.T) {
	o := mustParseTriggerDoc(t, ontologyWithTriggers(`
  tasks:
    description: t
    children:
      research:
        description: r
        triggers:
          - {name: research-all, on: learn, do: emit}
`))
	tr := activeNamed(CompileTriggers(o, testIdentity, "b"), "research-all")
	if tr == nil {
		t.Fatal("not active")
	}
	if !tr.Matches("tasks/research/a/b.md") || tr.Matches("tasks/other/x.md") || tr.Matches("tasks/x.md") {
		t.Errorf("absent match should be tasks/research/**, got %q", tr.Match)
	}
}

func TestTriggers_DuplicateNameShallowerWins(t *testing.T) {
	o := mustParseTriggerDoc(t, ontologyWithTriggers(`
  tasks:
    description: t
    triggers:
      - {name: dup, on: learn, do: emit}
    children:
      research:
        description: r
        triggers:
          - {name: dup, on: retract, do: emit}
`))
	set := CompileTriggers(o, testIdentity, "b")
	if len(set.Active) != 1 || set.Active[0].Node != "tasks" {
		t.Fatalf("want only the tasks-level dup active, got %+v", set.Active)
	}
	var dropped bool
	for _, s := range set.States {
		if s.Node == "tasks/research" && s.State == TriggerInvalid && strings.Contains(s.Error, "duplicate") {
			dropped = true
		}
	}
	if !dropped {
		t.Errorf("deeper duplicate not dropped: %+v", set.States)
	}
}

// `do: script` is ACTIVE since F07 PR 3: compiled with its script name, and
// the name must be kebab-case because it becomes .knomit/triggers/<name>.js
// (a slash or `..` would name something else). Sabotage: drop script from
// activeTriggerDo → unsupported; drop the name check → `../x` compiles.
func TestTriggers_ScriptIsActive(t *testing.T) {
	o := mustParseTriggerDoc(t, ontologyWithTriggers("  tasks:\n    description: t\n    triggers:\n      - {name: later, on: learn, do: script, script: inbox-dispatch}\n"))
	set := CompileTriggers(o, testIdentity, "b")
	st := stateOf(t, set, "later")
	if st.State != TriggerActive || st.Script != "inbox-dispatch" {
		t.Fatalf("want active with the script name, got %+v", st)
	}
	ct := activeNamed(set, "later")
	if ct == nil || ct.Script != "inbox-dispatch" || ct.Do != TriggerDoScript {
		t.Fatalf("the compiled trigger must carry the script name, got %+v", ct)
	}
	for _, bad := range []string{"../x", "a/b", "Inbox", "in box"} {
		o := mustParseTriggerDoc(t, ontologyWithTriggers("  tasks:\n    description: t\n    triggers:\n      - {name: later, on: learn, do: script, script: \""+bad+"\"}\n"))
		st := stateOf(t, CompileTriggers(o, testIdentity, "b"), "later")
		if st.State != TriggerInvalid || !strings.Contains(st.Error, "kebab-case") {
			t.Errorf("script %q: want invalid (kebab-case), got %+v", bad, st)
		}
	}
	if TriggerScriptPath("inbox-dispatch") != ".knomit/triggers/inbox-dispatch.js" {
		t.Fatalf("TriggerScriptPath: %s", TriggerScriptPath("inbox-dispatch"))
	}
}

// T10 (F07 PR 4): `do: push` compiles ACTIVE, with or without match/if.
// Sabotage: leave push out of activeTriggerDo — it compiles unsupported.
func TestTriggers_PushIsActive(t *testing.T) {
	for _, entry := range []string{
		"{name: fast, on: learn, do: push}",
		"{name: fast, on: [learn, update], do: push, match: \"tasks/**\", if: \"change.source === 'local'\"}",
	} {
		o := mustParseTriggerDoc(t, ontologyWithTriggers("  tasks:\n    description: t\n    triggers:\n      - "+entry+"\n"))
		set := CompileTriggers(o, testIdentity, "b")
		st := stateOf(t, set, "fast")
		if st.State != TriggerActive || st.Error != "" {
			t.Errorf("%s: want active, got %+v", entry, st)
		}
		if ct := activeNamed(set, "fast"); ct == nil || ct.Do != TriggerDoPush {
			t.Errorf("%s: want an active push trigger, got %+v", entry, ct)
		}
	}
}

// A reserved `do` (run, since F07 PR 4 activated push) is valid syntax this
// version does not act on: the trigger is `unsupported`, not `invalid` —
// declared (so a later version finds it), not active, and NOT a problem (no
// warning diagnostic).
func TestTriggers_ReservedDoIsUnsupported(t *testing.T) {
	for _, entry := range []string{
		"{name: later, on: learn, do: run, recipe: worker}",
	} {
		o := mustParseTriggerDoc(t, ontologyWithTriggers("  tasks:\n    description: t\n    triggers:\n      - "+entry+"\n"))
		set := CompileTriggers(o, testIdentity, "b")
		st := stateOf(t, set, "later")
		if st.State != TriggerUnsupported || !strings.Contains(st.Error, "not supported in this knomit version") {
			t.Errorf("%s: want unsupported, got %+v", entry, st)
		}
		if activeNamed(set, "later") != nil {
			t.Errorf("%s: an unsupported trigger must not be active", entry)
		}
		if !set.Declared["later"] {
			t.Errorf("%s: an unsupported trigger is still declared", entry)
		}
		for _, d := range triggerDiags(o) {
			if strings.Contains(d.Message, "later") {
				t.Errorf("%s: unsupported is not a problem, got diagnostic %q", entry, d.Message)
			}
		}
	}
}

// `on: due` is active since F07 PR 2 (the due sweep): alone or alongside the
// tree episodes, the trigger compiles and OnEpisode("due") is true for it and
// false for a trigger that does not list it. Sabotage: drop TriggerOnDue from
// activeTriggerOn.
func TestTriggers_DueIsActive(t *testing.T) {
	o := mustParseTriggerDoc(t, ontologyWithTriggers(`
  tasks:
    description: t
    triggers:
      - {name: expiring, on: [learn, due], do: emit}
      - {name: only-due, on: due, do: emit}
      - {name: plain, on: learn, do: emit}
`))
	set := CompileTriggers(o, testIdentity, "b")
	for _, n := range []string{"expiring", "only-due", "plain"} {
		st := stateOf(t, set, n)
		if st.State != TriggerActive || st.Error != "" {
			t.Errorf("%s: want active, got %+v", n, st)
		}
		ct := activeNamed(set, n)
		if ct == nil {
			t.Fatalf("%s: a due trigger compiles", n)
		}
		if got, want := ct.OnEpisode(TriggerOnDue), n != "plain"; got != want {
			t.Errorf("%s: OnEpisode(due) = %v, want %v", n, got, want)
		}
	}
	if activeNamed(set, "only-due").OnEpisode(TriggerOnLearn) {
		t.Errorf("only-due must not listen to learn")
	}
}

// Invalidity is decided before support: a reserved `do` with a broken match is
// `invalid`, so the author still learns about the typo.
func TestTriggers_InvalidBeatsUnsupported(t *testing.T) {
	o := mustParseTriggerDoc(t, ontologyWithTriggers(`
  tasks:
    description: t
    triggers:
      - {name: broken, on: learn, do: script, script: s, match: "other/**"}
`))
	st := stateOf(t, CompileTriggers(o, testIdentity, "b"), "broken")
	if st.State != TriggerInvalid || !strings.Contains(st.Error, "leaves its topic") {
		t.Errorf("want invalid (match leaves its topic), got %+v", st)
	}
}

// An uppercase grandchild key (deeper keys are not validated) used to yield an
// uppercase node path, which the lowercased glob could never start with: every
// trigger under such a node was permanently invalid. The node path is now
// lowercased in the walk.
func TestTriggers_UppercaseGrandchildNodeIsActive(t *testing.T) {
	o := mustParseTriggerDoc(t, ontologyWithTriggers(`
  tasks:
    description: t
    children:
      research:
        description: r
        children:
          Deep:
            description: d
            triggers:
              - {name: deep, on: learn, do: emit}
`))
	set := CompileTriggers(o, testIdentity, "b")
	tr := activeNamed(set, "deep")
	if tr == nil {
		t.Fatalf("trigger under an uppercase grandchild must be active: %+v", set.States)
	}
	if tr.Node != "tasks/research/deep" {
		t.Errorf("node path must be lowercase, got %q", tr.Node)
	}
	if !tr.Matches("tasks/research/deep/x.md") || tr.Matches("tasks/research/other/x.md") {
		t.Errorf("implicit prefix must cover exactly the node's subtree")
	}
}

func TestTriggers_EvalIfFrozenGlobals(t *testing.T) {
	compile := func(cond string) *CompiledTrigger {
		t.Helper()
		o := mustParseTriggerDoc(t, ontologyWithTriggers("  tasks:\n    description: t\n    triggers:\n      - name: c\n        on: learn\n        do: emit\n        if: \""+cond+"\"\n"))
		tr := activeNamed(CompileTriggers(o, testIdentity, "b"), "c")
		if tr == nil {
			t.Fatalf("condition %q did not compile", cond)
		}
		return tr
	}
	globals := map[string]any{
		"fact":   map[string]any{"type": "signal", "path": "tasks/a.md"},
		"agent":  map[string]any{"id": "mindev.local-8ef0cd32"},
		"change": map[string]any{"episode": "learn", "author": map[string]any{"kind": "agent"}},
	}
	if ok, err := compile("fact.type === 'signal' && change.author.kind === 'agent' && agent.id.startsWith('mindev')").EvalIf(globals, time.Now()); err != nil || !ok {
		t.Fatalf("condition over fact/agent/change: %v %v", ok, err)
	}
	if _, err := compile("fact.type = 'x'; true").EvalIf(globals, time.Now()); err == nil {
		t.Error("assigning into a frozen global did not throw")
	}
	if globals["fact"].(map[string]any)["type"] != "signal" {
		t.Error("the condition mutated the caller's map")
	}
	if ok, err := compile("typeof require === 'undefined' && typeof process === 'undefined'").EvalIf(globals, time.Now()); err != nil || !ok {
		t.Errorf("require/process reachable: %v %v", ok, err)
	}
	// Bounded here, so a missing interrupt fails THIS test by name instead of
	// hanging the package until the suite's timeout.
	busy := compile("while (true) {}")
	done := make(chan error, 1)
	go func() { _, err := busy.EvalIf(globals, time.Now()); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "exceeded") {
			t.Errorf("a busy loop was not interrupted at the budget: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a busy loop ran 5s: the condition's time budget is not enforced")
	}
	empty := activeNamed(CompileTriggers(mustParseTriggerDoc(t, ontologyWithTriggers("  tasks:\n    description: t\n    triggers:\n      - {name: e, on: learn, do: emit}\n")), testIdentity, "b"), "e")
	if ok, err := empty.EvalIf(nil, time.Now()); err != nil || !ok {
		t.Errorf("an empty condition must be true: %v %v", ok, err)
	}
}

func triggerDoc(cond string) []byte {
	return ontologyWithTriggers("  tasks:\n    description: t\n    triggers:\n      - name: c\n        on: learn\n        do: emit\n        if: \"" + cond + "\"\n")
}

func TestTriggerCache_ChangedIfRunsNewProgram(t *testing.T) {
	var c TriggerCache
	signal := map[string]any{"fact": map[string]any{"type": "signal"}}
	task := map[string]any{"fact": map[string]any{"type": "task"}}

	setA, err := c.Get("blobA", triggerDoc("fact.type === 'signal'"), testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := activeNamed(setA, "c").EvalIf(signal, time.Now()); !ok {
		t.Fatal("blob A: signal should match")
	}
	setB, err := c.Get("blobB", triggerDoc("fact.type === 'task'"), testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if setB == setA {
		t.Fatal("a changed blob returned the old set")
	}
	trB := activeNamed(setB, "c")
	if ok, _ := trB.EvalIf(signal, time.Now()); ok {
		t.Error("blob B still runs the OLD program (signal matched)")
	}
	if ok, _ := trB.EvalIf(task, time.Now()); !ok {
		t.Error("blob B does not run the new program (task did not match)")
	}
	// Single entry: going back to A recompiles; A's old set is not kept.
	before := c.compiles
	setA2, _ := c.Get("blobA", triggerDoc("fact.type === 'signal'"), testIdentity)
	if c.compiles != before+1 || setA2 == setA {
		t.Errorf("returning to blob A should recompile (compiles %d→%d, same set %v)", before, c.compiles, setA2 == setA)
	}
}

func TestTriggerCache_SameBlobNoRecompile(t *testing.T) {
	var c TriggerCache
	s1, _ := c.Get("blobA", triggerDoc("true"), testIdentity)
	s2, _ := c.Get("blobA", triggerDoc("true"), testIdentity)
	if c.compiles != 1 || s1 != s2 {
		t.Errorf("same blob compiled %d times (want 1), same set %v", c.compiles, s1 == s2)
	}
}

func TestTriggerCache_BrokenDocumentKeepsLastGood(t *testing.T) {
	var c TriggerCache
	good, err := c.Get("blobA", triggerDoc("true"), testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Get("blobBroken", []byte("id: t\nname: T\ntopics: [unclosed\n"), testIdentity)
	if err == nil {
		t.Fatal("an unparseable ontology returned no error")
	}
	if got != good {
		t.Error("an unparseable ontology did not keep the last good triggers")
	}
}

func TestTriggerCache_InvalidReplacementFreezes(t *testing.T) {
	var c TriggerCache
	if _, err := c.Get("blobA", triggerDoc("true"), testIdentity); err != nil {
		t.Fatal(err)
	}
	set, err := c.Get("blobD", triggerDoc("(("), testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if activeNamed(set, "c") != nil {
		t.Error("the broken replacement fell back to the old compiled program")
	}
	if st := stateOf(t, set, "c"); st.State != TriggerInvalid || !set.Declared["c"] {
		t.Errorf("want c invalid and still declared (frozen), got %+v declared=%v", st, set.Declared["c"])
	}
}

func TestTriggers_SerializeRoundTrip(t *testing.T) {
	data := ontologyWithTriggers(`
  tasks:
    description: t
    triggers:
      - name: my-inbox
        match: "tasks/*/inbox/{agent}/*.md"
        on: [learn, update]
        if: "fact.type === 'signal'"
        do: emit
      - {name: newer, on: learn, do: emit, rate: 5}
      - 7
`)
	o := mustParseTriggerDoc(t, data)
	y1, err := o.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	o2 := mustParseTriggerDoc(t, y1)
	y2, err := o2.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if string(y1) != string(y2) {
		t.Errorf("serialize is not byte-stable:\n%s\n---\n%s", y1, y2)
	}
	specs := o2.Topics["tasks"].Triggers.Specs
	if len(specs) != 3 {
		t.Fatalf("round trip kept %d trigger entries, want 3 (the malformed one included)", len(specs))
	}
	if specs[1].Raw["rate"] != 5 || specs[0].Match != "tasks/*/inbox/{agent}/*.md" || specs[0].If != "fact.type === 'signal'" {
		t.Errorf("round trip lost content: %+v", specs)
	}
}

func TestTriggers_AreDivergenceFromPreset(t *testing.T) {
	preset := DefaultOntology()
	base, err := preset.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	// Decorate the preset's first topic with a trigger taken from a parsed
	// document (TriggerList keeps its YAML node, so it is built by parsing).
	donor := mustParseTriggerDoc(t, ontologyWithTriggers("  x:\n    description: x\n    triggers:\n      - {name: d, on: learn, do: emit}\n"))
	o := mustParseTriggerDoc(t, base)
	topic := o.TopicNames()[0]
	if o.Topics[topic] == nil {
		o.Topics[topic] = &OntologyNode{}
	}
	o.Topics[topic].Triggers = donor.Topics["x"].Triggers

	if got := o.SubsetDivergence(preset); got != DivergenceTriggers {
		t.Errorf("a preset topic with a trigger: divergence %q, want %q", got, DivergenceTriggers)
	}
	if o.IsSubsetOf(preset) {
		t.Error("IsSubsetOf is true: the boot refresh would overwrite the file and erase the trigger")
	}
	o.Topics[topic].Attributes = map[string]any{AttrLearnDedup: "off"}
	if got := o.SubsetDivergence(preset); got != DivergenceAttributes {
		t.Errorf("trigger plus attribute: divergence %q, want %q", got, DivergenceAttributes)
	}
	o.Topics["zz-custom"] = &OntologyNode{Description: "c"}
	if got := o.SubsetDivergence(preset); got != DivergenceShape {
		t.Errorf("plus a custom topic: divergence %q, want %q", got, DivergenceShape)
	}
	if d := mustParseTriggerDoc(t, base).SubsetDivergence(preset); d != "" {
		t.Errorf("the undecorated preset diverges from itself: %q", d)
	}
}

func TestEmbeddedPresetsCarryNoTriggers(t *testing.T) {
	// A preset WITH triggers would be written over subset repos by the boot
	// refresh, silently adding fleet-wide behaviour in an own-signed commit.
	// Before a preset ships one, repos/builder.go refreshDivergence must refuse
	// to ADD triggers, as it refuses to add root attributes.
	for _, p := range []*Ontology{DefaultOntology(), CodeOntology()} {
		if n := len(collectTriggers(p)); n != 0 {
			t.Errorf("preset %s carries %d triggers", p.ID, n)
		}
	}
}
