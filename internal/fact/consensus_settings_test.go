package fact

import (
	"strings"
	"testing"
)

// TestReadConsensus: the root block only; absent and explicit off read off;
// auto reads auto; any other value reads OFF with Valid=false (a later value
// is ignored, never acted on); a topic-level `consensus` is not the setting.
//
// SABOTAGE: reading an invalid value as auto (`out.Mode = ConsensusAuto` in
// the invalid branch) turns "unknown value reads off" red.
func TestReadConsensus(t *testing.T) {
	const topics = "topics:\n  notes:\n    description: d\n"
	cases := []struct {
		name  string
		src   string
		mode  string
		valid bool
		err   bool
	}{
		{"absent", "id: x\nname: X\n" + topics, ConsensusOff, true, false},
		{"explicit off", "id: x\nname: X\nattributes:\n  consensus: off\n" + topics, ConsensusOff, true, false},
		{"auto", "id: x\nname: X\nattributes:\n  consensus: auto\n" + topics, ConsensusAuto, true, false},
		{"unknown value reads off", "id: x\nname: X\nattributes:\n  consensus: recipe:merge\n" + topics, ConsensusOff, false, false},
		{"yaml bool reads off", "id: x\nname: X\nattributes:\n  consensus: true\n" + topics, ConsensusOff, false, false},
		{"on a topic is not the setting", "id: x\nname: X\ntopics:\n  notes:\n    description: d\n    attributes:\n      consensus: auto\n", ConsensusOff, true, false},
		{"beside verify_signatures", "id: x\nname: X\nattributes:\n  verify_signatures: log\n  consensus: auto\n" + topics, ConsensusAuto, true, false},
		{"not yaml", "id: [x\n", ConsensusOff, false, true},
		{"root attributes not a mapping", "id: x\nname: X\nattributes: 3\n" + topics, ConsensusOff, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ReadConsensus([]byte(c.src))
			if (err != nil) != c.err {
				t.Fatalf("err = %v, want error %v", err, c.err)
			}
			if got.Mode != c.mode {
				t.Fatalf("Mode = %q, want %q", got.Mode, c.mode)
			}
			if err == nil && got.Valid != c.valid {
				t.Fatalf("Valid = %v, want %v", got.Valid, c.valid)
			}
		})
	}
}

// The registry entry: auto and off open cleanly; a bad value is a warning on
// the open path and fatal only for a new ontology, like verify_signatures; on
// a topic it is the wrong scope.
func TestConsensusAttribute_Registry(t *testing.T) {
	const topics = "topics:\n  notes:\n    description: d\n"
	for _, v := range []string{"auto", "off"} {
		src := "id: x\nname: X\nattributes:\n  consensus: " + v + "\n" + topics
		if _, err := ParseNewOntology([]byte(src)); err != nil {
			t.Fatalf("consensus: %s must be accepted by a new ontology: %v", v, err)
		}
		o, err := ParseOntology([]byte(src))
		if err != nil {
			t.Fatalf("open consensus: %s: %v", v, err)
		}
		if o.Attributes[AttrConsensus] != v {
			t.Fatalf("root attributes = %v, want consensus %s", o.Attributes, v)
		}
	}
	bad := "id: x\nname: X\nattributes:\n  consensus: sometimes\n" + topics
	if _, err := ParseOntology([]byte(bad)); err != nil {
		t.Fatalf("a bad root value must not fail the open path: %v", err)
	}
	_, err := ParseNewOntology([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), `"off" or "auto"`) {
		t.Fatalf("a new ontology with a bad consensus value must be refused naming the values, got %v", err)
	}
}
