package mcp

import (
	"strings"
	"testing"
)

func TestFactRoundTrip(t *testing.T) {
	f := Fact{
		Path:       "know/test/foo.md",
		Title:      "Test fact",
		Body:       "Some body text.",
		Domain:     []string{"testing"},
		Confidence: 0.9,
		Sources:    1,
		Entities:   []string{"foo"},
		Refs:       []string{},
	}
	serialized := SerializeFact(f)
	parsed, err := ParseFact(f.Path, serialized)
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
	if parsed.Path != f.Path {
		t.Fatalf("path: got %q want %q", parsed.Path, f.Path)
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
	f := Fact{
		Path:       "know/multi.md",
		Title:      "Multi-paragraph fact",
		Body:       "First paragraph.\n\nSecond paragraph with more detail.\n\nThird paragraph.",
		Domain:     []string{"testing", "multiline"},
		Confidence: 0.8,
		Sources:    3,
		Entities:   []string{"alpha", "beta"},
		Refs:       []string{"https://example.com"},
	}
	serialized := SerializeFact(f)
	parsed, err := ParseFact(f.Path, serialized)
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
	f := Fact{
		Path:       "know/fmt/test.md",
		Title:      "Format test",
		Body:       "Body content.",
		Domain:     []string{"testing"},
		Confidence: 0.9,
		Sources:    1,
		Entities:   []string{"foo"},
		Refs:       []string{},
	}
	got := SerializeFact(f)
	want := "---\ndomain: [testing]\nconfidence: 0.9\nsources: 1\nentities: [foo]\nrefs: []\n---\n# Format test\n\nBody content.\n"
	if got != want {
		t.Fatalf("SerializeFact output mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestSerializeFactEmptyBody(t *testing.T) {
	f := Fact{
		Path:       "know/empty.md",
		Title:      "No body",
		Body:       "",
		Domain:     []string{},
		Confidence: 0.5,
		Sources:    0,
		Entities:   []string{},
		Refs:       []string{},
	}
	got := SerializeFact(f)
	// Should end with the title line and no extra blank line.
	if !strings.HasSuffix(got, "---\n# No body\n") {
		t.Fatalf("unexpected serialized form for empty body: %q", got)
	}
}
