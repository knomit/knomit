package fact

import (
	"strings"
	"testing"
)

func TestParseOntologyValid(t *testing.T) {
	data := []byte(`
id: test
name: Test Ontology
description: A test ontology
topics:
  technology:
    description: Tech stuff
    children:
      software:
        description: Software things
      hardware:
        description: Hardware things
  people:
    description: People stuff
    children:
      individuals:
        description: Specific persons
`)
	ont, err := ParseOntology(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ont.ID != "test" {
		t.Errorf("expected ID 'test', got %q", ont.ID)
	}
	if ont.Name != "Test Ontology" {
		t.Errorf("expected Name 'Test Ontology', got %q", ont.Name)
	}
	if len(ont.Topics) != 2 {
		t.Errorf("expected 2 topics, got %d", len(ont.Topics))
	}
	tech := ont.Topics["technology"]
	if tech == nil {
		t.Fatal("expected 'technology' topic")
	}
	if len(tech.Children) != 2 {
		t.Errorf("expected 2 children for technology, got %d", len(tech.Children))
	}
	if tech.Children["software"] == nil {
		t.Error("expected 'software' child")
	}
}

func TestParseOntologyMissingID(t *testing.T) {
	data := []byte(`
name: Test
topics:
  people:
    description: People
`)
	_, err := ParseOntology(data)
	if err == nil {
		t.Fatal("expected error for missing ID")
	}
	if !strings.Contains(err.Error(), "id is required") {
		t.Errorf("expected error containing 'id is required', got: %v", err)
	}
}

func TestParseOntologyMissingName(t *testing.T) {
	data := []byte(`
id: test
topics:
  people:
    description: People
`)
	_, err := ParseOntology(data)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Errorf("expected error containing 'name is required', got: %v", err)
	}
}

func TestParseOntologyEmptyTopics(t *testing.T) {
	data := []byte(`
id: test
name: Test
topics: {}
`)
	_, err := ParseOntology(data)
	if err == nil {
		t.Fatal("expected error for empty topics")
	}
	if !strings.Contains(err.Error(), "at least one topic") {
		t.Errorf("expected error containing 'at least one topic', got: %v", err)
	}
}

func TestParseOntologyInvalidKey(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"spaces", `
id: test
name: Test
topics:
  has spaces:
    description: bad
`},
		{"uppercase", `
id: test
name: Test
topics:
  Technology:
    description: bad
`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseOntology([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected error for invalid key")
			}
			if !strings.Contains(err.Error(), "invalid key") {
				t.Errorf("expected error containing 'invalid key', got: %v", err)
			}
		})
	}
}

func TestTopicNamesSorted(t *testing.T) {
	data := []byte(`
id: test
name: Test
topics:
  zebra:
    description: Z
  alpha:
    description: A
  middle:
    description: M
`)
	ont, err := ParseOntology(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	names := ont.TopicNames()
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %d", len(names))
	}
	if names[0] != "alpha" || names[1] != "middle" || names[2] != "zebra" {
		t.Errorf("expected sorted names [alpha middle zebra], got %v", names)
	}
}

// testOntology is used by ValidatePath tests.
var testOntologyYAML = []byte(`
id: test
name: Test
topics:
  technology:
    description: Tech
    children:
      software:
        description: Languages, frameworks
        children:
          go:
            description: Go programming language
      hardware:
        description: Devices
  people:
    description: People
    children:
      individuals:
        description: Specific persons
`)

func TestValidatePath(t *testing.T) {
	ont, err := ParseOntology(testOntologyYAML)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		path    string
		wantErr bool
		errMsg  string
	}{
		{"technology", false, ""},
		{"technology/software", false, ""},
		{"technology/software/go/concurrency", false, ""},
		{"technology/quantum", false, ""},
		{"people", false, ""},
		{"people/alice", false, ""},
		{"cooking", true, "unknown topic"},
		{"", true, "empty path"},
		{"TECHNOLOGY", true, "unknown topic"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			err := ont.ValidatePath(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for path %q", tc.path)
				}
				if tc.errMsg != "" && !strings.Contains(err.Error(), tc.errMsg) {
					t.Errorf("expected error containing %q, got: %v", tc.errMsg, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error for path %q: %v", tc.path, err)
				}
			}
		})
	}
}

func TestSerializeDeterministic(t *testing.T) {
	// Keys intentionally unsorted.
	data := []byte(`
id: test
name: Test
description: Unsorted keys
topics:
  zebra:
    description: Z animal
    children:
      stripes:
        description: Black and white
      mane:
        description: Short mane
  alpha:
    description: First letter
    children:
      omega:
        description: Last letter
      beta:
        description: Second letter
`)
	ont, err := ParseOntology(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out1, err := ont.Serialize()
	if err != nil {
		t.Fatalf("serialize 1: %v", err)
	}

	// Parse the serialized output and serialize again.
	ont2, err := ParseOntology(out1)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	out2, err := ont2.Serialize()
	if err != nil {
		t.Fatalf("serialize 2: %v", err)
	}

	if string(out1) != string(out2) {
		t.Errorf("serialized outputs differ:\n--- first ---\n%s\n--- second ---\n%s", out1, out2)
	}

	// Verify keys are sorted in output.
	s := string(out1)
	alphaIdx := strings.Index(s, "alpha:")
	zebraIdx := strings.Index(s, "zebra:")
	if alphaIdx < 0 || zebraIdx < 0 {
		t.Fatal("expected both 'alpha:' and 'zebra:' in output")
	}
	if alphaIdx >= zebraIdx {
		t.Error("expected 'alpha' before 'zebra' in sorted output")
	}
}
