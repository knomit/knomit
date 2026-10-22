package fact

import (
	"strings"
	"testing"
)

func TestValidateOntologyYAML_CollectsAllErrorsWithLines(t *testing.T) {
	src := "id: x\nname: X\ntopics:\n  Bad One:\n    description: d\n  Bad Two:\n    description: d\n"
	_, diags := ValidateOntologyYAML([]byte(src))
	if len(diags) != 2 {
		t.Fatalf("diagnostics = %d, want 2: %+v", len(diags), diags)
	}
	if diags[0].Line != 4 {
		t.Errorf("first diagnostic line = %d, want 4", diags[0].Line)
	}
	if diags[1].Line != 6 {
		t.Errorf("second diagnostic line = %d, want 6", diags[1].Line)
	}
}

// Child keys are the case the test above does NOT cover, and they regressed
// independently: the walk descended into `children:` from the topic's KEY node
// rather than its value, so valueForKey always returned nil and every child
// diagnostic came back at line 0. Line 0 is not merely imprecise — it means "no
// position available" (see Diagnostic), the panel renders "Line 0 — …", and
// OntologyEditor's mapDiagnostic clamps it up to line 1, underlining `id:`
// instead of the offending key.
func TestValidateOntologyYAML_ChildKeyDiagnosticsCarryTheirLine(t *testing.T) {
	src := "id: x\nname: X\ntopics:\n  ok:\n    children:\n      Bad Child:\n        description: d\n" +
		"  also-ok:\n    children:\n      Another Bad:\n        description: d\n"
	_, diags := ValidateOntologyYAML([]byte(src))
	if len(diags) != 2 {
		t.Fatalf("diagnostics = %d, want 2: %+v", len(diags), diags)
	}
	// sortedKeys orders the topics, so "also-ok" reports before "ok".
	if diags[0].Line != 10 || diags[0].Column != 7 {
		t.Errorf("first diagnostic at line %d col %d, want line 10 col 7: %s",
			diags[0].Line, diags[0].Column, diags[0].Message)
	}
	if diags[1].Line != 6 || diags[1].Column != 7 {
		t.Errorf("second diagnostic at line %d col %d, want line 6 col 7: %s",
			diags[1].Line, diags[1].Column, diags[1].Message)
	}
}

func TestValidateOntologyYAML_ValidReturnsNoDiagnostics(t *testing.T) {
	src := "id: x\nname: X\ntopics:\n  alpha:\n    description: d\n"
	o, diags := ValidateOntologyYAML([]byte(src))
	if len(diags) != 0 {
		t.Fatalf("diagnostics = %+v, want none", diags)
	}
	if o == nil || o.ID != "x" {
		t.Fatalf("ontology not returned")
	}
}

func TestValidateOntologyYAML_MalformedYAMLIsOneDiagnostic(t *testing.T) {
	_, diags := ValidateOntologyYAML([]byte("id: [unclosed\n"))
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %d, want 1", len(diags))
	}
}

// A bare topic key decodes to a NIL *OntologyNode, and this is the shape a
// hand-written ontology takes most easily — "topics:\n  alpha:\n" is what you
// get by typing the topic and not yet the body. Every walk over o.Topics must
// therefore tolerate nil.
//
// This is remotely triggerable: the same YAML arrives through POST /repos with
// mode=custom (and mode=initialize) and through POST /ontologies:validate, so a nil
// deref here is a panic any caller can cause. ValidateOntologyYAML's child-key
// loop guards it with `node == nil || node.Children == nil`, buildRulesCache's
// walk with `if n == nil`, and countRules (internal/web) with its own — none
// of which anything pinned until now.
func TestValidateOntologyYAML_BareTopicKeyIsANilNodeAndDoesNotPanic(t *testing.T) {
	src := "id: x\nname: X\ntopics:\n  alpha:\n"
	o, diags := ValidateOntologyYAML([]byte(src))
	if len(diags) != 0 {
		t.Fatalf("diagnostics = %+v, want none", diags)
	}
	if o == nil {
		t.Fatal("ontology not returned")
	}
	node, ok := o.Topics["alpha"]
	if !ok {
		t.Fatal("topic alpha missing")
	}
	if node != nil {
		t.Fatalf("topic alpha = %+v, want a nil node — the whole point of this guard", node)
	}
	// The two exported walks a nil topic reaches from here.
	if err := o.ValidatePath("alpha/anything/deeper"); err != nil {
		t.Errorf("ValidatePath over a nil topic node: %v", err)
	}
	if got := o.TopicNames(); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("TopicNames() = %v, want [alpha]", got)
	}
	// Serialize is the one initInitialize calls on the ontology it is about to
	// commit onto the agent branch, so a panic here takes out a create.
	y, err := o.Serialize()
	if err != nil {
		t.Fatalf("Serialize over a nil topic node: %v", err)
	}
	if !strings.Contains(string(y), "alpha") {
		t.Errorf("Serialize dropped the bare topic: %s", y)
	}
	round, rerr := ParseOntology(y)
	if rerr != nil {
		t.Fatalf("re-parsing a serialized nil topic node: %v", rerr)
	}
	if _, ok := round.Topics["alpha"]; !ok {
		t.Errorf("round trip lost topic alpha: %s", y)
	}
}

