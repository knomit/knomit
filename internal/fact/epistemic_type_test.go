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
	if got := len(AllTypes()); got != 9 {
		t.Fatalf("AllTypes: got %d want 9", got)
	}
}

func TestHypothesisTypeValid(t *testing.T) {
	if !Hypothesis.Valid() {
		t.Error("hypothesis must be a valid epistemic type")
	}
	if err := Hypothesis.Validate(); err != nil {
		t.Errorf("Hypothesis.Validate() returned unexpected error: %v", err)
	}
}

func TestMethodologyTypeValid(t *testing.T) {
	if !Methodology.Valid() {
		t.Error("methodology must be a valid epistemic type")
	}
	if err := Methodology.Validate(); err != nil {
		t.Errorf("Methodology.Validate() returned unexpected error: %v", err)
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
