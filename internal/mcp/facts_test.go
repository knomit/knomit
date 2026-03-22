package mcp

import (
	"strings"
	"testing"

	"knomit/internal/fact"
)

func TestFactRoundTrip(t *testing.T) {
	f := fact.NewFact("general/test/foo.md")
	f.Title = "Test fact"
	f.Body = "Some body text."
	f.Domain = []string{"testing"}
	f.Confidence = 0.9
	f.Sources = 1
	f.Entities = []string{"foo"}
	f.Refs = []string{}

	serialized := SerializeFact(f)
	parsed, err := ParseFact(f.Path(), serialized)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Title != f.Title {
		t.Fatalf("title: got %q want %q", parsed.Title, f.Title)
	}
	if parsed.Confidence != f.Confidence {
		t.Fatalf("confidence mismatch: got %v want %v", parsed.Confidence, f.Confidence)
	}
	if len(parsed.Domain) == 0 || parsed.Domain[0] != f.Domain[0] {
		t.Fatalf("domain mismatch: got %v want %v", parsed.Domain, f.Domain)
	}
	if parsed.Body != f.Body {
		t.Fatalf("body: got %q want %q", parsed.Body, f.Body)
	}
	if parsed.Path() != f.Path() {
		t.Fatalf("path: got %q want %q", parsed.Path(), f.Path())
	}
	if parsed.Sources != f.Sources {
		t.Fatalf("sources: got %d want %d", parsed.Sources, f.Sources)
	}
	if len(parsed.Entities) != len(f.Entities) || (len(f.Entities) > 0 && parsed.Entities[0] != f.Entities[0]) {
		t.Fatalf("entities mismatch: got %v want %v", parsed.Entities, f.Entities)
	}
}

func TestParseFactMissingFrontmatter(t *testing.T) {
	content := "# Just a heading\n\nNo frontmatter here.\n"
	_, err := ParseFact("test/no-fm.md", content)
	if err == nil {
		t.Fatal("expected error for missing frontmatter, got nil")
	}
}

func TestParseFactMultilineBody(t *testing.T) {
	f := fact.NewFact("general/multi.md")
	f.Title = "Multi-paragraph fact"
	f.Body = "First paragraph.\n\nSecond paragraph with more detail.\n\nThird paragraph."
	f.Domain = []string{"testing", "multiline"}
	f.Confidence = 0.8
	f.Sources = 3
	f.Entities = []string{"alpha", "beta"}
	f.Refs = []string{"https://example.com"}

	serialized := SerializeFact(f)
	parsed, err := ParseFact(f.Path(), serialized)
	if err != nil {
		t.Fatalf("ParseFact error: %v", err)
	}
	if parsed.Title != f.Title {
		t.Fatalf("title: got %q want %q", parsed.Title, f.Title)
	}
	if parsed.Body != f.Body {
		t.Fatalf("body: got %q want %q", parsed.Body, f.Body)
	}
	if len(parsed.Domain) != len(f.Domain) {
		t.Fatalf("domain len: got %d want %d", len(parsed.Domain), len(f.Domain))
	}
	if len(parsed.Entities) != len(f.Entities) {
		t.Fatalf("entities len: got %d want %d", len(parsed.Entities), len(f.Entities))
	}
	if len(parsed.Refs) != len(f.Refs) || parsed.Refs[0] != f.Refs[0] {
		t.Fatalf("refs: got %v want %v", parsed.Refs, f.Refs)
	}
	if parsed.Confidence != f.Confidence {
		t.Fatalf("confidence: got %v want %v", parsed.Confidence, f.Confidence)
	}
	if parsed.Sources != f.Sources {
		t.Fatalf("sources: got %d want %d", parsed.Sources, f.Sources)
	}
}

func TestParseFactCRLFLineEndings(t *testing.T) {
	// Build a fact with CRLF endings.
	content := "---\r\ndomain: [testing]\r\nconfidence: 0.9\r\nsources: 1\r\nentities: [foo]\r\nrefs: []\r\n---\r\n# CRLF Fact\r\n\r\nBody with CRLF line endings.\r\n"
	f, err := ParseFact("test/crlf.md", content)
	if err != nil {
		t.Fatalf("ParseFact CRLF error: %v", err)
	}
	if f.Title != "CRLF Fact" {
		t.Fatalf("title: got %q want %q", f.Title, "CRLF Fact")
	}
	if f.Body != "Body with CRLF line endings." {
		t.Fatalf("body: got %q", f.Body)
	}
}

