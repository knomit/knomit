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
// mode=custom (and mode=seed) and through POST /ontologies:validate, so a nil
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
	// Serialize is the one initSeed calls on the ontology it is about to write
	// as the remote's root commit, so a panic here takes out a create.
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
