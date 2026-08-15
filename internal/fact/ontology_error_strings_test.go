package fact

import "testing"

// The strings below are the boot path for every repo open and are asserted by
// existing callers and tests. ValidateOntologyYAML must not alter them.
func TestParseOntology_ErrorStringsUnchanged(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"missing id", "name: X\ntopics:\n  a:\n", "parse ontology: id is required"},
		{"missing name", "id: x\ntopics:\n  a:\n", "parse ontology: name is required"},
		{"no topics", "id: x\nname: X\n", "parse ontology: at least one topic is required"},
		{"bad topic key", "id: x\nname: X\ntopics:\n  Bad Key:\n",
			`parse ontology: invalid key "Bad Key" in topic: must be lowercase kebab-case`},
		{"bad child key", "id: x\nname: X\ntopics:\n  ok:\n    children:\n      Bad:\n",
			`parse ontology: invalid key "Bad" in topic "ok" child: must be lowercase kebab-case`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseOntology([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected an error")
			}
			if err.Error() != tc.want {
				t.Fatalf("error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}
