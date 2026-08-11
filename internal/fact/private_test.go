package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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

func TestIsWritablePrivatePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Agent areas: any name works. Nothing here knows the word "jobs".
		{".knomit/jobs/agentic-engineering/crawl-state.md", true},
		{".knomit/jobs/x.md", true},
		{".knomit/anything/x.md", true},
		{".knomit/runs/2026/08/x.md", true},

		// Server-owned: loose files at the namespace root.
		{".knomit/ontology.yaml", false},
		{".knomit/x.md", false},

		// Not the namespace root at all.
		{".knomit", false},
		{"", false},
		{"kb/architecture/x.md", false},
		{".github/workflows/ci.yml", false},
		{".domains/ontology.yaml", false},

		// Segment, not prefix: ".knomitjobs" is a different directory.
		{".knomitjobs/x.md", false},

		// Under the ontology root is NOT the namespace, even spelled alike.
		{"kb/.knomit/jobs/x.md", false},

		// Traversal escapes the namespace it claims to be inside. This is an
		// AUTHORIZATION predicate: its answer must not depend on a `..` check
		// living in another package (store.validatePath) that this one never
		// references, or moving the rule behind a new caller silently
		// authorizes a write anywhere in the tree.
		{".knomit/a/../../kb/x.md", false},
		{".knomit/../kb/x.md", false},
		{".knomit/jobs/../../../etc/passwd", false},
		{".knomit/jobs/../ontology.yaml", false},
		{".knomit/..", false},
		{".knomit/", false},

		// "." and empty segments defeat the DEPTH rule the same way: each of
		// these names a loose file at the namespace root once normalized, and
		// the second one is the server-owned ontology.
		{".knomit/./x.md", false},
		{".knomit/./ontology.yaml", false},
		{".knomit//x.md", false},
		{".knomit/jobs/", false},

		// A segment merely CONTAINING dots is a real directory name, not
		// traversal, and stays writable.
		{".knomit/jobs/..hidden/x.md", true},
		{".knomit/jobs/a..b/x.md", true},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, IsWritablePrivatePath(c.path), "path %q", c.path)
	}
}

// A writable private path is still PRIVATE: the two predicates answer
// different questions and must not be conflated.
func TestWritablePrivateIsStillPrivate(t *testing.T) {
	p := ".knomit/jobs/agentic-engineering/crawl-state.md"
	require.True(t, IsPrivatePath(p), "must stay excluded from discovery")
	require.True(t, IsWritablePrivatePath(p), "but writable")
}