// ParseOntology is the create path's entry point (resolveOntology → this), and
// it reaches the same nil node through ValidateOntologyYAML. Pinned separately
// because that is the function the remotely reachable panic used to be in.
func TestParseOntology_BareTopicKeyDoesNotPanic(t *testing.T) {
	o, err := ParseOntology([]byte("id: x\nname: X\ntopics:\n  alpha:\n  beta:\n    description: d\n"))
	if err != nil {
		t.Fatalf("ParseOntology: %v", err)
	}
	if o.Topics["alpha"] != nil {
		t.Fatalf("topic alpha should decode to a nil node")
	}
}

// An unknown key used to be dropped in silence, so an ontology full of
// invented blocks validated ok:true — and a MISSPELT real key (descriptionn)
// discarded the value without a word. The ontology is immutable once the repo
// exists, so "it validated" is the only assurance a reader ever gets.
func TestValidateOntologyYAML_ReportsUnknownFields(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "invented top-level block",
			yaml: "id: x\nname: X\nfuusbar:\n  barfuus:\n    barf:\ntopics:\n  a:\n    description: d\n",
			want: "fuusbar",
		},
		{
			name: "misspelt key inside a topic",
			yaml: "id: x\nname: X\ntopics:\n  a:\n    descriptionn: typo\n",
			want: "descriptionn",
		},
		{
			name: "unknown key inside a validation",
			yaml: "id: x\nname: X\ntopics:\n  a:\n    description: d\nvalidations:\n  - name: r\n    mesage: typo\n    rule: \"true\"\n",
			want: "mesage",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, diags := ValidateOntologyYAML([]byte(c.yaml))
			// REPORTED, and non-fatal. The document is returned because an
			// unrecognised key does not stop this binary reading the rest of
			// it: the open path must be able to read a repo whose ontology was
			// written by a newer knomit, and the alternative — refusing — made
			// loadOntology substitute the DEFAULT taxonomy for the repo's own,
			// permanently and silently.
			//
			// This is not the editor going quiet. The validate endpoint reports
			// ok:false for any diagnostic at all, warnings included, which
			// TestValidateOntology_UnknownKeyIsStillRefused pins.
			if o == nil {
				t.Fatal("an unknown key must not withhold a document that otherwise parses")
			}
			var joined string
			for _, d := range diags {
				joined += d.Message + "\n"
				if d.IsError() {
					t.Fatalf("an unknown key must be a warning, not a fatal error: %+v", d)
				}
			}
			if !strings.Contains(joined, c.want) {
				t.Fatalf("diagnostics do not mention %q: %s", c.want, joined)
			}
		})
	}
}

// The counterpart, and the line this check must not cross: TOPIC names are map
// keys, so an ontology may name its topics anything (that is the entire point
// of a custom ontology). Only STRUCT fields are constrained.
func TestValidateOntologyYAML_TopicNamesAreNotFields(t *testing.T) {
	o, diags := ValidateOntologyYAML([]byte(
		"id: x\nname: X\ntopics:\n  anything-at-all:\n    description: d\n    children:\n      also-anything:\n        description: d\n"))
	if len(diags) != 0 {
		t.Fatalf("free-form topic names must validate: %+v", diags)
	}
	if _, ok := o.Topics["anything-at-all"]; !ok {
		t.Fatal("topic anything-at-all missing")
	}
}

// The line number has to survive into the diagnostic, or the editor cannot put
// a marker where the problem is.
func TestValidateOntologyYAML_UnknownFieldCarriesItsLine(t *testing.T) {
	_, diags := ValidateOntologyYAML([]byte("id: x\nname: X\nbogus: 1\ntopics:\n  a:\n    description: d\n"))
	var found bool
	for _, d := range diags {
		if !strings.Contains(d.Message, "bogus") {
			continue
		}
		found = true
		if d.Line != 3 {
			t.Fatalf("Line = %d, want 3 (bogus: is on line 3)", d.Line)
		}
		if strings.Contains(d.Message, "line 3") {
			t.Fatalf("the line belongs in Line, not the text: %q", d.Message)
		}
	}
	if !found {
		t.Fatalf("no diagnostic mentions bogus: %+v", diags)
	}
}

// A duplicate key reported its line INSIDE the message while Diagnostic.Line
// stayed 0, so the editor said "Line 0" next to a sentence naming line 4 and
// had nowhere to put a marker.
func TestValidateOntologyYAML_DuplicateKeyCarriesItsLine(t *testing.T) {
	_, diags := ValidateOntologyYAML([]byte("id: x\nname: X\ntopics:\n  a:\n    description: d\ntopics:\n  b:\n    description: d\n"))
	if len(diags) == 0 {
		t.Fatal("a duplicate mapping key must be reported")
	}
	d := diags[0]
	if d.Line != 6 {
		t.Fatalf("Line = %d, want 6 (the second topics: is on line 6): %+v", d.Line, diags)
	}
	if !strings.Contains(d.Message, "already defined") {
		t.Fatalf("message lost the cause: %q", d.Message)
	}
	if strings.Contains(d.Message, "line 6") {
		t.Fatalf("the line belongs in Line, not the text: %q", d.Message)
	}
}

// A plain syntax error must keep its position too.
func TestValidateOntologyYAML_SyntaxErrorCarriesItsLine(t *testing.T) {
	_, diags := ValidateOntologyYAML([]byte("id: x\nname: X\ntopics:\n  a: [unclosed\n"))
	if len(diags) == 0 {
		t.Fatal("a syntax error must be reported")
	}
	if diags[0].Line == 0 {
		t.Fatalf("syntax error lost its line: %+v", diags)
	}
}
