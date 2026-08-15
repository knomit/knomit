package fact

import "testing"

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
