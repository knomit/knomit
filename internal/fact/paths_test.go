package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizePath_RepoRootDotPathsAreNotPrefixed(t *testing.T) {
	// A dot-prefixed FIRST segment means a repo-root path. Prefixing it would
	// produce kb/.knomit/… — a path the write guard then refuses, making every
	// job-state write fail for a reason nobody could see from the arguments.
	require.Equal(t, ".knomit/jobs/ae/crawl-state.md",
		NormalizePath("kb", ".knomit/jobs/ae/crawl-state.md"))

	// .md is still appended, and the path is still lowercased.
	require.Equal(t, ".knomit/jobs/ae/crawl-state.md",
		NormalizePath("kb", ".knomit/jobs/AE/Crawl-State"))
}

func TestNormalizePath_OntologyRootPathsUnchanged(t *testing.T) {
	// The pre-existing behaviour must not regress.
	require.Equal(t, "kb/meta/x.md", NormalizePath("kb", "meta/x"))
	require.Equal(t, "kb/meta/x.md", NormalizePath("kb", "kb/meta/x.md"))
	require.Equal(t, "kb/meta/x.md", NormalizePath("kb", "kb/META/X.md"))
}
