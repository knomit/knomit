// Internal tests for unexported replay helpers.
package git

import (
	"strings"
	"testing"
)

func TestReplaceRefsInYAML_AddNewRefs(t *testing.T) {
	// YAML block with no refs line — adding refs should append the line.
	yaml := "type: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []"
	newRefs := []string{"kb/other.md", "https://example.com/ref"}

	result := replaceRefsInYAML(yaml, newRefs)

	if !strings.Contains(result, "refs: [kb/other.md, https://example.com/ref]") {
		t.Errorf("expected refs line to be appended, got:\n%s", result)
	}
	// Existing fields should still be present.
	if !strings.Contains(result, "type: observation") {
		t.Errorf("expected type field preserved, got:\n%s", result)
	}
}

func TestReplaceRefsInYAML_RemoveRefs(t *testing.T) {
	// YAML block with a refs line — removing all refs should eliminate the line.
	yaml := "type: observation\ndomain: []\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: [kb/a.md, kb/b.md]"

	result := replaceRefsInYAML(yaml, nil)

	if strings.Contains(result, "refs:") {
		t.Errorf("expected refs line to be removed, got:\n%s", result)
	}
	// All other fields should remain.
	if !strings.Contains(result, "type: observation") {
		t.Errorf("expected type field preserved, got:\n%s", result)
	}
	if !strings.Contains(result, "entities: []") {
		t.Errorf("expected entities field preserved, got:\n%s", result)
	}
}

func TestSerializeRefList_QuotesSpecialChars(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  string
	}{
		{
			name:  "ref containing comma is quoted",
			input: []string{"kb/a,b.md"},
			want:  `["kb/a,b.md"]`,
		},
		{
			name:  "ref containing closing bracket is quoted",
			input: []string{"kb/a]b.md"},
			want:  `["kb/a]b.md"]`,
		},
		{
			name:  "ref containing double quote is escaped and quoted",
			input: []string{`kb/a"b.md`},
			want:  `["kb/a\"b.md"]`,
		},
		{
			name:  "plain ref is not quoted",
			input: []string{"kb/plain.md"},
			want:  "[kb/plain.md]",
		},
		{
			name:  "mixed: plain and special",
			input: []string{"kb/plain.md", "kb/a,b.md"},
			want:  `[kb/plain.md, "kb/a,b.md"]`,
		},
		{
			name:  "empty list",
			input: nil,
			want:  "[]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := serializeRefList(tc.input)
			if got != tc.want {
				t.Errorf("serializeRefList(%v) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseFrontmatterRefs_MissingOpening(t *testing.T) {
	content := "type: observation\ndomain: []\n# Fact\n\nBody.\n"
	_, _, _, err := parseFrontmatterRefs(content)
	if err == nil {
		t.Fatal("expected error for content without opening ---")
	}
	if !strings.Contains(err.Error(), "missing opening") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestParseFrontmatterRefs_MissingClosing(t *testing.T) {
	// Has opening --- but no closing ---
	content := "---\ntype: observation\ndomain: []\n# Fact\n\nBody.\n"
	_, _, _, err := parseFrontmatterRefs(content)
	if err == nil {
		t.Fatal("expected error for content without closing ---")
	}
	if !strings.Contains(err.Error(), "missing closing") {
		t.Errorf("unexpected error message: %v", err)
	}
}
