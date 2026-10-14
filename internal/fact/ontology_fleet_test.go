package fact

import (
	"strings"
	"testing"
)

// The fleet ontology DECLARES consensus: auto in its own text (explicit,
// never inferred from the id, 3015a4d6), and it stays a valid NEW ontology.
// The value read is asserted, not "no error": the reader treats an unknown
// value as off.
// SABOTAGE: drop the key -> Mode off -> red.
func TestFleetOntology_DeclaresConsensusAuto(t *testing.T) {
	o := FleetOntology()
	if o.Attributes[AttrConsensus] != ConsensusAuto {
		t.Fatalf("consensus = %v, want %s", o.Attributes[AttrConsensus], ConsensusAuto)
	}
	raw, err := o.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseNewOntology(raw); err != nil {
		t.Fatalf("the serialized fleet ontology is not a valid new ontology: %v", err)
	}
	cs, err := ReadConsensus(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !cs.Valid || cs.Mode != ConsensusAuto {
		t.Fatalf("ReadConsensus = %+v, want auto and valid", cs)
	}
	vs, err := ReadVerifySettings(raw)
	if err != nil || vs.Mode != "enforce" {
		t.Fatalf("verify_signatures = %+v (%v), want enforce alongside", vs, err)
	}
}

// The description says who accepts a record (the fleet's master, by merging
// it into main) and names no other acceptance route: no human merge, no
// forge workflow.
// SABOTAGE: restore "a human accepts by merging it into main" -> red.
func TestFleetOntology_DescriptionNamesTheMaster(t *testing.T) {
	d := FleetOntology().Description
	for _, want := range []string{
		"An instance joins by writing its own record on its agent branch",
		"the record counts once the fleet's master merges it into main",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("description lacks %q:\n%s", want, d)
		}
	}
	for _, gone := range []string{"a human accepts", "workflow"} {
		if strings.Contains(d, gone) {
			t.Errorf("description still names %q:\n%s", gone, d)
		}
	}
}
