//go:build sqlite_fts5

package store

import "testing"

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
