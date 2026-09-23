package fact

import (
	"errors"
	"strings"
	"testing"
)

// attrOntologyYAML declares the attribute on a parent, overrides it on one
// child, and leaves a second child to inherit.
const attrOntologyYAML = `id: x
name: X
topics:
  tasks:
    description: d
    attributes:
      learn_dedup: off
    children:
      research:
        description: d
      reviewed:
        description: d
        attributes:
          learn_dedup: on
  notes:
    description: d
`

func mustParse(t *testing.T, src string) *Ontology {
	t.Helper()
	o, err := ParseOntology([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return o
}

func TestOntologyAttr_ResolvesLikeTheValidationWalk(t *testing.T) {
	o := mustParse(t, attrOntologyYAML)
	cases := []struct {
		path string
		want any
		ok   bool
	}{
		{"tasks", "off", true},
		{"tasks/research", "off", true},         // declared child inherits
		{"tasks/research/deep/er", "off", true}, // undeclared grandchild inherits
		{"tasks/freeform", "off", true},         // undeclared child inherits
		{"TASKS/Research", "off", true},         // case-insensitive, like ValidateFact
		{"tasks/reviewed", "on", true},          // child override wins
		{"tasks/reviewed/anything", "on", true}, // and is inherited below it
		{"notes", nil, false},                   // no attributes anywhere
		{"notes/sub", nil, false},
		{"unknown-topic", nil, false},
		{"", nil, false},
	}
	for _, c := range cases {
		got, ok := o.Attr(c.path, "learn_dedup")
		if ok != c.ok || got != c.want {
			t.Errorf("Attr(%q) = (%v, %v), want (%v, %v)", c.path, got, ok, c.want, c.ok)
		}
	}
	if got, ok := o.Attr("tasks", "no-such-key"); ok || got != nil {
		t.Errorf("Attr for an undeclared key = (%v, %v), want (nil, false)", got, ok)
	}
}

func TestOntologyLearnDedupOff(t *testing.T) {
	o := mustParse(t, attrOntologyYAML)
	for path, want := range map[string]bool{
		"tasks":            true,
		"tasks/research":   true,
		"tasks/x/y":        true,
		"tasks/reviewed":   false, // "on" behaves as absent
		"notes":            false,
		"notes/whatever":   false,
		"not-a-topic/here": false,
	} {
		if got := o.LearnDedupOff(path); got != want {
			t.Errorf("LearnDedupOff(%q) = %v, want %v", path, got, want)
		}
	}
	var nilOnt *Ontology
	if nilOnt.LearnDedupOff("tasks") {
		t.Error("a nil ontology must report learn_dedup on (false)")
	}
}

// The flag parses ONLY because go-yaml v3 decodes the bare scalar `off` into
// `any` as the STRING "off" (YAML 1.2 core schema; only true/false are bools).
// If a yaml upgrade ever turned it into a bool, the flag would stop parsing —
// this test is what notices.
func TestOntologyAttr_BareOffDecodesAsString(t *testing.T) {
	o := mustParse(t, attrOntologyYAML)
	raw := o.Topics["tasks"].Attributes["learn_dedup"]
	if s, ok := raw.(string); !ok || s != "off" {
		t.Fatalf("learn_dedup: off decoded as %T(%v), want string \"off\"", raw, raw)
	}
}

func TestOntologyAttr_BadValueForKnownKeyIsFatal(t *testing.T) {
	for _, val := range []string{"false", "true", "maybe", "0", `"OFF"`} {
		t.Run(val, func(t *testing.T) {
			src := "id: x\nname: X\ntopics:\n  tasks:\n    description: d\n    children:\n      inbox:\n        description: d\n        attributes:\n          learn_dedup: " + val + "\n"
			o, diags := ValidateOntologyYAML([]byte(src))
			if o != nil {
				t.Fatal("a bad value for a known key must withhold the ontology")
			}
			if !hasError(diags) {
				t.Fatalf("want a fatal diagnostic, got %+v", diags)
			}
			msg := diags[0].Message
			for _, want := range []string{"tasks/inbox", "learn_dedup", `"off"`, `"on"`} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not name %s", msg, want)
				}
			}
			if diags[0].Line != 10 {
				t.Errorf("diagnostic line = %d, want 10 (the learn_dedup key)", diags[0].Line)
			}
			if _, err := ParseOntology([]byte(src)); err == nil {
				t.Error("ParseOntology must fail on a bad value for a known key")
			}
		})
	}
}

