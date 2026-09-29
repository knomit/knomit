package fact

import (
	"strings"
	"testing"
)

// TestReadConflicts: the root block only; absent and explicit off read off
// (no merge); merge reads the confidence rule and merge:upstream the upstream
// rule; any other value reads OFF with Valid=false; a topic-level
// `conflicts` is not the setting.
//
// SABOTAGE: an absent key read as merge (`out := ConflictsSettings{Mode:
// ConflictsMerge…}`) turns "absent" red; an unknown value read as merge turns
// "reserved value reads off" red.
func TestReadConflicts(t *testing.T) {
	const topics = "topics:\n  notes:\n    description: d\n"
	cases := []struct {
		name  string
		src   string
		mode  string
		rule  MergeRule
		on    bool
		valid bool
		err   bool
	}{
		{"absent", "id: x\nname: X\n" + topics, ConflictsOff, "", false, true, false},
		{"explicit off", "id: x\nname: X\nattributes:\n  conflicts: off\n" + topics, ConflictsOff, "", false, true, false},
		{"merge", "id: x\nname: X\nattributes:\n  conflicts: merge\n" + topics, ConflictsMerge, MergeConfidence, true, true, false},
		{"merge:upstream", "id: x\nname: X\nattributes:\n  conflicts: merge:upstream\n" + topics, ConflictsMergeUpstream, MergeUpstream, true, true, false},
		{"reserved value reads off", "id: x\nname: X\nattributes:\n  conflicts: merge:trusted\n" + topics, ConflictsOff, "", false, false, false},
		{"yaml bool reads off", "id: x\nname: X\nattributes:\n  conflicts: true\n" + topics, ConflictsOff, "", false, false, false},
		{"on a topic is not the setting", "id: x\nname: X\ntopics:\n  notes:\n    description: d\n    attributes:\n      conflicts: merge\n", ConflictsOff, "", false, true, false},
		{"beside consensus", "id: x\nname: X\nattributes:\n  consensus: auto\n  conflicts: merge\n" + topics, ConflictsMerge, MergeConfidence, true, true, false},
		{"not yaml", "id: [x\n", ConflictsOff, "", false, false, true},
		{"root attributes not a mapping", "id: x\nname: X\nattributes: 3\n" + topics, ConflictsOff, "", false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ReadConflicts([]byte(c.src))
			if (err != nil) != c.err {
				t.Fatalf("err = %v, want error %v", err, c.err)
			}
			if got.Mode != c.mode {
				t.Fatalf("Mode = %q, want %q", got.Mode, c.mode)
			}
			if err == nil && got.Valid != c.valid {
				t.Fatalf("Valid = %v, want %v", got.Valid, c.valid)
			}
			rule, on := got.Rule()
			if rule != c.rule || on != c.on {
				t.Fatalf("Rule() = %q, %v; want %q, %v", rule, on, c.rule, c.on)
			}
		})
	}
}

// The registry entry: all three values open cleanly; a bad value is a warning
// on the open path and fatal only for a new ontology; on a topic it is the
// wrong scope.
func TestConflictsAttribute_Registry(t *testing.T) {
	const topics = "topics:\n  notes:\n    description: d\n"
	for _, v := range []string{"off", "merge", "merge:upstream"} {
		src := "id: x\nname: X\nattributes:\n  conflicts: " + v + "\n" + topics
		if _, err := ParseNewOntology([]byte(src)); err != nil {
			t.Fatalf("conflicts: %s must be accepted by a new ontology: %v", v, err)
		}
		o, err := ParseOntology([]byte(src))
		if err != nil {
			t.Fatalf("open conflicts: %s: %v", v, err)
		}
		if o.Attributes[AttrConflicts] != v {
			t.Fatalf("root attributes = %v, want conflicts %s", o.Attributes, v)
		}
	}
	bad := "id: x\nname: X\nattributes:\n  conflicts: sometimes\n" + topics
	if _, err := ParseOntology([]byte(bad)); err != nil {
		t.Fatalf("a bad root value must not fail the open path: %v", err)
	}
	_, err := ParseNewOntology([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), `"off", "merge" or "merge:upstream"`) {
		t.Fatalf("a new ontology with a bad conflicts value must be refused naming the values, got %v", err)
	}
	onTopic := "id: x\nname: X\ntopics:\n  notes:\n    description: d\n    attributes:\n      conflicts: merge\n"
	if _, err := ParseNewOntology([]byte(onTopic)); err == nil {
		t.Fatal("conflicts on a topic is the wrong scope and must be refused for a new ontology")
	}
}
