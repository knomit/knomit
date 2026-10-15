package fact

import (
	"strings"
	"testing"
)

// The fleet ontology declares NO consensus (round 3, R3.4): merging into main
// is the fleet master's duty, never a setting. Absent, `consensus` reads off
// and so does the `conflicts` default it would switch on (a conflicting
// member branch is refused and left behind, not settled). It stays a valid
// NEW ontology with verify_signatures: enforce.
// SABOTAGE: re-add `consensus: auto` -> the attribute is present and the
// conflicts default reads merge -> red.
func TestFleetOntology_DeclaresNoConsensus(t *testing.T) {
	o := FleetOntology()
	if v, ok := o.Attributes[AttrConsensus]; ok {
		t.Fatalf("consensus = %v, want the attribute absent", v)
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
	if !cs.Valid || cs.Mode != ConsensusOff {
		t.Fatalf("ReadConsensus = %+v, want off and valid", cs)
	}
	cf, err := ReadConflicts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cf.Facts != ConflictsOff || cf.State != ConflictsOff {
		t.Fatalf("ReadConflicts = %+v, want facts and state off", cf)
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