// Mirrors TestValidateOntologyYAML_ReportsUnknownFields: an attribute key this
// binary does not know may have been written by a NEWER knomit, so it is a
// warning and the ontology is still returned (see ParseOntology's comment).
func TestOntologyAttr_UnknownKeyIsAWarning(t *testing.T) {
	src := "id: x\nname: X\ntopics:\n  tasks:\n    description: d\n    children:\n      inbox:\n        description: d\n        attributes:\n          triggers: whatever\n"
	o, diags := ValidateOntologyYAML([]byte(src))
	if o == nil {
		t.Fatalf("an unknown attribute key must not withhold the ontology: %+v", diags)
	}
	if len(diags) != 1 {
		t.Fatalf("want exactly one diagnostic, got %+v", diags)
	}
	d := diags[0]
	if d.IsError() {
		t.Fatalf("an unknown attribute key must be a warning: %+v", d)
	}
	for _, want := range []string{"triggers", "tasks/inbox"} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("message %q does not name %s", d.Message, want)
		}
	}
	if d.Line != 10 {
		t.Errorf("diagnostic line = %d, want 10 (the triggers key)", d.Line)
	}
	if _, err := ParseOntology([]byte(src)); err != nil {
		t.Errorf("ParseOntology must accept an unknown attribute key: %v", err)
	}
	// The unknown key resolves like any other stored value would — but no
	// typed accessor reads it.
	if o.LearnDedupOff("tasks/inbox") {
		t.Error("an unknown key must not turn anything off")
	}
}

// Attribute checks walk the FULL tree; the kebab-case key check (which only
// reaches one level of children) must not be regressed by it.
func TestOntologyAttr_ValidatedAtEveryDepth(t *testing.T) {
	src := "id: x\nname: X\ntopics:\n  a:\n    description: d\n    children:\n      b:\n        description: d\n        children:\n          c:\n            description: d\n            attributes:\n              learn_dedup: nope\n"
	if _, err := ParseOntology([]byte(src)); err == nil || !strings.Contains(err.Error(), "a/b/c") {
		t.Fatalf("a bad value at grandchild depth must fail naming a/b/c, got %v", err)
	}
	// Kebab check unchanged: a bad child key still fails with its message.
	bad := "id: x\nname: X\ntopics:\n  a:\n    description: d\n    children:\n      Bad:\n        description: d\n"
	if _, err := ParseOntology([]byte(bad)); err == nil || !strings.Contains(err.Error(), "kebab-case") {
		t.Fatalf("kebab-case child check regressed: %v", err)
	}
}

func TestOntologyAttr_SerializeRoundTrip(t *testing.T) {
	o := mustParse(t, attrOntologyYAML)
	out, err := o.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "attributes:") {
		t.Fatalf("Serialize dropped the attributes block:\n%s", out)
	}
	back := mustParse(t, string(out))
	for _, p := range []string{"tasks", "tasks/research", "tasks/reviewed", "tasks/x", "notes"} {
		v1, ok1 := o.Attr(p, "learn_dedup")
		v2, ok2 := back.Attr(p, "learn_dedup")
		if v1 != v2 || ok1 != ok2 {
			t.Errorf("round trip changed Attr(%q): (%v,%v) -> (%v,%v)", p, v1, ok1, v2, ok2)
		}
	}
	// And the serialized form is itself stable.
	again, err := back.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(out) {
		t.Errorf("serialize is not stable across a round trip:\n%s\n---\n%s", out, again)
	}
}