func TestSerializeFactFormat(t *testing.T) {
	f := fact.NewFact("general/fmt/test.md")
	f.Title = "Format test"
	f.Body = "Body content."
	f.Type = fact.Observation
	f.Domain = []string{"testing"}
	f.Confidence = 0.9
	f.Sources = 1
	f.Entities = []string{"foo"}
	f.Refs = []string{}

	got := SerializeFact(f)
	want := "---\ntype: observation\ndomain: [testing]\nconfidence: 0.9\nsources: 1\nentities: [foo]\nrefs: []\n---\n# Format test\n\nBody content.\n"
	if got != want {
		t.Fatalf("SerializeFact output mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestEpistemicTypeRoundTrip(t *testing.T) {
	for _, et := range fact.AllTypes() {
		f := fact.NewFact("general/type-test.md")
		f.Title = "Type round-trip"
		f.Body = "Body."
		f.Type = et
		f.Domain = []string{}
		f.Confidence = 0.5
		f.Sources = 1
		f.Entities = []string{}
		f.Refs = []string{}

		serialized := SerializeFact(f)
		parsed, err := ParseFact(f.Path(), serialized)
		if err != nil {
			t.Fatalf("type %q: ParseFact error: %v", et, err)
		}
		if parsed.Type != et {
			t.Fatalf("type round-trip: got %q want %q", parsed.Type, et)
		}
	}
}

func TestParseFactDefaultsToObservation(t *testing.T) {
	content := "---\ndomain: [testing]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# No type field\n\nBody.\n"
	f, err := ParseFact("test/no-type.md", content)
	if err != nil {
		t.Fatalf("ParseFact error: %v", err)
	}
	if f.Type != fact.Observation {
		t.Fatalf("expected default type %q, got %q", fact.Observation, f.Type)
	}
}

func TestParseFactInvalidTypeReturnsError(t *testing.T) {
	content := "---\ntype: banana\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# Bad type\n\nBody.\n"
	_, err := ParseFact("test/bad-type.md", content)
	if err == nil {
		t.Fatal("expected error for invalid epistemic type, got nil")
	}
	if !strings.Contains(err.Error(), "invalid epistemic type") {
		t.Fatalf("expected 'invalid epistemic type' in error, got: %v", err)
	}
}

func TestSerializeFactWithURL(t *testing.T) {
	f := fact.NewFact("general/test/url.md")
	f.Title = "URL ref"
	f.Body = "Body."
	f.Domain = []string{"web"}
	f.Confidence = 0.8
	f.Sources = 1
	f.Entities = []string{}
	f.Refs = []string{"https://example.com/path?q=1,2"}

	serialized := SerializeFact(f)
	parsed, err := ParseFact(f.Path(), serialized)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Refs) != 1 || parsed.Refs[0] != f.Refs[0] {
		t.Fatalf("refs: got %v want %v", parsed.Refs, f.Refs)
	}
}

func TestFactEvidenceWeightRoundTrip(t *testing.T) {
	f := fact.NewFact("kb/merged.md")
	f.Title = "Merged"
	f.Body = "Some body."
	f.Domain = []string{"testing"}
	f.Confidence = 0.9
	f.Sources = 5
	f.Entities = []string{}
	f.Refs = []string{}
	f.EvidenceWeight = 0.75

	serialized := SerializeFact(f)
	if !strings.Contains(serialized, "evidence_weight: 0.75") {
		t.Fatalf("evidence_weight missing from serialized output:\n%s", serialized)
	}
	parsed, err := ParseFact(f.Path(), serialized)
	if err != nil {
		t.Fatalf("ParseFact: %v", err)
	}
	if parsed.EvidenceWeight != 0.75 {
		t.Fatalf("EvidenceWeight: got %v, want 0.75", parsed.EvidenceWeight)
	}
}

func TestFactEvidenceWeightOmittedWhenZero(t *testing.T) {
	f := fact.NewFact("kb/plain.md")
	f.Title = "Plain"
	f.Body = "Body."
	f.Domain = []string{}
	f.Confidence = 0.5
	f.Sources = 1
	f.Entities = []string{}
	f.Refs = []string{}
	// EvidenceWeight zero — must not appear in output.

	serialized := SerializeFact(f)
	if strings.Contains(serialized, "evidence_weight") {
		t.Fatalf("evidence_weight should be absent for zero value:\n%s", serialized)
	}
}

func TestSerializeFactEmptyBody(t *testing.T) {
	f := fact.NewFact("general/empty.md")
	f.Title = "No body"
	f.Body = ""
	f.Domain = []string{}
	f.Confidence = 0.5
	f.Sources = 0
	f.Entities = []string{}
	f.Refs = []string{}

	got := SerializeFact(f)
	// Should end with the title line and no extra blank line.
	if !strings.HasSuffix(got, "---\n# No body\n") {
		t.Fatalf("unexpected serialized form for empty body: %q", got)
	}
}
