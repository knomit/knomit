package synthesize

import (
	"strings"
	"testing"
)

// TestValidateOutputPath_RejectsOutsideOntologyRoot regresses the bug where
// the distill prompt's hardcoded `knomit/...` example produced facts whose
// path lived outside the configured ontology root (`kb/` by default). The
// LLM emitted paths like `knomit/meta/reasoning/X.md` and the apply path
// wrote them silently. Now those paths are rejected with a warn.
func TestValidateOutputPath_RejectsOutsideOntologyRoot(t *testing.T) {
	cases := []struct {
		name         string
		path         string
		ontologyRoot string
		wantErr      bool
		wantSubstr   string
	}{
		{"under root", "kb/technology/ai/x.md", "kb", false, ""},
		{"under root different case in path", "KB/Technology/Ai/x.md", "kb", false, ""},
		{"under root different case in root", "kb/technology/ai/x.md", "KB", false, ""},
		{"different prefix — the bug", "knomit/meta/reasoning/X.md", "kb", true, "outside ontology root"},
		{"empty prefix", "meta/reasoning/X.md", "kb", true, "outside ontology root"},
		{"root without trailing slash", "kb", "kb", true, "outside ontology root"},
		{"sibling dir whose name starts with root", "kb-archive/x.md", "kb", true, "outside ontology root"},
		{"empty ontology root config", "kb/x.md", "", true, "ontology root not configured"},
		// Trailing slashes in ontology root must be tolerated. Without
		// trim, prefix becomes "kb//" and every legitimate "kb/foo.md"
		// is rejected.
		{"trailing slash on root", "kb/foo.md", "kb/", false, ""},
		{"multiple trailing slashes", "kb/foo.md", "kb//", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOutputPath(tc.path, tc.ontologyRoot)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateOutputPath(%q, %q) err = %v, wantErr = %v", tc.path, tc.ontologyRoot, err, tc.wantErr)
			}
			if tc.wantErr && tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q does not contain expected substring %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

// TestValidateOutputType_RejectsEmptyOrInvalid regresses the bug where
// LLM output JSON omitted the `type` field, leading the synthesize layer
// to write facts with `type:` blanks in frontmatter. The API silently
// masked these by defaulting to "observation" on read; the on-disk facts
// were genuinely typeless. Now empty/invalid types are rejected.
func TestValidateOutputType_RejectsEmptyOrInvalid(t *testing.T) {
	cases := []struct {
		name    string
		typ     string
		wantErr bool
	}{
		{"empty — the bug", "", true},
		{"observation", "observation", false},
		{"synthesis", "synthesis", false},
		{"methodology", "methodology", false},
		{"hypothesis", "hypothesis", false},
		{"concept", "concept", false},
		{"process", "process", false},
		{"principle", "principle", false},
		{"pattern", "pattern", false},
		{"reference", "reference", false},
		{"unknown invented type", "speculation", true},
		{"capitalized — Validate is case-sensitive", "Synthesis", true},
		{"whitespace", "  ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOutputType(tc.typ)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateOutputType(%q) err = %v, wantErr = %v", tc.typ, err, tc.wantErr)
			}
		})
	}
}
