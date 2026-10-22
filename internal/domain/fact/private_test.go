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

// TestIsArtifactPath pins the agents' working-file root (F25): artifacts/
// <area>/<name…>, at least one folder deep, with no empty segment, no segment
// beginning with "." at ANY depth, and no ".." anywhere.
func TestIsArtifactPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Any area works. Nothing here knows the word "jobs".
		{"artifacts/jobs/agentic-engineering/crawl-state.md", true},
		{"artifacts/runs/x.md", true},
		{"artifacts/anything/x.md", true},
		{"artifacts/runs/2026/08/x.md", true},
		// A dot INSIDE a segment is an ordinary name.
		{"artifacts/runs/2026.08/x.md", true},
		{"artifacts/v1.2/x.md", true},

		// Too shallow: a loose file at the root, or the root itself.
		{"artifacts/x.md", false},
		{"artifacts/", false},
		{"artifacts", false},

		// No dot segment at ANY depth — unlike the pre-F25 .knomit/<area>/
		// rule, which checked only <area>. A dot path is private, and the
		// fact tools never write one.
		{"artifacts/.hidden/x.md", false},
		{"artifacts/runs/.git/x.md", false},
		{"artifacts/runs/.x.md", false},

		// "." and empty segments, and ".." anywhere (store.validatePath's
		// rule: a predicate that authorizes what the writer refuses is a
		// trap).
		{"artifacts/./x.md", false},
		{"artifacts//x.md", false},
		{"artifacts/runs/", false},
		{"artifacts/runs/../../kb/x.md", false},
		{"artifacts/runs/a..b/x.md", false},

		// Judged as given: the caller lowercases a verbatim path first.
		{"Artifacts/runs/x.md", false},
		// Not the root: a prefix that merely starts with the word.
		{"artifactsx/runs/x.md", false},
		{"kb/artifacts/runs/x.md", false},

		// The system and every other dot root: never artifacts.
		{".knomit/jobs/x.md", false},
		{".knomit/skills/s/extra.md", false},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, IsArtifactPath(c.path), "path %q", c.path)
	}
}

// TestIsUnderArtifactsRoot is the case-insensitive "does the caller mean the
// artifacts folder" test the REST routes use on a verbatim path.
func TestIsUnderArtifactsRoot(t *testing.T) {
	for p, want := range map[string]bool{
		"artifacts":          true,
		"artifacts/x.md":     true,
		"ARTIFACTS/a/b.md":   true,
		"Artifacts/x.md":     true,
		"artifactsx/a/b.md":  false,
		"kb/artifacts/a.md":  false,
		".knomit/artifacts/": false,
	} {
		require.Equalf(t, want, IsUnderArtifactsRoot(p), "path %q", p)
	}
}

// TestKnomitIsClosedToTheFactTools is THE F25 rule at the predicate level:
// nothing under .knomit/ is a path the fact tools may open — not the agent
// areas that used to be writable (skills, recipes, triggers, guidance, runs),
// not the ontology, not the ontology reused as a directory.
func TestKnomitIsClosedToTheFactTools(t *testing.T) {
	for _, p := range []string{
		".knomit/guidance/x.md",
		".knomit/skills/work-task/extra.md",
		".knomit/skills/new-skill/skill.md",
		".knomit/recipes/x.md",
		".knomit/triggers/x.md",
		".knomit/runs/x.md",
		".knomit/jobs/agentic-engineering/crawl-state.md",
		".knomit/ontology.yaml",
		".knomit/ontology.yaml/x.md",
		".knomit/x.md",
	} {
		require.Truef(t, IsPrivatePath(p), "%s must be private", p)
		require.Falsef(t, IsArtifactPath(p), "%s must not be an artifact", p)
		require.Falsef(t, IsFactFilePath("kb", p), "%s must not be a fact-tool path", p)
	}
}
