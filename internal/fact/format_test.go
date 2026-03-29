package fact

import (
	"encoding/json"
	"testing"
)

func TestNewFact_LowercasesPath(t *testing.T) {
	f := NewFact("KNomit/AI/Foo.md")
	if f.Path() != "knomit/ai/foo.md" {
		t.Errorf("got %q, want %q", f.Path(), "knomit/ai/foo.md")
	}
}

func TestNewFact_AlreadyLowercase(t *testing.T) {
	f := NewFact("kb/tech/foo.md")
	if f.Path() != "kb/tech/foo.md" {
		t.Errorf("got %q, want %q", f.Path(), "kb/tech/foo.md")
	}
}

func TestFact_JSONRoundTrip_LowercasesPath(t *testing.T) {
	f := NewFact("KB/AI/Foo.md")
	f.Title = "Test"
	f.Body = "Body."
	f.Domain = []string{}
	f.Entities = []string{}
	f.Refs = []string{}

	data, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Fact
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Path() != "kb/ai/foo.md" {
		t.Errorf("after round-trip got %q, want %q", got.Path(), "kb/ai/foo.md")
	}
}
