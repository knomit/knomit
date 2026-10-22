package fact

import (
	"strings"
	"testing"
)

const conflictsTopics = "topics:\n  notes:\n    description: d\n"

func conflictsOnt(attrs string) string {
	s := "id: x\nname: X\n"
	if attrs != "" {
		s += "attributes:\n" + attrs
	}
	return s + conflictsTopics
}

// TestReadConflicts: the root block only. `conflicts` is an object with two
// keys, `facts` (off|merge|merge:consensus|consensus) and `state`
// (off|consensus). An absent key reads off — or, under `consensus: auto`,
// facts: merge / state: consensus; an explicit off stays off. Any value the
// registry rejects — the old scalar form, an unknown inner key, a value of the
// wrong key's set — reads OFF for both keys with Valid=false, even under
// auto: an explicit bad value is not absent. A topic-level `conflicts` is not
// the setting.
//
// SABOTAGE: drop the auto default (the `consensus: auto` arm of ReadConflicts)
// → "auto, conflicts absent" and "auto, facts only" red; accept merge:consensus
// for state (validConflictsState) → "merge:consensus is not a state value" red.
func TestReadConflicts(t *testing.T) {
	cases := []struct {
		name         string
		src          string
		facts, state string
		rule         MergeRule
		ruleOn       bool
		valid        bool
		err          bool
	}{
		{"absent", conflictsOnt(""), ConflictsOff, ConflictsOff, "", false, true, false},
		{"explicit off", conflictsOnt("  conflicts:\n    facts: off\n    state: off\n"), ConflictsOff, ConflictsOff, "", false, true, false},
		{"facts merge", conflictsOnt("  conflicts:\n    facts: merge\n"), ConflictsMerge, ConflictsOff, MergeConfidence, true, true, false},
		{"facts merge:consensus", conflictsOnt("  conflicts:\n    facts: merge:consensus\n"), ConflictsMergeConsensus, ConflictsOff, MergeTakeConsensus, true, true, false},
		{"facts consensus", conflictsOnt("  conflicts:\n    facts: consensus\n"), ConflictsConsensus, ConflictsOff, "", false, true, false},
		{"state consensus", conflictsOnt("  conflicts:\n    state: consensus\n"), ConflictsOff, ConflictsConsensus, "", false, true, false},
		{"both", conflictsOnt("  conflicts:\n    facts: merge\n    state: consensus\n"), ConflictsMerge, ConflictsConsensus, MergeConfidence, true, true, false},
		{"empty object", conflictsOnt("  conflicts: {}\n"), ConflictsOff, ConflictsOff, "", false, true, false},

		{"auto, conflicts absent", conflictsOnt("  consensus: auto\n"), ConflictsMerge, ConflictsConsensus, MergeConfidence, true, true, false},
		{"auto, empty object", conflictsOnt("  consensus: auto\n  conflicts: {}\n"), ConflictsMerge, ConflictsConsensus, MergeConfidence, true, true, false},
		{"auto, facts only", conflictsOnt("  consensus: auto\n  conflicts:\n    facts: merge:consensus\n"), ConflictsMergeConsensus, ConflictsConsensus, MergeTakeConsensus, true, true, false},
		{"auto, state only", conflictsOnt("  consensus: auto\n  conflicts:\n    state: off\n"), ConflictsMerge, ConflictsOff, MergeConfidence, true, true, false},
		{"auto, explicit off honoured", conflictsOnt("  consensus: auto\n  conflicts:\n    facts: off\n    state: off\n"), ConflictsOff, ConflictsOff, "", false, true, false},
		{"consensus off: no default", conflictsOnt("  consensus: off\n"), ConflictsOff, ConflictsOff, "", false, true, false},
		{"bad consensus value: no default", conflictsOnt("  consensus: always\n"), ConflictsOff, ConflictsOff, "", false, true, false},

		{"scalar form reads off", conflictsOnt("  conflicts: merge\n"), ConflictsOff, ConflictsOff, "", false, false, false},
		{"scalar form under auto reads off", conflictsOnt("  consensus: auto\n  conflicts: merge\n"), ConflictsOff, ConflictsOff, "", false, false, false},
		{"merge:consensus is not a state value", conflictsOnt("  conflicts:\n    facts: merge\n    state: merge:consensus\n"), ConflictsOff, ConflictsOff, "", false, false, false},
		{"merge is not a state value", conflictsOnt("  conflicts:\n    state: merge\n"), ConflictsOff, ConflictsOff, "", false, false, false},
		{"unknown facts value", conflictsOnt("  conflicts:\n    facts: merge:trusted\n"), ConflictsOff, ConflictsOff, "", false, false, false},
		{"old merge:upstream", conflictsOnt("  conflicts:\n    facts: merge:upstream\n"), ConflictsOff, ConflictsOff, "", false, false, false},
		{"unknown inner key", conflictsOnt("  conflicts:\n    facts: merge\n    refs: union\n"), ConflictsOff, ConflictsOff, "", false, false, false},
		{"yaml bool", conflictsOnt("  conflicts:\n    facts: true\n"), ConflictsOff, ConflictsOff, "", false, false, false},
		{"null", conflictsOnt("  consensus: auto\n  conflicts:\n"), ConflictsOff, ConflictsOff, "", false, false, false},
		{"on a topic is not the setting", "id: x\nname: X\ntopics:\n  notes:\n    description: d\n    attributes:\n      conflicts:\n        facts: merge\n", ConflictsOff, ConflictsOff, "", false, true, false},
		{"not yaml", "id: [x\n", ConflictsOff, ConflictsOff, "", false, false, true},
		{"root attributes not a mapping", "id: x\nname: X\nattributes: 3\n" + conflictsTopics, ConflictsOff, ConflictsOff, "", false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ReadConflicts([]byte(c.src))
			if (err != nil) != c.err {
				t.Fatalf("err = %v, want error %v", err, c.err)
			}
			if got.Facts != c.facts || got.State != c.state {
				t.Fatalf("facts/state = %q/%q, want %q/%q", got.Facts, got.State, c.facts, c.state)
			}
			if err == nil && got.Valid != c.valid {
				t.Fatalf("Valid = %v, want %v", got.Valid, c.valid)
			}
			if got.On() != (c.facts != ConflictsOff || c.state != ConflictsOff) {
				t.Fatalf("On() = %v for %q/%q", got.On(), got.Facts, got.State)
			}
			rule, on := ConflictsFactsRule(got.Facts)
			if rule != c.rule || on != c.ruleOn {
				t.Fatalf("ConflictsFactsRule(%q) = %q, %v; want %q, %v", got.Facts, rule, on, c.rule, c.ruleOn)
			}
		})
	}
}

