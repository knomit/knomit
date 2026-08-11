package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPrivateNamespaceIsNotAFactPath pins the same isFactPath rule
// (factpath.go:48) that index_scope_test.go already covers for a generic
// dot-directory, but anchored on the concrete case this whole design exists
// to protect: job state a job writes through the fact tools at
// .knomit/<area>/… (see internal/fact.PrivateRoot).
//
// The fixture parses perfectly as a fact — the point being made is that
// location, not content, decides fact-index and Verify membership. If this
// ever regressed, job state would enter the index and Verify would report it
// as a "ghost" branch_facts row, exactly the failure mode
// TestIndex_SkipsFilesOutsideOntologyRoot pins for README.md.
func TestPrivateNamespaceIsNotAFactPath(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))
	ctx := context.Background()

	// A well-formed fact hand-placed at .knomit/jobs/x.md — the real shape a
	// job's write would take.
	_, err = svc.Facts().WriteFact(ctx, "agent/a", ".knomit/jobs/x.md",
		testFactBody("Job state", 0.9, nil), "add", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "agent/a", "kb/obs/real.md",
		testFactBody("Real", 0.9, nil), "add", "")
	require.NoError(t, err)

	require.Equal(t, []string{"kb/obs/real.md"}, indexedPaths(t, svc, "agent/a"),
		"job state under .knomit/ must never enter the fact index")

	// A rebuild must reach the same set — and must EVICT a private-namespace
	// stray an earlier build admitted, not merely stop adding new ones.
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "agent/a", nil))
	require.Equal(t, []string{"kb/obs/real.md"}, indexedPaths(t, svc, "agent/a"),
		"rebuild must not re-admit job state under .knomit/")

	// The whole point: Verify's expected set must agree, or job state surfaces
	// as a ghost branch_facts row on the next integrity run.
	report, err := svc.Verify(ctx, VerifyOpts{Deep: true})
	require.NoError(t, err)
	for _, iss := range report.Issues {
		require.NotEqual(t, CategoryFactsCoherence, iss.Category,
			"job state under .knomit/ must be invisible to Verify, not a ghost: %+v", iss)
	}
}