// A subset means "safe to OVERWRITE with the preset" (repos/builder.go's
// boot refresh). An attribute the preset lacks would be erased by that
// overwrite, so it is divergence — reported as such, so the refresh log can
// say why auto-upgrade stopped.
func TestOntologyAttr_AttributesAreDivergenceFromAPreset(t *testing.T) {
	plain := mustParse(t, "id: x\nname: X\ntopics:\n  tasks:\n    description: d\n    children:\n      research:\n        description: d\n")
	flagged := mustParse(t, "id: x\nname: X\ntopics:\n  tasks:\n    description: d\n    children:\n      research:\n        description: d\n        attributes:\n          learn_dedup: off\n")

	if !plain.IsSubsetOf(flagged) {
		t.Error("the preset side may carry attributes the stored side lacks: that is the preset only adding")
	}
	if flagged.IsSubsetOf(plain) {
		t.Error("a stored ontology differing from the preset ONLY by attributes must not be a subset — the refresh would erase them")
	}
	if got := flagged.SubsetDivergence(plain); got != DivergenceAttributes {
		t.Errorf("SubsetDivergence = %q, want %q", got, DivergenceAttributes)
	}
	if !flagged.IsSubsetOf(flagged) {
		t.Error("equal attributes are a subset")
	}
	onValue := mustParse(t, "id: x\nname: X\ntopics:\n  tasks:\n    description: d\n    children:\n      research:\n        description: d\n        attributes:\n          learn_dedup: on\n")
	if flagged.IsSubsetOf(onValue) {
		t.Error("a different value for the same key is divergence")
	}
	// `on` is defined as absent, so an explicit `on` must not stop upgrades.
	explicitOn := mustParse(t, "id: x\nname: X\ntopics:\n  tasks:\n    description: d\n    attributes:\n      learn_dedup: on\n    children:\n      research:\n        description: d\n")
	if !explicitOn.IsSubsetOf(plain) {
		t.Errorf("learn_dedup: on is the default and must compare as absent; divergence = %q", explicitOn.SubsetDivergence(plain))
	}
	extraTopic := mustParse(t, "id: x\nname: X\ntopics:\n  other:\n    description: d\n    attributes:\n      learn_dedup: off\n")
	if got := extraTopic.SubsetDivergence(plain); got != DivergenceShape {
		t.Errorf("a missing topic must report shape divergence even when attributes also differ, got %q", got)
	}
}

// A bare key parses to a NIL node. Guard at topic depth AND at child depth —
// the attribute walk can hit nil mid-walk exactly as ValidateFact can.
func TestOntologyAttr_NilNodeGuard(t *testing.T) {
	src := `id: x
name: X
topics:
  bare:
  tasks:
    description: d
    attributes:
      learn_dedup: off
    children:
      research:
      other:
        description: d
        attributes:
          learn_dedup: on
`
	o := mustParse(t, src)
	if o.Topics["bare"] != nil || o.Topics["tasks"].Children["research"] != nil {
		t.Fatal("fixture no longer produces nil nodes; the test is not testing the guard")
	}
	for path, want := range map[string]bool{
		"bare":                false,
		"bare/x":              false,
		"tasks/research":      true, // bare child still inherits from its parent
		"tasks/research/deep": true,
		"tasks/other":         false,
	} {
		if got := o.LearnDedupOff(path); got != want {
			t.Errorf("LearnDedupOff(%q) = %v, want %v", path, got, want)
		}
	}
	if _, err := o.Serialize(); err != nil {
		t.Fatal(err)
	}
}

// A value the encoder cannot represent must fail Serialize, not vanish from
// its output. Unreachable from YAML input; reachable from Go callers.
func TestOntologyAttr_SerializeReturnsEncodeError(t *testing.T) {
	o := mustParse(t, attrOntologyYAML)
	o.Topics["notes"].Attributes = map[string]any{"learn_dedup": failingMarshaler{}}
	if _, err := o.Serialize(); err == nil {
		t.Fatal("Serialize must return the encode error rather than drop the key")
	}
}

type failingMarshaler struct{}

func (failingMarshaler) MarshalYAML() (any, error) { return nil, errors.New("boom") }
