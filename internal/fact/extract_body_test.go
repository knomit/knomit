package fact

import "testing"

// TestExtractBody covers the canonical body extractor shared by the store
// indexer and tools/calibrate: strip the YAML frontmatter and the leading
// "# Title" heading, returning the trimmed prose body.
func TestExtractBody(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "frontmatter and title stripped",
			raw:  "---\ntype: observation\n---\n# A Title\n\nThe body text.",
			want: "The body text.",
		},
		{
			name: "multiline body preserved",
			raw:  "---\ntype: observation\n---\n# T\n\nline one\n\nline two",
			want: "line one\n\nline two",
		},
		{
			name: "no frontmatter returns input unchanged",
			raw:  "just some text",
			want: "just some text",
		},
		{
			name: "title only, no body",
			raw:  "---\ntype: observation\n---\n# Only A Title",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExtractBody([]byte(c.raw)); got != c.want {
				t.Errorf("ExtractBody = %q, want %q", got, c.want)
			}
		})
	}
}
