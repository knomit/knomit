package store

import (
	"testing"
)

func TestParseFact_TypeField(t *testing.T) {
	content := "---\ntype: concept\ndomain: [test]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# A concept\n\nBody.\n"
	rec, err := parseFact("test/typed.md", content, "abc123")
	if err != nil {
		t.Fatalf("parseFact error: %v", err)
	}
	if rec.Type != "concept" {
		t.Fatalf("type: got %q want %q", rec.Type, "concept")
	}
}

func TestParseFact_TypeDefaultsToObservation(t *testing.T) {
	content := "---\ndomain: [test]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# No type\n\nBody.\n"
	rec, err := parseFact("test/no-type.md", content, "abc123")
	if err != nil {
		t.Fatalf("parseFact error: %v", err)
	}
	if rec.Type != "observation" {
		t.Fatalf("type: got %q want %q", rec.Type, "observation")
	}
}

func TestParseFact_AllEpistemicTypes(t *testing.T) {
	types := []string{"observation", "concept", "process", "principle", "pattern", "reference"}
	for _, et := range types {
		content := "---\ntype: " + et + "\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# Title\n\nBody.\n"
		rec, err := parseFact("test/"+et+".md", content, "abc")
		if err != nil {
			t.Fatalf("type %q: parseFact error: %v", et, err)
		}
		if rec.Type != et {
			t.Fatalf("type %q: got %q", et, rec.Type)
		}
	}
}

func TestParseFact_EvidenceWeight(t *testing.T) {
	content := "---\ndomain: [test]\nconfidence: 0.9\nsources: 5\nevidence_weight: 0.8\nentities: []\nrefs: []\n---\n# Weighted\n\nBody.\n"
	rec, err := parseFact("kb/weighted.md", content, "abc123")
	if err != nil {
		t.Fatalf("parseFact: %v", err)
	}
	if rec.EvidenceWeight != 0.8 {
		t.Fatalf("EvidenceWeight: got %v, want 0.8", rec.EvidenceWeight)
	}
}

func TestParseFact_HeadingVariants(t *testing.T) {
	tests := []struct {
		name          string
		content       string
		expectedTitle string
	}{
		{
			name: "double-hash heading",
			content: "---\ndomain: [test]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n## double-hash heading\n\nBody text.\n",
			expectedTitle: "double-hash heading",
		},
		{
			name: "hash-tag style heading preserves inner hash",
			content: "---\ndomain: [test]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# #tag-style heading\n\nBody text.\n",
			expectedTitle: "#tag-style heading",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := parseFact("test/fact.md", tc.content, "abc123")
			if err != nil {
				t.Fatalf("parseFact returned error: %v", err)
			}
			if rec.Title != tc.expectedTitle {
				t.Fatalf("expected title %q, got %q", tc.expectedTitle, rec.Title)
			}
		})
	}
}
