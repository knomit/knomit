package fact

import "testing"

func TestIsPrivatePath(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"ordinary fact", "kb/architecture/store/a1b2c3d4.md", false},
		{"leading segment", ".github/workflows/ci.yml", true},
		{"ontology dir", ".domains/ontology.yaml", true},
		{"middle segment", "kb/.drafts/a1b2c3d4.md", true},
		{"filename segment", "kb/architecture/.wip.md", true},
		{"deep middle segment", "kb/a/b/.c/d.md", true},
		{"root manifest", "README.md", false},
		{"licence", "LICENSE", false},
		{"dot inside a segment", "kb/architecture/v1.2/a1b2c3d4.md", false},
		{"parent traversal", "kb/../secrets.md", true},
		{"current dir", "./kb/a/b/c.md", true},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPrivatePath(tc.path); got != tc.want {
				t.Errorf("IsPrivatePath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
