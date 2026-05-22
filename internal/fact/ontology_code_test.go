package fact

import "testing"

func TestCodeOntology_LoadsAndValidatesCanonicalPaths(t *testing.T) {
	o := CodeOntology()
	if o.ID != "source-code" {
		t.Fatalf("ID = %q, want %q", o.ID, "source-code")
	}
	wantPaths := []string{
		"invariants/architecture/historical-graph",
		"architecture/modules/store-resolver",
		"conventions/testing/use-mockgen",
		"decisions/accepted/2026-04-introduce-vtable",
		"gotchas/tools/sqlite-vtable-stub-removed",
		"incidents/bugs/2026-03-resolver-walks-past-root",
		"meta/reasoning/sample",
	}
	for _, p := range wantPaths {
		if err := o.ValidatePath(p); err != nil {
			t.Errorf("ValidatePath(%q) = %v, want nil", p, err)
		}
	}
}

func TestCodeOntology_RejectsUnknownTopLevel(t *testing.T) {
	o := CodeOntology()
	if err := o.ValidatePath("bogus/path"); err == nil {
		t.Fatal("ValidatePath(bogus/path) = nil, want error")
	}
}

func TestOntologyByPreset(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wantID  string
		wantErr bool
	}{
		{"default", "general", false},
		{"code", "source-code", false},
		{"unknown", "", true},
	} {
		o, err := OntologyByPreset(tc.name)
		if (err != nil) != tc.wantErr {
			t.Errorf("OntologyByPreset(%q) err = %v, wantErr = %v", tc.name, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && o.ID != tc.wantID {
			t.Errorf("OntologyByPreset(%q) ID = %q, want %q", tc.name, o.ID, tc.wantID)
		}
	}
}
