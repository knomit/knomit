package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizePath_RepoRootDotPathsAreNotPrefixed(t *testing.T) {
	// A dot-prefixed FIRST segment means a repo-root path. Prefixing it would
	// produce kb/.knomit/…, and the refusal every fact tool gives for a dot
	// path would then name a path the caller never wrote.
	require.Equal(t, ".knomit/jobs/ae/crawl-state.md",
		NormalizePath("kb", ".knomit/jobs/ae/crawl-state.md"))

	// .md is still appended, and the path is still lowercased.
	require.Equal(t, ".knomit/jobs/ae/crawl-state.md",
		NormalizePath("kb", ".knomit/jobs/AE/Crawl-State"))
}

// TestNormalizePath_ArtifactsStayAtTheRepoRoot pins F25's placement:
// artifacts/ sits BESIDE the ontology root, so an artifacts/… path is never
// prefixed — which is what keeps it out of the index, the export, changes and
// triggers without a filter of their own. Case-insensitive on the root
// segment, because the whole path is lowercased on its way to git anyway.
func TestNormalizePath_ArtifactsStayAtTheRepoRoot(t *testing.T) {
	require.Equal(t, "artifacts/runs/x.md", NormalizePath("kb", "artifacts/runs/x"))
	require.Equal(t, "artifacts/runs/x.md", NormalizePath("kb", "artifacts/runs/x.md"))
	require.Equal(t, "artifacts/runs/x.md", NormalizePath("kb", "Artifacts/Runs/X"))
	// A kb/ topic named artifacts stays a kb/ path when given in full.
	require.Equal(t, "kb/artifacts/x.md", NormalizePath("kb", "kb/artifacts/x"))
	// A word that merely starts with "artifacts" is an ordinary topic.
	require.Equal(t, "kb/artifactsx/y.md", NormalizePath("kb", "artifactsx/y"))
}

func TestNormalizePath_OntologyRootPathsUnchanged(t *testing.T) {
	// The pre-existing behaviour must not regress.
	require.Equal(t, "kb/meta/x.md", NormalizePath("kb", "meta/x"))
	require.Equal(t, "kb/meta/x.md", NormalizePath("kb", "kb/meta/x.md"))
	require.Equal(t, "kb/meta/x.md", NormalizePath("kb", "kb/META/X.md"))
}
