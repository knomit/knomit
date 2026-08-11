package fact

import (
	"strings"
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

		// The dotless rule binds the AREA segment only. Below it, a dotted
		// directory name is an ordinary name — nothing server-owned lives
		// that deep, so there is nothing there to shadow.
		{".knomit/runs/2026.08/x.md", true},

		// Server-owned: loose files at the namespace root.
		{".knomit/ontology.yaml", false},
		{".knomit/x.md", false},

		// A server-owned loose file must not be reachable as a DIRECTORY
		// either. Writing .knomit/ontology.yaml/x.md replaces the ontology
		// BLOB with a tree (store's buildTree drops the same-named entry
		// whatever its mode), after which the repo silently boots on the
		// default taxonomy. The depth rule alone protects the name, not the
		// name's reuse as a directory — hence the dotless-area rule.
		{".knomit/ontology.yaml/x.md", false},
		{".knomit/ontology.yaml/deeper/x.md", false},
		{".knomit/ONTOLOGY.YAML/x.md", false},

		// The rule is general, not a list of known filenames: any dotted area
		// is refused, so a server-owned loose file added later is covered by
		// the code that already exists.
		{".knomit/foo.bar/x.md", false},
		{".knomit/manifest.json/x.md", false},

		// A dot-PREFIXED area is a foreign tool's root smuggled inside the
		// namespace, and is refused by the same rule.
		{".knomit/.hidden/x.md", false},

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

		// ".." ANYWHERE, not just as a whole segment: store.validatePath
		// rejects any path CONTAINING "..", and batchWrite pre-flights every
		// path through it. An authorization predicate that says yes to what
		// the writer will refuse is a trap — learn is all-or-nothing, so the
		// doomed path takes the whole batch down at write time instead of
		// being refused up front with a comprehensible error.
		{".knomit/jobs/..hidden/x.md", false},
		{".knomit/jobs/a..b/x.md", false},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, IsWritablePrivatePath(c.path), "path %q", c.path)
	}
}

// TestServerOwnedLooseFilesAreDotted pins the premise the dotless-area rule
// rests on: a server-owned loose file inside PrivateRoot carries a dot in its
// name, so no writable area name can ever collide with it. Add
// ".knomit/manifest" (no extension) and this test fails — correctly, because
// such a file WOULD be shadowable as a directory and needs a reservedPrivate
// entry instead.
func TestServerOwnedLooseFilesAreDotted(t *testing.T) {
	for _, p := range []string{OntologyFile} {
		name, ok := strings.CutPrefix(p, PrivateRoot+"/")
		require.Truef(t, ok, "%s must sit inside %s", p, PrivateRoot)
		require.Containsf(t, name, ".",
			"%s must carry a dot: the dotless-area rule is what keeps it from being shadowed by a directory", p)
		require.Falsef(t, IsWritablePrivatePath(p+"/x.md"),
			"%s must not be reachable as a directory either", p)
	}
}

// A writable private path is still PRIVATE: the two predicates answer
// different questions and must not be conflated.
func TestWritablePrivateIsStillPrivate(t *testing.T) {
	p := ".knomit/jobs/agentic-engineering/crawl-state.md"
	require.True(t, IsPrivatePath(p), "must stay excluded from discovery")
	require.True(t, IsWritablePrivatePath(p), "but writable")
}