// The auto default keys on the ATTRIBUTE `consensus: auto` alone, not on how
// the repository is hosted: the reader never sees an origin, so the same file
// reads the same on a host with no origin and on a clone of a GitHub origin
// (where the merger ignores auto). Pinned so a later "only when hosting"
// refinement has to change this test on purpose.
func TestReadConflicts_AutoDefaultIsTheAttributeAlone(t *testing.T) {
	src := conflictsOnt("  consensus: auto\n")
	o, err := ParseOntology([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if o.Attributes[AttrConsensus] != ConsensusAuto {
		t.Fatalf("fixture: consensus = %v", o.Attributes[AttrConsensus])
	}
	got, err := ReadConflicts([]byte(src))
	if err != nil || got.Facts != ConflictsMerge || got.State != ConflictsConsensus {
		t.Fatalf("consensus: auto alone must default conflicts to merge/consensus, got %+v %v", got, err)
	}
}

// The registry entry: every valid object opens cleanly and reaches
// Ontology.Attributes as written; a bad value — the scalar form, a bad value
// in either key, an unknown inner key — is a WARNING on the open path (the
// repository keeps opening; ReadConflicts reads it as off) and FATAL only for
// a new ontology, exactly as for `consensus`; on a topic it is the wrong
// scope.
func TestConflictsAttribute_Registry(t *testing.T) {
	for _, body := range []string{
		"  conflicts:\n    facts: off\n",
		"  conflicts:\n    facts: merge\n",
		"  conflicts:\n    facts: merge:consensus\n",
		"  conflicts:\n    facts: consensus\n",
		"  conflicts:\n    state: off\n",
		"  conflicts:\n    state: consensus\n",
		"  conflicts:\n    facts: merge\n    state: consensus\n",
		"  conflicts: {}\n",
	} {
		src := conflictsOnt(body)
		if _, err := ParseNewOntology([]byte(src)); err != nil {
			t.Fatalf("%q must be accepted by a new ontology: %v", body, err)
		}
		o, err := ParseOntology([]byte(src))
		if err != nil {
			t.Fatalf("open %q: %v", body, err)
		}
		if _, ok := o.Attributes[AttrConflicts].(map[string]any); !ok {
			t.Fatalf("root attributes = %v, want a conflicts mapping", o.Attributes)
		}
	}
	for _, body := range []string{
		"  conflicts: merge\n",
		"  conflicts: off\n",
		"  conflicts:\n    facts: sometimes\n",
		"  conflicts:\n    state: merge:consensus\n",
		"  conflicts:\n    facts: merge\n    extra: x\n",
	} {
		bad := conflictsOnt(body)
		o, diags := ValidateOntologyYAML([]byte(bad))
		if o == nil {
			t.Fatalf("%q: no ontology", body)
		}
		warned := false
		for _, d := range diags {
			if d.IsError() {
				t.Fatalf("%q: a bad root value must not be an error on the open path: %s", body, d.Message)
			}
			if strings.Contains(d.Message, `root attribute "conflicts" must be a mapping with "facts"`) {
				warned = true
			}
		}
		if !warned {
			t.Fatalf("%q: ValidateOntologyYAML must warn naming the shape, got %+v", body, diags)
		}
		if _, err := ParseOntology([]byte(bad)); err != nil {
			t.Fatalf("%q: a bad root value must not fail the open path: %v", body, err)
		}
		if _, err := ParseNewOntology([]byte(bad)); err == nil {
			t.Fatalf("%q: a new ontology with a bad conflicts value must be refused", body)
		}
	}
	onTopic := "id: x\nname: X\ntopics:\n  notes:\n    description: d\n    attributes:\n      conflicts:\n        facts: merge\n"
	if _, err := ParseNewOntology([]byte(onTopic)); err == nil {
		t.Fatal("conflicts on a topic is the wrong scope and must be refused for a new ontology")
	}
}
