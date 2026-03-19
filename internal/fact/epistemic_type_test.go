package fact

import "testing"

func TestEpistemicTypeValid(t *testing.T) {
	for _, et := range AllTypes() {
		if !et.Valid() {
			t.Errorf("expected %q to be valid", et)
		}
		if err := et.Validate(); err != nil {
			t.Errorf("expected %q to pass Validate, got: %v", et, err)
		}
	}
}

func TestEpistemicTypeInvalid(t *testing.T) {
	invalid := []EpistemicType{"", "fact", "unknown", "OBSERVATION", "Concept"}
	for _, et := range invalid {
		if et.Valid() {
			t.Errorf("expected %q to be invalid", et)
		}
		if err := et.Validate(); err == nil {
			t.Errorf("expected %q to fail Validate", et)
		}
	}
}

func TestDefaultType(t *testing.T) {
	if !DefaultType.Valid() {
		t.Fatal("DefaultType must be valid")
	}
	if DefaultType != Observation {
		t.Fatalf("DefaultType: got %q want %q", DefaultType, Observation)
	}
}

func TestAllTypesLength(t *testing.T) {
	if got := len(AllTypes()); got != 7 {
		t.Fatalf("AllTypes: got %d want 7", got)
	}
}

func TestSynthesisTypeValid(t *testing.T) {
	if !Synthesis.Valid() {
		t.Error("synthesis must be a valid epistemic type")
	}
	if err := Synthesis.Validate(); err != nil {
		t.Errorf("Synthesis.Validate() returned unexpected error: %v", err)
	}
}
